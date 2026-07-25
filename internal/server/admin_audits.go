package server

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/audit"
)

type dashboardResources struct {
	TotalAccounts    int `json:"total_accounts"`
	UsableAccounts   int `json:"usable_accounts"`
	DisabledAccounts int `json:"disabled_accounts"`
	CoolingAccounts  int `json:"cooling_accounts"`
	PaidAccounts     int `json:"paid_accounts"`
	TotalModels      int `json:"total_models"`
}

type dashboardResponse struct {
	Period      string               `json:"period"`
	GeneratedAt string               `json:"generated_at"`
	Range       audit.TimeRange      `json:"range"`
	Resources   dashboardResources   `json:"resources"`
	Usage       audit.Metrics        `json:"usage"`
	ByModel     []audit.ModelMetrics `json:"by_model"`
	Series      []audit.SeriesBucket `json:"series"`
}

func (s *Server) adminAudits(w http.ResponseWriter, r *http.Request) {
	filter, err := auditListFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_parameter")
		return
	}
	if s.audits == nil {
		limit := filter.Limit
		if limit <= 0 {
			limit = 50
		}
		writeJSON(w, http.StatusOK, audit.ListPage{Items: []audit.Record{}, PageSize: limit})
		return
	}
	page, err := s.audits.List(r.Context(), filter)
	if err != nil {
		if errors.Is(err, audit.ErrInvalidCursor) || strings.Contains(err.Error(), "status must") {
			writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_parameter")
			return
		}
		slog.Error("query request audits", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to query request audits", "server_error", "audit_query_failed")
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) adminAuditHealth(w http.ResponseWriter, _ *http.Request) {
	if s.audits == nil {
		writeJSON(w, http.StatusOK, audit.QueueHealth{})
		return
	}
	writeJSON(w, http.StatusOK, s.audits.Health(time.Now()))
}

func (s *Server) adminAuditSummary(w http.ResponseWriter, r *http.Request) {
	period := auditPeriod(r)
	summary, err := s.auditSummary(r, period)
	if err != nil {
		writeAuditSummaryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) adminDashboard(w http.ResponseWriter, r *http.Request) {
	period := auditPeriod(r)
	summary, err := s.auditSummary(r, period)
	if err != nil {
		writeAuditSummaryError(w, err)
		return
	}
	resources := dashboardResources{}
	_, credentials := s.pool.CredentialSnapshot()
	resources.TotalAccounts = len(credentials)
	for _, credential := range credentials {
		if credential.Usable {
			resources.UsableAccounts++
		}
		if credential.Disabled {
			resources.DisabledAccounts++
		}
		if credential.Status == "cooling_down" {
			resources.CoolingAccounts++
		}
		if credential.Paid {
			resources.PaidAccounts++
		}
	}
	resources.TotalModels = len(s.pool.Models())
	writeJSON(w, http.StatusOK, dashboardResponse{
		Period: summary.Period, GeneratedAt: summary.GeneratedAt, Range: summary.Range,
		Resources: resources, Usage: summary.Metrics, ByModel: summary.ByModel, Series: summary.Series,
	})
}

func (s *Server) auditSummary(r *http.Request, period string) (audit.Summary, error) {
	if s.audits == nil {
		return audit.EmptySummary(period, time.Now())
	}
	return s.audits.Summary(r.Context(), period, time.Now())
}

func auditListFilter(r *http.Request) (audit.ListFilter, error) {
	query := r.URL.Query()
	filter := audit.ListFilter{
		Cursor: query.Get("cursor"), Model: query.Get("model"), AccountID: query.Get("account_id"),
		Protocol: query.Get("protocol"), Status: query.Get("status"),
	}
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			return audit.ListFilter{}, errors.New("limit must be an integer between 1 and 200")
		}
		filter.Limit = limit
	}
	if filter.Protocol != "" && filter.Protocol != "chat" && filter.Protocol != "responses" && filter.Protocol != "messages" {
		return audit.ListFilter{}, errors.New("protocol must be chat, responses, or messages")
	}
	if filter.Status != "" {
		switch strings.ToLower(filter.Status) {
		case "success", "successful", "failed", "failure":
		default:
			status, statusErr := strconv.Atoi(filter.Status)
			if statusErr != nil || status < 100 || status > 599 {
				return audit.ListFilter{}, errors.New("status must be an HTTP status, success, or failed")
			}
		}
	}
	var err error
	if filter.Since, err = parseAuditTime(query.Get("since")); err != nil {
		return audit.ListFilter{}, errors.New("since must be Unix time or RFC3339")
	}
	if filter.Until, err = parseAuditTime(query.Get("until")); err != nil {
		return audit.ListFilter{}, errors.New("until must be Unix time or RFC3339")
	}
	if filter.Since != 0 && filter.Until != 0 && filter.Since > filter.Until {
		return audit.ListFilter{}, errors.New("since must not be later than until")
	}
	return filter, nil
}

func parseAuditTime(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	if value, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if value > -1_000_000_000_000 && value < 1_000_000_000_000 {
			value *= 1000
		}
		return value, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0, err
	}
	return value.UnixMilli(), nil
}

func auditPeriod(r *http.Request) string {
	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		return "today"
	}
	return period
}

func writeAuditSummaryError(w http.ResponseWriter, err error) {
	if errors.Is(err, audit.ErrInvalidPeriod) {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_period")
		return
	}
	slog.Error("query audit summary", "error", err)
	writeError(w, http.StatusInternalServerError, "failed to query audit summary", "server_error", "audit_query_failed")
}
