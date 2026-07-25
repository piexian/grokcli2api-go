package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidCursor = errors.New("invalid audit cursor")
	ErrInvalidPeriod = errors.New("period must be today, 7d, or 30d")
)

type ListFilter struct {
	Cursor, Model, AccountID, Protocol, Status string
	Limit                                      int
	Since, Until                               int64
}

type ListPage struct {
	Items      []Record `json:"items"`
	PageSize   int      `json:"page_size"`
	NextCursor string   `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
}

type Metrics struct {
	Requests           int64   `json:"requests"`
	SuccessfulRequests int64   `json:"successful_requests"`
	FailedRequests     int64   `json:"failed_requests"`
	InputTokens        int64   `json:"input_tokens"`
	CachedInputTokens  int64   `json:"cached_input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens"`
	TotalTokens        int64   `json:"total_tokens"`
	AverageDurationMS  float64 `json:"average_duration_ms"`
	SuccessRate        float64 `json:"success_rate"`
}

type ModelMetrics struct {
	Model string `json:"model"`
	Metrics
}

type SeriesBucket struct {
	Bucket string `json:"bucket"`
	Start  string `json:"start"`
	End    string `json:"end"`
	Metrics
}

type TimeRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type Summary struct {
	Period      string    `json:"period"`
	GeneratedAt string    `json:"generated_at"`
	Range       TimeRange `json:"range"`
	Metrics
	ByModel []ModelMetrics `json:"by_model"`
	Series  []SeriesBucket `json:"series"`
}

type cursorValue struct {
	CreatedAt int64  `json:"created_at"`
	ID        string `json:"id"`
}

func (s *Store) List(ctx context.Context, filter ListFilter) (ListPage, error) {
	if err := s.Flush(ctx); err != nil {
		return ListPage{}, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	conditions := []string{"1=1"}
	args := make([]any, 0, 10)
	add := func(condition string, value any) {
		conditions = append(conditions, condition)
		args = append(args, value)
	}
	if filter.Model != "" {
		add("model = ?", filter.Model)
	}
	if filter.AccountID != "" {
		add("account_id = ?", filter.AccountID)
	}
	if filter.Protocol != "" {
		add("protocol = ?", filter.Protocol)
	}
	if filter.Since != 0 {
		add("created_at >= ?", filter.Since)
	}
	if filter.Until != 0 {
		add("created_at <= ?", filter.Until)
	}
	if filter.Status != "" {
		switch strings.ToLower(filter.Status) {
		case "success", "successful":
			conditions = append(conditions, "status_code >= 200 AND status_code < 400 AND error_code = ''")
		case "failed", "failure":
			conditions = append(conditions, "(status_code >= 400 OR error_code != '')")
		default:
			status, err := strconv.Atoi(filter.Status)
			if err != nil || status < 100 || status > 599 {
				return ListPage{}, fmt.Errorf("status must be an HTTP status, success, or failed")
			}
			add("status_code = ?", status)
		}
	}
	if filter.Cursor != "" {
		cursor, err := decodeCursor(filter.Cursor)
		if err != nil {
			return ListPage{}, err
		}
		conditions = append(conditions, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	args = append(args, limit+1)
	query := `SELECT id, request_id, created_at, protocol, model, account_id, tenant_key,
		status_code, streaming, duration_ms, input_tokens, cached_input_tokens,
		output_tokens, reasoning_tokens, total_tokens, error_code, attempt_count
		FROM request_audits WHERE ` + strings.Join(conditions, " AND ") +
		" ORDER BY created_at DESC, id DESC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ListPage{}, err
	}
	defer rows.Close()
	items := make([]Record, 0, limit+1)
	for rows.Next() {
		var record Record
		var streaming int
		if err := rows.Scan(
			&record.ID, &record.RequestID, &record.CreatedAt, &record.Protocol, &record.Model,
			&record.AccountID, &record.TenantKey, &record.StatusCode, &streaming,
			&record.DurationMS, &record.InputTokens, &record.CachedInputTokens,
			&record.OutputTokens, &record.ReasoningTokens, &record.TotalTokens,
			&record.ErrorCode, &record.AttemptCount,
		); err != nil {
			return ListPage{}, err
		}
		record.Streaming = streaming != 0
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return ListPage{}, err
	}
	page := ListPage{Items: items, PageSize: limit}
	if len(items) > limit {
		page.HasMore = true
		page.Items = items[:limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeCursor(cursorValue{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return page, nil
}

func (s *Store) Summary(ctx context.Context, period string, now time.Time) (Summary, error) {
	if err := s.Flush(ctx); err != nil {
		return Summary{}, err
	}
	return s.summary(ctx, period, now)
}

func EmptySummary(period string, now time.Time) (Summary, error) {
	return (&Store{}).summary(context.Background(), period, now)
}

func (s *Store) summary(ctx context.Context, period string, now time.Time) (Summary, error) {
	periodRange, bucketWidth, err := parsePeriod(period, now)
	if err != nil {
		return Summary{}, err
	}
	if s.db == nil {
		return Summary{
			Period: period, GeneratedAt: periodRange.end.Format(time.RFC3339),
			Range:   TimeRange{Start: periodRange.start.Format(time.RFC3339), End: periodRange.end.Format(time.RFC3339)},
			ByModel: []ModelMetrics{}, Series: []SeriesBucket{},
		}, nil
	}
	startMS, endMS := periodRange.start.UnixMilli(), periodRange.end.UnixMilli()
	metrics, err := s.queryMetrics(ctx, "created_at >= ? AND created_at < ?", startMS, endMS)
	if err != nil {
		return Summary{}, err
	}
	byModel, err := s.queryByModel(ctx, startMS, endMS)
	if err != nil {
		return Summary{}, err
	}
	series, err := s.querySeries(ctx, periodRange, bucketWidth)
	if err != nil {
		return Summary{}, err
	}
	return Summary{
		Period: period, GeneratedAt: periodRange.end.Format(time.RFC3339),
		Range:   TimeRange{Start: periodRange.start.Format(time.RFC3339), End: periodRange.end.Format(time.RFC3339)},
		Metrics: metrics, ByModel: byModel, Series: series,
	}, nil
}

const metricsColumns = `COUNT(*),
	COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 400 AND error_code = '' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status_code >= 400 OR error_code != '' THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(input_tokens), 0), COALESCE(SUM(cached_input_tokens), 0),
	COALESCE(SUM(output_tokens), 0), COALESCE(SUM(reasoning_tokens), 0),
	COALESCE(SUM(total_tokens), 0), COALESCE(AVG(duration_ms), 0)`

type scanner interface {
	Scan(...any) error
}

func scanMetrics(row scanner) (Metrics, error) {
	var metrics Metrics
	err := row.Scan(
		&metrics.Requests, &metrics.SuccessfulRequests, &metrics.FailedRequests,
		&metrics.InputTokens, &metrics.CachedInputTokens, &metrics.OutputTokens,
		&metrics.ReasoningTokens, &metrics.TotalTokens, &metrics.AverageDurationMS,
	)
	if err == nil && metrics.Requests > 0 {
		metrics.SuccessRate = float64(metrics.SuccessfulRequests) / float64(metrics.Requests)
	}
	return metrics, err
}

func (s *Store) queryMetrics(ctx context.Context, condition string, args ...any) (Metrics, error) {
	return scanMetrics(s.db.QueryRowContext(ctx, "SELECT "+metricsColumns+" FROM request_audits WHERE "+condition, args...))
}

func (s *Store) queryByModel(ctx context.Context, start, end int64) ([]ModelMetrics, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT model, "+metricsColumns+` FROM request_audits
		WHERE created_at >= ? AND created_at < ? GROUP BY model ORDER BY COUNT(*) DESC, model`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ModelMetrics, 0)
	for rows.Next() {
		var item ModelMetrics
		if err := rows.Scan(
			&item.Model, &item.Requests, &item.SuccessfulRequests, &item.FailedRequests,
			&item.InputTokens, &item.CachedInputTokens, &item.OutputTokens,
			&item.ReasoningTokens, &item.TotalTokens, &item.AverageDurationMS,
		); err != nil {
			return nil, err
		}
		if item.Requests > 0 {
			item.SuccessRate = float64(item.SuccessfulRequests) / float64(item.Requests)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type periodBounds struct {
	start, end time.Time
}

func (s *Store) querySeries(ctx context.Context, bounds periodBounds, width time.Duration) ([]SeriesBucket, error) {
	widthMS := width.Milliseconds()
	rows, err := s.db.QueryContext(ctx, "SELECT ((created_at - ?) / ?) AS bucket, "+metricsColumns+` FROM request_audits
		WHERE created_at >= ? AND created_at < ? GROUP BY bucket ORDER BY bucket`,
		bounds.start.UnixMilli(), widthMS, bounds.start.UnixMilli(), bounds.end.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]SeriesBucket, 0)
	for rows.Next() {
		var bucket int64
		var item SeriesBucket
		if err := rows.Scan(
			&bucket, &item.Requests, &item.SuccessfulRequests, &item.FailedRequests,
			&item.InputTokens, &item.CachedInputTokens, &item.OutputTokens,
			&item.ReasoningTokens, &item.TotalTokens, &item.AverageDurationMS,
		); err != nil {
			return nil, err
		}
		if item.Requests > 0 {
			item.SuccessRate = float64(item.SuccessfulRequests) / float64(item.Requests)
		}
		start := bounds.start.Add(time.Duration(bucket) * width)
		end := start.Add(width)
		if end.After(bounds.end) {
			end = bounds.end
		}
		item.Bucket = start.Format(time.RFC3339)
		item.Start, item.End = item.Bucket, end.Format(time.RFC3339)
		items = append(items, item)
	}
	return items, rows.Err()
}

func parsePeriod(period string, now time.Time) (periodBounds, time.Duration, error) {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case "today":
		return periodBounds{start: startOfToday, end: now}, time.Hour, nil
	case "7d":
		return periodBounds{start: startOfToday.AddDate(0, 0, -6), end: now}, 24 * time.Hour, nil
	case "30d":
		return periodBounds{start: startOfToday.AddDate(0, 0, -29), end: now}, 24 * time.Hour, nil
	default:
		return periodBounds{}, 0, ErrInvalidPeriod
	}
}

func encodeCursor(cursor cursorValue) string {
	payload, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeCursor(value string) (cursorValue, error) {
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursorValue{}, ErrInvalidCursor
	}
	var cursor cursorValue
	if json.Unmarshal(payload, &cursor) != nil || cursor.CreatedAt == 0 || cursor.ID == "" {
		return cursorValue{}, ErrInvalidCursor
	}
	return cursor, nil
}
