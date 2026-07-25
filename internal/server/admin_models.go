package server

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
)

type adminModelSummary struct {
	Model          string         `json:"model"`
	Accounts       int            `json:"accounts"`
	UsableAccounts int            `json:"usable_accounts"`
	StatusCounts   map[string]int `json:"status_counts"`
}

type adminModelsSummaryResponse struct {
	Object      string              `json:"object"`
	Data        []adminModelSummary `json:"data"`
	TotalModels int                 `json:"total_models"`
}

type adminModelSummarySnapshot struct {
	Generation  uint64
	Credentials []auth.CredentialInfo
	Data        []adminModelSummary
}

type adminModelSummaryCache struct {
	mu      sync.Mutex
	current atomic.Pointer[adminModelSummarySnapshot]
}

func (c *adminModelSummaryCache) get(generation uint64, credentials []auth.CredentialInfo) *adminModelSummarySnapshot {
	if current := c.current.Load(); sameAdminModelSummarySnapshot(current, generation, credentials) {
		return current
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if current := c.current.Load(); sameAdminModelSummarySnapshot(current, generation, credentials) {
		return current
	}
	next := &adminModelSummarySnapshot{
		Generation: generation, Credentials: credentials, Data: summarizeAdminModels(credentials),
	}
	c.current.Store(next)
	return next
}

func sameAdminModelSummarySnapshot(snapshot *adminModelSummarySnapshot, generation uint64, credentials []auth.CredentialInfo) bool {
	if snapshot == nil || snapshot.Generation != generation || len(snapshot.Credentials) != len(credentials) {
		return false
	}
	if len(credentials) == 0 {
		return true
	}
	return &snapshot.Credentials[0] == &credentials[0]
}

func summarizeAdminModels(credentials []auth.CredentialInfo) []adminModelSummary {
	byModel := make(map[string]*adminModelSummary)
	seenAt := make(map[string]int)
	for credentialIndex, credential := range credentials {
		seenMarker := credentialIndex + 1
		for _, model := range credential.Models {
			if model == "" || seenAt[model] == seenMarker {
				continue
			}
			seenAt[model] = seenMarker
			summary := byModel[model]
			if summary == nil {
				summary = &adminModelSummary{Model: model, StatusCounts: make(map[string]int)}
				byModel[model] = summary
			}
			summary.Accounts++
			if credential.Usable {
				summary.UsableAccounts++
			}
			summary.StatusCounts[credential.Status]++
		}
	}
	data := make([]adminModelSummary, 0, len(byModel))
	for _, summary := range byModel {
		data = append(data, *summary)
	}
	sort.Slice(data, func(i, j int) bool { return data[i].Model < data[j].Model })
	return data
}

func filterAdminModelSummaries(summaries []adminModelSummary, query string) []adminModelSummary {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return summaries
	}
	filtered := make([]adminModelSummary, 0)
	for _, summary := range summaries {
		if strings.Contains(strings.ToLower(summary.Model), query) {
			filtered = append(filtered, summary)
		}
	}
	return filtered
}

func (s *Server) adminModelsSummary(w http.ResponseWriter, r *http.Request) {
	generation, credentials := s.pool.CredentialSnapshot()
	snapshot := s.adminModels.get(generation, credentials)
	data := filterAdminModelSummaries(snapshot.Data, r.URL.Query().Get("q"))
	writeJSON(w, http.StatusOK, adminModelsSummaryResponse{
		Object: "list", Data: data, TotalModels: len(data),
	})
}
