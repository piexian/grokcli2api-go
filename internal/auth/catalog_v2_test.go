package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func TestMultiScopeAuthAndLogicalDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	writeJSONFile(t, path, map[string]any{
		"HTTPS://AUTH.EXAMPLE.test/team/": map[string]any{
			"key": "session-token", "auth_mode": "oidc", "user_id": "user-a",
			"principal_type": "Team", "principal_id": "team-a",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		},
		"xai::api_key": map[string]any{
			"key": "xai-api-key", "auth_mode": "api_key", "user_id": "api-user",
		},
	})
	pool, err := NewPool(context.Background(), PoolConfig{
		Dir: dir, ReloadInterval: time.Hour, AllowEmpty: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	infos := pool.Credentials()
	if len(infos) != 2 {
		t.Fatalf("logical credentials = %d, want 2", len(infos))
	}
	var sessionID, apiID string
	for _, info := range infos {
		switch info.AuthMode {
		case AuthModeOIDC:
			sessionID = info.ID
			if info.Scope != "https://auth.example.test/team" {
				t.Fatalf("normalized scope = %q", info.Scope)
			}
		case AuthModeAPIKey:
			apiID = info.ID
			lease, acquireErr := pool.AcquireAccount(context.Background(), info.ID)
			if acquireErr != nil {
				t.Fatal(acquireErr)
			}
			if !lease.Session().IsAPIKey() {
				t.Fatalf("session did not preserve API key auth mode: %#v", lease.Session())
			}
			lease.Release()
		}
	}
	if sessionID == "" || apiID == "" || sessionID == apiID {
		t.Fatalf("scope-aware IDs not distinct: session=%q api=%q", sessionID, apiID)
	}
	if err := pool.DeleteCredential(context.Background(), sessionID); err != nil {
		t.Fatal(err)
	}
	if got := pool.AccountIDs(); !slices.Equal(got, []string{apiID}) {
		t.Fatalf("remaining IDs = %v, want API key only", got)
	}
	var disk map[string]any
	readJSONFile(t, path, &disk)
	if disk["HTTPS://AUTH.EXAMPLE.test/team/"] != nil || disk["xai::api_key"] == nil {
		t.Fatalf("logical delete removed wrong scope: %#v", disk)
	}
}

func TestDeleteCredentialRechecksScopeAfterWaitingForFileLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	writeJSONFile(t, path, map[string]any{
		"scope:team": map[string]any{
			"key": "old-token", "auth_mode": "oidc", "user_id": "old-user",
			"principal_type": "team", "principal_id": "old-principal", "expires_at": expires,
		},
		"scope:survivor": map[string]any{
			"key": "survivor-token", "auth_mode": "external", "user_id": "survivor", "expires_at": expires,
		},
	})
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour, AllowEmpty: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	var targetID string
	for _, info := range pool.Credentials() {
		if info.Scope == "scope:team" {
			targetID = info.ID
			break
		}
	}
	if targetID == "" {
		t.Fatal("target logical credential was not loaded")
	}
	pool.mu.RLock()
	target := pool.accounts[targetID]
	pool.mu.RUnlock()
	if target == nil {
		t.Fatal("target account was not loaded")
	}
	// Initialize the per-account lock before observing its buffered token below.
	target.refreshOnce.Do(func() {
		target.refreshLock = make(chan struct{}, 1)
		target.refreshLock <- struct{}{}
	})

	fileLock, err := acquireAuthFileLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer fileLock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pool.DeleteCredential(ctx, targetID) }()

	deadline := time.Now().Add(time.Second)
	for len(target.refreshLock) != 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if len(target.refreshLock) != 0 {
		t.Fatal("delete did not acquire the target account lock")
	}
	if lease, acquireErr := pool.AcquireAccount(context.Background(), targetID); !errors.Is(acquireErr, ErrNoAuth) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("account remained schedulable while delete waited for the file lock: %v", acquireErr)
	}
	select {
	case deleteErr := <-done:
		t.Fatalf("delete returned before the held auth file lock was released: %v", deleteErr)
	case <-time.After(75 * time.Millisecond):
	}

	// Simulate another process replacing the same scope while this process is
	// waiting for auth.json.lock. The old account ID no longer owns this node.
	writeJSONFile(t, path, map[string]any{
		"scope:team": map[string]any{
			"key": "new-token", "auth_mode": "oidc", "user_id": "new-user",
			"principal_type": "team", "principal_id": "new-principal", "expires_at": expires,
		},
		"scope:survivor": map[string]any{
			"key": "survivor-token", "auth_mode": "external", "user_id": "survivor", "expires_at": expires,
		},
	})
	if err := fileLock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case deleteErr := <-done:
		if !errors.Is(deleteErr, ErrCredentialNotFound) {
			t.Fatalf("delete error = %v, want ErrCredentialNotFound", deleteErr)
		}
	case <-ctx.Done():
		t.Fatal("delete did not finish after the auth file lock was released")
	}
	if lease, acquireErr := pool.AcquireAccount(context.Background(), targetID); !errors.Is(acquireErr, ErrNoAuth) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("failed delete revived the old logical account: %v", acquireErr)
	}

	var disk map[string]any
	readJSONFile(t, path, &disk)
	replacement, _ := disk["scope:team"].(map[string]any)
	if replacement["principal_id"] != "new-principal" || replacement["key"] != "new-token" {
		t.Fatalf("replacement credential was deleted or rewritten: %#v", disk)
	}
}

