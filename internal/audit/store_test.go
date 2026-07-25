package audit

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreListCursorFiltersAndSummary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.July, 25, 12, 30, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "audit.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	records := []Record{
		{ID: "a", RequestID: "req-a", CreatedAt: now.Add(-2 * time.Hour).UnixMilli(), Protocol: "chat", Model: "grok-4", AccountID: "acct-a", TenantKey: "tenant-a", StatusCode: 200, DurationMS: 100, InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5, ReasoningTokens: 1, TotalTokens: 15, AttemptCount: 1},
		{ID: "b", RequestID: "req-b", CreatedAt: now.Add(-time.Hour).UnixMilli(), Protocol: "responses", Model: "grok-4", AccountID: "acct-b", TenantKey: "tenant-a", StatusCode: 500, Streaming: true, DurationMS: 300, InputTokens: 20, OutputTokens: 10, TotalTokens: 30, ErrorCode: "upstream_error", AttemptCount: 2},
		{ID: "c", RequestID: "req-c", CreatedAt: now.Add(-30 * time.Minute).UnixMilli(), Protocol: "messages", Model: "grok-3", AccountID: "acct-a", TenantKey: "tenant-b", StatusCode: 201, DurationMS: 200, InputTokens: 7, OutputTokens: 3, TotalTokens: 10, AttemptCount: 1},
	}
	if err := store.Insert(ctx, records...); err != nil {
		t.Fatal(err)
	}

	first, err := store.List(ctx, ListFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || !first.HasMore || first.NextCursor == "" || first.Items[0].ID != "c" || first.Items[1].ID != "b" {
		t.Fatalf("first page = %#v", first)
	}
	second, err := store.List(ctx, ListFilter{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.HasMore || second.Items[0].ID != "a" {
		t.Fatalf("second page = %#v", second)
	}

	failed, err := store.List(ctx, ListFilter{Limit: 10, Model: "grok-4", Status: "failed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed.Items) != 1 || failed.Items[0].ID != "b" {
		t.Fatalf("filtered page = %#v", failed)
	}

	summary, err := store.Summary(ctx, "today", now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 3 || summary.SuccessfulRequests != 2 || summary.FailedRequests != 1 {
		t.Fatalf("summary counts = %#v", summary.Metrics)
	}
	if summary.InputTokens != 37 || summary.CachedInputTokens != 2 || summary.OutputTokens != 18 || summary.ReasoningTokens != 1 || summary.TotalTokens != 55 {
		t.Fatalf("summary tokens = %#v", summary.Metrics)
	}
	if summary.AverageDurationMS != 200 || summary.SuccessRate < 0.666 || summary.SuccessRate > 0.667 {
		t.Fatalf("summary rates = %#v", summary.Metrics)
	}
	if len(summary.ByModel) != 2 || len(summary.Series) != 3 {
		t.Fatalf("summary groups: models=%d series=%d", len(summary.ByModel), len(summary.Series))
	}
	for _, bucket := range summary.Series {
		if bucket.Bucket == "" || bucket.Start != bucket.Bucket || bucket.End == "" {
			t.Fatalf("invalid series bucket = %#v", bucket)
		}
	}
}

func TestStoreRetentionAndAsyncCloseFlush(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "audit.db")
	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	store, err := Open(dbPath, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, Record{ID: "old", CreatedAt: now.AddDate(0, 0, -31).UnixMilli(), Protocol: "chat", StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.Cleanup(ctx, now); err != nil || deleted != 1 {
		t.Fatalf("Cleanup() = %d, %v", deleted, err)
	}
	store.Enqueue(Record{ID: "queued", CreatedAt: now.UnixMilli(), Protocol: "chat", StatusCode: 200})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dbPath, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page, err := reopened.List(ctx, ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "queued" {
		t.Fatalf("items after reopen = %#v", page.Items)
	}
}
