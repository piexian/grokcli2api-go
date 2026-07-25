package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/audit"
	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/config"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/inference"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

func TestAuditUsageExtractionFromUpstreamPayloadsAndStreams(t *testing.T) {
	chat := decodeChatResult(map[string]any{"usage": map[string]any{
		"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 17,
		"prompt_tokens_details":     map[string]any{"cached_tokens": 3},
		"completion_tokens_details": map[string]any{"reasoning_tokens": 2},
	}}).Usage
	if chat.Input != 10 || chat.Output != 5 || chat.Cached != 3 || chat.Reasoning != 2 || chat.total() != 17 {
		t.Fatalf("chat usage = %#v", chat)
	}

	responses := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolResponses, UpstreamBackend: modelcatalog.BackendResponses,
	}, "grok-4")
	_, err := responses.Handle(grok.SSEEvent{Event: "response.completed", Data: []byte(`{
		"type":"response.completed","response":{"status":"completed","usage":{
			"input_tokens":8,"output_tokens":4,"total_tokens":14,
			"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}
		}}
	}`)})
	if err != nil {
		t.Fatal(err)
	}
	if usage := responses.Usage(); usage.Input != 8 || usage.Output != 4 || usage.Cached != 2 || usage.Reasoning != 1 || usage.total() != 14 {
		t.Fatalf("responses stream usage = %#v", usage)
	}

	messages := newBackendStreamAdapter(inference.ResponseAdapter{
		ClientProtocol: inference.ProtocolMessages, UpstreamBackend: modelcatalog.BackendMessages,
	}, "grok-4")
	for _, event := range []grok.SSEEvent{
		{Event: "message_start", Data: []byte(`{"type":"message_start","message":{"usage":{"input_tokens":6,"cache_read_input_tokens":2}}}`)},
		{Event: "message_delta", Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)},
	} {
		if _, err := messages.Handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if usage := messages.Usage(); usage.Input != 6 || usage.Output != 3 || usage.Cached != 2 || usage.total() != 9 {
		t.Fatalf("messages stream usage = %#v", usage)
	}
}

