package server

import (
	"bytes"
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
	snapshot := newAdminPoolSnapshot(generation, credentials)
	for {
		cursorValue, err := decodeCredentialCursor(cursor)
		if err != nil {
			t.Fatalf("decode cursor %q: %v", cursor, err)
		}
		page := listCredentials(snapshot, adminCredentialListFilter{Limit: 100, Cursor: cursorValue})
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
			page := listCredentials(newAdminPoolSnapshot(1, credentials), adminCredentialListFilter{Query: normalizeCredentialQuery(tt.query)})
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
			page := listCredentials(newAdminPoolSnapshot(1, credentials), tt.filter)
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
	cursor := encodeCredentialCursor(credentialCursor{
		Generation: 11, Sort: credentialSortTier, Order: credentialOrderDesc,
		LastKey: "n:4", LastID: "account-100",
	})
	req := httptest.NewRequest("GET", "/v1/admin/credentials?limit=1000&sort=tier&order=desc&cursor="+cursor+"&q=%20GROK-4%20&status=READY&usable=TRUE", nil)
	filter, err := parseAdminCredentialListFilter(req)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Limit != 1000 || filter.Sort != credentialSortTier || filter.Order != credentialOrderDesc ||
		filter.Cursor.Generation != 11 || filter.Cursor.LastKey != "n:4" || filter.Cursor.LastID != "account-100" ||
		filter.Query != "grok-4" || filter.Status != "ready" || filter.Usable == nil || !*filter.Usable {
		t.Fatalf("filter = %#v", filter)
	}
	defaults, err := parseAdminCredentialListFilter(httptest.NewRequest("GET", "/v1/admin/credentials", nil))
	if err != nil || defaults.Sort != credentialSortID || defaults.Order != credentialOrderAsc {
		t.Fatalf("default filter = %#v, %v", defaults, err)
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
		{name: "invalid sort", url: "/v1/admin/credentials?sort=created_at"},
		{name: "invalid order", url: "/v1/admin/credentials?order=sideways"},
		{name: "cursor sort mismatch", url: "/v1/admin/credentials?limit=10&sort=id&order=desc&cursor=" + cursor},
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
	page := listCredentials(newAdminPoolSnapshot(9, credentials), adminCredentialListFilter{
		Limit: 2,
		Cursor: credentialCursor{
			Generation: 3, Sort: credentialSortID, Order: credentialOrderAsc,
			LastKey: "s:02", LastID: "02",
		},
	})
	if got := []string{page.Data[0].ID, page.Data[1].ID}; !reflect.DeepEqual(got, []string{"03", "04"}) || page.HasMore {
		t.Fatalf("page from stale cursor = %#v", page)
	}
}

func TestListCredentialsSortFields(t *testing.T) {
	early := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	tests := []struct {
		name        string
		sort        credentialSort
		credentials []auth.CredentialInfo
		wantAsc     []string
		wantDesc    []string
	}{
		{
			name: "id", sort: credentialSortID,
			credentials: []auth.CredentialInfo{{ID: "c"}, {ID: "a"}, {ID: "b"}},
			wantAsc:     []string{"a", "b", "c"}, wantDesc: []string{"c", "b", "a"},
		},
		{
			name: "tier", sort: credentialSortTier,
			credentials: []auth.CredentialInfo{
				{ID: "unknown", SubscriptionTier: "future"}, {ID: "lite", SubscriptionTier: "supergrok_lite"},
				{ID: "free", SubscriptionTier: "free"}, {ID: "heavy", SubscriptionTier: "supergrok_heavy"},
				{ID: "premium", SubscriptionTier: "x_premium"}, {ID: "super", SubscriptionTier: "supergrok"},
				{ID: "basic", SubscriptionTier: "x_basic"}, {ID: "plus", SubscriptionTier: "x_premium_plus"},
			},
			wantAsc:  []string{"free", "basic", "premium", "plus", "super", "heavy", "lite", "unknown"},
			wantDesc: []string{"lite", "heavy", "super", "plus", "premium", "basic", "free", "unknown"},
		},
		{
			name: "status", sort: credentialSortStatus,
			credentials: []auth.CredentialInfo{
				{ID: "disabled", Status: "disabled"}, {ID: "ready", Status: "ready"},
				{ID: "refresh", Status: "needs_refresh"}, {ID: "pending", Status: "pending_models"},
				{ID: "cooling", Status: "cooling_down"}, {ID: "unknown", Status: "unavailable"},
			},
			wantAsc:  []string{"ready", "cooling", "pending", "refresh", "disabled", "unknown"},
			wantDesc: []string{"disabled", "refresh", "pending", "cooling", "ready", "unknown"},
		},
		{
			name: "expires_at", sort: credentialSortExpiresAt,
			credentials: []auth.CredentialInfo{
				{ID: "none"}, {ID: "late", ExpiresAt: &late}, {ID: "early-b", ExpiresAt: &early}, {ID: "early-a", ExpiresAt: &early},
			},
			wantAsc: []string{"early-a", "early-b", "late", "none"}, wantDesc: []string{"late", "early-b", "early-a", "none"},
		},
		{
			name: "models_count", sort: credentialSortModelsCount,
			credentials: []auth.CredentialInfo{
				{ID: "two", Models: []string{"a", "b"}}, {ID: "zero"}, {ID: "one-b", Models: []string{"a"}}, {ID: "one-a", Models: []string{"a"}},
			},
			wantAsc: []string{"zero", "one-a", "one-b", "two"}, wantDesc: []string{"two", "one-b", "one-a", "zero"},
		},
		{
			name: "usable", sort: credentialSortUsable,
			credentials: []auth.CredentialInfo{{ID: "false-b"}, {ID: "true-b", Usable: true}, {ID: "false-a"}, {ID: "true-a", Usable: true}},
			wantAsc:     []string{"true-a", "true-b", "false-a", "false-b"}, wantDesc: []string{"false-b", "false-a", "true-b", "true-a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := newAdminPoolSnapshot(1, tt.credentials)
			for _, direction := range []struct {
				order credentialOrder
				want  []string
			}{{credentialOrderAsc, tt.wantAsc}, {credentialOrderDesc, tt.wantDesc}} {
				page := listCredentials(snapshot, adminCredentialListFilter{Sort: tt.sort, Order: direction.order})
				if got := credentialIDs(page.Data); !reflect.DeepEqual(got, direction.want) {
					t.Fatalf("%s IDs = %v, want %v", direction.order, got, direction.want)
				}
			}
		})
	}
}

func TestListCredentialsCompositeCursorIsStableAcrossSorts(t *testing.T) {
	early := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(time.Hour)
	credentials := []auth.CredentialInfo{
		{ID: "a", SubscriptionTier: "free", Status: "ready", ExpiresAt: &early, Models: []string{"m1"}, Usable: true},
		{ID: "b", SubscriptionTier: "free", Status: "ready", ExpiresAt: &early, Models: []string{"m1"}, Usable: true},
		{ID: "c", SubscriptionTier: "x_basic", Status: "cooling_down", ExpiresAt: &late, Models: []string{"m1", "m2"}},
		{ID: "d", SubscriptionTier: "x_basic", Status: "cooling_down", ExpiresAt: &late, Models: []string{"m1", "m2"}},
		{ID: "e", SubscriptionTier: "future", Status: "unavailable"},
	}
	snapshot := newAdminPoolSnapshot(19, credentials)
	for _, sortField := range credentialSorts {
		for _, order := range credentialOrders {
			t.Run(string(sortField)+"_"+string(order), func(t *testing.T) {
				want := credentialIDs(listCredentials(snapshot, adminCredentialListFilter{Sort: sortField, Order: order}).Data)
				var cursor credentialCursor
				var got []string
				for {
					page := listCredentials(snapshot, adminCredentialListFilter{Limit: 2, Sort: sortField, Order: order, Cursor: cursor})
					got = append(got, credentialIDs(page.Data)...)
					if !page.HasMore {
						break
					}
					var err error
					cursor, err = decodeCredentialCursor(page.NextCursor)
					if err != nil {
						t.Fatal(err)
					}
					if cursor.Generation != 19 || cursor.Sort != sortField || cursor.Order != order || cursor.LastKey == "" || cursor.LastID == "" {
						t.Fatalf("cursor = %#v", cursor)
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("paged IDs = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestAdminCredentialsRejectsCursorSortMismatch(t *testing.T) {
	s := newAdminTestServer(t, "http://127.0.0.1:1")
	defer s.Close()
	cursor := encodeCredentialCursor(credentialCursor{
		Generation: 1, Sort: credentialSortID, Order: credentialOrderAsc, LastKey: "s:a", LastID: "a",
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/credentials?limit=10&sort=tier&cursor="+cursor, nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminPoolSnapshotCacheTracksSnapshotIdentity(t *testing.T) {
	cache := &adminPoolSnapshotCache{}
	firstCredentials := []auth.CredentialInfo{{ID: "a", Status: "ready"}}
	first := cache.get(7, firstCredentials)
	if reused := cache.get(7, firstCredentials); reused != first {
		t.Fatal("unchanged Pool snapshot was not reused")
	}
	refreshedCredentials := []auth.CredentialInfo{{ID: "a", Status: "cooling_down"}}
	refreshed := cache.get(7, refreshedCredentials)
	if refreshed == first || refreshed.Credentials[0].Status != "cooling_down" {
		t.Fatalf("same-generation replacement snapshot = %#v", refreshed)
	}
}

func credentialIDs(credentials []auth.CredentialInfo) []string {
	ids := make([]string, len(credentials))
	for index := range credentials {
		ids[index] = credentials[index].ID
	}
	return ids
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
	snapshot := newAdminPoolSnapshot(1, credentials)
	filter := adminCredentialListFilter{Limit: 100, Query: "grok-4", Status: "ready", Usable: &usable, Sort: credentialSortTier, Order: credentialOrderDesc}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page := listCredentials(snapshot, filter)
		if len(page.Data) != 100 || page.Total != len(credentials) || !page.HasMore {
			b.Fatalf("unexpected page: data=%d total=%d has_more=%v", len(page.Data), page.Total, page.HasMore)
		}
	}
}

// BenchmarkAdminCredentialsHandler_16k reports p95 page and full 32-page
// latency. Baseline on linux/amd64 (i5-13420H): 0.82ms/page p95 and
// 85.48ms/32 pages p95 with -benchtime=5x.
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
			path := "/v1/admin/credentials?limit=500&sort=tier&order=desc"
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
	if p95Page >= 50*time.Millisecond {
		b.Fatalf("p95 latency for one page = %s, want < 50ms", p95Page)
	}
	if p95Total >= 200*time.Millisecond {
		b.Fatalf("p95 latency for 32 pages = %s, want < 200ms", p95Total)
	}
}

func TestAdminCredentialRoutingUpdatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(map[string]any{
		"key": "token", "auth_mode": "external", "user_id": "user", "expires_at": expires,
		"models": []string{"grok-4.5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "account.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	pool, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 16,
	}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{pool: pool}
	id := pool.AccountIDs()[0]
	request := httptest.NewRequest(http.MethodPatch, "/v1/admin/credentials/"+id, bytes.NewBufferString(`{
		"build_route_mode":"xai",
		"build_super_entitled":true
	}`))
	request.SetPathValue("id", id)
	response := httptest.NewRecorder()
	server.adminCredentialRouting(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	info, ok := pool.Credential(id)
	if !ok || info.RouteMode != auth.BuildRouteXAI || !info.SuperEntitledOverride ||
		info.EffectiveInferenceRoute != auth.InferencePlaneXAI {
		t.Fatalf("routing info = %#v, %v", info.BuildRoutingInfo, ok)
	}
	pool.Close()

	reloaded, err := auth.NewPool(context.Background(), auth.PoolConfig{
		Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 1,
		AffinityTTL: time.Hour, AffinityMaxEntries: 16,
	}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	reloadedInfo, ok := reloaded.Credential(id)
	if !ok || reloadedInfo.RouteMode != auth.BuildRouteXAI || !reloadedInfo.SuperEntitledOverride {
		t.Fatalf("reloaded routing info = %#v, %v", reloadedInfo.BuildRoutingInfo, ok)
	}
}

func TestAdminCredentialRoutingRejectsInvalidMode(t *testing.T) {
	request := httptest.NewRequest(http.MethodPatch, "/v1/admin/credentials/0123456789abcdef01234567", bytes.NewBufferString(`{"build_route_mode":"random"}`))
	request.SetPathValue("id", "0123456789abcdef01234567")
	response := httptest.NewRecorder()
	server := &Server{}
	server.adminCredentialRouting(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminCredentialRoutingRejectsSettingFallbackRecord(t *testing.T) {
	request := httptest.NewRequest(http.MethodPatch, "/v1/admin/credentials/0123456789abcdef01234567", bytes.NewBufferString(`{"build_api_fallback":true}`))
	request.SetPathValue("id", "0123456789abcdef01234567")
	response := httptest.NewRecorder()
	server := &Server{}
	server.adminCredentialRouting(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
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
