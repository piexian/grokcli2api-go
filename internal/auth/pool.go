package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	stateFileName          = ".grokcli2api-state.json"
	billingExhaustedReason = "billing_exhausted"
	refreshBackoffReason   = "refresh_backoff"
)

var errAccountBusy = errors.New("credential account is at its in-flight limit")

var ErrCredentialNotFound = errors.New("credential not found")

type AffinityMode uint8

const (
	AffinityNone AffinityMode = iota
	AffinitySoft
	AffinityHard
)

type Affinity struct {
	Key  string
	Mode AffinityMode
}

type PoolConfig struct {
	Dir                string
	Surface            string
	ReloadInterval     time.Duration
	RefreshConcurrency int
	AccountMaxInflight int
	AffinityTTL        time.Duration
	AffinityMaxEntries int
	AllowEmpty         bool
}

// CredentialInfo is a redacted view of a credential account. It deliberately
// excludes subjects, file paths, client IDs, and token values.
type CredentialInfo struct {
	ID                      string       `json:"id"`
	Status                  string       `json:"status"`
	Usable                  bool         `json:"usable"`
	Disabled                bool         `json:"disabled"`
	ExpiresAt               *time.Time   `json:"expires_at,omitempty"`
	CooldownUntil           *time.Time   `json:"cooldown_until,omitempty"`
	Models                  []string     `json:"models"`
	HasRefreshToken         bool         `json:"has_refresh_token"`
	SubscriptionTier        string       `json:"subscription_tier,omitempty"`
	SubscriptionTierDisplay string       `json:"subscription_tier_display,omitempty"`
	Billing                 *BillingInfo `json:"billing,omitempty"`
}

