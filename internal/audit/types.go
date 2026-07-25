package audit

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Record struct {
	ID                string `json:"id"`
	RequestID         string `json:"request_id"`
	CreatedAt         int64  `json:"created_at"`
	Protocol          string `json:"protocol"`
	Model             string `json:"model"`
	AccountID         string `json:"account_id"`
	TenantKey         string `json:"tenant_key"`
	StatusCode        int    `json:"status_code"`
	Streaming         bool   `json:"streaming"`
	DurationMS        int64  `json:"duration_ms"`
	InputTokens       int64  `json:"input_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	ReasoningTokens   int64  `json:"reasoning_tokens"`
	TotalTokens       int64  `json:"total_tokens"`
	ErrorCode         string `json:"error_code"`
	AttemptCount      int    `json:"attempt_count"`
}

type Usage struct {
	Input, CachedInput, Output, Reasoning, Total int64
}

type Capture struct {
	mu      sync.Mutex
	started time.Time
	record  Record
}

const maxCapturedModelCharacters = 256

func NewCapture(protocol, requestID string, started time.Time) *Capture {
	if started.IsZero() {
		started = time.Now()
	}
	return &Capture{started: started, record: Record{
		RequestID: requestID, Protocol: protocol, TenantKey: "public", AttemptCount: 0,
	}}
}

func (c *Capture) RequestID() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.record.RequestID
}

func (c *Capture) SetRequest(model string, streaming bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.record.Model, c.record.Streaming = truncateCapturedModel(model), streaming
	c.mu.Unlock()
}

func truncateCapturedModel(model string) string {
	if len(model) <= maxCapturedModelCharacters {
		return model
	}
	characters := 0
	for index := range model {
		if characters == maxCapturedModelCharacters {
			return strings.Clone(model[:index])
		}
		characters++
	}
	return model
}

func (c *Capture) SetTenant(tenant string) {
	if c == nil || tenant == "" {
		return
	}
	c.mu.Lock()
	c.record.TenantKey = tenant
	c.mu.Unlock()
}

func (c *Capture) SetAccount(accountID string) {
	if c == nil || accountID == "" {
		return
	}
	c.mu.Lock()
	c.record.AccountID = accountID
	c.mu.Unlock()
}

func (c *Capture) MarkAttempt() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.record.AttemptCount++
	c.mu.Unlock()
}

func (c *Capture) SetUsage(usage Usage) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.record.InputTokens = nonNegative(usage.Input)
	c.record.CachedInputTokens = nonNegative(usage.CachedInput)
	c.record.OutputTokens = nonNegative(usage.Output)
	c.record.ReasoningTokens = nonNegative(usage.Reasoning)
	c.record.TotalTokens = nonNegative(usage.Total)
	if c.record.TotalTokens == 0 {
		c.record.TotalTokens = c.record.InputTokens + c.record.OutputTokens
	}
	c.mu.Unlock()
}

func (c *Capture) SetError(code string) {
	if c == nil || code == "" {
		return
	}
	c.mu.Lock()
	c.record.ErrorCode = code
	c.mu.Unlock()
}

func (c *Capture) Finish(status int, finished time.Time) Record {
	if finished.IsZero() {
		finished = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	record := c.record
	record.ID = NewID()
	record.CreatedAt = c.started.UnixMilli()
	record.StatusCode = status
	record.DurationMS = max(finished.Sub(c.started).Milliseconds(), 0)
	return record
}

type captureContextKey struct{}

func WithCapture(ctx context.Context, capture *Capture) context.Context {
	if capture == nil {
		return ctx
	}
	return context.WithValue(ctx, captureContextKey{}, capture)
}

func CaptureFromContext(ctx context.Context) *Capture {
	capture, _ := ctx.Value(captureContextKey{}).(*Capture)
	return capture
}

func SetAccount(ctx context.Context, accountID string) {
	CaptureFromContext(ctx).SetAccount(accountID)
}

func MarkAttempt(ctx context.Context) {
	CaptureFromContext(ctx).MarkAttempt()
}

var fallbackID atomic.Uint64

func NewID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		binary.BigEndian.PutUint64(value[:8], uint64(time.Now().UnixNano()))
		binary.BigEndian.PutUint64(value[8:], fallbackID.Add(1))
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
