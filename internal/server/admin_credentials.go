package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
)

const maxAdminCredentialsPageSize = 1000

const maxAdminCredentialUpdateSize = 4 << 10

type credentialSort string

const (
	credentialSortID          credentialSort = "id"
	credentialSortTier        credentialSort = "tier"
	credentialSortStatus      credentialSort = "status"
	credentialSortExpiresAt   credentialSort = "expires_at"
	credentialSortModelsCount credentialSort = "models_count"
	credentialSortUsable      credentialSort = "usable"
)

var credentialSorts = []credentialSort{
	credentialSortID,
	credentialSortTier,
	credentialSortStatus,
	credentialSortExpiresAt,
	credentialSortModelsCount,
	credentialSortUsable,
}

type credentialOrder string

const (
	credentialOrderAsc  credentialOrder = "asc"
	credentialOrderDesc credentialOrder = "desc"
)

var credentialOrders = []credentialOrder{credentialOrderAsc, credentialOrderDesc}

type adminCredentialListFilter struct {
	Limit  int
	Cursor credentialCursor
	Query  string
	Status string
	Usable *bool
	Sort   credentialSort
	Order  credentialOrder
}

type credentialCursor struct {
	Generation uint64          `json:"generation"`
	Sort       credentialSort  `json:"sort"`
	Order      credentialOrder `json:"order"`
	LastKey    string          `json:"last_key"`
	LastID     string          `json:"last_id"`
}

type credentialSortOrder struct {
	Sort  credentialSort
	Order credentialOrder
}

type credentialSortValue struct {
	Missing bool
	Text    string
	Number  int64
	IsText  bool
}

type adminPoolSnapshot struct {
	Generation  uint64
	Credentials []auth.CredentialInfo
	Orders      map[credentialSortOrder][]int
}

type adminPoolSnapshotCache struct {
	mu      sync.Mutex
	current atomic.Pointer[adminPoolSnapshot]
}

type credentialListPage struct {
	Data       []auth.CredentialInfo
	HasMore    bool
	NextCursor string
	Total      int
}

type adminCredentialListResponse struct {
	Object     string                `json:"object"`
	Data       []auth.CredentialInfo `json:"data"`
	HasMore    *bool                 `json:"has_more,omitempty"`
	NextCursor *string               `json:"next_cursor,omitempty"`
	Total      *int                  `json:"total,omitempty"`
}

func parseAdminCredentialListFilter(r *http.Request) (adminCredentialListFilter, error) {
	query := r.URL.Query()
	filter := adminCredentialListFilter{
		Query: normalizeCredentialQuery(query.Get("q")),
		Sort:  credentialSortID,
		Order: credentialOrderAsc,
	}

	if raw := credentialSort(strings.ToLower(strings.TrimSpace(query.Get("sort")))); raw != "" {
		if !validCredentialSort(raw) {
			return adminCredentialListFilter{}, errors.New("sort must be id, tier, status, expires_at, models_count, or usable")
		}
		filter.Sort = raw
	}
	if raw := credentialOrder(strings.ToLower(strings.TrimSpace(query.Get("order")))); raw != "" {
		if !validCredentialOrder(raw) {
			return adminCredentialListFilter{}, errors.New("order must be asc or desc")
		}
		filter.Order = raw
	}

	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 || limit > maxAdminCredentialsPageSize {
			return adminCredentialListFilter{}, errors.New("limit must be an integer between 0 and 1000")
		}
		filter.Limit = limit
	}

	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		if filter.Limit == 0 {
			return adminCredentialListFilter{}, errors.New("cursor requires a positive limit")
		}
		cursor, err := decodeCredentialCursor(raw)
		if err != nil {
			return adminCredentialListFilter{}, errors.New("invalid credential cursor")
		}
		if cursor.Sort != filter.Sort || cursor.Order != filter.Order {
			return adminCredentialListFilter{}, errors.New("credential cursor sort and order must match the request")
		}
		filter.Cursor = cursor
	}

	filter.Status = strings.ToLower(strings.TrimSpace(query.Get("status")))
	if filter.Status != "" {
		switch filter.Status {
		case "ready", "cooling_down", "disabled", "needs_refresh", "pending_models":
		default:
			return adminCredentialListFilter{}, errors.New("status must be ready, cooling_down, disabled, needs_refresh, or pending_models")
		}
	}

	if raw := strings.ToLower(strings.TrimSpace(query.Get("usable"))); raw != "" {
		var usable bool
		switch raw {
		case "true":
			usable = true
		case "false":
		default:
			return adminCredentialListFilter{}, errors.New("usable must be true or false")
		}
		filter.Usable = &usable
	}
	return filter, nil
}

