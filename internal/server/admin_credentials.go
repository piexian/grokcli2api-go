package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
)

const maxAdminCredentialsPageSize = 1000

type adminCredentialListFilter struct {
	Limit  int
	Cursor credentialCursor
	Query  string
	Status string
	Usable *bool
}

type credentialCursor struct {
	Generation uint64 `json:"generation"`
	ID         string `json:"id"`
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
	filter := adminCredentialListFilter{Query: normalizeCredentialQuery(query.Get("q"))}

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

func listCredentials(credentials []auth.CredentialInfo, generation uint64, filter adminCredentialListFilter) credentialListPage {
	if filter.Query == "" && filter.Status == "" && filter.Usable == nil {
		return unfilteredCredentialPage(credentials, generation, filter)
	}

	capacity := len(credentials)
	if filter.Limit > 0 && capacity > filter.Limit+1 {
		capacity = filter.Limit + 1
	}
	page := credentialListPage{Data: make([]auth.CredentialInfo, 0, capacity)}
	query := normalizeCredentialQuery(filter.Query)
	for _, credential := range credentials {
		if !credentialMatchesListFilter(credential, query, filter) {
			continue
		}
		page.Total++
		// A cursor from an older generation intentionally degrades to applying its
		// ID boundary to the current snapshot instead of failing the request.
		if filter.Cursor.ID != "" && credential.ID <= filter.Cursor.ID {
			continue
		}
		if filter.Limit == 0 || len(page.Data) < filter.Limit+1 {
			page.Data = append(page.Data, credential)
		}
	}

	if filter.Limit > 0 && len(page.Data) > filter.Limit {
		page.HasMore = true
		page.Data = page.Data[:filter.Limit]
		page.NextCursor = encodeCredentialCursor(credentialCursor{Generation: generation, ID: page.Data[len(page.Data)-1].ID})
	}
	return page
}

func unfilteredCredentialPage(credentials []auth.CredentialInfo, generation uint64, filter adminCredentialListFilter) credentialListPage {
	page := credentialListPage{Total: len(credentials)}
	start := 0
	if filter.Cursor.ID != "" {
		start = sort.Search(len(credentials), func(index int) bool { return credentials[index].ID > filter.Cursor.ID })
	}
	if filter.Limit == 0 {
		page.Data = credentials[start:]
		return page
	}
	end := min(start+filter.Limit, len(credentials))
	page.Data = credentials[start:end]
	if end < len(credentials) {
		page.HasMore = true
		page.NextCursor = encodeCredentialCursor(credentialCursor{Generation: generation, ID: page.Data[len(page.Data)-1].ID})
	}
	return page
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
	if json.Unmarshal(payload, &cursor) != nil || cursor.Generation == 0 || cursor.ID == "" {
		return credentialCursor{}, errors.New("invalid credential cursor")
	}
	return cursor, nil
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
