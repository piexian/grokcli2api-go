package grok

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func TestBypassProxy(t *testing.T) {
	tests := []struct {
		host     string
		patterns []string
		want     bool
	}{
		{"localhost", []string{"localhost"}, true},
		{"api.internal", []string{"*.internal"}, true},
		{"deep.api.example.com", []string{"example.com"}, true},
		{"10.2.3.4", []string{"10.0.0.0/8"}, true},
		{"cli-chat-proxy.grok.com", []string{"localhost", "*.internal"}, false},
	}
	for _, tt := range tests {
		if got := bypassProxy(tt.host, tt.patterns); got != tt.want {
			t.Errorf("bypassProxy(%q, %v) = %v, want %v", tt.host, tt.patterns, got, tt.want)
		}
	}
}

func TestHTTPClientKeepsWarmConnections(t *testing.T) {
	client, err := NewHTTPClient(config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if transport.IdleConnTimeout != 5*time.Minute || transport.MaxIdleConnsPerHost != 32 {
		t.Fatalf("idle timeout=%s per-host=%d", transport.IdleConnTimeout, transport.MaxIdleConnsPerHost)
	}
}

func TestPermanentAccountDenialDetection(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "new long error", status: http.StatusForbidden, body: `{"status_code":403,"error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials. If you believe this is a mistake, please log into ***.x.ai and update the permissions, or contact support."}`, want: true},
		{name: "old error", status: http.StatusForbidden, body: `{"error":"Access to the chat endpoint is denied. Please update the permissions."}`, want: true},
		{name: "nested error", status: http.StatusForbidden, body: `{"error":{"code":"permission_denied","message":"Access to the chat endpoint is denied. Please update the permissions."}}`, want: true},
		{name: "changed suffix", status: http.StatusForbidden, body: `{"error":"Access to the chat endpoint is denied. Please update permissions."}`, want: true},
		{name: "different case", status: http.StatusForbidden, body: `{"error":"ACCESS TO THE CHAT ENDPOINT IS DENIED. PLEASE UPDATE THE PERMISSIONS."}`},
		{name: "trailing whitespace", status: http.StatusForbidden, body: `{"error":"Access to the chat endpoint is denied. Please update the permissions. "}`, want: true},
		{name: "short denial", status: http.StatusForbidden, body: `{"error":"Access to the chat endpoint is denied."}`, want: true},
		{name: "raw log error", status: http.StatusForbidden, body: `status_code=403, Access to the chat endpoint is denied. Contact support.`, want: true},
		{name: "generic account denial", status: http.StatusForbidden, body: `{"error":"Access denied."}`},
		{name: "other forbidden", status: http.StatusForbidden, body: `{"error":"model access denied"}`},
		{name: "quota forbidden", status: http.StatusForbidden, body: `{"code":"personal-team-blocked:spending-limit","error":"quota exhausted"}`},
		{name: "unauthorized with keyword", status: http.StatusUnauthorized, body: `{"error":"Access to the chat endpoint is denied"}`},
		{name: "rate limited with keyword", status: http.StatusTooManyRequests, body: `{"error":"Access to the chat endpoint is denied"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.status, Header: http.Header{}}
			apiErr := parseAPIError(response, []byte(test.body))
			if got := isPermanentAccountDenial(apiErr); got != test.want {
				t.Fatalf("isPermanentAccountDenial() = %v, want %v; error=%#v", got, test.want, apiErr)
			}
		})
	}
}

func TestParseAPIErrorPreservesParameter(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}}
	err := parseAPIError(response, []byte(`{"error":{"type":"invalid_request_error","code":"invalid_value","message":"bad","param":"input[0]"}}`))
	if err.UpstreamCode != "invalid_value" || err.UpstreamMessage != "bad" || err.UpstreamParam != "input[0]" {
		t.Fatalf("parsed error = %#v", err)
	}
}

func TestFreeModelQuotaDetection(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "top-level error", status: http.StatusTooManyRequests, body: `{"status_code":429,"error":"You've used all the included free usage for model grok-4.5-build-free for now."}`, want: true},
		{name: "nested error", status: http.StatusTooManyRequests, body: `{"error":{"message":"YOU'VE USED ALL THE INCLUDED FREE USAGE FOR MODEL grok-build"}}`, want: true},
		{name: "ordinary rate limit", status: http.StatusTooManyRequests, body: `{"error":"too many requests"}`},
		{name: "different quota", status: http.StatusTooManyRequests, body: `{"error":"monthly credits exhausted"}`},
		{name: "matching text without 429", status: http.StatusForbidden, body: `{"error":"You've used all the included free usage for model grok-build"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{StatusCode: test.status, Header: http.Header{}}
			apiErr := parseAPIError(response, []byte(test.body))
			if got := isFreeModelQuotaExhausted(apiErr); got != test.want {
				t.Fatalf("isFreeModelQuotaExhausted() = %v, want %v; error=%#v", got, test.want, apiErr)
			}
		})
	}
}

func TestParseBillingInfoDetectsOnlyExplicitExhaustion(t *testing.T) {
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	periodEnd := "2026-07-21T08:02:22.599357+00:00"
	tests := []struct {
		name      string
		config    map[string]any
		exhausted bool
	}{
		{name: "usage available", config: map[string]any{"creditUsagePercent": 5.0, "currentPeriod": map[string]any{"end": periodEnd}}},
		{name: "included exhausted", config: map[string]any{"creditUsagePercent": 100.0}, exhausted: true},
		{name: "on demand remains", config: map[string]any{"creditUsagePercent": 100.0, "onDemandCap": map[string]any{"val": 5000}, "onDemandUsed": map[string]any{"val": 300}}},
		{name: "prepaid remains", config: map[string]any{"creditUsagePercent": 100.0, "prepaidBalance": map[string]any{"val": 100}}},
		{name: "negative prepaid is invalid", config: map[string]any{"creditUsagePercent": 100.0, "prepaidBalance": map[string]any{"val": -100}}, exhausted: true},
		{name: "legacy exhausted", config: map[string]any{"monthlyLimit": map[string]any{"val": 2000}, "used": map[string]any{"val": 2000}, "billingPeriodEnd": periodEnd}, exhausted: true},
		{name: "unknown usage", config: map[string]any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"config": test.config})
			if err != nil {
				t.Fatal(err)
			}
			info, err := parseBillingInfo(payload, now)
			if err != nil {
				t.Fatal(err)
			}
			if info.Exhausted != test.exhausted {
				t.Fatalf("billing = %#v, exhausted want %t", info, test.exhausted)
			}
			if test.name == "usage available" || test.name == "legacy exhausted" {
				wantEnd, err := time.Parse(time.RFC3339Nano, periodEnd)
				if err != nil {
					t.Fatal(err)
				}
				if info.PeriodEnd == nil || !info.PeriodEnd.Equal(wantEnd) {
					t.Fatalf("period end = %v, want %v", info.PeriodEnd, wantEnd)
				}
			}
		})
	}
}

