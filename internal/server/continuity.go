package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Futureppo/grokcli2api-go/internal/auth"
	"github.com/Futureppo/grokcli2api-go/internal/grok"
	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

const affinityStateFileName = ".grokcli2api-affinity.json"

// continuityBinding is intentionally content-free. In particular it must not
// grow fields containing prompts, response bodies, API keys, tool arguments,
// or the un-hashed downstream session/response identifier.
type continuityBinding struct {
	Kind      string               `json:"kind"`
	AccountID string               `json:"account_id"`
	Model     string               `json:"model"`
	Backend   modelcatalog.Backend `json:"backend"`
	SessionID string               `json:"upstream_session"`
	NextTurn  uint64               `json:"next_turn"`
	ExpiresAt time.Time            `json:"expires_at"`
}

type continuityState struct {
	Version        int                          `json:"version"`
	HardAffinities map[string]continuityBinding `json:"hard_affinities"`
}

type continuityStore struct {
	path  string
	ttl   time.Duration
	limit int

	mu       sync.Mutex
	bindings map[string]continuityBinding
	dirty    bool
	wake     chan struct{}
	closed   chan struct{}
	done     chan struct{}
	close    sync.Once
}

func newContinuityStore(dir string, ttl time.Duration, limit int) *continuityStore {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if limit < 1 {
		limit = 100000
	}
	s := &continuityStore{
		path: filepath.Join(dir, affinityStateFileName), ttl: ttl, limit: limit,
		bindings: make(map[string]continuityBinding), wake: make(chan struct{}, 1),
		closed: make(chan struct{}), done: make(chan struct{}),
	}
	s.load()
	go s.checkpointLoop()
	return s
}

func (s *continuityStore) Close() {
	if s == nil {
		return
	}
	s.close.Do(func() { close(s.closed); <-s.done })
}

func continuityKey(tenant string, affinity auth.Affinity, model string) string {
	if affinity.Key == "" || affinity.Mode != auth.AffinityHard {
		return ""
	}
	if tenant == "" {
		tenant = "public"
	}
	sum := sha256.Sum256([]byte("grokcli2api-affinity-v1\x00" + tenant + "\x00" + affinity.Key + "\x00" + model))
	return hex.EncodeToString(sum[:])
}

func affinityKind(affinity auth.Affinity) string {
	switch {
	case len(affinity.Key) > len("previous:") && affinity.Key[:len("previous:")] == "previous:":
		return "previous_response"
	case len(affinity.Key) > len("session:") && affinity.Key[:len("session:")] == "session:":
		return "session"
	default:
		return "state"
	}
}

func (s *continuityStore) Lookup(tenant string, affinity auth.Affinity, model string) (string, continuityBinding, bool) {
	key := continuityKey(tenant, affinity, model)
	if key == "" {
		return "", continuityBinding{}, false
	}
	now := time.Now()
	s.mu.Lock()
	binding, ok := s.bindings[key]
	if ok && !now.Before(binding.ExpiresAt) {
		delete(s.bindings, key)
		s.dirty = true
		ok = false
		s.signalLocked()
	}
	if ok {
		binding.ExpiresAt = now.Add(s.ttl)
		s.bindings[key] = binding
		s.dirty = true
		s.signalLocked()
	}
	s.mu.Unlock()
	return key, binding, ok
}

// ReserveTurn atomically reserves a zero-based turn for a persisted hard
// session. The reservation is reused by every HTTP retry of the logical
// request because callers invoke it exactly once, before transport attempts.
func (s *continuityStore) ReserveTurn(key string) (continuityBinding, uint64, bool) {
	if key == "" {
		return continuityBinding{}, 0, false
	}
	now := time.Now()
	s.mu.Lock()
	binding, ok := s.bindings[key]
	if !ok || !now.Before(binding.ExpiresAt) {
		if ok {
			delete(s.bindings, key)
			s.dirty = true
			s.signalLocked()
		}
		s.mu.Unlock()
		return continuityBinding{}, 0, false
	}
	turn := binding.NextTurn
	binding.NextTurn++
	binding.ExpiresAt = now.Add(s.ttl)
	s.bindings[key] = binding
	s.dirty = true
	s.signalLocked()
	s.mu.Unlock()
	return binding, turn, true
}

