package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const (
	DefaultQueueSize    = 4096
	defaultBatchSize    = 100
	flushInterval       = 250 * time.Millisecond
	retentionBatchSize  = 500
	retentionBatchDelay = 10 * time.Millisecond
	retentionTimeout    = 5 * time.Minute
)

var defaultBatchRetryDelays = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}

var ErrClosed = errors.New("audit store is closed")

type Store struct {
	db               *sql.DB
	retentionDays    int
	queue            chan Record
	flushRequests    chan chan error
	stop             chan struct{}
	done             chan struct{}
	closed           atomic.Bool
	dropped          atomic.Uint64
	pendingMu        sync.Mutex
	pendingTimes     []time.Time
	pendingHead      int
	closeOnce        sync.Once
	enqueueMu        sync.RWMutex
	closeErr         error
	batchWriter      func(context.Context, []Record) error
	retryDelays      []time.Duration
	cleanupBatchSize int
	cleanupDelay     time.Duration
	cleanupWait      func(context.Context, time.Duration) error
	summaryMu        sync.Mutex
	summaryCache     map[string]summaryCacheEntry
	summaryTTL       time.Duration
	summaryNow       func() time.Time
}

type QueueHealth struct {
	QueueSize       int    `json:"queue_size"`
	QueueCap        int    `json:"queue_cap"`
	DroppedTotal    uint64 `json:"dropped_total"`
	OldestPendingMS int64  `json:"oldest_pending_ms"`
}