func TestRefreshBillingCoolsAndRestoresPaidAccount(t *testing.T) {
	var exhausted atomic.Bool
	exhausted.Store(true)
	periodEnd := time.Now().Add(2 * time.Hour).UTC()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/billing" || r.URL.Query().Get("format") != "credits" {
			t.Errorf("path = %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
			return
		}
		usage := 10.0
		if exhausted.Load() {
			usage = 100.0
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"config": map[string]any{
			"creditUsagePercent": usage,
			"currentPeriod":      map[string]any{"end": periodEnd.Format(time.RFC3339Nano)},
			"onDemandCap":        map[string]any{"val": 0},
			"onDemandUsed":       map[string]any{"val": 0},
			"prepaidBalance":     map[string]any{"val": 0},
		}})
	}))
	defer upstream.Close()
	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "paid.json", "paid-subject", 4)
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, BillingRefreshInterval: 5 * time.Minute,
		RetryMaxAttempts: 1, QuotaCooldown: 24 * time.Hour, AffinityTTL: time.Hour, AffinityMaxEntries: 128,
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 128,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.RefreshBilling(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	info := pool.Credentials()[0]
	if info.Billing == nil || !info.Billing.Exhausted || info.CooldownUntil == nil {
		t.Fatalf("exhausted credential = %#v", info)
	}
	if delta := info.CooldownUntil.Sub(periodEnd); delta < -time.Second || delta > time.Second {
		t.Fatalf("cooldown until = %s, period end = %s", info.CooldownUntil, periodEnd)
	}
	if _, err := pool.Acquire(context.Background(), auth.Affinity{}, "grok-4.5", nil); err == nil {
		t.Fatal("billing-exhausted account remained schedulable")
	}

	exhausted.Store(false)
	if err := client.RefreshBilling(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	info = pool.Credentials()[0]
	if info.Billing == nil || info.Billing.Exhausted || info.CooldownUntil != nil {
		t.Fatalf("restored credential = %#v", info)
	}
	lease, err := pool.Acquire(context.Background(), auth.Affinity{}, "grok-4.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestRefreshModelsDiscoversEveryAccountAndPersistsCatalogsInState(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		calls.Add(1)
		models := []map[string]any{{"id": "grok-shared"}}
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			models = append(models, map[string]any{"id": "grok-alpha"})
		case "Bearer token-b":
			models = append(models, map[string]any{"id": "grok-beta"})
		default:
			t.Errorf("unexpected authorization header")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
	}))
	defer upstream.Close()

	dir := t.TempDir()
	writeModelTestCredential(t, dir, "account-a.json", "subject-a", "token-a")
	writeModelTestCredential(t, dir, "account-b.json", "subject-b", "token-b")
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 2, ModelsRefreshInterval: 6 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024, ClientName: "grok-shell",
		ClientVersion: "0.2.93", ClientSurface: "tui", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: cfg.ClientSurface, ReloadInterval: time.Hour, RefreshConcurrency: 2,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.RefreshModels(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model discovery calls = %d, want 2", got)
	}
	if got, want := strings.Join(pool.Models(), ","), "grok-alpha,grok-beta,grok-shared"; got != want {
		t.Fatalf("aggregated models = %q, want %q", got, want)
	}
	for _, id := range pool.AccountIDs() {
		if _, ok := pool.AccountDescriptor(id, "grok-shared"); !ok {
			t.Fatalf("account %s missing structured shared-model descriptor", id)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		payload, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := json.Unmarshal(payload, &raw); err != nil {
			t.Fatal(err)
		}
		if _, exists := raw["models"]; exists {
			t.Fatalf("credential %s was polluted with model metadata", entry.Name())
		}
		if _, exists := raw["models_updated_at"]; exists {
			t.Fatalf("credential %s was polluted with model refresh time", entry.Name())
		}
	}
	statePayload, err := os.ReadFile(filepath.Join(dir, ".grokcli2api-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(statePayload, &state); err != nil {
		t.Fatal(err)
	}
	if state["version"] != float64(2) {
		t.Fatalf("state version = %v, want 2", state["version"])
	}
	if catalogs, _ := state["catalogs"].(map[string]any); len(catalogs) != 2 {
		t.Fatalf("persisted catalogs = %d, want 2", len(catalogs))
	}
}

func TestRefreshAccountModelsUsesETagAndHandlesNotModified(t *testing.T) {
	var calls atomic.Int32
	var secondIfNoneMatch atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.Header().Set("ETag", `"catalog-1"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
				"id": "grok", "model": "wire-grok", "apiBackend": "responses",
			}}})
			return
		}
		secondIfNoneMatch.Store(r.Header.Get("If-None-Match"))
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	writeModelTestCredential(t, dir, "account.json", "subject", "token")
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, ModelsRefreshInterval: 6 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accountID := pool.AccountIDs()[0]
	statePath := filepath.Join(dir, ".grokcli2api-state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.refreshAccountModelsPath(context.Background(), accountID, "models", false); err != nil {
		t.Fatal(err)
	}
	deferred, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, deferred) {
		t.Fatal("background account model refresh bypassed the state checkpoint batch")
	}
	if err := pool.FlushState(); err != nil {
		t.Fatal(err)
	}
	flushed, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, flushed) {
		t.Fatal("explicit model state flush did not persist the refreshed catalog")
	}
	if err := client.RefreshAccountModels(context.Background(), accountID); err != nil {
		t.Fatal(err)
	}
	if got, _ := secondIfNoneMatch.Load().(string); got != `"catalog-1"` {
		t.Fatalf("If-None-Match = %q", got)
	}
	if descriptor, ok := pool.AccountDescriptor(accountID, "grok"); !ok || descriptor.WireModel != "wire-grok" {
		t.Fatalf("descriptor after 304 = %#v, %v", descriptor, ok)
	}
}

func TestRefreshModelsPreservesDeniedAccountAndCoolsIt(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"status_code":403,"error":"Access to the chat endpoint is denied. Please update the permissions."}`)
		case "Bearer token-b":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "grok-4"}}})
		default:
			t.Errorf("unexpected authorization header")
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	writeModelTestCredential(t, dir, "account-a.json", "subject-a", "token-a")
	writeModelTestCredential(t, dir, "account-b.json", "subject-b", "token-b")
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 2, ModelsRefreshInterval: 6 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 2,
		AffinityTTL: time.Hour, AffinityMaxEntries: 1024,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.RefreshModels(context.Background(), true); err == nil {
		t.Fatal("partial model refresh unexpectedly succeeded")
	}

	available := 0
	for _, id := range pool.AccountIDs() {
		lease, err := pool.AcquireAccount(context.Background(), id)
		if err != nil {
			continue
		}
		available++
		if lease.Session().Token != "token-b" {
			t.Fatalf("denied credential remained available: %q", lease.Session().Token)
		}
		lease.Release()
	}
	if available != 1 {
		t.Fatalf("available accounts=%d, want 1", available)
	}
	if _, err := os.Stat(filepath.Join(dir, "account-a.json")); err != nil {
		t.Fatalf("denied credential file was removed: %v", err)
	}
	var deniedInfo auth.CredentialInfo
	for _, info := range pool.Credentials() {
		if info.CooldownReason == permanentChatDenialReason {
			deniedInfo = info
			break
		}
	}
	if deniedInfo.ID == "" || deniedInfo.Status != "cooling_down" {
		t.Fatalf("denied credential info = %#v", deniedInfo)
	}
	if _, err := os.Stat(filepath.Join(dir, "account-b.json")); err != nil {
		t.Fatalf("unrelated credential file was removed: %v", err)
	}
}

