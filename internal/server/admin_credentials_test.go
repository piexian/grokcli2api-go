package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
)

func TestListCredentialsPagination(t *testing.T) {
	credentials := make([]auth.CredentialInfo, 257)
	for i := range credentials {
		credentials[i] = auth.CredentialInfo{ID: fmt.Sprintf("%024x", i), Status: "ready", Usable: true}
	}

	var (
		cursor string
		gotIDs []string
	)
	const generation = 7
	for {
		cursorValue, err := decodeCredentialCursor(cursor)
		if err != nil {
			t.Fatalf("decode cursor %q: %v", cursor, err)
		}
		page := listCredentials(credentials, generation, adminCredentialListFilter{Limit: 100, Cursor: cursorValue})
		if page.Total != len(credentials) {
			t.Fatalf("total = %d, want %d", page.Total, len(credentials))
		}
		if page.HasMore && len(page.Data) != 100 {
			t.Fatalf("page with has_more contains %d credentials, want 100", len(page.Data))
		}
		if len(page.Data) > 100 {
			t.Fatalf("page contains %d credentials, want at most 100", len(page.Data))
		}
		for _, credential := range page.Data {
			if len(gotIDs) > 0 && gotIDs[len(gotIDs)-1] >= credential.ID {
				t.Fatalf("IDs are not strictly ascending: %q then %q", gotIDs[len(gotIDs)-1], credential.ID)
			}
			gotIDs = append(gotIDs, credential.ID)
		}
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Fatalf("final next_cursor = %q, want empty", page.NextCursor)
			}
			break
		}
		if page.NextCursor == "" {
			t.Fatal("has_more response has an empty next_cursor")
		}
		decoded, err := decodeCredentialCursor(page.NextCursor)
		if err != nil || decoded.Generation != generation {
			t.Fatalf("next cursor = %#v, %v", decoded, err)
		}
		cursor = page.NextCursor
	}

	if len(gotIDs) != len(credentials) {
		t.Fatalf("retrieved %d credentials, want %d", len(gotIDs), len(credentials))
	}
	seen := make(map[string]struct{}, len(gotIDs))
	for _, id := range gotIDs {
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate credential ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestListCredentialsQueryFiltersAllSearchFields(t *testing.T) {
	credentials := []auth.CredentialInfo{
		{ID: "id-alpha", Scope: "scope-one", SubscriptionTierDisplay: "Free", Models: []string{"grok-3"}},
		{ID: "id-beta", Scope: "scope-special", SubscriptionTierDisplay: "Enterprise", Models: []string{"grok-4-fast"}},
		{ID: "id-gamma", Scope: "scope-three", SubscriptionTierDisplay: "SuperGrok Heavy", Models: []string{"vision-preview"}},
	}
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "id", query: "ALPHA", want: []string{"id-alpha"}},
		{name: "scope", query: "SPECIAL", want: []string{"id-beta"}},
		{name: "tier", query: "heavy", want: []string{"id-gamma"}},
		{name: "models", query: "GROK-4", want: []string{"id-beta"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := listCredentials(credentials, 1, adminCredentialListFilter{Query: normalizeCredentialQuery(tt.query)})
			got := make([]string, len(page.Data))
			for i, credential := range page.Data {
				got[i] = credential.ID
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("IDs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListCredentialsStatusAndUsableFilters(t *testing.T) {
	credentials := []auth.CredentialInfo{
		{ID: "01", Status: "ready", Usable: true},
		{ID: "02", Status: "cooling_down", Usable: false},
		{ID: "03", Status: "disabled", Usable: false},
		{ID: "04", Status: "needs_refresh", Usable: false},
		{ID: "05", Status: "pending_models", Usable: true},
	}
	usable, unusable := true, false
	tests := []struct {
		name   string
		filter adminCredentialListFilter
		want   []string
	}{
		{name: "status", filter: adminCredentialListFilter{Status: "cooling_down"}, want: []string{"02"}},
		{name: "usable", filter: adminCredentialListFilter{Usable: &usable}, want: []string{"01", "05"}},
		{name: "unusable", filter: adminCredentialListFilter{Usable: &unusable}, want: []string{"02", "03", "04"}},
		{name: "combined", filter: adminCredentialListFilter{Status: "ready", Usable: &usable}, want: []string{"01"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := listCredentials(credentials, 1, tt.filter)
			got := make([]string, len(page.Data))
			for i, credential := range page.Data {
				got[i] = credential.ID
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("IDs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAdminCredentialListResponseCompatibility(t *testing.T) {
	page := credentialListPage{Data: []auth.CredentialInfo{}, Total: 0}
	legacy := marshalCredentialListResponse(t, newAdminCredentialListResponse(page, false))
	for _, key := range []string{"has_more", "next_cursor", "total"} {
		if _, exists := legacy[key]; exists {
			t.Fatalf("legacy response unexpectedly contains %q: %v", key, legacy)
		}
	}

	paginated := marshalCredentialListResponse(t, newAdminCredentialListResponse(page, true))
	for _, key := range []string{"has_more", "next_cursor", "total"} {
		if _, exists := paginated[key]; !exists {
			t.Fatalf("paginated response does not contain %q: %v", key, paginated)
		}
	}
	if paginated["has_more"] != false || paginated["next_cursor"] != "" || paginated["total"] != float64(0) {
		t.Fatalf("paginated zero values = %v", paginated)
	}
}

func TestAdminCredentialsHandlerResponseShape(t *testing.T) {
	s := newAdminTestServer(t, "http://127.0.0.1:1")
	defer s.Close()
	handler := s.Handler()

	request := func(path string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Admin-Key", "admin-secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	legacy := request("/v1/admin/credentials")
	for _, key := range []string{"has_more", "next_cursor", "total"} {
		if _, exists := legacy[key]; exists {
			t.Fatalf("legacy handler response unexpectedly contains %q: %v", key, legacy)
		}
	}
	paginated := request("/v1/admin/credentials?limit=100")
	for _, key := range []string{"has_more", "next_cursor", "total"} {
		if _, exists := paginated[key]; !exists {
			t.Fatalf("paginated handler response does not contain %q: %v", key, paginated)
		}
	}
}

func TestParseAdminCredentialListFilter(t *testing.T) {
	cursor := encodeCredentialCursor(credentialCursor{Generation: 11, ID: "account-100"})
	req := httptest.NewRequest("GET", "/v1/admin/credentials?limit=1000&cursor="+cursor+"&q=%20GROK-4%20&status=READY&usable=TRUE", nil)
	filter, err := parseAdminCredentialListFilter(req)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Limit != 1000 || filter.Cursor.Generation != 11 || filter.Cursor.ID != "account-100" || filter.Query != "grok-4" || filter.Status != "ready" || filter.Usable == nil || !*filter.Usable {
		t.Fatalf("filter = %#v", filter)
	}

	tests := []struct {
		name string
		url  string
	}{
		{name: "negative limit", url: "/v1/admin/credentials?limit=-1"},
		{name: "limit too large", url: "/v1/admin/credentials?limit=1001"},
		{name: "non-integer limit", url: "/v1/admin/credentials?limit=many"},
		{name: "cursor without pagination", url: "/v1/admin/credentials?cursor=" + cursor},
		{name: "invalid cursor", url: "/v1/admin/credentials?limit=10&cursor=not!base64"},
		{name: "invalid status", url: "/v1/admin/credentials?status=unknown"},
		{name: "invalid usable", url: "/v1/admin/credentials?usable=1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseAdminCredentialListFilter(httptest.NewRequest("GET", tt.url, nil)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestListCredentialsAcceptsCursorFromOlderGeneration(t *testing.T) {
	credentials := []auth.CredentialInfo{{ID: "01"}, {ID: "02"}, {ID: "03"}, {ID: "04"}}
	page := listCredentials(credentials, 9, adminCredentialListFilter{
		Limit:  2,
		Cursor: credentialCursor{Generation: 3, ID: "02"},
	})
	if got := []string{page.Data[0].ID, page.Data[1].ID}; !reflect.DeepEqual(got, []string{"03", "04"}) || page.HasMore {
		t.Fatalf("page from stale cursor = %#v", page)
	}
}

func marshalCredentialListResponse(t *testing.T, response adminCredentialListResponse) map[string]any {
	t.Helper()
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func BenchmarkListCredentials(b *testing.B) {
	credentials := make([]auth.CredentialInfo, 16_000)
	for i := range credentials {
		credentials[i] = auth.CredentialInfo{
			ID: fmt.Sprintf("%024x", i), Scope: "production", Status: "ready", Usable: true,
			SubscriptionTierDisplay: "SuperGrok", Models: []string{"grok-3", "grok-4-fast"},
		}
	}
	usable := true
	filter := adminCredentialListFilter{Limit: 100, Query: "grok-4", Status: "ready", Usable: &usable}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page := listCredentials(credentials, 1, filter)
		if len(page.Data) != 100 || page.Total != len(credentials) || !page.HasMore {
			b.Fatalf("unexpected page: data=%d total=%d has_more=%v", len(page.Data), page.Total, page.HasMore)
		}
	}
}

// BenchmarkAdminCredentialsHandler_16k reports p95 page and full 32-page
// latency. Baseline on linux/amd64 (i5-13420H): 0.89ms/page p95 and
// 71.40ms/32 pages p95 with -benchtime=5x.
func BenchmarkAdminCredentialsHandler_16k(b *testing.B) {
	pool := benchmarkCredentialPool(b, 16_000)
	handler := http.HandlerFunc((&Server{pool: pool}).adminCredentials)
	pageDurations := make([]time.Duration, 0, b.N*32)
	iterationDurations := make([]time.Duration, 0, b.N)

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		started := time.Now()
		cursor := ""
		pages := 0
		for {
			path := "/v1/admin/credentials?limit=500"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, path, nil)
			pageStarted := time.Now()
			handler.ServeHTTP(recorder, request)
			pageDurations = append(pageDurations, time.Since(pageStarted))
			if recorder.Code != http.StatusOK {
				b.Fatalf("page %d status=%d body=%s", pages+1, recorder.Code, recorder.Body.String())
			}
			var response struct {
				Data       []json.RawMessage `json:"data"`
				HasMore    bool              `json:"has_more"`
				NextCursor string            `json:"next_cursor"`
				Total      int               `json:"total"`
			}
			if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
				b.Fatal(err)
			}
			pages++
			if response.Total != 16_000 || len(response.Data) != 500 {
				b.Fatalf("page %d data=%d total=%d", pages, len(response.Data), response.Total)
			}
			if !response.HasMore {
				break
			}
			if response.NextCursor == "" {
				b.Fatalf("page %d has_more with empty cursor", pages)
			}
			cursor = response.NextCursor
		}
		if pages != 32 {
			b.Fatalf("pages = %d, want 32", pages)
		}
		iterationDurations = append(iterationDurations, time.Since(started))
	}
	b.StopTimer()

	p95Page := percentileDuration(pageDurations, 0.95)
	p95Total := percentileDuration(iterationDurations, 0.95)
	b.ReportMetric(float64(p95Page)/float64(time.Millisecond), "p95-page-ms")
	b.ReportMetric(float64(p95Total)/float64(time.Millisecond), "p95-32-pages-ms")
	if p95Total >= 200*time.Millisecond {
		b.Fatalf("p95 latency for 32 pages = %s, want < 200ms", p95Total)
	}
}

func benchmarkCredentialPool(b *testing.B, count int) *auth.Pool {
	b.Helper()
	dir := b.TempDir()
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	for index := 0; index < count; index++ {
		payload, err := json.Marshal(map[string]any{
			"key": fmt.Sprintf("token-%05d", index), "auth_mode": "external",
			"user_id": fmt.Sprintf("user-%05d", index), "expires_at": expires,
		})
		if err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("account-%05d.json", index)), payload, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AccountMaxInflight: 16, AffinityTTL: time.Hour, AffinityMaxEntries: count,
	}, http.DefaultClient)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(pool.Close)
	return pool
}

func percentileDuration(values []time.Duration, percentile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := int(math.Ceil(float64(len(values))*percentile)) - 1
	return values[index]
}