func (c *adminPoolSnapshotCache) get(generation uint64, credentials []auth.CredentialInfo) *adminPoolSnapshot {
	if current := c.current.Load(); sameAdminPoolSnapshot(current, generation, credentials) {
		return current
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if current := c.current.Load(); sameAdminPoolSnapshot(current, generation, credentials) {
		return current
	}
	next := newAdminPoolSnapshot(generation, credentials)
	c.current.Store(next)
	return next
}

func sameAdminPoolSnapshot(snapshot *adminPoolSnapshot, generation uint64, credentials []auth.CredentialInfo) bool {
	if snapshot == nil || snapshot.Generation != generation || len(snapshot.Credentials) != len(credentials) {
		return false
	}
	if len(credentials) == 0 {
		return true
	}
	// Pool may rebuild a time-sensitive snapshot without changing generation.
	return &snapshot.Credentials[0] == &credentials[0]
}

func newAdminPoolSnapshot(generation uint64, credentials []auth.CredentialInfo) *adminPoolSnapshot {
	snapshot := &adminPoolSnapshot{
		Generation: generation, Credentials: credentials,
		Orders: make(map[credentialSortOrder][]int, len(credentialSorts)*len(credentialOrders)),
	}
	for _, sortField := range credentialSorts {
		ascending := make([]int, len(credentials))
		for index := range ascending {
			ascending[index] = index
		}
		sort.Slice(ascending, func(i, j int) bool {
			return compareCredentialPositions(
				credentialSortValueFor(credentials[ascending[i]], sortField), credentials[ascending[i]].ID,
				credentialSortValueFor(credentials[ascending[j]], sortField), credentials[ascending[j]].ID,
				credentialOrderAsc,
			) < 0
		})
		snapshot.Orders[credentialSortOrder{Sort: sortField, Order: credentialOrderAsc}] = ascending
		snapshot.Orders[credentialSortOrder{Sort: sortField, Order: credentialOrderDesc}] = descendingCredentialOrder(ascending, credentials, sortField)
	}
	return snapshot
}

func descendingCredentialOrder(ascending []int, credentials []auth.CredentialInfo, sortField credentialSort) []int {
	missingStart := len(ascending)
	for position, index := range ascending {
		if credentialSortValueFor(credentials[index], sortField).Missing {
			missingStart = position
			break
		}
	}
	descending := make([]int, 0, len(ascending))
	for position := missingStart - 1; position >= 0; position-- {
		descending = append(descending, ascending[position])
	}
	for position := len(ascending) - 1; position >= missingStart; position-- {
		descending = append(descending, ascending[position])
	}
	return descending
}

func listCredentials(snapshot *adminPoolSnapshot, filter adminCredentialListFilter) credentialListPage {
	filter.Sort, filter.Order = normalizedCredentialSortOrder(filter.Sort, filter.Order)
	if filter.Query == "" && filter.Status == "" && filter.Usable == nil {
		return unfilteredCredentialPage(snapshot, filter)
	}

	capacity := len(snapshot.Credentials)
	if filter.Limit > 0 && capacity > filter.Limit+1 {
		capacity = filter.Limit + 1
	}
	page := credentialListPage{Data: make([]auth.CredentialInfo, 0, capacity)}
	query := normalizeCredentialQuery(filter.Query)
	cursorValue, _ := decodeCredentialSortKey(filter.Sort, filter.Cursor.LastKey)
	for _, index := range snapshot.Orders[credentialSortOrder{Sort: filter.Sort, Order: filter.Order}] {
		credential := snapshot.Credentials[index]
		if !credentialMatchesListFilter(credential, query, filter) {
			continue
		}
		page.Total++
		// A cursor from an older generation intentionally degrades to applying its
		// composite boundary to the current snapshot instead of failing the request.
		if filter.Cursor.LastID != "" && compareCredentialPositions(
			credentialSortValueFor(credential, filter.Sort), credential.ID,
			cursorValue, filter.Cursor.LastID, filter.Order,
		) <= 0 {
			continue
		}
		if filter.Limit == 0 || len(page.Data) < filter.Limit+1 {
			page.Data = append(page.Data, credential)
		}
	}

	if filter.Limit > 0 && len(page.Data) > filter.Limit {
		page.HasMore = true
		page.Data = page.Data[:filter.Limit]
		page.NextCursor = nextCredentialCursor(snapshot.Generation, page.Data[len(page.Data)-1], filter.Sort, filter.Order)
	}
	return page
}

func unfilteredCredentialPage(snapshot *adminPoolSnapshot, filter adminCredentialListFilter) credentialListPage {
	order := snapshot.Orders[credentialSortOrder{Sort: filter.Sort, Order: filter.Order}]
	page := credentialListPage{Total: len(snapshot.Credentials)}
	start := 0
	if filter.Cursor.LastID != "" {
		cursorValue, _ := decodeCredentialSortKey(filter.Sort, filter.Cursor.LastKey)
		start = sort.Search(len(order), func(position int) bool {
			credential := snapshot.Credentials[order[position]]
			return compareCredentialPositions(
				credentialSortValueFor(credential, filter.Sort), credential.ID,
				cursorValue, filter.Cursor.LastID, filter.Order,
			) > 0
		})
	}
	if filter.Limit == 0 {
		page.Data = credentialsForOrder(snapshot.Credentials, order[start:])
		return page
	}
	end := min(start+filter.Limit, len(order))
	page.Data = credentialsForOrder(snapshot.Credentials, order[start:end])
	if end < len(order) {
		page.HasMore = true
		page.NextCursor = nextCredentialCursor(snapshot.Generation, page.Data[len(page.Data)-1], filter.Sort, filter.Order)
	}
	return page
}

func credentialsForOrder(credentials []auth.CredentialInfo, order []int) []auth.CredentialInfo {
	items := make([]auth.CredentialInfo, len(order))
	for position, index := range order {
		items[position] = credentials[index]
	}
	return items
}

func credentialMatchesListFilter(credential auth.CredentialInfo, query string, filter adminCredentialListFilter) bool {
	if filter.Status != "" && credential.Status != filter.Status {
		return false
	}
	if filter.Usable != nil && credential.Usable != *filter.Usable {
		return false
	}
	if query == "" {
		return true
	}
	for _, value := range []string{credential.ID, credential.Scope, credential.SubscriptionTierDisplay} {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	for _, model := range credential.Models {
		if strings.Contains(strings.ToLower(model), query) {
			return true
		}
	}
	return false
}

func normalizeCredentialQuery(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func encodeCredentialCursor(cursor credentialCursor) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeCredentialCursor(value string) (credentialCursor, error) {
	if value == "" {
		return credentialCursor{}, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(payload) == 0 || base64.RawURLEncoding.EncodeToString(payload) != value {
		return credentialCursor{}, errors.New("invalid credential cursor")
	}
	var cursor credentialCursor
	if json.Unmarshal(payload, &cursor) != nil || cursor.Generation == 0 || cursor.LastID == "" ||
		!validCredentialSort(cursor.Sort) || !validCredentialOrder(cursor.Order) {
		return credentialCursor{}, errors.New("invalid credential cursor")
	}
	if _, err := decodeCredentialSortKey(cursor.Sort, cursor.LastKey); err != nil {
		return credentialCursor{}, errors.New("invalid credential cursor")
	}
	return cursor, nil
}

func nextCredentialCursor(generation uint64, credential auth.CredentialInfo, sortField credentialSort, order credentialOrder) string {
	return encodeCredentialCursor(credentialCursor{
		Generation: generation,
		Sort:       sortField,
		Order:      order,
		LastKey:    encodeCredentialSortKey(credentialSortValueFor(credential, sortField)),
		LastID:     credential.ID,
	})
}

func normalizedCredentialSortOrder(sortField credentialSort, order credentialOrder) (credentialSort, credentialOrder) {
	if !validCredentialSort(sortField) {
		sortField = credentialSortID
	}
	if !validCredentialOrder(order) {
		order = credentialOrderAsc
	}
	return sortField, order
}

func validCredentialSort(value credentialSort) bool {
	switch value {
	case credentialSortID, credentialSortTier, credentialSortStatus, credentialSortExpiresAt, credentialSortModelsCount, credentialSortUsable:
		return true
	default:
		return false
	}
}

func validCredentialOrder(value credentialOrder) bool {
	return value == credentialOrderAsc || value == credentialOrderDesc
}

func credentialSortValueFor(credential auth.CredentialInfo, sortField credentialSort) credentialSortValue {
	switch sortField {
	case credentialSortTier:
		if rank, ok := credentialTierRank(credential.SubscriptionTier); ok {
			return credentialSortValue{Number: int64(rank)}
		}
		return credentialSortValue{Missing: true}
	case credentialSortStatus:
		if rank, ok := credentialStatusRank(credential.Status); ok {
			return credentialSortValue{Number: int64(rank)}
		}
		return credentialSortValue{Missing: true}
	case credentialSortExpiresAt:
		if credential.ExpiresAt == nil {
			return credentialSortValue{Missing: true}
		}
		return credentialSortValue{Number: credential.ExpiresAt.UnixNano()}
	case credentialSortModelsCount:
		return credentialSortValue{Number: int64(len(credential.Models))}
	case credentialSortUsable:
		if credential.Usable {
			return credentialSortValue{Number: 0}
		}
		return credentialSortValue{Number: 1}
	default:
		return credentialSortValue{Text: credential.ID, IsText: true}
	}
}

func credentialTierRank(tier string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "free":
		return 0, true
	case "x_basic":
		return 1, true
	case "x_premium":
		return 2, true
	case "x_premium_plus":
		return 3, true
	case "supergrok":
		return 4, true
	case "supergrok_heavy":
		return 5, true
	case "supergrok_lite":
		return 6, true
	default:
		return 0, false
	}
}

func credentialStatusRank(status string) (int, bool) {
	switch status {
	case "ready":
		return 0, true
	case "cooling_down":
		return 1, true
	case "pending_models":
		return 2, true
	case "needs_refresh":
		return 3, true
	case "disabled":
		return 4, true
	default:
		return 0, false
	}
}

func compareCredentialPositions(aValue credentialSortValue, aID string, bValue credentialSortValue, bID string, order credentialOrder) int {
	comparison := compareCredentialSortValues(aValue, bValue)
	if !aValue.Missing && !bValue.Missing && order == credentialOrderDesc {
		comparison = -comparison
	}
	if comparison != 0 {
		return comparison
	}
	comparison = strings.Compare(aID, bID)
	if order == credentialOrderDesc {
		comparison = -comparison
	}
	return comparison
}

func compareCredentialSortValues(a, b credentialSortValue) int {
	if a.Missing != b.Missing {
		if a.Missing {
			return 1
		}
		return -1
	}
	if a.Missing {
		return 0
	}
	if a.IsText || b.IsText {
		return strings.Compare(a.Text, b.Text)
	}
	if a.Number < b.Number {
		return -1
	}
	if a.Number > b.Number {
		return 1
	}
	return 0
}

func encodeCredentialSortKey(value credentialSortValue) string {
	if value.Missing {
		return "m"
	}
	if value.IsText {
		return "s:" + value.Text
	}
	return "n:" + strconv.FormatInt(value.Number, 10)
}

func decodeCredentialSortKey(sortField credentialSort, raw string) (credentialSortValue, error) {
	if raw == "m" {
		if sortField == credentialSortTier || sortField == credentialSortStatus || sortField == credentialSortExpiresAt {
			return credentialSortValue{Missing: true}, nil
		}
		return credentialSortValue{}, errors.New("invalid credential sort key")
	}
	if sortField == credentialSortID {
		if !strings.HasPrefix(raw, "s:") || len(raw) == len("s:") {
			return credentialSortValue{}, errors.New("invalid credential sort key")
		}
		return credentialSortValue{Text: strings.TrimPrefix(raw, "s:"), IsText: true}, nil
	}
	if !strings.HasPrefix(raw, "n:") {
		return credentialSortValue{}, errors.New("invalid credential sort key")
	}
	number, err := strconv.ParseInt(strings.TrimPrefix(raw, "n:"), 10, 64)
	if err != nil {
		return credentialSortValue{}, errors.New("invalid credential sort key")
	}
	switch sortField {
	case credentialSortTier:
		if number < 0 || number > 6 {
			return credentialSortValue{}, errors.New("invalid credential sort key")
		}
	case credentialSortStatus:
		if number < 0 || number > 4 {
			return credentialSortValue{}, errors.New("invalid credential sort key")
		}
	case credentialSortModelsCount:
		if number < 0 {
			return credentialSortValue{}, errors.New("invalid credential sort key")
		}
	case credentialSortUsable:
		if number < 0 || number > 1 {
			return credentialSortValue{}, errors.New("invalid credential sort key")
		}
	case credentialSortExpiresAt:
	default:
		return credentialSortValue{}, errors.New("invalid credential sort key")
	}
	return credentialSortValue{Number: number}, nil
}

type adminCredentialRoutingRequest struct {
	BuildRouteMode     *string `json:"build_route_mode"`
	BuildSuperEntitled *bool   `json:"build_super_entitled"`
	BuildAPIFallback   *bool   `json:"build_api_fallback"`
}

func (s *Server) adminCredentialRouting(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !validCredentialID(id) {
		writeError(w, http.StatusNotFound, "credential not found", "invalid_request_error", "credential_not_found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminCredentialUpdateSize)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request adminCredentialRoutingRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid credential routing body", "invalid_request_error", "invalid_credential_routing")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "credential routing body must contain one JSON object", "invalid_request_error", "invalid_credential_routing")
		return
	}
	if request.BuildRouteMode == nil && request.BuildSuperEntitled == nil && request.BuildAPIFallback == nil {
		writeError(w, http.StatusBadRequest, "credential routing body has no updates", "invalid_request_error", "empty_credential_routing")
		return
	}
	if request.BuildAPIFallback != nil && *request.BuildAPIFallback {
		writeError(w, http.StatusBadRequest, "build_api_fallback is observational and may only be cleared", "invalid_request_error", "invalid_build_api_fallback")
		return
	}
	update := auth.BuildRoutingUpdate{
		SuperEntitledOverride: request.BuildSuperEntitled,
		APIFallback:           request.BuildAPIFallback,
	}
	if request.BuildRouteMode != nil {
		mode, err := auth.ParseBuildRouteMode(*request.BuildRouteMode)
		if err != nil {
			writeError(w, http.StatusBadRequest, "build_route_mode must be auto, build, or xai", "invalid_request_error", "invalid_build_route_mode")
			return
		}
		update.RouteMode = &mode
	}
	if _, err := s.pool.UpdateBuildRouting(id, update); err != nil {
		if errors.Is(err, auth.ErrCredentialNotFound) {
			writeError(w, http.StatusNotFound, "credential not found", "invalid_request_error", "credential_not_found")
			return
		}
		writeError(w, http.StatusInternalServerError, "credential routing could not be updated", "server_error", "credential_routing_update_failed")
		return
	}
	credential, ok := s.pool.Credential(id)
	if !ok {
		writeError(w, http.StatusNotFound, "credential not found", "invalid_request_error", "credential_not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "credential", "credential": credential})
}

func newAdminCredentialListResponse(page credentialListPage, paginated bool) adminCredentialListResponse {
	response := adminCredentialListResponse{Object: "list", Data: page.Data}
	if paginated {
		hasMore, nextCursor, total := page.HasMore, page.NextCursor, page.Total
		response.HasMore = &hasMore
		response.NextCursor = &nextCursor
		response.Total = &total
	}
	return response
}