func writeModelTestCredential(t *testing.T, dir, name, subject, token string) {
	t.Helper()
	raw := map[string]any{
		"access_token": token, "refresh_token": "refresh", "client_id": "client", "sub": subject,
		"expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func billingTestToken(subject string, tier int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"sub": subject, "tier": tier})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func writeBillingTestCredential(t *testing.T, dir, name, subject string, tier int64) {
	t.Helper()
	raw := map[string]any{
		"access_token":  billingTestToken(subject, tier),
		"refresh_token": "refresh", "client_id": "client", "sub": subject,
		"expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		"models":  []string{"grok-4.5"}, "models_updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyProvisionalDescriptorDefaultsToChatCompletions(t *testing.T) {
	for _, protocol := range []inference.Protocol{
		inference.ProtocolChatCompletions,
		inference.ProtocolResponses,
		inference.ProtocolMessages,
	} {
		descriptor := provisionalDescriptor(protocol, "legacy-model")
		if descriptor.Backend != modelcatalog.BackendChatCompletions || descriptor.WireModel != "legacy-model" {
			t.Fatalf("protocol %s provisional descriptor = %#v", protocol, descriptor)
		}
	}
}

func TestEventStreamPreservesSSEFieldsAndMultilineData(t *testing.T) {
	body := "event: response.output_text.delta\n" +
		"id: evt-1\nretry: 1000\n" +
		"data: {\"type\":\"response.output_text.delta\",\n" +
		"data: \"delta\":\"hello\"}\n\n"
	reader := strings.NewReader(body)
	stream := &EventStream{
		response: &http.Response{Body: io.NopCloser(reader)},
		scanner:  bufio.NewScanner(strings.NewReader(body)),
	}
	event, ok, err := stream.Next()
	if err != nil || !ok {
		t.Fatalf("Next() = %#v, %v, %v", event, ok, err)
	}
	if event.Event != "response.output_text.delta" || event.ID != "evt-1" || event.Retry != "1000" {
		t.Fatalf("event fields lost: %#v", event)
	}
	if string(event.Data) != "{\"type\":\"response.output_text.delta\",\n\"delta\":\"hello\"}" {
		t.Fatalf("data = %q", event.Data)
	}
}

func TestBuildInferenceHeadersUsesModernIdentity(t *testing.T) {
	turn := uint64(0)
	compactionAt := uint64(217600)
	compactionsRemaining := uint8(1)
	identity := RequestIdentity{
		RequestID: "req-1", SessionID: "session-1", ConversationID: "conv-1",
		AgentID: "agent-1", TurnIndex: &turn, Model: "wire-model",
		DeploymentID:       "deployment-1",
		CompactionAtTokens: &compactionAt, CompactionsRemaining: &compactionsRemaining,
	}
	cfg := config.Config{
		ClientName: "grok-shell", ClientVersion: "0.2.102", ClientSurface: "tui",
		ClientMode: "headless", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	h := BuildInferenceHeaders(cfg, auth.Session{Token: "token", UserID: "user-1"}, identity, false)
	want := map[string]string{
		"x-grok-client-version": "0.2.102", "x-grok-client-mode": "headless",
		"x-authenticateresponse": "authenticate-response", "x-xai-token-auth": "xai-grok-cli",
		"x-grok-agent-id": "agent-1", "x-grok-session-id": "session-1",
		"x-grok-conv-id": "conv-1", "x-grok-req-id": "req-1", "x-grok-turn-idx": "0",
		"x-grok-model-override": "wire-model", "x-grok-user-id": "user-1",
		"x-userid": "user-1", "x-grok-deployment-id": "deployment-1",
		"x-compaction-at": "217600", "x-compactions-remaining": "1",
	}
	for name, value := range want {
		if got := h.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	for _, legacy := range []string{"x-grok-conversation-id", "x-grok-session-id-legacy", "x-grok-request-id"} {
		if got := h.Get(legacy); got != "" {
			t.Errorf("legacy header %s unexpectedly set to %q", legacy, got)
		}
	}
}

func TestIdleSessionRefreshesModelsV2BeforeRenderingNextAttempt(t *testing.T) {
	var modelRefreshes atomic.Int32
	var inferencePaths []string
	var inferenceBodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models-v2" {
			modelRefreshes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{
				"id": "grok", "model": "wire-chat", "apiBackend": "chat_completions",
				"supportsReasoningEffort": false,
			}}})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode inference body: %v", err)
		}
		inferencePaths = append(inferencePaths, r.URL.Path)
		inferenceBodies = append(inferenceBodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "ok"})
	}))
	defer upstream.Close()

	dir := t.TempDir()
	writeModelTestCredential(t, dir, "account.json", "subject", "token")
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, RetryMaxAttempts: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 32,
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "headless", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 32,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	accountID := pool.AccountIDs()[0]
	if err := pool.UpdateModelDescriptors(accountID, []modelcatalog.ModelDescriptor{{
		ID: "grok", WireModel: "wire-responses", Backend: modelcatalog.BackendResponses,
		SupportsReasoningEffort: true, ReasoningEfforts: []string{"high"}, SupportedInAPI: true,
	}}, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	plan, err := inference.NewRequestPlan(inference.ProtocolResponses, map[string]any{
		"model": "grok", "input": "hello", "reasoning": map[string]any{"effort": "high"},
	}, inference.PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DoInference(context.Background(), plan, InferenceOptions{}); err != nil {
		t.Fatal(err)
	}
	if modelRefreshes.Load() != 0 {
		t.Fatalf("first inference unexpectedly refreshed models-v2")
	}
	client.lastInference.Store(accountID, time.Now().Add(-11*time.Minute))
	if _, err := client.DoInference(context.Background(), plan, InferenceOptions{}); err != nil {
		t.Fatal(err)
	}
	if modelRefreshes.Load() != 1 {
		t.Fatalf("models-v2 calls=%d", modelRefreshes.Load())
	}
	if len(inferencePaths) != 2 || inferencePaths[0] != "/v1/responses" || inferencePaths[1] != "/v1/chat/completions" {
		t.Fatalf("inference paths=%#v", inferencePaths)
	}
	if inferenceBodies[0]["model"] != "wire-responses" || inferenceBodies[1]["model"] != "wire-chat" {
		t.Fatalf("wire models=%#v", inferenceBodies)
	}
	if inferenceBodies[1]["reasoning_effort"] != "low" {
		t.Fatalf("refreshed effort=%#v", inferenceBodies[1]["reasoning_effort"])
	}
}

func TestRendererRejectionDoesNotConsumeTransportAttemptBudget(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-responses" {
			t.Errorf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_1", "status": "completed", "output": []any{}})
	}))
	defer upstream.Close()

	dir := t.TempDir()
	writeModelTestCredential(t, dir, "messages.json", "subject-messages", "token-messages")
	writeModelTestCredential(t, dir, "responses.json", "subject-responses", "token-responses")
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, RetryMaxAttempts: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 32,
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "headless", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 32,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, accountID := range pool.AccountIDs() {
		lease, err := pool.AcquireAccount(context.Background(), accountID)
		if err != nil {
			t.Fatal(err)
		}
		backend := modelcatalog.BackendMessages
		if lease.Session().Token == "token-responses" {
			backend = modelcatalog.BackendResponses
		}
		lease.Release()
		if err := pool.UpdateModelDescriptors(accountID, []modelcatalog.ModelDescriptor{{
			ID: "grok", WireModel: "wire-" + string(backend), Backend: backend,
		}}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The native Messages renderer drops the invalid in-band system role and
	// has no message left. The Responses renderer can still express the valid
	// top-level system instruction, so selecting that account must not be
	// prevented by RetryMaxAttempts=1.
	plan, err := inference.NewRequestPlan(inference.ProtocolMessages, map[string]any{
		"model": "grok", "max_tokens": float64(8), "system": "stay concise",
		"messages": []any{map[string]any{"role": "system", "content": "drop me"}},
	}, inference.PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.DoInference(context.Background(), plan, InferenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Attempt.Backend != modelcatalog.BackendResponses || calls.Load() != 1 {
		t.Fatalf("backend=%q transport calls=%d", result.Attempt.Backend, calls.Load())
	}
}

func TestUnauthorizedRetryRerendersForDifferentAccountDescriptor(t *testing.T) {
	type capturedAttempt struct {
		path, authorization, requestID, sessionID, turn string
		body                                            map[string]any
	}
	var attempts []capturedAttempt
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		attempts = append(attempts, capturedAttempt{
			path: r.URL.Path, authorization: r.Header.Get("Authorization"),
			requestID: r.Header.Get("x-grok-req-id"), sessionID: r.Header.Get("x-grok-session-id"),
			turn: r.Header.Get("x-grok-turn-idx"), body: body,
		})
		if r.Header.Get("Authorization") == "Bearer token-responses" {
			writeJSONResponse(t, w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized", "message": "expired"}})
			return
		}
		writeJSONResponse(t, w, http.StatusOK, map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant",
			"content":     []any{map[string]any{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer upstream.Close()

	dir := t.TempDir()
	for name, credential := range map[string]map[string]any{
		"responses.json": {"access_token": "token-responses", "sub": "subject-responses", "expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)},
		"messages.json":  {"access_token": "token-messages", "sub": "subject-messages", "expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)},
	} {
		payload, err := json.Marshal(credential)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, RetryMaxAttempts: 2,
		RetryBaseDelay: time.Millisecond, AffinityTTL: time.Hour, AffinityMaxEntries: 32,
		ClientVersion: "0.2.102", ClientMode: "headless", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "headless", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 32,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, accountID := range pool.AccountIDs() {
		lease, err := pool.AcquireAccount(context.Background(), accountID)
		if err != nil {
			t.Fatal(err)
		}
		token := lease.Session().Token
		lease.Release()
		descriptor := modelcatalog.ModelDescriptor{
			ID: "grok", SupportedInAPI: true, SupportsReasoningEffort: true,
		}
		switch token {
		case "token-responses":
			descriptor.WireModel = "wire-responses"
			descriptor.Backend = modelcatalog.BackendResponses
			descriptor.ReasoningEfforts = []string{"high"}
		case "token-messages":
			descriptor.WireModel = "wire-messages"
			descriptor.Backend = modelcatalog.BackendMessages
			descriptor.ReasoningEfforts = []string{"medium"}
		default:
			t.Fatalf("unexpected test token")
		}
		if err := pool.UpdateModelDescriptors(accountID, []modelcatalog.ModelDescriptor{descriptor}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	plan, err := inference.NewRequestPlan(inference.ProtocolResponses, map[string]any{
		"model": "grok", "input": "hello", "reasoning": map[string]any{"effort": "high"},
		"tools": []any{map[string]any{
			"type": "namespace", "name": "ns__", "tools": []any{map[string]any{
				"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"},
			}},
		}},
	}, inference.PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	turn := uint64(0)
	result, err := client.DoInference(context.Background(), plan, InferenceOptions{Identity: RequestIdentity{
		RequestID: "logical-request", SessionID: "logical-session", ConversationID: "logical-session", TurnIndex: &turn,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("transport attempts = %d, want 2", len(attempts))
	}
	first, second := attempts[0], attempts[1]
	if first.path != "/v1/responses" || first.authorization != "Bearer token-responses" || first.body["model"] != "wire-responses" {
		t.Fatalf("first attempt = %#v", first)
	}
	firstReasoning, _ := first.body["reasoning"].(map[string]any)
	firstTools, _ := first.body["tools"].([]any)
	if firstReasoning["effort"] != "high" || len(firstTools) != 1 || firstTools[0].(map[string]any)["name"] != "ns__lookup" {
		t.Fatalf("first attempt was not rendered for Responses: %#v", first.body)
	}
	if second.path != "/v1/messages" || second.authorization != "Bearer token-messages" || second.body["model"] != "wire-messages" {
		t.Fatalf("second attempt = %#v", second)
	}
	secondOutput, _ := second.body["output_config"].(map[string]any)
	secondTools, _ := second.body["tools"].([]any)
	if secondOutput["effort"] != "low" || second.body["reasoning"] != nil || len(secondTools) != 1 || secondTools[0].(map[string]any)["name"] != "ns__lookup" || second.body["tool_choice"] != nil {
		t.Fatalf("second attempt was not rerendered and silently cleaned: %#v", second.body)
	}
	if first.requestID != second.requestID || first.sessionID != second.sessionID || first.turn != second.turn ||
		first.requestID != "logical-request" || first.sessionID != "logical-session" || first.turn != "0" {
		t.Fatalf("logical identity changed across retry: first=%#v second=%#v", first, second)
	}
	alias := result.Attempt.Adapter.ToolAliases["ns__lookup"]
	if result.Attempt.Backend != modelcatalog.BackendMessages || result.Attempt.ReasoningEffort != "low" || alias.Name != "lookup" || alias.Namespace != "ns__" {
		t.Fatalf("result attempt = %#v", result.Attempt)
	}
}

func TestChatDenialKeywordCoolsOnlyMatchingScope(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer token-denied" {
			t.Errorf("authorization = %q", got)
		}
		writeJSONResponse(t, w, http.StatusForbidden, map[string]any{
			"status_code": float64(403),
			"error":       permanentChatDenialKeyword + ". Please ensure you're using the correct credentials. Contact support.",
		})
	}))
	defer upstream.Close()

	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	credential := map[string]any{"tokens": map[string]any{
		"scope-denied": map[string]any{
			"key": "token-denied", "auth_mode": "web_login", "user_id": "user-denied",
			"principal_type": "team", "principal_id": "principal-denied", "expires_at": expires,
		},
		"scope-keep": map[string]any{
			"key": "token-keep", "auth_mode": "web_login", "user_id": "user-keep",
			"principal_type": "team", "principal_id": "principal-keep", "expires_at": expires,
		},
	}}
	payload, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "headless", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 32,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var deniedID, keepID string
	for _, accountID := range pool.AccountIDs() {
		lease, err := pool.AcquireAccount(context.Background(), accountID)
		if err != nil {
			t.Fatal(err)
		}
		token := lease.Session().Token
		lease.Release()
		descriptor := modelcatalog.ModelDescriptor{
			WireModel: "wire-keep", Backend: modelcatalog.BackendChatCompletions, SupportedInAPI: true,
		}
		switch token {
		case "token-denied":
			deniedID = accountID
			descriptor.ID, descriptor.WireModel = "grok-denied", "wire-denied"
		case "token-keep":
			keepID = accountID
			descriptor.ID = "grok-keep"
		default:
			t.Fatalf("unexpected token %q", token)
		}
		if err := pool.UpdateModelDescriptors(accountID, []modelcatalog.ModelDescriptor{descriptor}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if deniedID == "" || keepID == "" {
		t.Fatalf("logical accounts not found: denied=%q keep=%q", deniedID, keepID)
	}

	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, RetryMaxAttempts: 1,
		RetryBaseDelay: time.Millisecond, AffinityTTL: time.Hour, AffinityMaxEntries: 32,
		ClientVersion: "0.2.102", ClientMode: "headless", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	plan, err := inference.NewRequestPlan(inference.ProtocolChatCompletions, map[string]any{
		"model": "grok-denied", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, inference.PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.DoInference(context.Background(), plan, InferenceOptions{}); err == nil {
		t.Fatal("keyword denial unexpectedly succeeded")
	} else {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !isPermanentAccountDenial(apiErr) {
			t.Fatalf("error = %#v, want keyword denial", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	deniedInfo, ok := pool.Credential(deniedID)
	if !ok {
		t.Fatal("denied logical credential was removed from the pool")
	}
	if cooldown, cooling := deniedInfo.ModelCooldowns["grok-denied"]; !cooling || cooldown.Reason != permanentChatDenialReason {
		t.Fatalf("denied model cooldown = %#v", deniedInfo.ModelCooldowns)
	}
	if _, ok := pool.Credential(keepID); !ok {
		t.Fatal("sibling logical credential was removed")
	}

	written, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(written, &root); err != nil {
		t.Fatal(err)
	}
	tokens, _ := root["tokens"].(map[string]any)
	if _, exists := tokens["scope-denied"]; !exists {
		t.Fatalf("denied scope was removed from disk: %s", written)
	}
	if _, exists := tokens["scope-keep"]; !exists || len(tokens) != 2 {
		t.Fatalf("sibling scopes were not preserved: %s", written)
	}
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, status int, payload map[string]any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Error(err)
	}
}

func TestPinnedInferenceRejectsBackendChangeBeforeTransport(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	writeModelTestCredential(t, dir, "account.json", "subject", "token")
	cfg := config.Config{
		ChatProxyBaseURL: upstream.URL, ChatProxyVersion: "v1", AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 1, RetryMaxAttempts: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 32,
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "headless", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 32,
	}, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	accountID := pool.AccountIDs()[0]
	if err := pool.UpdateModelDescriptors(accountID, []modelcatalog.ModelDescriptor{{
		ID: "grok", WireModel: "wire-responses", Backend: modelcatalog.BackendResponses,
	}}, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(cfg, pool, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	plan, err := inference.NewRequestPlan(inference.ProtocolMessages, map[string]any{
		"model": "grok", "max_tokens": float64(8),
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, inference.PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	options := InferenceOptions{
		Affinity:      auth.Affinity{Tenant: "tenant", Key: "state", Mode: auth.AffinityHard},
		PinnedAccount: accountID, ExpectedBackend: modelcatalog.BackendMessages,
	}
	assertUnavailable := func(t *testing.T, err error) {
		t.Helper()
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.UpstreamCode != "upstream_state_unavailable" {
			t.Fatalf("error = %#v", err)
		}
	}
	t.Run("non-stream", func(t *testing.T) {
		_, err := client.DoInference(context.Background(), plan, options)
		assertUnavailable(t, err)
	})
	t.Run("stream", func(t *testing.T) {
		_, err := client.OpenInference(context.Background(), plan, options)
		assertUnavailable(t, err)
	})
	if calls.Load() != 0 {
		t.Fatalf("backend mismatch reached upstream %d times", calls.Load())
	}
}

func TestAPIKeyUsesConfiguredXAIOriginWithoutProxyAuthHeaders(t *testing.T) {
	cfg := config.Config{
		ChatProxyBaseURL: "https://proxy.example", ChatProxyVersion: "private-v9",
		XAIAPIBaseURL: "https://xai.example", ClientVersion: "0.2.102",
		ClientMode: "headless", ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
	}
	session := auth.Session{Token: "api-key", AuthMode: auth.AuthModeAPIKey}
	client := &Client{cfg: cfg}
	if got := client.URLForSession(session, "responses"); got != "https://xai.example/v1/responses" {
		t.Fatalf("API key URL = %q", got)
	}
	if got := client.URLForSession(auth.Session{Token: "oauth", AuthMode: auth.AuthModeOIDC}, "responses"); got != "https://proxy.example/private-v9/responses" {
		t.Fatalf("CLI proxy URL = %q", got)
	}
	h := BuildInferenceHeaders(cfg, session, RequestIdentity{RequestID: "req"}, false)
	if h.Get("x-xai-token-auth") != "" || h.Get("x-authenticateresponse") != "" {
		t.Fatalf("proxy auth headers leaked to xAI origin: %#v", h)
	}
}

func TestEventStreamStripsUTF8BOM(t *testing.T) {
	body := "\uFEFFevent: message\ndata: {\"ok\":true}\n\n"
	stream := &EventStream{
		response: &http.Response{Body: io.NopCloser(strings.NewReader(body))},
		scanner:  newSSEScanner(strings.NewReader(body)),
	}
	event, ok, err := stream.Next()
	if err != nil || !ok {
		t.Fatalf("Next() = %#v, %v, %v", event, ok, err)
	}
	if event.Event != "message" || string(event.Data) != `{"ok":true}` {
		t.Fatalf("BOM event = %#v", event)
	}
}

func TestEventStreamAcceptsEventLargerThan32MiB(t *testing.T) {
	const chunkSize = 1 << 20
	var body strings.Builder
	for i := 0; i < 33; i++ {
		body.WriteString("data: ")
		body.WriteString(strings.Repeat("x", chunkSize))
		body.WriteByte('\n')
	}
	body.WriteByte('\n')
	reader := strings.NewReader(body.String())
	stream := &EventStream{response: &http.Response{Body: io.NopCloser(reader)}, scanner: newSSEScanner(reader)}
	event, ok, err := stream.Next()
	if err != nil || !ok {
		t.Fatalf("Next() ok=%v err=%v", ok, err)
	}
	if len(event.Data) <= 32<<20 {
		t.Fatalf("event data size = %d, want >32 MiB", len(event.Data))
	}
}

func TestMalformedGzipIsExplicitError(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"gzip"}},
		Body:   io.NopCloser(strings.NewReader("not-gzip")),
	}
	if _, err := readResponseBody(resp, 1024); err == nil {
		t.Fatal("readResponseBody() unexpectedly accepted malformed gzip")
	}
}

func TestValidGzipResponseIsDecoded(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte("decoded"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{
		Header: http.Header{"Content-Encoding": []string{"gzip"}},
		Body:   io.NopCloser(bytes.NewReader(compressed.Bytes())),
	}
	got, err := readResponseBody(resp, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "decoded" {
		t.Fatalf("decoded body = %q", got)
	}
}

func TestEventStreamIdleTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := newIdleReadCloser(reader, 20*time.Millisecond)
	stream := &EventStream{response: &http.Response{Body: body}, scanner: newSSEScanner(body)}
	_, ok, err := stream.Next()
	if ok {
		t.Fatal("idle stream unexpectedly produced an event")
	}
	var idleErr *StreamIdleTimeoutError
	if !errors.As(err, &idleErr) {
		t.Fatalf("Next() error = %v, want StreamIdleTimeoutError", err)
	}
}

func testInferencePlan(t *testing.T) *inference.RequestPlan {
	t.Helper()
	plan, err := inference.NewRequestPlan(inference.ProtocolChatCompletions, map[string]any{
		"model":    "grok-4.5",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, inference.PlanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func newInferenceTestPool(t *testing.T, dir string, client *http.Client) *auth.Pool {
	t.Helper()
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "headless", ReloadInterval: time.Hour, RefreshConcurrency: 2, AccountMaxInflight: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 64,
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
func useResponsesBackend(t *testing.T, pool *auth.Pool) {
	t.Helper()
	for _, id := range pool.AccountIDs() {
		if err := pool.UpdateModelDescriptors(id, []modelcatalog.ModelDescriptor{{
			ID: "grok-4.5", WireModel: "grok-4.5", Backend: modelcatalog.BackendResponses,
		}}, "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func newInferenceTestClient(t *testing.T, dir string, pool *auth.Pool, client *http.Client, buildURL, xaiURL string, attempts int) *Client {
	t.Helper()
	cfg := config.Config{
		ChatProxyBaseURL: buildURL, ChatProxyVersion: "v1", XAIAPIBaseURL: xaiURL, AuthsDir: dir,
		AuthsReloadInterval: time.Hour, AuthRefreshConcurrency: 2, RetryMaxAttempts: attempts,
		RetryBaseDelay: time.Millisecond, RateLimitCooldown: time.Minute, QuotaCooldown: 24 * time.Hour,
		AffinityTTL: time.Hour, AffinityMaxEntries: 64, ClientVersion: "test", ClientMode: "headless",
		ClientIdentifier: "grok-shell", ClientSurface: "headless", ClientName: "grok", TokenAuth: "xai-grok-cli",
	}
	clientUnderTest, err := NewClient(cfg, pool, client)
	if err != nil {
		t.Fatal(err)
	}
	return clientUnderTest
}

func TestBuild403FallsBackToXAIAndPersistsMarker(t *testing.T) {
	var buildCalls, xaiCalls atomic.Int32
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buildCalls.Add(1)
		writeJSONResponse(t, w, http.StatusForbidden, map[string]any{
			"status_code": float64(http.StatusForbidden), "code": "permission-denied", "error": "permission denied",
		})
	}))
	defer build.Close()
	xai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xaiCalls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("xai path = %q", r.URL.Path)
		}
		writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "chatcmpl-fallback", "choices": []any{}})
	}))
	defer xai.Close()

	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "paid.json", "paid-subject", 4)
	pool := newInferenceTestPool(t, dir, build.Client())
	useResponsesBackend(t, pool)
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, xai.URL, 1)
	result, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Payload["id"] != "chatcmpl-fallback" || buildCalls.Load() != 1 || xaiCalls.Load() != 1 {
		t.Fatalf("result=%#v build=%d xai=%d", result.Payload, buildCalls.Load(), xaiCalls.Load())
	}
	info, ok := pool.Credential(result.AccountID)
	if !ok || !info.APIFallback || info.EffectiveInferenceRoute != auth.InferencePlaneBuild {
		t.Fatalf("routing info = %#v, %v", info.BuildRoutingInfo, ok)
	}
	if _, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{}); err != nil {
		t.Fatal(err)
	}
	if buildCalls.Load() != 2 || xaiCalls.Load() != 2 {
		t.Fatalf("observed fallback marker build=%d xai=%d", buildCalls.Load(), xaiCalls.Load())
	}
	client.Close()
	pool.Close()

	reloaded := newInferenceTestPool(t, dir, build.Client())
	defer reloaded.Close()
	reloadedInfo, ok := reloaded.Credential(result.AccountID)
	if !ok || !reloadedInfo.APIFallback || reloadedInfo.EffectiveInferenceRoute != auth.InferencePlaneBuild {
		t.Fatalf("reloaded routing info = %#v, %v", reloadedInfo.BuildRoutingInfo, ok)
	}
}

func TestUnknownBuild403ProbesSubscriptionBeforeXAI(t *testing.T) {
	var buildCalls, billingCalls, userCalls, xaiCalls atomic.Int32
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/billing":
			billingCalls.Add(1)
			writeJSONResponse(t, w, http.StatusOK, map[string]any{"config": map[string]any{"currentPeriod": map[string]any{"end": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}})
		case "/v1/user":
			userCalls.Add(1)
			writeJSONResponse(t, w, http.StatusOK, map[string]any{"subscriptionTier": "SuperGrok"})
		case "/v1/responses":
			buildCalls.Add(1)
			writeJSONResponse(t, w, http.StatusForbidden, map[string]any{"code": "permission-denied", "error": "permission denied"})
		default:
			t.Fatalf("unexpected Build path %q", r.URL.Path)
		}
	}))
	defer build.Close()
	xai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xaiCalls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("xai path = %q", r.URL.Path)
		}
		writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "chatcmpl-unknown-fallback", "choices": []any{}})
	}))
	defer xai.Close()

	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "unknown.json", "unknown-subject", 99)
	pool := newInferenceTestPool(t, dir, build.Client())
	useResponsesBackend(t, pool)
	defer pool.Close()
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, xai.URL, 1)
	defer client.Close()
	result, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Payload["id"] != "chatcmpl-unknown-fallback" || buildCalls.Load() != 1 || billingCalls.Load() != 1 || userCalls.Load() != 1 || xaiCalls.Load() != 1 {
		t.Fatalf("result=%#v build=%d billing=%d user=%d xai=%d", result.Payload, buildCalls.Load(), billingCalls.Load(), userCalls.Load(), xaiCalls.Load())
	}
	info, ok := pool.Credential(result.AccountID)
	if !ok || !info.SuperEntitled || info.Billing == nil || info.Billing.PlanName != "SuperGrok" {
		t.Fatalf("unknown entitlement info = %#v, %v", info, ok)
	}
	if info.EffectiveInferenceRoute != auth.InferencePlaneBuild {
		t.Fatalf("fallback marker changed future route: %#v", info.BuildRoutingInfo)
	}
}

func TestRejectedXAIFallbackAppliesAccountLifecycle(t *testing.T) {
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusForbidden, map[string]any{
			"status_code": float64(http.StatusForbidden), "code": "permission-denied", "error": "permission denied",
		})
	}))
	defer build.Close()
	xai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusPaymentRequired, map[string]any{
			"status_code": float64(http.StatusPaymentRequired), "code": "quota-exhausted", "error": "payment required",
		})
	}))
	defer xai.Close()

	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "paid.json", "fallback-402-subject", 4)
	pool := newInferenceTestPool(t, dir, build.Client())
	useResponsesBackend(t, pool)
	defer pool.Close()
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, xai.URL, 1)
	defer client.Close()
	_, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
		t.Fatalf("error = %v, want original Build 403", err)
	}
	info, ok := pool.Credential(pool.AccountIDs()[0])
	if !ok || info.CooldownReason != paymentRequiredReason || info.CooldownUntil == nil {
		t.Fatalf("credential lifecycle = %#v, %v", info, ok)
	}
}

