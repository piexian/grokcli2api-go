package audit

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStoreListCursorFiltersAndSummary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.July, 25, 12, 30, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "audit.db"), 30, DefaultQueueSize)
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
	store, err := Open(dbPath, 30, DefaultQueueSize)
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

	reopened, err := Open(dbPath, 30, DefaultQueueSize)
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

func TestStoreQueueDropHealthAndModelTruncation(t *testing.T) {
	store := &Store{queue: make(chan Record, 1)}
	if !store.Enqueue(Record{ID: "queued"}) {
		t.Fatal("first enqueue failed")
	}
	if store.Enqueue(Record{ID: "dropped"}) {
		t.Fatal("second enqueue unexpectedly succeeded")
	}
	health := store.Health(time.Now().Add(25 * time.Millisecond))
	if store.Dropped() != 1 || health.QueueSize != 1 || health.QueueCap != 1 || health.DroppedTotal != 1 || health.OldestPendingMS < 20 {
		t.Fatalf("queue health = %#v, dropped = %d", health, store.Dropped())
	}
	now := time.Now()
	store.pendingTimes = []time.Time{now.Add(-time.Second), now.Add(-25 * time.Millisecond)}
	store.pendingHead = 0
	store.completePending(1)
	if oldest := store.Health(now).OldestPendingMS; oldest < 20 || oldest > 50 {
		t.Fatalf("oldest pending after completion = %dms", oldest)
	}

	capture := NewCapture("chat", "request", time.Now())
	capture.SetRequest(strings.Repeat("界", 300), false)
	record := capture.Finish(200, time.Now())
	if utf8.RuneCountInString(record.Model) != maxCapturedModelCharacters {
		t.Fatalf("captured model characters = %d", utf8.RuneCountInString(record.Model))
	}
}

func TestStoreBatchRetriesAndCountsPermanentFailure(t *testing.T) {
	expected := errors.New("write failed")
	attempts := 0
	store := &Store{
		retryDelays: []time.Duration{0, 0, 0},
		batchWriter: func(context.Context, []Record) error {
			attempts++
			if attempts < 4 {
				return expected
			}
			return nil
		},
	}
	if err := store.writeBatchWithRetry(context.Background(), []Record{{ID: "retry"}}); err != nil {
		t.Fatal(err)
	}
	if attempts != 4 || store.Dropped() != 0 {
		t.Fatalf("successful retry attempts=%d dropped=%d", attempts, store.Dropped())
	}

	failedAttempts := 0
	failed := &Store{
		retryDelays: []time.Duration{0, 0, 0},
		batchWriter: func(context.Context, []Record) error {
			failedAttempts++
			return expected
		},
	}
	if err := failed.writeBatchWithRetry(context.Background(), []Record{{ID: "one"}, {ID: "two"}}); !errors.Is(err, expected) {
		t.Fatalf("permanent failure = %v", err)
	}
	if failedAttempts != 4 || failed.Dropped() != 2 {
		t.Fatalf("permanent failure attempts=%d dropped=%d", failedAttempts, failed.Dropped())
	}
}

func TestStoreSummaryCachesByPeriodForTenSeconds(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "audit.db"), 30, DefaultQueueSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	periodNow := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	cacheNow := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	store.summaryNow = func() time.Time { return cacheNow }

	if err := store.Insert(ctx, Record{ID: "first", CreatedAt: periodNow.Add(-time.Hour).UnixMilli(), Protocol: "chat", StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	first, err := store.Summary(ctx, "today", periodNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Insert(ctx, Record{ID: "second", CreatedAt: periodNow.Add(-30 * time.Minute).UnixMilli(), Protocol: "chat", StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	cached, err := store.Summary(ctx, "today", periodNow)
	if err != nil {
		t.Fatal(err)
	}
	if first.Requests != 1 || cached.Requests != 1 || cached.GeneratedAt != first.GeneratedAt {
		t.Fatalf("cached summaries = first %#v, second %#v", first, cached)
	}

	cacheNow = cacheNow.Add(11 * time.Second)
	refreshed, err := store.Summary(ctx, "today", periodNow)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Requests != 2 {
		t.Fatalf("refreshed summary requests = %d, want 2", refreshed.Requests)
	}
}

func TestStoreRetentionDeletesInBatchesOfFiveHundred(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "audit.db"), 30, DefaultQueueSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.cleanupBatchSize != 500 || store.cleanupDelay != 10*time.Millisecond {
		t.Fatalf("cleanup defaults = batch %d delay %s", store.cleanupBatchSize, store.cleanupDelay)
	}

	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	records := make([]Record, 0, 1002)
	for index := 0; index < 1001; index++ {
		records = append(records, Record{
			ID: fmt.Sprintf("old-%04d", index), CreatedAt: now.AddDate(0, 0, -31).UnixMilli(), Protocol: "chat", StatusCode: 200,
		})
	}
	records = append(records, Record{ID: "current", CreatedAt: now.UnixMilli(), Protocol: "chat", StatusCode: 200})
	if err := store.Insert(ctx, records...); err != nil {
		t.Fatal(err)
	}
	yields := 0
	store.cleanupWait = func(_ context.Context, delay time.Duration) error {
		if delay != 10*time.Millisecond {
			t.Fatalf("cleanup delay = %s", delay)
		}
		yields++
		return nil
	}
	deleted, err := store.Cleanup(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1001 || yields != 2 {
		t.Fatalf("Cleanup() deleted=%d yields=%d", deleted, yields)
	}
	page, err := store.List(ctx, ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "current" {
		t.Fatalf("remaining records = %#v", page.Items)
	}
}