// BillingInfo is a redacted, in-memory snapshot of the authoritative credits
// endpoint. Monetary values are remaining cents and never include payment data.
type BillingInfo struct {
	UsagePercent           *float64   `json:"usage_percent,omitempty"`
	PeriodType             string     `json:"period_type,omitempty"`
	PeriodEnd              *time.Time `json:"period_end,omitempty"`
	OnDemandRemainingCents *int64     `json:"on_demand_remaining_cents,omitempty"`
	PrepaidBalanceCents    *int64     `json:"prepaid_balance_cents,omitempty"`
	UnifiedBilling         *bool      `json:"unified_billing,omitempty"`
	Exhausted              bool       `json:"exhausted"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

type UnavailableError struct {
	Cooling    bool
	RetryAfter time.Duration
}

type ModelUnavailableError struct{ Model string }

func (e *ModelUnavailableError) Error() string {
	return "no credential account advertises model " + e.Model
}

func (e *UnavailableError) Error() string {
	if e.Cooling {
		return "all credential accounts are cooling down"
	}
	return "no usable credential accounts"
}

type account struct {
	id        string
	agentID   string
	sessionID string

	mu             sync.RWMutex
	credential     *credential
	cooldowns      map[string]cooldownState
	modelCooldowns map[string]cooldownState
	disabled       bool
	disableReason  string
	billing        *BillingInfo
	billingPending bool
	refreshOnce    sync.Once
	refreshLock    chan struct{}
	generation     atomic.Uint64
	inflight       atomic.Int64
	paidTier       atomic.Bool
}

func (a *account) currentGeneration() uint64 {
	if generation := a.generation.Load(); generation != 0 {
		return generation
	}
	a.generation.CompareAndSwap(0, 1)
	return a.generation.Load()
}

func (a *account) acquireRefresh(ctx context.Context) error {
	a.refreshOnce.Do(func() {
		a.refreshLock = make(chan struct{}, 1)
		a.refreshLock <- struct{}{}
	})
	select {
	case <-a.refreshLock:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *account) releaseRefresh() { a.refreshLock <- struct{}{} }

func (a *account) available(now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	until, _ := cooldownDeadline(a.cooldowns, now)
	return !a.disabled && !a.billingPending && until.IsZero() && a.credential != nil
}

func (a *account) requestUsable(now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	until, _ := cooldownDeadline(a.cooldowns, now)
	return !a.disabled && !a.billingPending && until.IsZero() && a.credential != nil && a.credential.usable(now)
}

func (a *account) prefersPaidTier() bool { return a.paidTier.Load() }

func (a *account) supportsModel(model string) bool {
	if model == "" {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.disabled || a.credential == nil {
		return false
	}
	index := sort.SearchStrings(a.credential.Models, model)
	if index >= len(a.credential.Models) || a.credential.Models[index] != model {
		return false
	}
	cooldown, cooling := a.modelCooldowns[model]
	return !cooling || !time.Now().Before(cooldown.Until)
}

func (a *account) snapshot() (*credential, time.Time, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	until, _ := cooldownDeadline(a.cooldowns, time.Now())
	return a.credential, until, a.disabled
}

type fileEntry struct {
	size    int64
	modTime time.Time
	cred    *credential
}

type persistedState struct {
	Version  int                     `json:"version"`
	Accounts map[string]accountState `json:"accounts"`
}

type accountState struct {
	CooldownUntil         time.Time                `json:"cooldown_until,omitempty"`
	Reason                string                   `json:"reason,omitempty"`
	Cooldowns             map[string]cooldownState `json:"cooldowns,omitempty"`
	Disabled              bool                     `json:"disabled,omitempty"`
	CredentialFingerprint string                   `json:"credential_fingerprint,omitempty"`
	ModelCooldowns        map[string]cooldownState `json:"model_cooldowns,omitempty"`
}

type cooldownState struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

func cooldownDeadline(cooldowns map[string]cooldownState, now time.Time) (time.Time, string) {
	var until time.Time
	reason := ""
	for key, cooldown := range cooldowns {
		if !now.Before(cooldown.Until) {
			continue
		}
		if cooldown.Until.After(until) || cooldown.Until.Equal(until) && (reason == "" || key < reason) {
			until, reason = cooldown.Until, key
		}
	}
	return until, reason
}

func activeAccountCooldowns(source map[string]cooldownState, now time.Time) map[string]cooldownState {
	if len(source) == 0 {
		return nil
	}
	active := make(map[string]cooldownState, len(source))
	for reason, cooldown := range source {
		if now.Before(cooldown.Until) {
			cooldown.Reason = reason
			active[reason] = cooldown
		}
	}
	if len(active) == 0 {
		return nil
	}
	return active
}

func cloneAccountCooldowns(source map[string]cooldownState) map[string]cooldownState {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]cooldownState, len(source))
	for reason, cooldown := range source {
		cloned[reason] = cooldown
	}
	return cloned
}

func syncLegacyCooldownState(state *accountState, now time.Time) {
	if state.Disabled {
		state.CooldownUntil = time.Time{}
		return
	}
	state.CooldownUntil, state.Reason = cooldownDeadline(state.Cooldowns, now)
}

func accountStateEmpty(state accountState) bool {
	return !state.Disabled && len(state.Cooldowns) == 0 && len(state.ModelCooldowns) == 0
}

type Pool struct {
	cfg           PoolConfig
	http          *http.Client
	mu            sync.RWMutex
	accounts      map[string]*account
	files         map[string]fileEntry
	states        map[string]accountState
	active        atomic.Value // []*account
	activeByModel atomic.Value // map[string][]*account
	cursor        atomic.Uint64
	affinity      *affinityCache
	refreshSem    chan struct{}
	capacityCh    chan struct{}
	capacityMu    sync.Mutex
	rebuildCh     chan struct{}
	billingCh     chan struct{}
	billingGate   atomic.Bool
	closed        chan struct{}
	closeOnce     sync.Once
	wg            sync.WaitGroup
	stateMu       sync.Mutex
	mutationMu    sync.Mutex
}

type Lease struct {
	pool       *Pool
	account    *account
	credential *credential
	generation uint64
	once       sync.Once
}

func (p *Pool) newLease(a *account) *Lease {
	a.mu.RLock()
	cred := a.credential
	generation := a.currentGeneration()
	a.mu.RUnlock()
	return &Lease{pool: p, account: a, credential: cred, generation: generation}
}

func (l *Lease) Session() Session {
	if l.credential == nil {
		return Session{}
	}
	return l.credential.session()
}
func (l *Lease) AccountID() string  { return l.account.id }
func (l *Lease) AgentID() string    { return l.account.agentID }
func (l *Lease) SessionID() string  { return l.account.sessionID }
func (l *Lease) Generation() uint64 { return l.generation }
func (l *Lease) Release() {
	if l == nil || l.account == nil {
		return
	}
	l.once.Do(func() {
		l.account.inflight.Add(-1)
		l.pool.notifyCapacity()
	})
}

func NewPool(ctx context.Context, cfg PoolConfig, client *http.Client) (*Pool, error) {
	if cfg.Dir == "" {
		cfg.Dir = "./auths"
	}
	if cfg.ReloadInterval <= 0 {
		cfg.ReloadInterval = 30 * time.Second
	}
	if cfg.RefreshConcurrency < 1 {
		cfg.RefreshConcurrency = 4
	}
	if cfg.AccountMaxInflight < 1 {
		cfg.AccountMaxInflight = 16
	}
	if cfg.AffinityTTL <= 0 {
		cfg.AffinityTTL = time.Hour
	}
	if cfg.AffinityMaxEntries < 1 {
		cfg.AffinityMaxEntries = 100000
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if cfg.AllowEmpty {
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("create auths directory: %w", err)
		}
	}
	p := &Pool{
		cfg: cfg, http: client, accounts: map[string]*account{}, files: map[string]fileEntry{},
		states: map[string]accountState{}, affinity: newAffinityCache(cfg.AffinityTTL, cfg.AffinityMaxEntries),
		refreshSem: make(chan struct{}, cfg.RefreshConcurrency), capacityCh: make(chan struct{}),
		rebuildCh: make(chan struct{}, 1), billingCh: make(chan struct{}, 1), closed: make(chan struct{}),
	}
	p.active.Store([]*account{})
	p.activeByModel.Store(map[string][]*account{})
	_ = p.loadState()
	if err := p.scan(); err != nil {
		return nil, err
	}
	if len(p.accounts) == 0 && !cfg.AllowEmpty {
		return nil, ErrNoAuth
	}
	if len(p.accounts) > 0 && !p.hasReadyAccount() {
		warmup, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := p.warmup(warmup)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	p.wg.Add(2)
	go p.background()
	go p.rebuildLoop()
	return p, nil
}

func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.wg.Wait()
		_ = p.persistState()
	})
}

func (p *Pool) Acquire(ctx context.Context, affinity Affinity, model string, exclude map[string]struct{}) (*Lease, error) {
	cacheKey := modelAffinityKey(affinity.Key, model)
	var refreshFailed map[string]struct{}
	for {
		capacity := p.capacitySignal()
		if affinity.Key != "" {
			if id, ok := p.affinity.Get(cacheKey); ok {
				var lease *Lease
				var err error
				if affinity.Mode == AffinitySoft && !p.accountRequestUsable(id) {
					err = errAccountBusy
				} else {
					lease, err = p.acquireID(ctx, id, model, exclude)
				}
				if err == nil {
					return lease, nil
				}
				if errors.Is(err, errAccountBusy) && affinity.Mode == AffinityHard {
					if err := p.waitForCapacity(ctx, capacity); err != nil {
						return nil, err
					}
					continue
				}
				if !errors.Is(err, errAccountBusy) {
					p.affinity.Delete(cacheKey)
				}
			}
		}
		active := p.schedulingSnapshot(model)
		if len(active) == 0 {
			p.rebuildActive()
			active = p.schedulingSnapshot(model)
			if len(active) == 0 {
				if !p.hasAccounts() {
					return nil, p.unavailable(model)
				}
				if model != "" && !p.hasKnownModel(model) {
					return nil, &ModelUnavailableError{Model: model}
				}
				return nil, p.unavailable(model)
			}
		}
		cursor := p.cursor.Add(1) - 1
		saturated := false
		var contentionFailed map[string]struct{}
		const schedulingChoices = 4
		paidEnd := sort.Search(len(active), func(i int) bool { return !active[i].prefersPaidTier() })
		for _, candidates := range [2][]*account{active[:paidEnd], active[paidEnd:]} {
			if len(candidates) == 0 {
				continue
			}
			start := int(cursor % uint64(len(candidates)))
			for {
				retryCandidates := false
				for batch := 0; batch < len(candidates); batch += schedulingChoices {
					var selected *account
					selectedInflight := int64(^uint64(0) >> 1)
					selectedUsable := false
					for offset := 0; offset < schedulingChoices && batch+offset < len(candidates); offset++ {
						a := candidates[(start+batch+offset)%len(candidates)]
						_, excluded := exclude[a.id]
						_, failedRefresh := refreshFailed[a.id]
						_, failedContention := contentionFailed[a.id]
						if excluded || failedRefresh || failedContention || !a.available(time.Now()) || !a.supportsModel(model) {
							continue
						}
						inflight := a.inflight.Load()
						if p.cfg.AccountMaxInflight > 0 && inflight >= int64(p.cfg.AccountMaxInflight) {
							saturated = true
							continue
						}
						usable := a.requestUsable(time.Now())
						if selected == nil || usable && !selectedUsable || usable == selectedUsable && inflight < selectedInflight {
							selected, selectedInflight, selectedUsable = a, inflight, usable
						}
					}
					if selected != nil {
						if !selected.tryAcquire(p.cfg.AccountMaxInflight) {
							saturated = true
							if contentionFailed == nil {
								contentionFailed = make(map[string]struct{})
							}
							contentionFailed[selected.id] = struct{}{}
							retryCandidates = true
							break
						}
						if err := p.ensureUsable(ctx, selected); err != nil {
							selected.inflight.Add(-1)
							p.notifyCapacity()
							if refreshFailed == nil {
								refreshFailed = make(map[string]struct{})
							}
							refreshFailed[selected.id] = struct{}{}
							retryCandidates = true
							break
						}
						if affinity.Key != "" {
							p.affinity.Set(cacheKey, selected.id)
						}
						return p.newLease(selected), nil
					}
				}
				if !retryCandidates {
					break
				}
			}
		}
		if saturated {
			if err := p.waitForCapacity(ctx, capacity); err != nil {
				return nil, err
			}
			continue
		}
		if !p.hasAccounts() {
			return nil, p.unavailable(model)
		}
		if model != "" && !p.hasKnownModel(model) {
			return nil, &ModelUnavailableError{Model: model}
		}
		return nil, p.unavailable(model)
	}
}

func (p *Pool) hasAccounts() bool {
	p.mu.RLock()
	hasAccounts := len(p.accounts) > 0
	p.mu.RUnlock()
	return hasAccounts
}

func (p *Pool) accountRequestUsable(id string) bool {
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	return a != nil && a.requestUsable(time.Now())
}

func (a *account) tryAcquire(limit int) bool {
	if limit < 1 {
		a.inflight.Add(1)
		return true
	}
	for {
		current := a.inflight.Load()
		if current >= int64(limit) {
			return false
		}
		if a.inflight.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (p *Pool) notifyCapacity() {
	p.capacityMu.Lock()
	if p.capacityCh != nil {
		close(p.capacityCh)
	}
	p.capacityCh = make(chan struct{})
	p.capacityMu.Unlock()
}

func (p *Pool) capacitySignal() <-chan struct{} {
	p.capacityMu.Lock()
	if p.capacityCh == nil {
		p.capacityCh = make(chan struct{})
	}
	ch := p.capacityCh
	p.capacityMu.Unlock()
	return ch
}

func (p *Pool) waitForCapacity(ctx context.Context, capacity <-chan struct{}) error {
	select {
	case <-capacity:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closed:
		return ErrNoAuth
	}
}

func (p *Pool) schedulingSnapshot(model string) []*account {
	if model == "" {
		return p.active.Load().([]*account)
	}
	return p.activeByModel.Load().(map[string][]*account)[model]
}

func (p *Pool) AcquireAccount(ctx context.Context, id string) (*Lease, error) {
	for {
		capacity := p.capacitySignal()
		lease, err := p.acquireID(ctx, id, "", nil)
		if !errors.Is(err, errAccountBusy) {
			return lease, err
		}
		if err := p.waitForCapacity(ctx, capacity); err != nil {
			return nil, err
		}
	}
}

// AcquireAccountForMetadata ignores quota/rate cooldowns so non-generative
// capability discovery can still refresh an account's model catalog.
func (p *Pool) AcquireAccountForMetadata(ctx context.Context, id string) (*Lease, error) {
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	if a == nil {
		return nil, ErrNoAuth
	}
	_, _, disabled := a.snapshot()
	if disabled {
		return nil, ErrNoAuth
	}
	if err := p.ensureUsable(ctx, a); err != nil {
		return nil, err
	}
	a.inflight.Add(1)
	return p.newLease(a), nil
}

func (p *Pool) acquireID(ctx context.Context, id, model string, exclude map[string]struct{}) (*Lease, error) {
	if _, skipped := exclude[id]; skipped {
		return nil, ErrNoAuth
	}
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	if a == nil || !a.available(time.Now()) || !a.supportsModel(model) {
		return nil, ErrNoAuth
	}
	if !a.tryAcquire(p.cfg.AccountMaxInflight) {
		return nil, errAccountBusy
	}
	if err := p.ensureUsable(ctx, a); err != nil {
		a.inflight.Add(-1)
		p.notifyCapacity()
		return nil, err
	}
	return p.newLease(a), nil
}

func (p *Pool) Bind(affinity Affinity, model, accountID string) {
	if affinity.Key != "" && accountID != "" {
		p.affinity.Set(modelAffinityKey(affinity.Key, model), accountID)
	}
}

func (p *Pool) BindResponseID(responseID, model, accountID string) {
	if responseID != "" {
		p.affinity.Set(modelAffinityKey("previous:"+responseID, model), accountID)
	}
}

// AccountIDs returns anonymous stable identifiers for diagnostics and tests.
// It never exposes credential paths, subjects, or tokens.
func (p *Pool) AccountIDs() []string {
	p.mu.RLock()
	ids := make([]string, 0, len(p.accounts))
	for id := range p.accounts {
		ids = append(ids, id)
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// Credentials returns redacted credential metadata sorted by account ID.
func (p *Pool) Credentials() []CredentialInfo {
	p.mu.RLock()
	items := make([]CredentialInfo, 0, len(p.accounts))
	for id, a := range p.accounts {
		items = append(items, credentialInfo(id, a, time.Now()))
	}
	p.mu.RUnlock()
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// Credential returns redacted metadata for one account.
func (p *Pool) Credential(id string) (CredentialInfo, bool) {
	p.mu.RLock()
	a := p.accounts[id]
	if a == nil {
		p.mu.RUnlock()
		return CredentialInfo{}, false
	}
	info := credentialInfo(id, a, time.Now())
	p.mu.RUnlock()
	return info, true
}

func credentialInfo(id string, a *account, now time.Time) CredentialInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	info := CredentialInfo{ID: id, Disabled: a.disabled, Models: []string{}}
	if a.credential == nil {
		info.Status = "unavailable"
		return info
	}
	info.Models = append(info.Models, a.credential.Models...)
	info.HasRefreshToken = a.credential.RefreshToken != ""
	info.SubscriptionTier = a.credential.Tier.Key
	info.SubscriptionTierDisplay = a.credential.Tier.Display
	info.Billing = cloneBillingInfo(a.billing)
	cooldownUntil, _ := cooldownDeadline(a.cooldowns, now)
	if !a.credential.ExpiresAt.IsZero() {
		expires := a.credential.ExpiresAt.UTC()
		info.ExpiresAt = &expires
	}
	if !cooldownUntil.IsZero() {
		cooldown := cooldownUntil.UTC()
		info.CooldownUntil = &cooldown
	}
	info.Usable = !a.disabled && !a.billingPending && cooldownUntil.IsZero() && a.credential.usable(now)
	switch {
	case a.disabled:
		info.Status = "disabled"
	case a.billingPending:
		info.Status = "pending_billing"
	case !cooldownUntil.IsZero():
		info.Status = "cooling_down"
	case !a.credential.usable(now):
		info.Status = "needs_refresh"
	case len(a.credential.Models) == 0:
		info.Status = "pending_models"
	default:
		info.Status = "ready"
	}
	return info
}

// ImportCredential validates and atomically creates or replaces a credential.
// Account identity, rather than a caller-supplied filename, determines the
// destination so remote uploads cannot escape the configured directory.
func (p *Pool) ImportCredential(ctx context.Context, raw []byte) (CredentialInfo, bool, error) {
	parsed, err := parseCredential(raw, "", p.cfg.Surface)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	id := accountID(parsed.Subject)
	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()

	p.mu.RLock()
	existing := p.accounts[id]
	p.mu.RUnlock()
	created := existing == nil
	path := filepath.Join(p.cfg.Dir, id+".json")
	if existing != nil {
		if err := existing.acquireRefresh(ctx); err != nil {
			return CredentialInfo{}, false, err
		}
		defer existing.releaseRefresh()
		existing.mu.RLock()
		if existing.credential != nil && existing.credential.Path != "" {
			path = existing.credential.Path
		}
		existing.mu.RUnlock()
	}

	if _, statErr := os.Stat(path); statErr != nil && !os.IsNotExist(statErr) {
		return CredentialInfo{}, false, statErr
	}
	if err := writeCredentialAtomicMode(path, parsed.Raw, 0o600); err != nil {
		return CredentialInfo{}, false, err
	}
	// Do not let a same-size, same-timestamp replacement reuse cached content.
	p.mu.Lock()
	delete(p.files, path)
	p.mu.Unlock()
	if err := p.scanUnlocked(); err != nil {
		return CredentialInfo{}, false, err
	}
	info, ok := p.Credential(id)
	if !ok {
		return CredentialInfo{}, false, errors.New("credential was saved but not loaded")
	}
	return info, created, nil
}

// DeleteCredential removes every valid file for an account and immediately
// evicts that account from scheduling. Existing leases retain their snapshots.
func (p *Pool) DeleteCredential(ctx context.Context, id string) error {
	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	if a == nil {
		return ErrCredentialNotFound
	}
	if err := a.acquireRefresh(ctx); err != nil {
		return err
	}
	defer a.releaseRefresh()

	entries, err := os.ReadDir(p.cfg.Dir)
	if err != nil {
		return err
	}
	removed := false
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(p.cfg.Dir, entry.Name())
		cred, loadErr := loadCredential(path, p.cfg.Surface)
		if loadErr != nil || accountID(cred.Subject) != id {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		removed = true
	}
	if !removed {
		return ErrCredentialNotFound
	}
	p.mu.Lock()
	delete(p.states, id)
	p.mu.Unlock()
	if err := p.scanUnlocked(); err != nil {
		return err
	}
	return p.persistState()
}

func (p *Pool) Models() []string {
	seen := map[string]struct{}{}
	p.mu.RLock()
	for _, a := range p.accounts {
		a.mu.RLock()
		if !a.disabled && a.credential != nil {
			for _, model := range a.credential.Models {
				seen[model] = struct{}{}
			}
		}
		a.mu.RUnlock()
	}
	p.mu.RUnlock()
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (p *Pool) HasModel(model string) bool {
	if model == "" {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		a.mu.RLock()
		index := 0
		if a.credential != nil {
			index = sort.SearchStrings(a.credential.Models, model)
		}
		available := !a.disabled && a.credential != nil && index < len(a.credential.Models) && a.credential.Models[index] == model
		a.mu.RUnlock()
		if available {
			return true
		}
	}
	return false
}

func (p *Pool) hasKnownModel(model string) bool {
	if model == "" {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		a.mu.RLock()
		known := false
		if cred := a.credential; cred != nil {
			index := sort.SearchStrings(cred.Models, model)
			known = index < len(cred.Models) && cred.Models[index] == model
		}
		a.mu.RUnlock()
		if known {
			return true
		}
	}
	return false
}

func (p *Pool) AccountsNeedingModelRefresh(interval time.Duration) []string {
	now := time.Now()
	p.mu.RLock()
	ids := make([]string, 0, len(p.accounts))
	for id, a := range p.accounts {
		a.mu.RLock()
		needs := !a.disabled && a.credential != nil && (len(a.credential.Models) == 0 || a.credential.ModelsUpdatedAt.IsZero() || now.Sub(a.credential.ModelsUpdatedAt) >= interval)
		a.mu.RUnlock()
		if needs {
			ids = append(ids, id)
		}
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// AccountsNeedingBillingRefresh returns accounts whose official tier exposes
// the usage surface and whose in-memory credits snapshot is absent or stale.
// Cooldowns do not suppress metadata refresh, allowing a topped-up account to
// recover before its old deadline.
func (p *Pool) AccountsNeedingBillingRefresh(interval time.Duration) []string {
	now := time.Now()
	p.mu.RLock()
	ids := make([]string, 0, len(p.accounts))
	for id, a := range p.accounts {
		if accountNeedsBillingRefresh(a, now, interval) {
			ids = append(ids, id)
		}
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
}

// AccountNeedsBillingRefresh reports whether one account is eligible and due.
func (p *Pool) AccountNeedsBillingRefresh(accountID string, interval time.Duration) bool {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	return accountNeedsBillingRefresh(a, time.Now(), interval)
}

func accountNeedsBillingRefresh(a *account, now time.Time, interval time.Duration) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.disabled && a.credential != nil && a.credential.Tier.Billing &&
		(a.billing == nil || interval <= 0 || now.Sub(a.billing.UpdatedAt) >= interval)
}

// BillingRefreshSignal wakes metadata polling after eligible credentials change.
func (p *Pool) BillingRefreshSignal() <-chan struct{} { return p.billingCh }

// EnableBillingPreflight keeps newly eligible accounts out of request
// scheduling until the Grok client completes one credits probe attempt.
func (p *Pool) EnableBillingPreflight() {
	if !p.billingGate.CompareAndSwap(false, true) {
		return
	}
	p.mu.RLock()
	accounts := make([]*account, 0, len(p.accounts))
	for _, a := range p.accounts {
		accounts = append(accounts, a)
	}
	p.mu.RUnlock()
	changed := false
	for _, a := range accounts {
		a.mu.Lock()
		pending := !a.disabled && a.credential != nil && a.credential.Tier.Billing && a.billing == nil
		if pending && !a.billingPending {
			a.billingPending = true
			changed = true
		}
		a.mu.Unlock()
	}
	if changed {
		p.rebuildActive()
		p.notifyCapacity()
	}
	p.requestBillingRefresh()
}

// CompleteBillingRefresh releases the first-probe scheduling gate. Billing
// cooldowns still decide availability when the completed probe found exhaustion.
func (p *Pool) CompleteBillingRefresh(accountID string) {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return
	}
	a.mu.Lock()
	changed := a.billingPending
	a.billingPending = false
	a.mu.Unlock()
	if changed {
		p.requestRebuild()
		p.notifyCapacity()
	}
}

func (p *Pool) requestBillingRefresh() {
	select {
	case p.billingCh <- struct{}{}:
	default:
	}
}

// UpdateBilling replaces one account's redacted in-memory credits snapshot.
func (p *Pool) UpdateBilling(accountID string, info BillingInfo) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	if info.UpdatedAt.IsZero() {
		info.UpdatedAt = time.Now().UTC()
	}
	a.mu.Lock()
	a.billing = cloneBillingInfo(&info)
	a.mu.Unlock()
	return nil
}

func (p *Pool) UpdateModels(accountID string, models []string, updatedAt time.Time) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	if err := a.acquireRefresh(context.Background()); err != nil {
		return err
	}
	defer a.releaseRefresh()
	cred, _, disabled := a.snapshot()
	if cred == nil || disabled {
		return ErrNoAuth
	}
	normalized := stringSlice(models)
	if len(normalized) == 0 {
		return errors.New("upstream returned no models")
	}
	next := *cred
	next.Raw = cloneMap(cred.Raw)
	next.Models = normalized
	next.ModelsUpdatedAt = updatedAt.UTC()
	node := credentialNode(next.Raw)
	node["models"] = normalized
	node["models_updated_at"] = next.ModelsUpdatedAt.Format(time.RFC3339Nano)
	if err := writeCredentialAtomic(next.Path, next.Raw); err != nil {
		return err
	}
	a.mu.Lock()
	a.credential = &next
	a.mu.Unlock()
	p.mu.Lock()
	if info, err := os.Stat(next.Path); err == nil {
		p.files[next.Path] = fileEntry{size: info.Size(), modTime: info.ModTime(), cred: &next}
	}
	p.mu.Unlock()
	p.requestRebuild()
	return nil
}

// RebuildSchedulingSnapshot publishes a consistent account index after a
// batch of model-catalog updates. Routine state changes remain debounce-batched.
func (p *Pool) RebuildSchedulingSnapshot() {
	p.rebuildActive()
}

func (p *Pool) Refresh(ctx context.Context, accountID string) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	return p.ensureFresh(ctx, a, true)
}

// RefreshIfUnchanged collapses concurrent 401 recovery for requests that used
// the same credential generation. Once one caller refreshes the account, later
// callers observe the new generation and reuse it without another OAuth call.
func (p *Pool) RefreshIfUnchanged(ctx context.Context, accountID string, observedGeneration uint64) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	return p.refreshCredential(ctx, a, true, observedGeneration, true)
}

func (p *Pool) MarkCooldown(accountID, reason string, duration time.Duration) {
	p.markCooldown(accountID, reason, duration)
}

func (p *Pool) markCooldown(accountID, reason string, duration time.Duration) bool {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return false
	}
	now := time.Now()
	until := now.Add(duration)
	a.mu.Lock()
	a.cooldowns = activeAccountCooldowns(a.cooldowns, now)
	if a.cooldowns == nil {
		a.cooldowns = make(map[string]cooldownState)
	}
	existing := a.cooldowns[reason]
	remaining := time.Until(existing.Until)
	extensionThreshold := remaining / 10
	if extensionThreshold < 5*time.Second {
		extensionThreshold = 5 * time.Second
	}
	changed := !now.Before(existing.Until) || until.After(existing.Until.Add(extensionThreshold))
	if changed {
		a.cooldowns[reason] = cooldownState{Until: until, Reason: reason}
	} else {
		until = existing.Until
	}
	cooldowns := cloneAccountCooldowns(a.cooldowns)
	a.mu.Unlock()
	if !changed {
		return false
	}
	p.mu.Lock()
	state := p.states[accountID]
	state.Cooldowns = cooldowns
	syncLegacyCooldownState(&state, now)
	p.states[accountID] = state
	p.mu.Unlock()
	p.requestRebuild()
	p.rebuildWhenCooldownExpires(until)
	_ = p.persistState()
	slog.Warn("credential account cooling", "account", accountID, "reason", reason, "until", until.UTC().Format(time.RFC3339))
	return true
}

// ClearCooldownReason clears only the named account-level cooldown reason.
func (p *Pool) ClearCooldownReason(accountID, reason string) bool {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return false
	}
	a.mu.Lock()
	a.cooldowns = activeAccountCooldowns(a.cooldowns, time.Now())
	if _, ok := a.cooldowns[reason]; !ok {
		a.mu.Unlock()
		return false
	}
	delete(a.cooldowns, reason)
	cooldowns := cloneAccountCooldowns(a.cooldowns)
	a.mu.Unlock()

	p.mu.Lock()
	state := p.states[accountID]
	state.Cooldowns = cooldowns
	syncLegacyCooldownState(&state, time.Now())
	if accountStateEmpty(state) {
		delete(p.states, accountID)
	} else {
		p.states[accountID] = state
	}
	p.mu.Unlock()
	p.requestRebuild()
	p.notifyCapacity()
	_ = p.persistState()
	return true
}

func (p *Pool) MarkModelCooldown(accountID, model, reason string, duration time.Duration) {
	if model == "" {
		p.MarkCooldown(accountID, reason, duration)
		return
	}
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return
	}
	now := time.Now()
	until := now.Add(duration)
	a.mu.Lock()
	if a.modelCooldowns == nil {
		a.modelCooldowns = make(map[string]cooldownState)
	}
	existing := a.modelCooldowns[model]
	remaining := time.Until(existing.Until)
	extensionThreshold := remaining / 10
	if extensionThreshold < 5*time.Second {
		extensionThreshold = 5 * time.Second
	}
	changed := !now.Before(existing.Until) || until.After(existing.Until.Add(extensionThreshold))
	if changed {
		a.modelCooldowns[model] = cooldownState{Until: until, Reason: reason}
	} else {
		until, reason = existing.Until, existing.Reason
	}
	a.mu.Unlock()
	if !changed {
		return
	}
	p.mu.Lock()
	state := p.states[accountID]
	state.ModelCooldowns = cloneModelCooldowns(state.ModelCooldowns)
	if state.ModelCooldowns == nil {
		state.ModelCooldowns = make(map[string]cooldownState)
	}
	state.ModelCooldowns[model] = cooldownState{Until: until, Reason: reason}
	p.states[accountID] = state
	p.mu.Unlock()
	p.requestRebuild()
	p.rebuildWhenCooldownExpires(until)
	if err := p.persistState(); err != nil {
		slog.Error("credential scheduler state persistence failed", "error", err)
	}
	slog.Warn("credential model cooling", "account", accountID, "model", model, "reason", reason, "until", until.UTC().Format(time.RFC3339))
}

func cloneModelCooldowns(source map[string]cooldownState) map[string]cooldownState {
	if len(source) == 0 {
		return nil
	}
	cloned := make(map[string]cooldownState, len(source))
	for model, cooldown := range source {
		cloned[model] = cooldown
	}
	return cloned
}

func (p *Pool) rebuildWhenCooldownExpires(until time.Time) {
	go func() {
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		select {
		case <-timer.C:
			p.rebuildActive()
			p.notifyCapacity()
		case <-p.closed:
		}
	}()
}

func (p *Pool) Disable(accountID, reason string) {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return
	}
	a.mu.Lock()
	changed := !a.disabled || a.disableReason != reason
	a.disabled, a.disableReason = true, reason
	fingerprint := credentialFingerprint(a.credential)
	a.mu.Unlock()
	if !changed {
		return
	}
	p.mu.Lock()
	state := p.states[accountID]
	state.Disabled, state.Reason, state.CredentialFingerprint = true, reason, fingerprint
	state.CooldownUntil = time.Time{}
	p.states[accountID] = state
	p.mu.Unlock()
	p.requestRebuild()
	if err := p.persistState(); err != nil {
		slog.Error("credential scheduler state persistence failed", "error", err)
	}
	slog.Warn("credential account disabled", "account", accountID, "reason", reason)
}

func credentialFingerprint(cred *credential) string {
	if cred == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(cred.AccessToken + "\x00" + cred.RefreshToken))
	return hex.EncodeToString(sum[:])
}

func (p *Pool) hasReadyAccount() bool {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		cred, _, disabled := a.snapshot()
		if !disabled && cred != nil && cred.AccessToken != "" && !cred.needsRefresh(now, 0) {
			return true
		}
	}
	return false
}

func (p *Pool) warmup(ctx context.Context) error {
	p.mu.RLock()
	accounts := make([]*account, 0, len(p.accounts))
	for _, a := range p.accounts {
		accounts = append(accounts, a)
	}
	p.mu.RUnlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *account)
	results := make(chan error, len(accounts))
	workers := p.cfg.RefreshConcurrency
	if workers > len(accounts) {
		workers = len(accounts)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				results <- p.ensureFresh(ctx, a, true)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, a := range accounts {
			select {
			case jobs <- a:
			case <-ctx.Done():
				return
			}
		}
	}()
	var last error
	for range accounts {
		select {
		case err := <-results:
			if err == nil {
				cancel()
				wg.Wait()
				return nil
			}
			last = err
		case <-ctx.Done():
			wg.Wait()
			return fmt.Errorf("credential warmup: %w", ctx.Err())
		}
	}
	if last == nil {
		last = ErrNoAuth
	}
	wg.Wait()
	return fmt.Errorf("credential warmup: %w", last)
}

func (p *Pool) ensureUsable(ctx context.Context, a *account) error {
	cred, _, disabled := a.snapshot()
	if cred == nil || disabled {
		return ErrNoAuth
	}
	if cred.usable(time.Now()) {
		return nil
	}
	return p.ensureFresh(ctx, a, false)
}

func (p *Pool) ensureFresh(ctx context.Context, a *account, force bool) error {
	return p.refreshCredential(ctx, a, force, 0, false)
}

func (p *Pool) refreshCredential(ctx context.Context, a *account, force bool, observedGeneration uint64, compareGeneration bool) error {
	if err := a.acquireRefresh(ctx); err != nil {
		return err
	}
	defer a.releaseRefresh()
	if compareGeneration && a.currentGeneration() != observedGeneration {
		return nil
	}
	cred, _, disabled := a.snapshot()
	if cred == nil || disabled {
		return ErrNoAuth
	}
	if !force && !cred.needsRefresh(time.Now(), deterministicJitter(a.id)) {
		return nil
	}
	select {
	case p.refreshSem <- struct{}{}:
		defer func() { <-p.refreshSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	next, err := cred.refresh(refreshCtx, p.http)
	if err != nil {
		if ctx.Err() != nil || refreshCtx.Err() == context.Canceled {
			return err
		}
		var refreshErr *RefreshError
		if errors.As(err, &refreshErr) && refreshErr.Permanent {
			p.Disable(a.id, "refresh_invalid")
		} else {
			p.MarkCooldown(a.id, refreshBackoffReason, time.Minute)
		}
		return err
	}
	a.mu.Lock()
	a.credential = next
	a.generation.Add(1)
	a.disabled = false
	a.disableReason = ""
	now := time.Now()
	a.cooldowns = activeAccountCooldowns(a.cooldowns, now)
	delete(a.cooldowns, refreshBackoffReason)
	tierChanged := cred.Tier != next.Tier
	if !next.Tier.Billing {
		a.billing = nil
		a.billingPending = false
		delete(a.cooldowns, billingExhaustedReason)
	} else if tierChanged {
		a.billing = nil
	}
	if next.Tier.Billing && a.billing == nil && p.billingGate.Load() {
		a.billingPending = true
	}
	billingRefreshNeeded := a.billingPending
	cooldowns := cloneAccountCooldowns(a.cooldowns)
	a.mu.Unlock()
	p.mu.Lock()
	state := p.states[a.id]
	state.Disabled = false
	state.CredentialFingerprint = ""
	state.Cooldowns = cooldowns
	syncLegacyCooldownState(&state, now)
	if accountStateEmpty(state) {
		delete(p.states, a.id)
	} else {
		p.states[a.id] = state
	}
	if cached, ok := p.files[next.Path]; ok {
		if info, statErr := os.Stat(next.Path); statErr == nil {
			cached.modTime, cached.size, cached.cred = info.ModTime(), info.Size(), next
			p.files[next.Path] = cached
		}
	}
	p.mu.Unlock()
	p.requestRebuild()
	p.notifyCapacity()
	if billingRefreshNeeded {
		p.requestBillingRefresh()
	}
	if err := p.persistState(); err != nil {
		slog.Error("credential scheduler state persistence failed", "error", err)
	}
	slog.Info("credential refreshed", "account", a.id)
	return nil
}

func (p *Pool) background() {
	defer p.wg.Done()
	p.refreshAllDue()
	ticker := time.NewTicker(p.cfg.ReloadInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := p.scan(); err != nil {
				slog.Error("credential directory reload failed", "error", err)
			}
			p.refreshAllDue()
		case <-p.closed:
			return
		}
	}
}

func (p *Pool) refreshAllDue() {
	p.mu.RLock()
	accounts := make([]*account, 0, len(p.accounts))
	for _, a := range p.accounts {
		cred, _, disabled := a.snapshot()
		if !disabled && cred != nil && cred.needsRefresh(time.Now(), deterministicJitter(a.id)) {
			accounts = append(accounts, a)
		}
	}
	p.mu.RUnlock()
	if len(accounts) == 0 {
		return
	}
	jobs := make(chan *account)
	var wg sync.WaitGroup
	workers := p.cfg.RefreshConcurrency
	if workers > len(accounts) {
		workers = len(accounts)
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				select {
				case <-p.closed:
					return
				default:
					_ = p.ensureFresh(context.Background(), a, false)
				}
			}
		}()
	}
	for _, a := range accounts {
		select {
		case jobs <- a:
		case <-p.closed:
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}

func (p *Pool) scan() error {
	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()
	return p.scanUnlocked()
}

func (p *Pool) scanUnlocked() error {
	entries, err := os.ReadDir(p.cfg.Dir)
	if err != nil {
		return fmt.Errorf("read auths directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	seen := map[string]struct{}{}
	parsed := map[string]*credential{}
	newFiles := map[string]fileEntry{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		path := filepath.Join(p.cfg.Dir, name)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		p.mu.RLock()
		cached, ok := p.files[path]
		p.mu.RUnlock()
		var cred *credential
		if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
			cred = cached.cred
		} else {
			cred, err = loadCredential(path, p.cfg.Surface)
			if err != nil {
				slog.Warn("credential file skipped", "reason", "invalid_format")
				continue
			}
		}
		id := accountID(cred.Subject)
		if _, duplicate := seen[id]; duplicate {
			slog.Warn("duplicate credential skipped", "account", id)
			continue
		}
		seen[id] = struct{}{}
		parsed[id] = cred
		newFiles[path] = fileEntry{size: info.Size(), modTime: info.ModTime(), cred: cred}
	}
	if len(parsed) == 0 {
		p.mu.Lock()
		hadAccounts := len(p.accounts) > 0
		for _, a := range p.accounts {
			a.mu.Lock()
			a.disabled, a.disableReason = true, "removed"
			a.mu.Unlock()
		}
		p.accounts = map[string]*account{}
		p.files = map[string]fileEntry{}
		p.mu.Unlock()
		p.rebuildActive()
		if hadAccounts {
			slog.Warn("credential pool is empty")
			return nil
		}
		if p.cfg.AllowEmpty {
			return nil
		}
		return ErrNoAuth
	}
	p.mu.Lock()
	poolChanged := len(p.files) != len(newFiles) || len(p.accounts) != len(parsed)
	if !poolChanged {
		for path, next := range newFiles {
			current, ok := p.files[path]
			if !ok || current.size != next.size || !current.modTime.Equal(next.modTime) {
				poolChanged = true
				break
			}
		}
	}
	var cooldownDeadlines []time.Time
	billingRefreshNeeded := false
	for id, cred := range parsed {
		if existing := p.accounts[id]; existing != nil {
			existing.mu.Lock()
			credentialChanged := existing.credential == nil || existing.credential.Path != cred.Path || existing.credential.AccessToken != cred.AccessToken || existing.credential.RefreshToken != cred.RefreshToken
			existing.credential = cred
			if credentialChanged {
				existing.generation.Add(1)
				poolChanged = true
				existing.disabled = false
				existing.disableReason = ""
				existing.cooldowns = nil
				existing.billing = nil
				existing.billingPending = p.billingGate.Load() && cred.Tier.Billing
				state := p.states[id]
				state.Disabled = false
				state.CredentialFingerprint = ""
				state.CooldownUntil = time.Time{}
				state.Reason = ""
				state.Cooldowns = nil
				if accountStateEmpty(state) {
					delete(p.states, id)
				} else {
					p.states[id] = state
				}
				billingRefreshNeeded = billingRefreshNeeded || existing.billingPending
			}
			existing.mu.Unlock()
			continue
		}
		a := &account{id: id, credential: cred, agentID: randomHex(16), sessionID: randomUUID(), billingPending: p.billingGate.Load() && cred.Tier.Billing}
		a.generation.Store(1)
		if state, ok := p.states[id]; ok {
			if state.Disabled {
				fingerprintChanged := state.CredentialFingerprint != "" && state.CredentialFingerprint != credentialFingerprint(cred)
				if fingerprintChanged {
					state.Disabled = false
					state.CredentialFingerprint = ""
					state.CooldownUntil = time.Time{}
					state.Reason = ""
					state.Cooldowns = nil
					if accountStateEmpty(state) {
						delete(p.states, id)
					} else {
						p.states[id] = state
					}
				} else {
					a.disabled, a.disableReason = true, state.Reason
					a.billingPending = false
				}
			}
			a.cooldowns = activeAccountCooldowns(state.Cooldowns, time.Now())
			for _, cooldown := range a.cooldowns {
				cooldownDeadlines = append(cooldownDeadlines, cooldown.Until)
			}
			for model, cooldown := range state.ModelCooldowns {
				if time.Now().Before(cooldown.Until) {
					if a.modelCooldowns == nil {
						a.modelCooldowns = make(map[string]cooldownState)
					}
					a.modelCooldowns[model] = cooldown
					cooldownDeadlines = append(cooldownDeadlines, cooldown.Until)
				}
			}
		}
		p.accounts[id] = a
		billingRefreshNeeded = billingRefreshNeeded || a.billingPending
		poolChanged = true
	}
	for id, a := range p.accounts {
		if _, ok := parsed[id]; !ok {
			a.mu.Lock()
			a.disabled, a.disableReason = true, "removed"
			a.mu.Unlock()
			delete(p.accounts, id)
			poolChanged = true
		}
	}
	p.files = newFiles
	count := len(p.accounts)
	p.mu.Unlock()
	for _, until := range cooldownDeadlines {
		p.rebuildWhenCooldownExpires(until)
	}
	if poolChanged {
		p.rebuildActive()
		p.notifyCapacity()
		slog.Info("credential pool loaded", "accounts", count)
	}
	if billingRefreshNeeded {
		p.requestBillingRefresh()
	}
	return nil
}

func cloneBillingInfo(source *BillingInfo) *BillingInfo {
	if source == nil {
		return nil
	}
	cloned := *source
	if source.UsagePercent != nil {
		value := *source.UsagePercent
		cloned.UsagePercent = &value
	}
	if source.PeriodEnd != nil {
		value := *source.PeriodEnd
		cloned.PeriodEnd = &value
	}
	if source.OnDemandRemainingCents != nil {
		value := *source.OnDemandRemainingCents
		cloned.OnDemandRemainingCents = &value
	}
	if source.PrepaidBalanceCents != nil {
		value := *source.PrepaidBalanceCents
		cloned.PrepaidBalanceCents = &value
	}
	if source.UnifiedBilling != nil {
		value := *source.UnifiedBilling
		cloned.UnifiedBilling = &value
	}
	return &cloned
}

func (p *Pool) rebuildActive() {
	now := time.Now()
	p.mu.RLock()
	active := make([]*account, 0, len(p.accounts))
	byModel := map[string][]*account{}
	for _, a := range p.accounts {
		a.mu.RLock()
		cooldownUntil, _ := cooldownDeadline(a.cooldowns, now)
		available := !a.disabled && !a.billingPending && cooldownUntil.IsZero() && a.credential != nil
		var models []string
		if available {
			a.paidTier.Store(a.credential.Tier.Paid)
			for _, model := range a.credential.Models {
				cooldown, cooling := a.modelCooldowns[model]
				if !cooling || !now.Before(cooldown.Until) {
					models = append(models, model)
				}
			}
		}
		a.mu.RUnlock()
		if !available {
			continue
		}
		active = append(active, a)
		for _, model := range models {
			byModel[model] = append(byModel[model], a)
		}
	}
	p.mu.RUnlock()
	sort.Slice(active, func(i, j int) bool {
		paidI, paidJ := active[i].prefersPaidTier(), active[j].prefersPaidTier()
		if paidI != paidJ {
			return paidI
		}
		return active[i].id < active[j].id
	})
	for model := range byModel {
		accounts := byModel[model]
		sort.Slice(accounts, func(i, j int) bool {
			paidI, paidJ := accounts[i].prefersPaidTier(), accounts[j].prefersPaidTier()
			if paidI != paidJ {
				return paidI
			}
			return accounts[i].id < accounts[j].id
		})
	}
	p.active.Store(active)
	p.activeByModel.Store(byModel)
}

func (p *Pool) requestRebuild() {
	select {
	case p.rebuildCh <- struct{}{}:
	default:
	}
}

func (p *Pool) rebuildLoop() {
	defer p.wg.Done()
	for {
		select {
		case <-p.rebuildCh:
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-timer.C:
				p.rebuildActive()
			case <-p.closed:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
		case <-p.closed:
			return
		}
	}
}

func (p *Pool) unavailable(model string) error {
	now := time.Now()
	p.mu.RLock()
	defer p.mu.RUnlock()
	var earliest time.Time
	hasCooling := false
	for _, a := range p.accounts {
		a.mu.RLock()
		until, _ := cooldownDeadline(a.cooldowns, now)
		disabled := a.disabled
		if model != "" && !disabled && a.credential != nil {
			index := sort.SearchStrings(a.credential.Models, model)
			if index < len(a.credential.Models) && a.credential.Models[index] == model {
				if cooldown, ok := a.modelCooldowns[model]; ok && now.Before(cooldown.Until) && cooldown.Until.After(until) {
					until = cooldown.Until
				}
			}
		}
		a.mu.RUnlock()
		if !disabled && now.Before(until) {
			hasCooling = true
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
		}
	}
	if hasCooling {
		return &UnavailableError{Cooling: true, RetryAfter: time.Until(earliest)}
	}
	return &UnavailableError{}
}

func (p *Pool) loadState() error {
	b, err := os.ReadFile(filepath.Join(p.cfg.Dir, stateFileName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persistedState
	if err := json.Unmarshal(b, &state); err != nil {
		return err
	}
	now := time.Now()
	for id, item := range state.Accounts {
		cooldowns := activeAccountCooldowns(item.Cooldowns, now)
		if !item.Disabled && item.Reason != "" && now.Before(item.CooldownUntil) {
			if existing, ok := cooldowns[item.Reason]; !ok || item.CooldownUntil.After(existing.Until) {
				if cooldowns == nil {
					cooldowns = make(map[string]cooldownState)
				}
				cooldowns[item.Reason] = cooldownState{Until: item.CooldownUntil, Reason: item.Reason}
			}
		}
		item.Cooldowns = cooldowns
		activeModels := make(map[string]cooldownState)
		for model, cooldown := range item.ModelCooldowns {
			if now.Before(cooldown.Until) {
				activeModels[model] = cooldown
			}
		}
		item.ModelCooldowns = activeModels
		syncLegacyCooldownState(&item, now)
		if !accountStateEmpty(item) {
			p.states[id] = item
		}
	}
	return nil
}

func (p *Pool) persistState() error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.mu.RLock()
	state := persistedState{Version: 2, Accounts: map[string]accountState{}}
	now := time.Now()
	for id, item := range p.states {
		item.Cooldowns = activeAccountCooldowns(item.Cooldowns, now)
		activeModels := make(map[string]cooldownState)
		for model, cooldown := range item.ModelCooldowns {
			if now.Before(cooldown.Until) {
				activeModels[model] = cooldown
			}
		}
		item.ModelCooldowns = activeModels
		syncLegacyCooldownState(&item, now)
		if !accountStateEmpty(item) {
			state.Accounts[id] = item
		}
	}
	p.mu.RUnlock()
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	path := filepath.Join(p.cfg.Dir, stateFileName)
	tmp, err := os.CreateTemp(p.cfg.Dir, ".grok-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

type affinityEntry struct {
	accountID string
	expiresAt time.Time
}
type affinityShard struct {
	sync.Mutex
	entries map[string]affinityEntry
}
type affinityCache struct {
	shards []affinityShard
	ttl    time.Duration
	limit  int
}

func newAffinityCache(ttl time.Duration, maxEntries int) *affinityCache {
	const shardCount = 64
	c := &affinityCache{shards: make([]affinityShard, shardCount), ttl: ttl, limit: (maxEntries + shardCount - 1) / shardCount}
	for i := range c.shards {
		c.shards[i].entries = map[string]affinityEntry{}
	}
	return c
}

func affinityHash(key string) (string, byte) {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), sum[0]
}

func modelAffinityKey(affinity, model string) string {
	if affinity == "" {
		return ""
	}
	return affinity + "\x00" + model
}

func (c *affinityCache) Get(key string) (string, bool) {
	hash, shardKey := affinityHash(key)
	shard := &c.shards[int(shardKey)%len(c.shards)]
	shard.Lock()
	defer shard.Unlock()
	entry, ok := shard.entries[hash]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(shard.entries, hash)
		return "", false
	}
	entry.expiresAt = time.Now().Add(c.ttl)
	shard.entries[hash] = entry
	return entry.accountID, true
}

func (c *affinityCache) Set(key, accountID string) {
	hash, shardKey := affinityHash(key)
	shard := &c.shards[int(shardKey)%len(c.shards)]
	shard.Lock()
	defer shard.Unlock()
	now := time.Now()
	if len(shard.entries) >= c.limit {
		var oldestKey string
		var oldest time.Time
		for k, entry := range shard.entries {
			if now.After(entry.expiresAt) {
				delete(shard.entries, k)
				continue
			}
			if oldest.IsZero() || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = k, entry.expiresAt
			}
		}
		if len(shard.entries) >= c.limit && oldestKey != "" {
			delete(shard.entries, oldestKey)
		}
	}
	shard.entries[hash] = affinityEntry{accountID: accountID, expiresAt: now.Add(c.ttl)}
}

func (c *affinityCache) Delete(key string) {
	hash, shardKey := affinityHash(key)
	shard := &c.shards[int(shardKey)%len(c.shards)]
	shard.Lock()
	delete(shard.entries, hash)
	shard.Unlock()
}
