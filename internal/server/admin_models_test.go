package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
)

func TestSummarizeAdminModels(t *testing.T) {
	credentials := []auth.CredentialInfo{
		{ID: "a", Status: "ready", Usable: true, Models: []string{"grok-4.5", "grok-3-mini", "grok-4.5"}, ModelCooldowns: map[string]auth.ModelCooldownInfo{
			"grok-4.5": {Until: time.Now().Add(time.Hour), Reason: "model_free_quota_exhausted"},
		}},
		{ID: "b", Status: "cooling_down", Models: []string{"grok-4.5"}},
		{ID: "c", Status: "disabled", Models: []string{"grok-3-mini"}, ModelCooldowns: map[string]auth.ModelCooldownInfo{
			"grok-3-mini": {Until: time.Now().Add(time.Hour)},
		}},
	}
	got := summarizeAdminModels(credentials)
	want := []adminModelSummary{
		{Model: "grok-3-mini", Accounts: 2, UsableAccounts: 1, StatusCounts: map[string]int{"disabled": 1, "ready": 1}},
		{Model: "grok-4.5", Accounts: 2, StatusCounts: map[string]int{"cooling_down": 2}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}
}

func TestFilterAdminModelSummaries(t *testing.T) {
	summaries := []adminModelSummary{{Model: "grok-3-mini"}, {Model: "grok-4.5"}, {Model: "vision-preview"}}
	if got := filterAdminModelSummaries(summaries, " GROK-4 "); !reflect.DeepEqual(got, []adminModelSummary{{Model: "grok-4.5"}}) {
		t.Fatalf("filtered summaries = %#v", got)
	}
	if got := filterAdminModelSummaries(summaries, "missing"); got == nil || len(got) != 0 {
		t.Fatalf("empty filtered summaries = %#v", got)
	}
}

func TestAdminModelSummaryCacheTracksSnapshotIdentity(t *testing.T) {
	cache := &adminModelSummaryCache{}
	firstCredentials := []auth.CredentialInfo{{ID: "a", Status: "ready", Models: []string{"grok-4.5"}}}
	first := cache.get(9, firstCredentials)
	if reused := cache.get(9, firstCredentials); reused != first {
		t.Fatal("unchanged Pool snapshot was not reused")
	}
	refreshedCredentials := []auth.CredentialInfo{{ID: "a", Status: "cooling_down", Models: []string{"grok-4.5"}}}
	refreshed := cache.get(9, refreshedCredentials)
	if refreshed == first || refreshed.Data[0].StatusCounts["cooling_down"] != 1 {
		t.Fatalf("same-generation replacement summary = %#v", refreshed)
	}
}

func TestAdminModelsSummaryHandler(t *testing.T) {
	pool := modelSummaryTestPool(t, map[string][]string{
		"account-a": {"grok-4.5", "grok-3-mini"},
		"account-b": {"grok-4.5"},
	})
	s := &Server{pool: pool}
	s.cfg.AdminKey = "admin-secret"
	handler := s.adminKeyGate(http.HandlerFunc(s.adminModelsSummary))

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/admin/models/summary", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/models/summary?q=GROK-4", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var response adminModelsSummaryResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Object != "list" || response.TotalModels != 1 || len(response.Data) != 1 {
		t.Fatalf("response = %#v", response)
	}
	summary := response.Data[0]
	if summary.Model != "grok-4.5" || summary.Accounts != 2 || summary.UsableAccounts != 2 || summary.StatusCounts["ready"] != 2 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestAdminModelsSummaryRoute(t *testing.T) {
	s := newAdminTestServer(t, "http://127.0.0.1:1")
	defer s.Close()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/models/summary", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func BenchmarkSummarizeAdminModels_16k(b *testing.B) {
	credentials := make([]auth.CredentialInfo, 16_000)
	for index := range credentials {
		credentials[index] = auth.CredentialInfo{
			ID: fmt.Sprintf("account-%05d", index), Status: "ready", Usable: true,
			Models: []string{"grok-3-mini", "grok-4.5", "grok-4.5-fast"},
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	started := time.Now()
	for iteration := 0; iteration < b.N; iteration++ {
		if summaries := summarizeAdminModels(credentials); len(summaries) != 3 {
			b.Fatalf("summary count = %d", len(summaries))
		}
	}
	elapsed := time.Since(started)
	b.StopTimer()
	average := elapsed / time.Duration(b.N)
	b.ReportMetric(float64(average)/float64(time.Millisecond), "avg-ms")
	if average >= 30*time.Millisecond {
		b.Fatalf("average aggregation latency = %s, want < 30ms", average)
	}
}

func modelSummaryTestPool(t *testing.T, accounts map[string][]string) *auth.Pool {
	t.Helper()
	dir := t.TempDir()
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	index := 0
	for account, models := range accounts {
		payload, err := json.Marshal(map[string]any{
			"key": fmt.Sprintf("token-%d", index), "auth_mode": "external",
			"user_id": account, "expires_at": expires, "models": models,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, account+".json"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		index++
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AccountMaxInflight: 2, AffinityTTL: time.Hour, AffinityMaxEntries: 16,
	}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