func TestStreamingBuild403FallsBackToXAI(t *testing.T) {
	var buildCalls, xaiCalls atomic.Int32
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buildCalls.Add(1)
		writeJSONResponse(t, w, http.StatusForbidden, map[string]any{
			"status_code": float64(http.StatusForbidden), "code": "permission-denied", "error": "permission denied",
		})
	}))
	defer build.Close()
	xai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xaiCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer xai.Close()

	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "paid.json", "stream-subject", 4)
	pool := newInferenceTestPool(t, dir, build.Client())
	useResponsesBackend(t, pool)
	defer pool.Close()
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, xai.URL, 1)
	defer client.Close()
	stream, err := client.OpenInference(context.Background(), testInferencePlan(t), InferenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, ok, err := stream.Next()
	if err != nil || !ok || !strings.Contains(string(event.Data), "response.completed") {
		t.Fatalf("event=%#v ok=%v err=%v", event, ok, err)
	}
	if buildCalls.Load() != 1 || xaiCalls.Load() != 1 {
		t.Fatalf("stream fallback build=%d xai=%d", buildCalls.Load(), xaiCalls.Load())
	}
}
func TestChatEndpointDenialDoesNotProbeXAI(t *testing.T) {
	var xaiCalls atomic.Int32
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusForbidden, map[string]any{
			"status_code": float64(http.StatusForbidden), "error": permanentChatDenialKeyword,
		})
	}))
	defer build.Close()
	xai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		xaiCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer xai.Close()

	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "paid.json", "paid-subject", 4)
	pool := newInferenceTestPool(t, dir, build.Client())
	useResponsesBackend(t, pool)
	defer pool.Close()
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, xai.URL, 1)
	defer client.Close()
	if _, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{}); err == nil {
		t.Fatal("chat endpoint denial unexpectedly succeeded")
	}
	if xaiCalls.Load() != 0 {
		t.Fatalf("chat endpoint denial probed XAI %d times", xaiCalls.Load())
	}
	info := pool.Credentials()[0]
	if info.Status != "cooling_down" || info.ModelCooldowns["grok-4.5"].Reason != permanentChatDenialReason {
		t.Fatalf("chat endpoint denial info = %#v", info)
	}
}
func TestForcedXAIUsesBuildForUnsupportedBackend(t *testing.T) {
	var buildCalls, xaiCalls atomic.Int32
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		buildCalls.Add(1)
		writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "chatcmpl-build", "choices": []any{}})
	}))
	defer build.Close()
	xai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		xaiCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer xai.Close()

	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "paid.json", "paid-subject", 4)
	pool := newInferenceTestPool(t, dir, build.Client())
	defer pool.Close()
	id := pool.AccountIDs()[0]
	mode := auth.BuildRouteXAI
	if _, err := pool.UpdateBuildRouting(id, auth.BuildRoutingUpdate{RouteMode: &mode}); err != nil {
		t.Fatal(err)
	}
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, xai.URL, 1)
	defer client.Close()
	if _, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{}); err != nil {
		t.Fatal(err)
	}
	if buildCalls.Load() != 1 || xaiCalls.Load() != 0 {
		t.Fatalf("unsupported backend build=%d xai=%d", buildCalls.Load(), xaiCalls.Load())
	}
}

