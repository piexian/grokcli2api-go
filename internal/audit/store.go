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
	defaultQueueSize = 4096
	defaultBatchSize = 100
	flushInterval    = 250 * time.Millisecond
)

var ErrClosed = errors.New("audit store is closed")

type Store struct {
	db            *sql.DB
	retentionDays int
	queue         chan Record
	flushRequests chan chan error
	stop          chan struct{}
	done          chan struct{}
	closed        atomic.Bool
	dropped       atomic.Uint64
	closeOnce     sync.Once
	enqueueMu     sync.RWMutex
	closeErr      error
}

func Open(path string, retentionDays int) (*Store, error) {
	if path == "" {
		return nil, errors.New("audit database path is empty")
	}
	if retentionDays < 1 {
		return nil, errors.New("audit retention days must be positive")
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
		db: db, retentionDays: retentionDays, queue: make(chan Record, defaultQueueSize),
		flushRequests: make(chan chan error), stop: make(chan struct{}), done: make(chan struct{}),
	}
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
	select {
	case s.queue <- record:
		return true
	default:
		dropped := s.dropped.Add(1)
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
	return s.insertBatch(ctx, records)
}

func (s *Store) Cleanup(ctx context.Context, now time.Time) (int64, error) {
	if s == nil {
		return 0, ErrClosed
	}
	cutoff := now.AddDate(0, 0, -s.retentionDays).UnixMilli()
	result, err := s.db.ExecContext(ctx, "DELETE FROM request_audits WHERE created_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
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
		err := s.insertBatch(context.Background(), batch)
		if err != nil {
			slog.Error("write audit batch", "error", err, "records", len(batch))
		}
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