func TestDeleteCredentialCannotBeRevivedByConcurrentRefresh(t *testing.T) {
	refreshEntered := make(chan struct{})
	releaseRefresh := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseRefresh)
		}
	}()
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(refreshEntered)
		select {
		case <-releaseRefresh:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-refreshed", "refresh_token": "refresh-refreshed", "expires_in": 3600,
		})
	}))
	defer tokenServer.Close()

	dir := t.TempDir()
	path := writeTestCredential(t, dir, "auth.json", "refresh-delete", "token-old", time.Now().Add(time.Hour), tokenServer.URL)
	pool, err := NewPool(context.Background(), PoolConfig{
		Dir: dir, Surface: "headless", ReloadInterval: time.Hour, RefreshConcurrency: 1, AllowEmpty: true,
	}, tokenServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	accountID := pool.AccountIDs()[0]
	pool.mu.RLock()
	target := pool.accounts[accountID]
	pool.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- pool.Refresh(ctx, accountID) }()
	select {
	case <-refreshEntered:
	case <-ctx.Done():
		t.Fatal("refresh did not reach the token endpoint")
	}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- pool.DeleteCredential(ctx, accountID) }()

	deadline := time.Now().Add(time.Second)
	for {
		target.mu.RLock()
		deleting := target.deleting
		target.mu.RUnlock()
		if deleting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delete did not mark the account before waiting for refresh")
		}
		runtime.Gosched()
	}
	if lease, acquireErr := pool.AcquireAccount(context.Background(), accountID); !errors.Is(acquireErr, ErrNoAuth) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("deleting account remained schedulable: %v", acquireErr)
	}

	close(releaseRefresh)
	released = true
	select {
	case refreshErr := <-refreshDone:
		if !errors.Is(refreshErr, ErrNoAuth) {
			t.Fatalf("concurrent refresh error = %v, want ErrNoAuth", refreshErr)
		}
	case <-ctx.Done():
		t.Fatal("refresh did not finish")
	}
	select {
	case deleteErr := <-deleteDone:
		if deleteErr != nil {
			t.Fatalf("delete error = %v", deleteErr)
		}
	case <-ctx.Done():
		t.Fatal("delete did not finish")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("refreshed credential was recreated after deletion: %v", err)
	}
	if len(pool.AccountIDs()) != 0 {
		t.Fatalf("deleted account returned to pool: %v", pool.AccountIDs())
	}
}