func TestPaymentRequiredCoolsAccountUntilBillingResetAndRetries(t *testing.T) {
	paidToken := billingTestToken("paid-subject", 4)
	var calls atomic.Int32
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer "+paidToken {
			w.Header().Set("X-Should-Retry", "false")
			writeJSONResponse(t, w, http.StatusPaymentRequired, map[string]any{"error": "payment required"})
			return
		}
		writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "chatcmpl-second", "choices": []any{}})
	}))
	defer build.Close()

	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "paid.json", "paid-subject", 4)
	writeBillingTestCredential(t, dir, "basic.json", "basic-subject", 2)
	pool := newInferenceTestPool(t, dir, build.Client())
	defer pool.Close()
	var paidID string
	for _, info := range pool.Credentials() {
		if info.SubscriptionTier == "x_premium_plus" {
			paidID = info.ID
			break
		}
	}
	periodEnd := time.Now().Add(2 * time.Hour).UTC()
	if paidID == "" {
		t.Fatal("paid account not found")
	}
	if err := pool.UpdateBilling(paidID, auth.BillingInfo{PeriodEnd: &periodEnd, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, build.URL, 2)
	defer client.Close()
	result, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.AccountID == paidID || calls.Load() != 2 {
		t.Fatalf("result account=%q paid=%q calls=%d", result.AccountID, paidID, calls.Load())
	}
	info, ok := pool.Credential(paidID)
	if !ok || info.Status != "cooling_down" || info.CooldownReason != paymentRequiredReason || info.CooldownUntil == nil {
		t.Fatalf("payment-required info = %#v, %v", info, ok)
	}
	if delta := info.CooldownUntil.Sub(periodEnd); delta < -time.Second || delta > time.Second {
		t.Fatalf("cooldown until=%s period end=%s", info.CooldownUntil, periodEnd)
	}
	if _, err := os.Stat(filepath.Join(dir, "paid.json")); err != nil {
		t.Fatalf("payment-required credential removed: %v", err)
	}
}