func Open(path string, retentionDays, queueSize int) (*Store, error) {
	if path == "" {
		return nil, errors.New("audit database path is empty")
	}
	if retentionDays < 1 {
		return nil, errors.New("audit retention days must be positive")
	}
	if queueSize < 1 {
		return nil, errors.New("audit queue size must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit database directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create audit database: %w", err)
	}
	if closeErr := file.Close(); closeErr != nil {
		return nil, fmt.Errorf("close audit database file: %w", closeErr)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("secure audit database permissions: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open audit database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{
		db: db, retentionDays: retentionDays, queue: make(chan Record, queueSize),
		flushRequests: make(chan chan error), stop: make(chan struct{}), done: make(chan struct{}),
		retryDelays:      append([]time.Duration(nil), defaultBatchRetryDelays...),
		cleanupBatchSize: retentionBatchSize, cleanupDelay: retentionBatchDelay, cleanupWait: waitForDelay,
		summaryCache: make(map[string]summaryCacheEntry), summaryTTL: defaultSummaryTTL, summaryNow: time.Now,
	}
	store.batchWriter = store.insertBatch
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := store.Cleanup(context.Background(), time.Now()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("clean expired audit records: %w", err)
	}
	go store.run()
	return store, nil
}

func (s *Store) initialize(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS request_audits (
			id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			protocol TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			tenant_key TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL,
			streaming INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL,
			input_tokens INTEGER NOT NULL,
			cached_input_tokens INTEGER NOT NULL,
			output_tokens INTEGER NOT NULL,
			reasoning_tokens INTEGER NOT NULL,
			total_tokens INTEGER NOT NULL,
			error_code TEXT NOT NULL DEFAULT '',
			attempt_count INTEGER NOT NULL
		)`,
		"CREATE INDEX IF NOT EXISTS idx_request_audits_created_at ON request_audits(created_at)",
		"CREATE INDEX IF NOT EXISTS idx_request_audits_model_created_at ON request_audits(model, created_at)",
		"CREATE INDEX IF NOT EXISTS idx_request_audits_account_created_at ON request_audits(account_id, created_at)",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize audit database: %w", err)
		}
	}
	return nil
}

func (s *Store) Enqueue(record Record) bool {
	if s == nil {
		return false
	}
	s.enqueueMu.RLock()
	defer s.enqueueMu.RUnlock()
	if s.closed.Load() {
		return false
	}
	record = normalizeRecord(record)
	s.pendingMu.Lock()
	select {
	case s.queue <- record:
		s.pendingTimes = append(s.pendingTimes, time.Now())
		s.pendingMu.Unlock()
		return true
	default:
		s.pendingMu.Unlock()
		dropped := s.recordDropped(1)
		if dropped == 1 || dropped%100 == 0 {
			slog.Warn("audit queue full; record dropped", "dropped", dropped)
		}
		return false
	}
}

func (s *Store) Insert(ctx context.Context, records ...Record) error {
	if s == nil || s.closed.Load() {
		return ErrClosed
	}
	for index := range records {
		records[index] = normalizeRecord(records[index])
	}
	return s.writeBatchWithRetry(ctx, records)
}

func (s *Store) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

func (s *Store) Health(now time.Time) QueueHealth {
	if s == nil {
		return QueueHealth{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	health := QueueHealth{QueueSize: len(s.queue), QueueCap: cap(s.queue), DroppedTotal: s.Dropped()}
	s.pendingMu.Lock()
	if s.pendingHead < len(s.pendingTimes) {
		health.OldestPendingMS = max(now.Sub(s.pendingTimes[s.pendingHead]).Milliseconds(), 0)
	}
	s.pendingMu.Unlock()
	return health
}

func (s *Store) Cleanup(ctx context.Context, now time.Time) (int64, error) {
	if s == nil {
		return 0, ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, retentionTimeout)
	defer cancel()
	cutoff := now.AddDate(0, 0, -s.retentionDays).UnixMilli()
	batchSize := s.cleanupBatchSize
	if batchSize < 1 {
		batchSize = retentionBatchSize
	}
	wait := s.cleanupWait
	if wait == nil {
		wait = waitForDelay
	}
	var total int64
	for {
		result, err := s.db.ExecContext(ctx, `DELETE FROM request_audits WHERE rowid IN (
			SELECT rowid FROM request_audits WHERE created_at < ? ORDER BY created_at LIMIT ?
		)`, cutoff, batchSize)
		if err != nil {
			return total, err
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return total, err
		}
		total += deleted
		if deleted < int64(batchSize) {
			return total, nil
		}
		if err := wait(ctx, s.cleanupDelay); err != nil {
			return total, err
		}
	}
}

func (s *Store) Flush(ctx context.Context) error {
	if s == nil || s.closed.Load() {
		return ErrClosed
	}
	result := make(chan error, 1)
	select {
	case s.flushRequests <- result:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return ErrClosed
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return ErrClosed
	}
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.enqueueMu.Lock()
		s.closed.Store(true)
		close(s.stop)
		s.enqueueMu.Unlock()
		<-s.done
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

func (s *Store) run() {
	defer close(s.done)
	flushTicker := time.NewTicker(flushInterval)
	cleanupTicker := time.NewTicker(24 * time.Hour)
	defer flushTicker.Stop()
	defer cleanupTicker.Stop()
	batch := make([]Record, 0, defaultBatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.writeBatchWithRetry(context.Background(), batch)
		if err != nil {
			slog.Error("write audit batch", "error", err, "records", len(batch))
		}
		s.completePending(len(batch))
		batch = batch[:0]
		return err
	}
	drain := func() {
		for {
			select {
			case record := <-s.queue:
				batch = append(batch, record)
			default:
				return
			}
		}
	}
	for {
		select {
		case record := <-s.queue:
			batch = append(batch, record)
			if len(batch) >= defaultBatchSize {
				_ = flush()
			}
		case <-flushTicker.C:
			_ = flush()
		case result := <-s.flushRequests:
			drain()
			result <- flush()
		case now := <-cleanupTicker.C:
			_ = flush()
			if deleted, err := s.Cleanup(context.Background(), now); err != nil {
				slog.Error("clean expired audit records", "error", err)
			} else if deleted > 0 {
				slog.Info("expired audit records removed", "records", deleted)
			}
		case <-s.stop:
			drain()
			_ = flush()
			return
		}
	}
}

func (s *Store) writeBatchWithRetry(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	writer := s.batchWriter
	if writer == nil {
		writer = s.insertBatch
	}
	var err error
	for attempt := 0; ; attempt++ {
		err = writer(ctx, records)
		if err == nil {
			return nil
		}
		if attempt >= len(s.retryDelays) {
			break
		}
		if waitErr := waitForDelay(ctx, s.retryDelays[attempt]); waitErr != nil {
			err = errors.Join(err, waitErr)
			break
		}
	}
	s.recordDropped(uint64(len(records)))
	return err
}

func waitForDelay(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) completePending(count int) {
	if count <= 0 {
		return
	}
	s.pendingMu.Lock()
	remaining := len(s.pendingTimes) - s.pendingHead
	if count >= remaining {
		s.pendingTimes = s.pendingTimes[:0]
		s.pendingHead = 0
	} else {
		s.pendingHead += count
		if s.pendingHead >= 1024 && s.pendingHead*2 >= len(s.pendingTimes) {
			s.pendingTimes = append(s.pendingTimes[:0], s.pendingTimes[s.pendingHead:]...)
			s.pendingHead = 0
		}
	}
	s.pendingMu.Unlock()
}

func (s *Store) recordDropped(count uint64) uint64 {
	if count == 0 {
		return s.dropped.Load()
	}
	return s.dropped.Add(count)
}

func (s *Store) insertBatch(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO request_audits (
		id, request_id, created_at, protocol, model, account_id, tenant_key, status_code,
		streaming, duration_ms, input_tokens, cached_input_tokens, output_tokens,
		reasoning_tokens, total_tokens, error_code, attempt_count
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer statement.Close()
	for _, record := range records {
		streaming := 0
		if record.Streaming {
			streaming = 1
		}
		if _, err := statement.ExecContext(ctx,
			record.ID, record.RequestID, record.CreatedAt, record.Protocol, record.Model,
			record.AccountID, record.TenantKey, record.StatusCode, streaming, record.DurationMS,
			record.InputTokens, record.CachedInputTokens, record.OutputTokens,
			record.ReasoningTokens, record.TotalTokens, record.ErrorCode, record.AttemptCount,
		); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func normalizeRecord(record Record) Record {
	if record.ID == "" {
		record.ID = NewID()
	}
	if record.CreatedAt == 0 {
		record.CreatedAt = time.Now().UnixMilli()
	}
	record.InputTokens = nonNegative(record.InputTokens)
	record.CachedInputTokens = nonNegative(record.CachedInputTokens)
	record.OutputTokens = nonNegative(record.OutputTokens)
	record.ReasoningTokens = nonNegative(record.ReasoningTokens)
	record.TotalTokens = nonNegative(record.TotalTokens)
	if record.TotalTokens == 0 {
		record.TotalTokens = record.InputTokens + record.OutputTokens
	}
	if record.DurationMS < 0 {
		record.DurationMS = 0
	}
	if record.AttemptCount < 0 {
		record.AttemptCount = 0
	}
	return record
}