func TestImportCredentialsReturnsEveryScope(t *testing.T) {
	dir := t.TempDir()
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour, AllowEmpty: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	body, err := json.Marshal(map[string]any{
		"scope:a": map[string]any{
			"key": "token-a", "auth_mode": "external", "user_id": "user-a",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		},
		"scope:b": map[string]any{
			"key": "token-b", "auth_mode": "external", "user_id": "user-b",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	results, err := pool.ImportCredentials(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || len(pool.AccountIDs()) != 2 {
		t.Fatalf("import results=%d accounts=%d", len(results), len(pool.AccountIDs()))
	}
	for _, result := range results {
		if !result.Created || result.Credential.DiscoveryStatus != "pending" || result.Credential.AuthMode != AuthModeExternal {
			t.Fatalf("unexpected scope import result: %#v", result)
		}
	}
}

func TestImportCredentialsMergesScopesIntoTheirExistingFiles(t *testing.T) {
	dir := t.TempDir()
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	directPath := filepath.Join(dir, "direct.json")
	wrapperPath := filepath.Join(dir, "wrapper.json")
	writeJSONFile(t, directPath, map[string]any{
		"scope:a": map[string]any{
			"key": "old-a", "auth_mode": "external", "user_id": "user-a", "expires_at": expires,
		},
		"scope:keep-a": map[string]any{
			"key": "keep-a", "auth_mode": "external", "user_id": "keep-a", "expires_at": expires,
		},
		"file_metadata": "preserve-direct",
	})
	writeJSONFile(t, wrapperPath, map[string]any{
		"tokens": map[string]any{
			"scope:b": map[string]any{
				"key": "old-b", "auth_mode": "external", "user_id": "user-b", "expires_at": expires,
			},
			"scope:keep-b": map[string]any{
				"key": "keep-b", "auth_mode": "external", "user_id": "keep-b", "expires_at": expires,
			},
		},
		"file_metadata": "preserve-wrapper",
	})
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour, AllowEmpty: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	body, err := json.Marshal(map[string]any{
		"scope:a": map[string]any{
			"key": "new-a", "auth_mode": "external", "user_id": "user-a", "expires_at": expires,
		},
		"scope:b": map[string]any{
			"key": "new-b", "auth_mode": "external", "user_id": "user-b", "expires_at": expires,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	results, err := pool.ImportCredentials(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for _, result := range results {
		if result.Created {
			t.Fatalf("existing scope reported as created: %#v", result)
		}
		lease, err := pool.AcquireAccount(context.Background(), result.Credential.ID)
		if err != nil {
			t.Fatal(err)
		}
		token := lease.Session().Token
		lease.Release()
		if token != "new-a" && token != "new-b" {
			t.Fatalf("scope retained stale token %q", token)
		}
	}

	var direct map[string]any
	readJSONFile(t, directPath, &direct)
	if direct["file_metadata"] != "preserve-direct" || direct["scope:keep-a"].(map[string]any)["key"] != "keep-a" || direct["scope:a"].(map[string]any)["key"] != "new-a" {
		t.Fatalf("direct-scope merge overwrote sibling data: %#v", direct)
	}
	var wrapped map[string]any
	readJSONFile(t, wrapperPath, &wrapped)
	tokens, _ := wrapped["tokens"].(map[string]any)
	if wrapped["file_metadata"] != "preserve-wrapper" || tokens["scope:keep-b"].(map[string]any)["key"] != "keep-b" || tokens["scope:b"].(map[string]any)["key"] != "new-b" {
		t.Fatalf("tokens-wrapper merge overwrote sibling data: %#v", wrapped)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	physical := 0
	for _, entry := range entries {
		if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") && strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			physical++
		}
	}
	if physical != 2 {
		t.Fatalf("physical credential files = %d, want existing two", physical)
	}
}

func TestOIDCDiscoveryRetriesAndTargetScopeMerge(t *testing.T) {
	var discoveryCalls, tokenCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			discoveryCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"token_endpoint": server.URL + "/token"})
		case "/token":
			call := tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("principal_type") != "Team" || r.Form.Get("principal_id") != "team-a" {
				t.Errorf("principal form fields missing: %v", r.Form)
			}
			if call < 3 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "token-new", "refresh_token": "refresh-new", "expires_in": 3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	discoveryCache.Lock()
	discoveryCache.entries = make(map[string]discoveryEntry)
	discoveryCache.Unlock()

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	writeJSONFile(t, path, map[string]any{
		"scope:a": map[string]any{
			"key": "token-old", "refresh_token": "refresh-old", "auth_mode": "oidc",
			"user_id": "user-a", "principal_type": "Team", "principal_id": "team-a",
			"oidc_issuer": server.URL, "oidc_client_id": "client-a",
			"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
		},
		"scope:b": map[string]any{
			"key": "sibling-token", "auth_mode": "external", "user_id": "user-b",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		},
	})
	credentials, err := loadCredentials(path, "headless")
	if err != nil {
		t.Fatal(err)
	}
	var target *credential
	for _, credential := range credentials {
		if credential.Scope == "scope:a" {
			target = credential
		}
	}
	if target == nil {
		t.Fatal("OIDC scope not parsed")
	}
	next, err := target.refresh(context.Background(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if next.AccessToken != "token-new" || discoveryCalls.Load() != 1 || tokenCalls.Load() != 3 {
		t.Fatalf("refresh result token=%q discovery=%d exchange=%d", next.AccessToken, discoveryCalls.Load(), tokenCalls.Load())
	}
	var disk map[string]any
	readJSONFile(t, path, &disk)
	a := disk["scope:a"].(map[string]any)
	b := disk["scope:b"].(map[string]any)
	if a["key"] != "token-new" || b["key"] != "sibling-token" {
		t.Fatalf("targeted scope merge failed: %#v", disk)
	}
	if info, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("credential mode not owner-only: info=%v err=%v", info, err)
	}
}

func TestTokenExchangePermanentClassification(t *testing.T) {
	for _, test := range []struct {
		code      string
		permanent bool
		calls     int32
	}{
		{code: "invalid_grant", permanent: true, calls: 1},
		{code: "invalid_client", permanent: true, calls: 1},
		{code: "access_denied", permanent: false, calls: 3},
	} {
		t.Run(test.code, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": test.code})
			}))
			defer server.Close()
			_, _, err := exchangeRefreshToken(context.Background(), server.Client(), server.URL, &credential{
				RefreshToken: "refresh", ClientID: "client",
			})
			var refreshErr *RefreshError
			if !errors.As(err, &refreshErr) || refreshErr.Permanent != test.permanent {
				t.Fatalf("error = %#v, want permanent=%v", err, test.permanent)
			}
			if calls.Load() != test.calls {
				t.Fatalf("exchange calls = %d, want %d", calls.Load(), test.calls)
			}
		})
	}
}

func TestStateV1CooldownMigratesToV2(t *testing.T) {
	dir := t.TempDir()
	const subject = "legacy-state-subject"
	writeTestCredentialModels(t, dir, "legacy.json", subject, "token", time.Now().Add(time.Hour), "", []string{"grok"})
	accountID := accountID(subject)
	accountCooldown := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	modelCooldown := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	writeJSONFile(t, filepath.Join(dir, stateFileName), map[string]any{
		"version": 1,
		"accounts": map[string]any{
			accountID: map[string]any{
				"cooldown_until": accountCooldown,
				"reason":         "legacy_quota",
				"model_cooldowns": map[string]any{
					"grok": map[string]any{"until": modelCooldown, "reason": "legacy_model_quota"},
				},
			},
		},
	})

	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.AcquireAccountForModel(context.Background(), accountID, "grok"); err == nil {
		t.Fatal("v1 cooldown was not enforced after migration")
	} else {
		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) || !unavailable.Cooling {
			t.Fatalf("migrated cooldown error = %T %v", err, err)
		}
	}

	var migrated persistedState
	readJSONFile(t, filepath.Join(dir, stateFileName), &migrated)
	if migrated.Version != 2 || !validGlobalAgentID(migrated.GlobalAgentID) || !validNamespaceKey(migrated.NamespaceKey) {
		t.Fatalf("migrated state identity = %#v", migrated)
	}
	state, ok := migrated.Accounts[accountID]
	if !ok || !state.CooldownUntil.Equal(accountCooldown) || state.Reason != "legacy_quota" {
		t.Fatalf("migrated account cooldown = %#v", state)
	}
	cooldown, ok := state.ModelCooldowns["grok"]
	if !ok || !cooldown.Until.Equal(modelCooldown) || cooldown.Reason != "legacy_model_quota" {
		t.Fatalf("migrated model cooldown = %#v", state.ModelCooldowns)
	}
}

func TestStateV2DescriptorLeaseAggregationAndTenantPersistence(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentialModels(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "", []string{"grok"})
	writeTestCredentialModels(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "", []string{"grok"})
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := pool.AccountIDs()
	provisionalCreated := pool.ModelFirstSeen()["grok"]
	if provisionalCreated <= 0 {
		t.Fatal("legacy provisional model has no persisted first-seen timestamp")
	}
	updatedAt := time.Unix(200, 0).UTC()
	if err := pool.UpdateModelDescriptors(ids[0], []modelcatalog.ModelDescriptor{{
		ID: "grok", WireModel: "wire-responses", Backend: modelcatalog.BackendResponses,
		ContextWindow: 500000, MaxCompletionTokens: 32000, SupportsReasoningEffort: true,
		ReasoningEfforts: []string{"low", "high"}, SupportsBackendSearch: true, StreamToolCalls: true,
	}}, `"etag-a"`, updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.UpdateModelDescriptors(ids[1], []modelcatalog.ModelDescriptor{{
		ID: "grok", WireModel: "wire-messages", Backend: modelcatalog.BackendMessages,
		ContextWindow: 300000, MaxCompletionTokens: 16000, SupportsReasoningEffort: true,
		ReasoningEfforts: []string{"low", "medium"}, StreamToolCalls: true,
	}}, `"etag-b"`, updatedAt); err != nil {
		t.Fatal(err)
	}
	if err := pool.UpdateModelDescriptors(ids[0], []modelcatalog.ModelDescriptor{{
		ID: "grok", WireModel: "wire-responses-v2", Backend: modelcatalog.BackendResponses,
		ContextWindow: 500000, SupportedInAPI: true,
	}}, `"etag-a2"`, time.Unix(300, 0)); err != nil {
		t.Fatal(err)
	}
	if refreshed, ok := pool.AccountDescriptor(ids[0], "grok"); !ok || refreshed.Created != provisionalCreated {
		t.Fatalf("first discovery timestamp was not preserved: %#v %v", refreshed, ok)
	}
	pool.MarkModelCooldown(ids[0], "grok", "test", time.Minute)
	if _, acquireErr := pool.AcquireAccountForModel(context.Background(), ids[0], "grok"); acquireErr == nil {
		t.Fatal("pinned cooled account unexpectedly acquired")
	} else {
		var unavailable *UnavailableError
		if !errors.As(acquireErr, &unavailable) || !unavailable.Cooling {
			t.Fatalf("pinned cooldown error = %T %v", acquireErr, acquireErr)
		}
	}
	pool.RebuildSchedulingSnapshot()
	lease, err := pool.AcquireForBackends(context.Background(), Affinity{}, "grok", []modelcatalog.Backend{modelcatalog.BackendMessages}, nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, ok := lease.Descriptor()
	if !ok || descriptor.Backend != modelcatalog.BackendMessages || descriptor.WireModel != "wire-messages" {
		t.Fatalf("lease descriptor = %#v, %v", descriptor, ok)
	}
	agentID := lease.AgentID()
	lease.Release()
	aggregated := pool.AggregatedModels()
	if len(aggregated) != 1 || aggregated[0].ContextWindow != 300000 || aggregated[0].SupportsBackendSearch {
		t.Fatalf("aggregate = %#v", aggregated)
	}
	tenant := pool.TenantID("local-secret")
	if tenant == "" || tenant == "public" || tenant == pool.TenantID("different") {
		t.Fatalf("invalid tenant derivation: %q", tenant)
	}
	pool.Close()

	reloaded, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if reloaded.TenantID("local-secret") != tenant {
		t.Fatal("tenant namespace key did not persist")
	}
	lease, err = reloaded.AcquireForBackends(context.Background(), Affinity{}, "grok", []modelcatalog.Backend{modelcatalog.BackendMessages}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if lease.AgentID() != agentID {
		t.Fatalf("global agent ID changed across restart: %q != %q", lease.AgentID(), agentID)
	}
	if descriptor, ok = lease.Descriptor(); !ok || descriptor.Backend != modelcatalog.BackendMessages {
		t.Fatalf("state v2 descriptor not restored: %#v %v", descriptor, ok)
	}
	lease.Release()
	var state map[string]any
	readJSONFile(t, filepath.Join(dir, stateFileName), &state)
	if state["version"] != float64(2) || state["namespace_key"] == "" || state["global_agent_id"] == "" {
		t.Fatalf("invalid v2 state: %#v", state)
	}
}

func TestInvalidNamespaceClearsPersistedAffinity(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentialModels(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "", []string{"grok"})
	writeJSONFile(t, filepath.Join(dir, stateFileName), map[string]any{
		"version": 2, "namespace_key": "not-a-key", "global_agent_id": "00112233445566778899aabbccddeeff",
		"accounts": map[string]any{},
	})
	affinityPath := filepath.Join(dir, affinityStateFileName)
	writeJSONFile(t, affinityPath, map[string]any{"version": 1, "hard_affinities": map[string]any{"stale": map[string]any{}}})
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := os.Stat(affinityPath); !os.IsNotExist(err) {
		t.Fatalf("stale affinity state was not cleared: %v", err)
	}
	if pool.TenantID("secret") == "public" {
		t.Fatal("invalid namespace was not regenerated")
	}
}

func TestUpdateModelsKeepsCleanLegacyCredentialUnmodified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	writeJSONFile(t, path, map[string]any{
		"access_token": "session-token", "refresh_token": "refresh-token",
		"client_id": "client-id", "sub": "clean-user",
		"expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	updatedAt := time.Now().UTC().Truncate(time.Nanosecond)
	if err := pool.UpdateModels(accountID("clean-user"), []string{"grok-new"}, updatedAt); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	if string(after) != string(before) {
		pool.Close()
		t.Fatalf("clean CLI credential was polluted by model discovery\nbefore=%s\nafter=%s", before, after)
	}
	if got := pool.Models(); len(got) != 1 || got[0] != "grok-new" {
		pool.Close()
		t.Fatalf("in-memory provisional catalog=%#v", got)
	}
	pool.Close()

	reloaded, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if got := reloaded.Models(); len(got) != 1 || got[0] != "grok-new" {
		t.Fatalf("state v2 provisional catalog=%#v", got)
	}
}

func TestLegacyProvisionalFirstSeenPersistsIntoStructuredCatalog(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentialModels(t, dir, "legacy.json", "legacy-user", "token", time.Now().Add(time.Hour), "", []string{"grok-legacy"})
	before := time.Now().Unix()
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	created := pool.ModelFirstSeen()["grok-legacy"]
	if created < before || created > time.Now().Unix() {
		pool.Close()
		t.Fatalf("provisional first-seen = %d, want current discovery time", created)
	}
	account := accountID("legacy-user")
	var state persistedState
	readJSONFile(t, filepath.Join(dir, stateFileName), &state)
	if got := state.Catalogs[account].FirstSeen["grok-legacy"]; got != created {
		pool.Close()
		t.Fatalf("persisted provisional first-seen = %d, want %d", got, created)
	}
	if got := state.Catalogs[account].Provisional; !slices.Equal(got, []string{"grok-legacy"}) {
		pool.Close()
		t.Fatalf("persisted provisional models = %#v", got)
	}
	pool.Close()

	reloaded, err := NewPool(context.Background(), PoolConfig{Dir: dir, ReloadInterval: time.Hour}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if got := reloaded.ModelFirstSeen()["grok-legacy"]; got != created {
		t.Fatalf("reloaded first-seen = %d, want %d", got, created)
	}
	if err := reloaded.UpdateModelDescriptors(account, []modelcatalog.ModelDescriptor{{
		ID: "grok-legacy", WireModel: "wire-legacy", Backend: modelcatalog.BackendResponses,
		Created: created + 3600,
	}}, `"structured"`, time.Unix(created+7200, 0)); err != nil {
		t.Fatal(err)
	}
	descriptor, ok := reloaded.AccountDescriptor(account, "grok-legacy")
	if !ok || descriptor.Created != created {
		t.Fatalf("structured descriptor did not inherit provisional first-seen: %#v, %v", descriptor, ok)
	}
}

func TestBackgroundScanPersistsProvisionalCatalogAdditionAndRemoval(t *testing.T) {
	dir := t.TempDir()
	pathA := writeTestCredentialModels(t, dir, "a.json", "scan-a", "token-a", time.Now().Add(24*time.Hour), "", []string{"grok-a"})
	pool, err := NewPool(context.Background(), PoolConfig{
		Dir: dir, ReloadInterval: 15 * time.Millisecond, RefreshConcurrency: 1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	writeTestCredentialModels(t, dir, "b.json", "scan-b", "token-b", time.Now().Add(24*time.Hour), "", []string{"grok-b"})
	waitForPersistedCatalog := func(account, model string, present bool) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var state persistedState
			if _, statErr := os.Stat(filepath.Join(dir, stateFileName)); statErr == nil {
				readJSONFile(t, filepath.Join(dir, stateFileName), &state)
				catalog, exists := state.Catalogs[account]
				if present && exists && catalog.FirstSeen[model] > 0 {
					return
				}
				if !present && !exists {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("persisted catalog presence for %s = %v, want %v", account, !present, present)
	}
	waitForPersistedCatalog(accountID("scan-b"), "grok-b", true)
	if err := os.Remove(pathA); err != nil {
		t.Fatal(err)
	}
	waitForPersistedCatalog(accountID("scan-a"), "grok-a", false)
}

func TestHasModelLockedDoesNotRecursivelyAcquireRWMutex(t *testing.T) {
	a := &account{
		credential: &credential{AuthMode: AuthModeOIDC, Models: []string{"grok"}},
		descriptors: map[string]modelcatalog.ModelDescriptor{
			"grok": {ID: "grok", Backend: modelcatalog.BackendResponses},
		},
	}

	// Hold a read lock while a writer queues. sync.RWMutex blocks new readers
	// once a writer is waiting, so an accidental nested RLock inside
	// hasModelLocked would deadlock this otherwise valid locked call.
	a.mu.RLock()
	writerStarted := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		close(writerStarted)
		a.mu.Lock()
		a.descriptors["grok"] = modelcatalog.ModelDescriptor{ID: "grok", Backend: modelcatalog.BackendMessages}
		a.mu.Unlock()
		close(writerDone)
	}()
	<-writerStarted
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)

	result := make(chan bool, 1)
	go func() { result <- a.hasModelLocked("grok") }()
	select {
	case supported := <-result:
		if !supported {
			t.Error("locked model lookup unexpectedly failed")
		}
		a.mu.RUnlock()
	case <-time.After(250 * time.Millisecond):
		a.mu.RUnlock()
		<-result
		t.Fatal("hasModelLocked attempted a recursive read lock while a writer was queued")
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("queued descriptor writer did not finish")
	}
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatal(err)
	}
}