func TestBuildPermissionDeniedCoolsAndRetriesWithoutFallbackWhenForcedBuild(t *testing.T) {
	paidToken := billingTestToken("paid-subject", 4)
	var buildCalls, xaiCalls atomic.Int32
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buildCalls.Add(1)
		if r.Header.Get("Authorization") == "Bearer "+paidToken {
			w.Header().Set("X-Should-Retry", "false")
			writeJSONResponse(t, w, http.StatusForbidden, map[string]any{"code": "permission-denied", "error": "permission denied"})
			return
		}
		writeJSONResponse(t, w, http.StatusOK, map[string]any{"id": "chatcmpl-second", "choices": []any{}})
	}))
	defer build.Close()
	xai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xaiCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer xai.Close()

	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "paid.json", "paid-subject", 4)
	writeBillingTestCredential(t, dir, "basic.json", "basic-subject", 2)
	pool := newInferenceTestPool(t, dir, build.Client())
	defer pool.Close()
	var paidID string
	for _, info := range pool.Credentials() {
		if info.SubscriptionTier == "x_premium_plus" {
			paidID = info.ID
		}
	}
	mode := auth.BuildRouteBuild
	if _, err := pool.UpdateBuildRouting(paidID, auth.BuildRoutingUpdate{RouteMode: &mode}); err != nil {
		t.Fatal(err)
	}
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, xai.URL, 2)
	defer client.Close()
	result, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.AccountID == paidID || buildCalls.Load() != 2 || xaiCalls.Load() != 0 {
		t.Fatalf("result account=%q build=%d xai=%d", result.AccountID, buildCalls.Load(), xaiCalls.Load())
	}
	info, _ := pool.Credential(paidID)
	if info.Status != "cooling_down" || info.CooldownReason != buildPermissionDeniedReason {
		t.Fatalf("permission-denied info = %#v", info)
	}
}