func (s *continuityStore) Bind(key string, affinity auth.Affinity, accountID, model string, backend modelcatalog.Backend, sessionID string, nextTurn uint64) {
	if s == nil || key == "" || accountID == "" || model == "" || sessionID == "" {
		return
	}
	binding := continuityBinding{
		Kind: affinityKind(affinity), AccountID: accountID, Model: model,
		Backend: backend, SessionID: sessionID, NextTurn: nextTurn,
		ExpiresAt: time.Now().Add(s.ttl),
	}
	s.mu.Lock()
	s.evictLocked(time.Now())
	s.bindings[key] = binding
	s.dirty = true
	s.signalLocked()
	s.mu.Unlock()
}

// BindResponse records response ownership under the already pseudonymous
// tenant. Only the response identifier participates in the in-memory hash and
// is never written to disk.
func (s *continuityStore) BindResponse(tenant, responseID, accountID, model string, backend modelcatalog.Backend, sessionID string, nextTurn uint64) {
	if responseID == "" {
		return
	}
	affinity := auth.Affinity{Key: "previous:" + responseID, Mode: auth.AffinityHard}
	key := continuityKey(tenant, affinity, model)
	s.Bind(key, affinity, accountID, model, backend, sessionID, nextTurn)
}

func (s *continuityStore) BindStateToken(tenant, token, accountID, model string, backend modelcatalog.Backend, sessionID string, nextTurn uint64) {
	if token == "" {
		return
	}
	affinity := auth.Affinity{Key: "signature:" + token, Mode: auth.AffinityHard}
	key := continuityKey(tenant, affinity, model)
	s.Bind(key, affinity, accountID, model, backend, sessionID, nextTurn)
}

func (s *continuityStore) DeleteAccount(accountID string) {
	if s == nil || accountID == "" {
		return
	}
	s.mu.Lock()
	for key, binding := range s.bindings {
		if binding.AccountID == accountID {
			delete(s.bindings, key)
			s.dirty = true
		}
	}
	if s.dirty {
		s.signalLocked()
	}
	s.mu.Unlock()
}

func newLogicalSession() string { return grok.NewSessionID() }

func (s *continuityStore) signalLocked() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *continuityStore) evictLocked(now time.Time) {
	for key, binding := range s.bindings {
		if !now.Before(binding.ExpiresAt) {
			delete(s.bindings, key)
		}
	}
	if len(s.bindings) < s.limit {
		return
	}
	type candidate struct {
		key string
		at  time.Time
	}
	items := make([]candidate, 0, len(s.bindings))
	for key, binding := range s.bindings {
		items = append(items, candidate{key: key, at: binding.ExpiresAt})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	remove := len(s.bindings) - s.limit + 1
	for i := 0; i < remove && i < len(items); i++ {
		delete(s.bindings, items[i].key)
	}
}

func (s *continuityStore) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var state continuityState
	if json.Unmarshal(b, &state) != nil || state.Version != 1 || state.HardAffinities == nil {
		// Fail closed. A corrupted file must never be guessed or partially
		// recovered into a possibly cross-tenant binding.
		return
	}
	now := time.Now()
	for key, binding := range state.HardAffinities {
		if len(key) == sha256.Size*2 && now.Before(binding.ExpiresAt) && binding.AccountID != "" && binding.SessionID != "" {
			s.bindings[key] = binding
		}
	}
	if len(s.bindings) > s.limit {
		s.evictLocked(now)
	}
}

func (s *continuityStore) checkpointLoop() {
	defer close(s.done)
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-s.wake:
			if timer == nil {
				timer = time.NewTimer(time.Second)
				timerC = timer.C
			}
		case <-timerC:
			_ = s.flush()
			timer = nil
			timerC = nil
		case <-s.closed:
			if timer != nil {
				timer.Stop()
			}
			_ = s.flush()
			return
		}
	}
}

func (s *continuityStore) flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	s.evictLocked(time.Now())
	bindings := make(map[string]continuityBinding, len(s.bindings))
	for key, binding := range s.bindings {
		bindings[key] = binding
	}
	s.dirty = false
	s.mu.Unlock()

	b, err := json.MarshalIndent(continuityState{Version: 1, HardAffinities: bindings}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".grok-affinity-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		s.mu.Lock()
		s.dirty = true
		s.signalLocked()
		s.mu.Unlock()
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.signalLocked()
		s.mu.Unlock()
		return err
	}
	return nil
}