func TestAuditMiddlewareRecordsSuccessFailureAndStreamCompletion(t *testing.T) {
	store, err := audit.Open(filepath.Join(t.TempDir(), "audit.db"), 30, audit.DefaultQueueSize)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &Server{audits: store}
	streamStarted := make(chan struct{})
	finishStream := make(chan struct{})
	handler := s.auditRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := audit.CaptureFromContext(r.Context())
		switch r.URL.Path {
		case "/v1/responses":
			captureAuditRequest(r.Context(), map[string]any{"model": "grok-stream", "stream": true})
			audit.SetAccount(r.Context(), "acct-stream")
			audit.MarkAttempt(r.Context())
			prepareSSE(w)
			_ = writeRawSSE(w, grok.SSEEvent{Event: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"hello"}`)})
			close(streamStarted)
			<-finishStream
			capture.SetUsage(audit.Usage{Input: 3, CachedInput: 1, Output: 2, Reasoning: 1, Total: 5})
			_ = writeRawSSE(w, grok.SSEEvent{Event: "response.completed", Data: []byte(`{"type":"response.completed"}`)})
		case "/v1/messages":
			captureAuditRequest(r.Context(), map[string]any{"model": "grok-failure"})
			writeAnthropicError(w, http.StatusBadGateway, "upstream failed", "api_error")
		default:
			captureAuditRequest(r.Context(), map[string]any{"model": "grok-success"})
			audit.SetAccount(r.Context(), "acct-success")
			audit.MarkAttempt(r.Context())
			capture.SetUsage(audit.Usage{Input: 10, Output: 5, Total: 15})
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		}
	}))

	streamDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))
		close(streamDone)
	}()
	<-streamStarted
	page, err := store.List(context.Background(), audit.ListFilter{Protocol: "responses", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("stream audit written before completion: %#v", page.Items)
	}
	close(finishStream)
	<-streamDone

	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
	}
	page, err = store.List(context.Background(), audit.ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("audit records = %#v", page.Items)
	}
	items := make(map[string]audit.Record, len(page.Items))
	for _, item := range page.Items {
		items[item.Protocol] = item
		if item.ID == "" || item.RequestID == "" {
			t.Fatalf("audit identifiers are empty: %#v", item)
		}
	}
	if item := items["responses"]; item.StatusCode != 200 || !item.Streaming || item.AccountID != "acct-stream" || item.TotalTokens != 5 || item.AttemptCount != 1 {
		t.Fatalf("stream audit = %#v", item)
	}
	if item := items["chat"]; item.StatusCode != 200 || item.TotalTokens != 15 || item.AccountID != "acct-success" {
		t.Fatalf("success audit = %#v", item)
	}
	if item := items["messages"]; item.StatusCode != http.StatusBadGateway || item.ErrorCode != "api_error" || item.AccountID != "" {
		t.Fatalf("failure audit = %#v", item)
	}
}

func TestAdminAuditPaginationSummaryAndDashboard(t *testing.T) {
	store, err := audit.Open(filepath.Join(t.TempDir(), "audit.db"), 30, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Insert(context.Background(),
		audit.Record{ID: "one", CreatedAt: now.Add(-2 * time.Hour).UnixMilli(), Protocol: "chat", Model: "grok-4", AccountID: "acct-one", StatusCode: 200, InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		audit.Record{ID: "two", CreatedAt: now.Add(-time.Hour).UnixMilli(), Protocol: "chat", Model: "grok-4", AccountID: "acct-two", StatusCode: 500, ErrorCode: "upstream_error", InputTokens: 2, TotalTokens: 2},
		audit.Record{ID: "three", CreatedAt: now.Add(-30 * time.Minute).UnixMilli(), Protocol: "messages", Model: "grok-3", AccountID: "acct-three", StatusCode: 200, InputTokens: 3, OutputTokens: 1, TotalTokens: 4},
	); err != nil {
		t.Fatal(err)
	}

	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: t.TempDir(), Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AccountMaxInflight: 1, AffinityTTL: time.Hour, AffinityMaxEntries: 16, AllowEmpty: true,
	}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := &Server{cfg: config.Config{AdminKey: "admin-secret"}, pool: pool, audits: store, mux: http.NewServeMux()}
	s.routes()
	handler := s.Handler()
	request := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Admin-Key", "admin-secret")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	first := request("/v1/admin/audits?limit=1&model=grok-4")
	if first.Code != http.StatusOK {
		t.Fatalf("first page status=%d body=%s", first.Code, first.Body.String())
	}
	var firstPage audit.ListPage
	if err := json.Unmarshal(first.Body.Bytes(), &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Items) != 1 || !firstPage.HasMore || firstPage.NextCursor == "" || firstPage.Items[0].ID != "two" {
		t.Fatalf("first page = %#v", firstPage)
	}
	second := request("/v1/admin/audits?limit=1&model=grok-4&cursor=" + url.QueryEscape(firstPage.NextCursor))
	var secondPage audit.ListPage
	if err := json.Unmarshal(second.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if second.Code != http.StatusOK || len(secondPage.Items) != 1 || secondPage.Items[0].ID != "one" || secondPage.HasMore {
		t.Fatalf("second page status=%d page=%#v", second.Code, secondPage)
	}
	filteredPath := "/v1/admin/audits?account_id=acct-two&protocol=chat&status=failed&since=" +
		strconv.FormatInt(now.Add(-90*time.Minute).Unix(), 10) + "&until=" +
		url.QueryEscape(now.Add(-30*time.Minute).Format(time.RFC3339))
	filtered := request(filteredPath)
	var filteredPage audit.ListPage
	if err := json.Unmarshal(filtered.Body.Bytes(), &filteredPage); err != nil {
		t.Fatal(err)
	}
	if filtered.Code != http.StatusOK || len(filteredPage.Items) != 1 || filteredPage.Items[0].ID != "two" {
		t.Fatalf("filtered status=%d page=%#v", filtered.Code, filteredPage)
	}

	summaryRecorder := request("/v1/admin/audits/summary?period=7d")
	var summary audit.Summary
	if err := json.Unmarshal(summaryRecorder.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summaryRecorder.Code != http.StatusOK || summary.Requests != 3 || summary.SuccessfulRequests != 2 || summary.FailedRequests != 1 || len(summary.ByModel) != 2 {
		t.Fatalf("summary status=%d value=%#v", summaryRecorder.Code, summary)
	}

	dashboard := request("/v1/admin/dashboard?period=7d")
	var decoded dashboardResponse
	if err := json.Unmarshal(dashboard.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if dashboard.Code != http.StatusOK || decoded.Usage.Requests != 3 || decoded.Resources.TotalAccounts != 0 || decoded.Resources.TotalModels != 0 {
		t.Fatalf("dashboard status=%d value=%#v", dashboard.Code, decoded)
	}

	dropped := false
	for index := 0; index < 10_000; index++ {
		if !store.Enqueue(audit.Record{Protocol: "chat", StatusCode: 200}) {
			dropped = true
			break
		}
	}
	if !dropped {
		t.Fatal("audit queue did not report overload")
	}
	healthRecorder := request("/v1/admin/audits/health")
	var health audit.QueueHealth
	if err := json.Unmarshal(healthRecorder.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if healthRecorder.Code != http.StatusOK || health.QueueCap != 1 || health.DroppedTotal == 0 || health.DroppedTotal != store.Dropped() {
		t.Fatalf("health status=%d value=%#v dropped=%d", healthRecorder.Code, health, store.Dropped())
	}

	invalid := request("/v1/admin/audits/summary?period=90d")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid period status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/admin/audits", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.Code)
	}
}

func TestAdminDashboardCountsAccountAndModelCooldownsOnce(t *testing.T) {
	pool := modelSummaryTestPool(t, map[string][]string{
		"account-cooldown": {"grok-4.5"},
		"model-cooldown":   {"grok-4.5"},
		"both-cooldowns":   {"grok-4.5"},
		"ready":            {"grok-4.5"},
	})
	ids := pool.AccountIDs()
	if len(ids) != 4 {
		t.Fatalf("account IDs = %v", ids)
	}
	pool.MarkCooldown(ids[0], "rate_limited", time.Hour)
	pool.MarkModelCooldown(ids[1], "grok-4.5", "model_free_quota_exhausted", time.Hour)
	pool.MarkCooldown(ids[2], "rate_limited", time.Hour)
	pool.MarkModelCooldown(ids[2], "grok-4.5", "model_free_quota_exhausted", time.Hour)

	s := &Server{pool: pool}
	recorder := httptest.NewRecorder()
	s.adminDashboard(recorder, httptest.NewRequest(http.MethodGet, "/v1/admin/dashboard", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response dashboardResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Resources.TotalAccounts != 4 || response.Resources.CoolingAccounts != 3 || response.Resources.UsableAccounts != 1 || response.Resources.DisabledAccounts != 0 {
		t.Fatalf("resources = %#v", response.Resources)
	}
}