func TestBlockedUserDisablesButPreservesCredential(t *testing.T) {
	build := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResponse(t, w, http.StatusForbidden, map[string]any{"code": "blocked-user", "error": "blocked user"})
	}))
	defer build.Close()
	dir := t.TempDir()
	writeBillingTestCredential(t, dir, "blocked.json", "blocked-subject", 2)
	pool := newInferenceTestPool(t, dir, build.Client())
	defer pool.Close()
	client := newInferenceTestClient(t, dir, pool, build.Client(), build.URL, build.URL, 1)
	defer client.Close()
	if _, err := client.DoInference(context.Background(), testInferencePlan(t), InferenceOptions{}); err == nil {
		t.Fatal("blocked account unexpectedly succeeded")
	}
	info := pool.Credentials()[0]
	if !info.Disabled || info.Status != "disabled" {
		t.Fatalf("blocked credential info = %#v", info)
	}
	if _, err := os.Stat(filepath.Join(dir, "blocked.json")); err != nil {
		t.Fatalf("blocked credential removed: %v", err)
	}
}

func TestParseRetryHintsAndRetryableStatuses(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{
		"Retry-After":    []string{"7"},
		"X-Should-Retry": []string{"false"},
	}}
	apiErr := parseAPIError(resp, []byte(`{"error":"temporary"}`))
	if apiErr.RetryAfter != 7*time.Second || apiErr.ShouldRetry == nil || *apiErr.ShouldRetry {
		t.Fatalf("retry hints = after %s should-retry %v", apiErr.RetryAfter, apiErr.ShouldRetry)
	}
	client := &Client{}
	if client.handleRetryable("", "", apiErr, auth.InferencePlaneBuild) {
		t.Fatal("x-should-retry:false must suppress a 500 retry")
	}
	for _, status := range []int{500, 502, 503, 504, 520} {
		if !client.handleRetryable("", "", &APIError{Status: status}, auth.InferencePlaneBuild) {
			t.Errorf("status %d should be retryable", status)
		}
	}
	if client.handleRetryable("", "", &APIError{Status: http.StatusBadRequest, ShouldRetry: boolPointer(true)}, auth.InferencePlaneBuild) {
		t.Fatal("x-should-retry:true must not force retry of a non-retryable status")
	}
}

func boolPointer(value bool) *bool { return &value }
