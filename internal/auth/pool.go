package auth

import (
	"context"
	"crypto/hmac"
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

	"github.com/Futureppo/grokcli2api-go/internal/modelcatalog"
)

const stateFileName = ".grokcli2api-state.json"
const affinityStateFileName = ".grokcli2api-affinity.json"

var errAccountBusy = errors.New("credential account is at its in-flight limit")

var ErrCredentialNotFound = errors.New("credential not found")

type AffinityMode uint8

const (
	AffinityNone AffinityMode = iota
	AffinitySoft
	AffinityHard
)

type Affinity struct {
	Key    string
	Mode   AffinityMode
	Tenant string
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
	ID                      string                       `json:"id"`
	Scope                   string                       `json:"scope,omitempty"`
	AuthMode                AuthMode                     `json:"auth_mode,omitempty"`
	Status                  string                       `json:"status"`
	Usable                  bool                         `json:"usable"`
	Disabled                bool                         `json:"disabled"`
	DisabledReason          string                       `json:"disabled_reason,omitempty"`
	ExpiresAt               *time.Time                   `json:"expires_at,omitempty"`
	CooldownUntil           *time.Time                   `json:"cooldown_until,omitempty"`
	CooldownReason          string                       `json:"cooldown_reason,omitempty"`
	ModelCooldowns          map[string]ModelCooldownInfo `json:"model_cooldowns,omitempty"`
	Models                  []string                     `json:"models"`
	DiscoveryStatus         string                       `json:"discovery_status"`
	CatalogETag             string                       `json:"catalog_etag,omitempty"`
	CatalogUpdated          *time.Time                   `json:"catalog_updated_at,omitempty"`
	HasRefreshToken         bool                         `json:"has_refresh_token"`
	SubscriptionTier        string                       `json:"subscription_tier,omitempty"`
	SubscriptionTierDisplay string                       `json:"subscription_tier_display,omitempty"`
	Paid                    bool                         `json:"-"`
	Billing                 *BillingInfo                 `json:"billing,omitempty"`
	BuildRoutingInfo
}

// ModelCooldownInfo is redacted model-scoped cooldown metadata for administrators.
type ModelCooldownInfo struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

type credentialInfoSnapshot struct {
	generation uint64
	validUntil time.Time
	infos      []CredentialInfo
}

// BillingInfo is a redacted, in-memory snapshot of the authoritative credits
// endpoint. Monetary values are remaining cents and never include payment data.
type BillingInfo struct {
	PlanCode               string     `json:"plan_code,omitempty"`
	PlanName               string     `json:"plan_name,omitempty"`
	Paid                   bool       `json:"paid"`
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

	mu                 sync.RWMutex
	credential         *credential
	descriptors        map[string]modelcatalog.ModelDescriptor
	catalogETag        string
	catalogUpdated     time.Time
	cooldownUntil      time.Time
	cooldownCause      string
	modelCooldowns     map[string]cooldownState
	disabled           bool
	disableReason      string
	billing            *BillingInfo
	buildRouteMode     BuildRouteMode
	buildSuperEntitled bool
	buildAPIFallback   bool
	deleting           bool
	refreshOnce        sync.Once
	refreshLock        chan struct{}
	generation         atomic.Uint64
	inflight           atomic.Int64
	paidTier           atomic.Bool
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
	return !a.disabled && !now.Before(a.cooldownUntil) && a.credential != nil
}

func (a *account) requestUsable(now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.disabled && !now.Before(a.cooldownUntil) && a.credential != nil && a.credential.usable(now)
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
	if !a.hasModelLocked(model) {
		return false
	}
	cooldown, cooling := a.modelCooldowns[model]
	return !cooling || !time.Now().Before(cooldown.Until)
}

func (a *account) hasModelLocked(model string) bool {
	if len(a.descriptors) > 0 {
		descriptor, exists := a.descriptors[model]
		if !exists || descriptor.Hidden {
			return false
		}
		return a.credential.AuthMode != AuthModeAPIKey || descriptor.SupportedInAPI
	}
	index := sort.SearchStrings(a.credential.Models, model)
	return index < len(a.credential.Models) && a.credential.Models[index] == model
}

func (a *account) descriptor(model string) (modelcatalog.ModelDescriptor, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	descriptor, ok := a.descriptors[model]
	descriptor.ReasoningEfforts = append([]string(nil), descriptor.ReasoningEfforts...)
	return descriptor, ok
}

func (a *account) modelIDsLocked() []string {
	if len(a.descriptors) == 0 {
		return append([]string(nil), a.credential.Models...)
	}
	models := make([]string, 0, len(a.descriptors))
	for model := range a.descriptors {
		if a.hasModelLocked(model) {
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models
}

func (a *account) snapshot() (*credential, time.Time, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.credential, a.cooldownUntil, a.disabled
}

type fileEntry struct {
	size    int64
	modTime time.Time
	cred    *credential
	creds   []*credential
}

type persistedState struct {
	Version       int                     `json:"version"`
	GlobalAgentID string                  `json:"global_agent_id,omitempty"`
	NamespaceKey  string                  `json:"namespace_key,omitempty"`
	Accounts      map[string]accountState `json:"accounts"`
	Catalogs      map[string]catalogState `json:"catalogs,omitempty"`
}

type catalogState struct {
	ETag        string                                  `json:"etag,omitempty"`
	UpdatedAt   time.Time                               `json:"updated_at,omitempty"`
	Models      map[string]modelcatalog.ModelDescriptor `json:"models,omitempty"`
	Provisional []string                                `json:"provisional_models,omitempty"`
	FirstSeen   map[string]int64                        `json:"first_seen,omitempty"`
}

type accountState struct {
	CooldownUntil         time.Time                `json:"cooldown_until,omitempty"`
	Reason                string                   `json:"reason,omitempty"`
	Disabled              bool                     `json:"disabled,omitempty"`
	CredentialFingerprint string                   `json:"credential_fingerprint,omitempty"`
	ModelCooldowns        map[string]cooldownState `json:"model_cooldowns,omitempty"`
	BuildRouteMode        BuildRouteMode           `json:"build_route_mode,omitempty"`
	BuildSuperEntitled    bool                     `json:"build_super_entitled,omitempty"`
	BuildAPIFallback      bool                     `json:"build_api_fallback,omitempty"`
}

type cooldownState struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

type Pool struct {
	cfg                          PoolConfig
	http                         *http.Client
	mu                           sync.RWMutex
	accounts                     map[string]*account
	files                        map[string]fileEntry
	states                       map[string]accountState
	catalogs                     map[string]catalogState
	globalAgentID                string
	namespaceKey                 string
	active                       atomic.Value // []*account
	activeByModel                atomic.Value // map[string][]*account
	cursor                       atomic.Uint64
	credentialSnapshotGeneration atomic.Uint64
	credentialSnapshot           atomic.Pointer[credentialInfoSnapshot]
	credentialSnapshotMu         sync.Mutex
	affinity                     *affinityCache
	refreshSem                   chan struct{}
	capacityCh                   chan struct{}
	capacityMu                   sync.Mutex
	rebuildCh                    chan struct{}
	stateWake                    chan struct{}
	statePersistDelay            time.Duration
	closed                       chan struct{}
	closeOnce                    sync.Once
	wg                           sync.WaitGroup
	stateMu                      sync.Mutex
	stateGeneration              atomic.Uint64
	statePersistedGeneration     atomic.Uint64
	stateDigest                  [sha256.Size]byte
	stateDigestValid             bool
	stateWriteCount              atomic.Uint64
	mutationMu                   sync.Mutex
}

type Lease struct {
	pool       *Pool
	account    *account
	credential *credential
	generation uint64
	model      string
	descriptor modelcatalog.ModelDescriptor
	described  bool
	routing    BuildRoutingInfo
	once       sync.Once
}

func (p *Pool) newLease(a *account, model string) *Lease {
	a.mu.RLock()
	cred := a.credential
	generation := a.currentGeneration()
	descriptor, described := a.descriptors[model]
	descriptor.ReasoningEfforts = append([]string(nil), descriptor.ReasoningEfforts...)
	routing := buildRoutingInfoWithBilling(cred, a.billing, a.buildRouteMode, a.buildSuperEntitled, a.buildAPIFallback)
	a.mu.RUnlock()
	return &Lease{
		pool: p, account: a, credential: cred, generation: generation, model: model,
		descriptor: descriptor, described: described, routing: routing,
	}
}

func (l *Lease) Session() Session {
	if l.credential == nil {
		return Session{}
	}
	return l.credential.session()
}
func (l *Lease) AccountID() string              { return l.account.id }
func (l *Lease) AgentID() string                { return l.account.agentID }
func (l *Lease) SessionID() string              { return l.account.sessionID }
func (l *Lease) Generation() uint64             { return l.generation }
func (l *Lease) BuildRouting() BuildRoutingInfo { return l.routing }

// Descriptor returns the immutable descriptor selected with this lease. A
// false result means scheduling used only a legacy provisional []string list.
func (l *Lease) Descriptor() (modelcatalog.ModelDescriptor, bool) {
	return l.descriptor, l.described
}

// DescriptorFor is useful for metadata leases that were not acquired for a
// particular model.
func (l *Lease) DescriptorFor(model string) (modelcatalog.ModelDescriptor, bool) {
	if l == nil || l.account == nil {
		return modelcatalog.ModelDescriptor{}, false
	}
	return l.account.descriptor(model)
}
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
		states: map[string]accountState{}, catalogs: map[string]catalogState{},
		affinity:          newAffinityCache(cfg.AffinityTTL, cfg.AffinityMaxEntries),
		refreshSem:        make(chan struct{}, cfg.RefreshConcurrency),
		capacityCh:        make(chan struct{}),
		rebuildCh:         make(chan struct{}, 1),
		stateWake:         make(chan struct{}, 1),
		statePersistDelay: time.Second,
		closed:            make(chan struct{}),
	}
	p.active.Store([]*account{})
	p.activeByModel.Store(map[string][]*account{})
	p.credentialSnapshotGeneration.Store(1)
	resetPersistedAffinity := false
	if err := p.loadState(); err != nil {
		// Corrupt/unreadable state is never guessed. Fresh identity material below
		// makes every old affinity key unreachable.
		slog.Warn("credential state ignored", "reason", "invalid_state")
		p.states = map[string]accountState{}
		p.catalogs = map[string]catalogState{}
		p.globalAgentID = ""
		p.namespaceKey = ""
		resetPersistedAffinity = true
	}
	if !validGlobalAgentID(p.globalAgentID) {
		p.globalAgentID = randomHex(16)
		resetPersistedAffinity = true
	}
	if !validNamespaceKey(p.namespaceKey) {
		p.namespaceKey = randomHex(32)
		p.affinity = newAffinityCache(cfg.AffinityTTL, cfg.AffinityMaxEntries)
		resetPersistedAffinity = true
	}
	if resetPersistedAffinity {
		if err := os.Remove(filepath.Join(cfg.Dir, affinityStateFileName)); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("clear stale affinity state: %w", err)
		}
	}
	if err := p.scan(); err != nil {
		return nil, err
	}
	// Persist generated v2 identity material and any migrated v1 cooldowns
	// before serving requests.
	if err := p.persistState(); err != nil {
		return nil, fmt.Errorf("persist credential state: %w", err)
	}
	// The synchronous startup checkpoint already includes scan changes.
	select {
	case <-p.stateWake:
	default:
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
	p.wg.Add(3)
	go p.background()
	go p.rebuildLoop()
	go p.stateCheckpointLoop()
	return p, nil
}

func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.wg.Wait()
		_ = p.FlushState()
	})
}

func (p *Pool) Acquire(ctx context.Context, affinity Affinity, model string, exclude map[string]struct{}) (*Lease, error) {
	cacheKey := affinityModelKey(affinity, model)
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
						if excluded || failedRefresh || !a.available(time.Now()) || !a.supportsModel(model) {
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
						return p.newLease(selected, model), nil
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

// AcquireForBackends prefers accounts whose structured descriptor uses the
// requested backends, in order. Legacy provisional model lists are considered
// only after every structured choice has been exhausted.
func (p *Pool) AcquireForBackends(
	ctx context.Context,
	affinity Affinity,
	model string,
	preferred []modelcatalog.Backend,
	exclude map[string]struct{},
) (*Lease, error) {
	if len(preferred) == 0 {
		return p.Acquire(ctx, affinity, model, exclude)
	}
	var lastErr error
	seenBackend := make(map[modelcatalog.Backend]struct{}, len(preferred))
	for _, backend := range preferred {
		if _, duplicate := seenBackend[backend]; duplicate {
			continue
		}
		seenBackend[backend] = struct{}{}
		allowed := p.accountsForBackend(model, backend, false)
		if len(allowed) == 0 {
			continue
		}
		lease, err := p.Acquire(ctx, affinity, model, exclusionsExcept(allowed, p.AccountIDs(), exclude))
		if err == nil {
			return lease, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		lastErr = err
	}
	provisional := p.accountsForBackend(model, "", true)
	if len(provisional) > 0 {
		lease, err := p.Acquire(ctx, affinity, model, exclusionsExcept(provisional, p.AccountIDs(), exclude))
		if err == nil {
			return lease, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	if !p.hasAccounts() {
		return nil, p.unavailable(model)
	}
	if model != "" && !p.hasKnownModel(model) {
		return nil, &ModelUnavailableError{Model: model}
	}
	return nil, p.unavailable(model)
}

func (p *Pool) accountsForBackend(model string, backend modelcatalog.Backend, provisional bool) map[string]struct{} {
	result := make(map[string]struct{})
	p.mu.RLock()
	for id, a := range p.accounts {
		a.mu.RLock()
		if !a.disabled && a.credential != nil {
			if provisional {
				if len(a.descriptors) == 0 && a.hasModelLocked(model) {
					result[id] = struct{}{}
				}
			} else if descriptor, ok := a.descriptors[model]; ok && descriptor.Backend == backend && a.hasModelLocked(model) {
				result[id] = struct{}{}
			}
		}
		a.mu.RUnlock()
	}
	p.mu.RUnlock()
	return result
}

func exclusionsExcept(allowed map[string]struct{}, all []string, existing map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(existing)+len(all))
	for id := range existing {
		result[id] = struct{}{}
	}
	for _, id := range all {
		if _, ok := allowed[id]; !ok {
			result[id] = struct{}{}
		}
	}
	return result
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

// AcquireAccountForModel obtains a hard-pinned account while validating the
// model catalog and model-scoped cooldown, and snapshots its descriptor.
func (p *Pool) AcquireAccountForModel(ctx context.Context, id, model string) (*Lease, error) {
	for {
		capacity := p.capacitySignal()
		lease, err := p.acquireID(ctx, id, model, nil)
		if !errors.Is(err, errAccountBusy) {
			if errors.Is(err, ErrNoAuth) {
				err = p.boundAccountUnavailable(id, model)
			}
			return lease, err
		}
		if err := p.waitForCapacity(ctx, capacity); err != nil {
			return nil, err
		}
	}
}

func (p *Pool) boundAccountUnavailable(id, model string) error {
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.disabled || a.credential == nil || !a.hasModelLocked(model) {
		return ErrNoAuth
	}
	now := time.Now()
	until := a.cooldownUntil
	if cooldown, ok := a.modelCooldowns[model]; ok && cooldown.Until.After(until) {
		until = cooldown.Until
	}
	if now.Before(until) {
		return &UnavailableError{Cooling: true, RetryAfter: time.Until(until)}
	}
	return ErrNoAuth
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
	return p.newLease(a, ""), nil
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
	return p.newLease(a, model), nil
}

func (p *Pool) Bind(affinity Affinity, model, accountID string) {
	if affinity.Key != "" && accountID != "" {
		p.affinity.Set(affinityModelKey(affinity, model), accountID)
	}
}

// Unbind removes the current request's scheduler affinity when a renderer
// silently drops the state field that made the request hard-affine.
func (p *Pool) Unbind(affinity Affinity, model string) {
	if affinity.Key != "" {
		p.affinity.Delete(affinityModelKey(affinity, model))
	}
}

func (p *Pool) BindResponseID(responseID, model, accountID string) {
	p.BindResponseIDForTenant("public", responseID, model, accountID)
}

func (p *Pool) BindResponseIDForTenant(tenant, responseID, model, accountID string) {
	if responseID != "" {
		p.affinity.Set(affinityModelKey(Affinity{Tenant: tenant, Key: "previous:" + responseID}, model), accountID)
	}
}

// TenantID derives a non-reversible namespace for downstream continuity. The
// raw local API key is never retained or persisted. Without a configured key,
// all callers intentionally share the public tenant.
func (p *Pool) TenantID(apiKey string) string {
	if apiKey == "" {
		return "public"
	}
	key, err := hex.DecodeString(p.namespaceKey)
	if err != nil || len(key) != 32 {
		return "public"
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(apiKey))
	return hex.EncodeToString(mac.Sum(nil))
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

// Credentials returns an independent copy of redacted credential metadata
// sorted by account ID.
func (p *Pool) Credentials() []CredentialInfo {
	_, snapshot := p.CredentialSnapshot()
	items := make([]CredentialInfo, len(snapshot))
	for index := range snapshot {
		items[index] = cloneCredentialInfo(snapshot[index])
	}
	return items
}

// CredentialSnapshot returns a generation-keyed immutable redacted snapshot
// sorted by account ID. Callers must treat the returned slice and nested values
// as read-only; a generation change publishes a newly built snapshot.
func (p *Pool) CredentialSnapshot() (uint64, []CredentialInfo) {
	for {
		now := time.Now()
		generation := p.credentialSnapshotGeneration.Load()
		if cached := p.credentialSnapshot.Load(); credentialSnapshotCurrent(cached, generation, now) {
			return cached.generation, cached.infos
		}

		p.credentialSnapshotMu.Lock()
		generation = p.credentialSnapshotGeneration.Load()
		now = time.Now()
		if cached := p.credentialSnapshot.Load(); credentialSnapshotCurrent(cached, generation, now) {
			p.credentialSnapshotMu.Unlock()
			return cached.generation, cached.infos
		}

		p.mu.RLock()
		infos := make([]CredentialInfo, 0, len(p.accounts))
		var validUntil time.Time
		for id, account := range p.accounts {
			info, transition := credentialInfoAt(id, account, now)
			infos = append(infos, info)
			if !transition.IsZero() && (validUntil.IsZero() || transition.Before(validUntil)) {
				validUntil = transition
			}
		}
		p.mu.RUnlock()
		sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
		if p.credentialSnapshotGeneration.Load() != generation {
			p.credentialSnapshotMu.Unlock()
			continue
		}
		next := &credentialInfoSnapshot{generation: generation, validUntil: validUntil, infos: infos}
		p.credentialSnapshot.Store(next)
		p.credentialSnapshotMu.Unlock()
		if p.credentialSnapshotGeneration.Load() == generation {
			return generation, infos
		}
	}
}

func credentialSnapshotCurrent(snapshot *credentialInfoSnapshot, generation uint64, now time.Time) bool {
	return snapshot != nil && snapshot.generation == generation &&
		(snapshot.validUntil.IsZero() || now.Before(snapshot.validUntil))
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
	info, _ := credentialInfoAt(id, a, now)
	return info
}

func credentialInfoAt(id string, a *account, now time.Time) (CredentialInfo, time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	info := CredentialInfo{ID: id, Disabled: a.disabled, DisabledReason: a.disableReason, Models: []string{}}
	if a.credential == nil {
		info.Status = "unavailable"
		return info, time.Time{}
	}
	var validUntil time.Time
	accountCooling := now.Before(a.cooldownUntil)
	if accountCooling {
		validUntil = a.cooldownUntil
	}
	for model, cooldown := range a.modelCooldowns {
		if !now.Before(cooldown.Until) {
			continue
		}
		if info.ModelCooldowns == nil {
			info.ModelCooldowns = make(map[string]ModelCooldownInfo)
		}
		info.ModelCooldowns[model] = ModelCooldownInfo{Until: cooldown.Until.UTC(), Reason: cooldown.Reason}
		if validUntil.IsZero() || cooldown.Until.Before(validUntil) {
			validUntil = cooldown.Until
		}
	}
	if !a.credential.ExpiresAt.IsZero() {
		usableUntil := a.credential.ExpiresAt.Add(-time.Minute)
		if now.Before(usableUntil) && (validUntil.IsZero() || usableUntil.Before(validUntil)) {
			validUntil = usableUntil
		}
	}
	info.Models = a.modelIDsLocked()
	info.Scope = a.credential.Scope
	info.AuthMode = a.credential.AuthMode
	info.BuildRoutingInfo = buildRoutingInfoWithBilling(a.credential, a.billing, a.buildRouteMode, a.buildSuperEntitled, a.buildAPIFallback)
	info.CatalogETag = a.catalogETag
	if !a.catalogUpdated.IsZero() {
		updated := a.catalogUpdated.UTC()
		info.CatalogUpdated = &updated
	}
	switch {
	case len(a.descriptors) > 0:
		info.DiscoveryStatus = "ready"
	case len(a.credential.Models) > 0:
		info.DiscoveryStatus = "provisional"
	default:
		info.DiscoveryStatus = "pending"
	}
	info.HasRefreshToken = a.credential.RefreshToken != ""
	info.SubscriptionTier = a.credential.Tier.Key
	info.SubscriptionTierDisplay = a.credential.Tier.Display
	info.Paid = a.credential.Tier.Paid || a.billing != nil && a.billing.Paid
	info.Billing = cloneBillingInfo(a.billing)
	if !a.credential.ExpiresAt.IsZero() {
		expires := a.credential.ExpiresAt.UTC()
		info.ExpiresAt = &expires
	}
	if accountCooling {
		cooldown := a.cooldownUntil.UTC()
		info.CooldownUntil = &cooldown
		info.CooldownReason = a.cooldownCause
	}
	allModelsCooling := len(info.Models) > 0
	var earliestModelReady time.Time
	for _, model := range info.Models {
		cooldown, cooling := info.ModelCooldowns[model]
		if !cooling {
			allModelsCooling = false
			continue
		}
		if earliestModelReady.IsZero() || cooldown.Until.Before(earliestModelReady) {
			earliestModelReady = cooldown.Until
		}
	}
	baseUsable := !a.disabled && !accountCooling && a.credential.usable(now)
	info.Usable = baseUsable && len(info.Models) > 0 && !allModelsCooling
	switch {
	case a.disabled:
		info.Status = "disabled"
	case accountCooling:
		info.Status = "cooling_down"
	case !a.credential.usable(now):
		info.Status = "needs_refresh"
	case len(info.Models) == 0:
		info.Status = "pending_models"
	case allModelsCooling:
		info.Status = "cooling_down"
		cooldown := earliestModelReady.UTC()
		info.CooldownUntil = &cooldown
	default:
		info.Status = "ready"
	}
	return info, validUntil
}

// ImportCredential validates and atomically creates or replaces a credential.
// Account identity, rather than a caller-supplied filename, determines the
// destination so remote uploads cannot escape the configured directory.
func (p *Pool) ImportCredential(ctx context.Context, raw []byte) (CredentialInfo, bool, error) {
	results, err := p.ImportCredentials(ctx, raw)
	if err != nil {
		return CredentialInfo{}, false, err
	}
	if len(results) == 0 {
		return CredentialInfo{}, false, ErrInvalidCredential
	}
	return results[0].Credential, results[0].Created, nil
}

type CredentialImportResult struct {
	Credential CredentialInfo `json:"credential"`
	Created    bool           `json:"created"`
}

// ImportCredentials imports every logical scope from one physical auth.json.
// The legacy ImportCredential API returns the first entry for old callers.
func (p *Pool) ImportCredentials(ctx context.Context, raw []byte) ([]CredentialImportResult, error) {
	parsed, err := parseCredentials(raw, "", p.cfg.Surface)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(parsed))
	seenIDs := make(map[string]struct{}, len(parsed))
	uploaded := make(map[string]*credential, len(parsed))
	for _, credential := range parsed {
		id := credential.accountID()
		if _, duplicate := seenIDs[id]; duplicate {
			continue
		}
		seenIDs[id] = struct{}{}
		ids = append(ids, id)
		uploaded[id] = credential
	}
	sort.Strings(ids)
	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()

	locked := make([]*account, 0, len(ids))
	created := make(map[string]bool, len(ids))
	paths := make(map[string]string, len(ids))
	for _, id := range ids {
		p.mu.RLock()
		existing := p.accounts[id]
		p.mu.RUnlock()
		created[id] = existing == nil
		paths[id] = filepath.Join(p.cfg.Dir, id+".json")
		if existing == nil {
			continue
		}
		if err := existing.acquireRefresh(ctx); err != nil {
			for _, item := range locked {
				item.releaseRefresh()
			}
			return nil, err
		}
		locked = append(locked, existing)
		existing.mu.RLock()
		if existing.credential != nil && existing.credential.Path != "" {
			paths[id] = existing.credential.Path
		}
		existing.mu.RUnlock()
	}
	defer func() {
		for _, item := range locked {
			item.releaseRefresh()
		}
	}()

	// Existing logical credentials may live in different physical auth files.
	// Merge each uploaded scope into its own current file so an update cannot
	// overwrite an unmentioned sibling scope or leave a stale copy selected by
	// the directory scanner. New logical credentials get independent,
	// account-derived filenames.
	byPath := make(map[string][]string, len(ids))
	for _, id := range ids {
		byPath[paths[id]] = append(byPath[paths[id]], id)
	}
	orderedPaths := make([]string, 0, len(byPath))
	for path := range byPath {
		orderedPaths = append(orderedPaths, path)
		sort.Strings(byPath[path])
	}
	sort.Strings(orderedPaths)
	for _, path := range orderedPaths {
		fileLock, lockErr := acquireAuthFileLock(ctx, path)
		if lockErr != nil {
			return nil, lockErr
		}
		root := make(map[string]any)
		var current []*credential
		body, readErr := os.ReadFile(path)
		switch {
		case readErr == nil:
			if json.Unmarshal(body, &root) != nil || root == nil {
				fileLock.Close()
				return nil, ErrInvalidCredentialJSON
			}
			current, _ = parseCredentials(body, path, p.cfg.Surface)
		case os.IsNotExist(readErr):
			// A new per-account file starts as an empty merge target.
		default:
			fileLock.Close()
			return nil, readErr
		}
		for _, id := range byPath[path] {
			var existing *credential
			for _, candidate := range current {
				if candidate.accountID() == id {
					existing = candidate
					break
				}
			}
			if err := mergeImportedCredential(root, uploaded[id], existing); err != nil {
				fileLock.Close()
				return nil, err
			}
		}
		if err := writeCredentialAtomicMode(path, root, 0o600); err != nil {
			fileLock.Close()
			return nil, err
		}
		if err := fileLock.Close(); err != nil {
			return nil, err
		}
		// Do not let a same-size, same-timestamp replacement reuse cached content.
		p.mu.Lock()
		delete(p.files, path)
		p.mu.Unlock()
	}
	// An explicit administrator import is the only in-process operation allowed
	// to cancel a previous failed-delete tombstone. The normal scan path keeps
	// deleting accounts fail-closed so a concurrent refresh cannot revive one.
	p.mu.RLock()
	for _, id := range ids {
		if existing := p.accounts[id]; existing != nil {
			existing.mu.Lock()
			existing.deleting = false
			existing.mu.Unlock()
		}
	}
	p.mu.RUnlock()
	stateChanged, err := p.scanUnlocked()
	if err != nil {
		return nil, err
	}
	if stateChanged {
		if err := p.persistState(); err != nil {
			return nil, err
		}
	}
	results := make([]CredentialImportResult, 0, len(ids))
	for _, id := range ids {
		info, ok := p.Credential(id)
		if !ok {
			return nil, errors.New("credential was saved but not loaded")
		}
		results = append(results, CredentialImportResult{Credential: info, Created: created[id]})
	}
	return results, nil
}

// mergeImportedCredential updates one logical credential without replacing
// unrelated nodes in the physical auth file. Existing files retain their
// direct-vs-tokens wrapper layout and original scope spelling.
func mergeImportedCredential(root map[string]any, imported, existing *credential) error {
	if imported == nil {
		return ErrInvalidCredential
	}
	node := credentialNodeFor(imported.Raw, imported)
	if node == nil {
		return ErrInvalidCredential
	}
	node = cloneMap(node)
	if !imported.Scoped {
		replacement := cloneMap(imported.Raw)
		for key := range root {
			delete(root, key)
		}
		for key, value := range replacement {
			root[key] = value
		}
		return nil
	}

	wrapper := imported.TokensWrapper
	scopeKey := imported.ScopeKey
	if existing != nil && existing.Scoped {
		wrapper = existing.TokensWrapper
		scopeKey = existing.ScopeKey
	} else {
		// When adding another scope to an existing scoped file, use the
		// container already parsed by the CLI. Mixing direct scopes with a
		// tokens wrapper would make one set invisible to credentialCandidates.
		for _, candidate := range credentialCandidates(root) {
			if candidate.scoped {
				wrapper = candidate.tokensWrapper
				break
			}
		}
	}
	if strings.TrimSpace(scopeKey) == "" {
		return ErrInvalidCredential
	}
	container := root
	if wrapper {
		var ok bool
		container, ok = root["tokens"].(map[string]any)
		if !ok {
			container = make(map[string]any)
			root["tokens"] = container
		}
	}
	// Avoid introducing a second spelling of the same normalized scope.
	for key := range container {
		if normalizeScope(key) == imported.Scope {
			scopeKey = key
			break
		}
	}
	container[scopeKey] = node
	return nil
}

// DeleteCredential removes every valid file for an account. The account is
// marked as deleting before any cross-process file-lock wait, making the
// operation fail-closed for new leases and concurrent refreshes. Existing
// leases retain their snapshots.
func (p *Pool) DeleteCredential(ctx context.Context, id string) error {
	p.mutationMu.Lock()
	defer p.mutationMu.Unlock()
	p.mu.RLock()
	a := p.accounts[id]
	p.mu.RUnlock()
	if a == nil {
		return ErrCredentialNotFound
	}
	p.markAccountDeleting(a, id)
	if err := a.acquireRefresh(ctx); err != nil {
		return err
	}
	defer a.releaseRefresh()
	// A refresh which already held refreshLock when deletion began may have
	// completed immediately before this acquisition. Reassert the terminal
	// state while holding the same lock used by refreshCredential.
	p.markAccountDeleting(a, id)

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
		credentials, loadErr := loadCredentials(path, p.cfg.Surface)
		if loadErr != nil {
			continue
		}
		matches := make([]*credential, 0, 1)
		for _, credential := range credentials {
			if credential.accountID() == id {
				matches = append(matches, credential)
			}
		}
		if len(matches) == 0 {
			continue
		}
		lock, lockErr := acquireAuthFileLock(ctx, path)
		if lockErr != nil {
			return lockErr
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			lock.Close()
			return readErr
		}
		// The file may have changed while this process waited for auth.json.lock.
		// Re-parse the locked snapshot and only remove logical credentials which
		// still resolve to the requested account ID. In particular, do not delete
		// a scope which another process replaced with a different principal or
		// authentication mode after the optimistic scan above.
		current, parseErr := parseCredentials(body, path, p.cfg.Surface)
		if parseErr != nil {
			lock.Close()
			continue
		}
		lockedMatches := make([]*credential, 0, len(matches))
		for _, credential := range current {
			if credential.accountID() == id {
				lockedMatches = append(lockedMatches, credential)
			}
		}
		if len(lockedMatches) == 0 {
			lock.Close()
			continue
		}
		var root map[string]any
		if json.Unmarshal(body, &root) != nil {
			lock.Close()
			return ErrInvalidCredentialJSON
		}
		for _, credential := range lockedMatches {
			removeLogicalCredential(root, credential)
		}
		if len(credentialCandidates(root)) == 0 {
			readErr = os.Remove(path)
		} else {
			readErr = writeCredentialAtomicMode(path, root, 0o600)
		}
		lock.Close()
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		p.mu.Lock()
		delete(p.files, path)
		p.mu.Unlock()
		removed = true
	}
	if !removed {
		return ErrCredentialNotFound
	}
	p.mu.Lock()
	delete(p.states, id)
	delete(p.catalogs, id)
	p.mu.Unlock()
	if _, err := p.scanUnlocked(); err != nil {
		return err
	}
	return p.persistState()
}

func (p *Pool) markAccountDeleting(a *account, id string) {
	if a == nil || id == "" {
		return
	}
	a.mu.Lock()
	fingerprint := credentialFingerprint(a.credential)
	a.deleting = true
	a.disabled = true
	a.disableReason = "removed"
	a.generation.Add(1)
	a.mu.Unlock()
	p.mu.Lock()
	state := p.states[id]
	state.Disabled = true
	state.Reason = "removed"
	state.CredentialFingerprint = fingerprint
	p.states[id] = state
	p.mu.Unlock()
	p.invalidateCredentialSnapshot()
	p.rebuildActive()
	p.notifyCapacity()
	if err := p.persistState(); err != nil {
		slog.Error("credential deleting state persistence failed", "account", id, "error", err)
	}
}

func removeLogicalCredential(root map[string]any, credential *credential) {
	if credential.Scoped {
		container := root
		if credential.TokensWrapper {
			container, _ = root["tokens"].(map[string]any)
		}
		if container == nil {
			return
		}
		if _, exists := container[credential.ScopeKey]; exists {
			delete(container, credential.ScopeKey)
			return
		}
		for scope := range container {
			if normalizeScope(scope) == credential.Scope {
				delete(container, scope)
				return
			}
		}
		return
	}
	if credential.TokensWrapper {
		delete(root, "tokens")
		return
	}
	for key := range root {
		delete(root, key)
	}
}

func (p *Pool) Models() []string {
	seen := map[string]struct{}{}
	p.mu.RLock()
	for _, a := range p.accounts {
		a.mu.RLock()
		if !a.disabled && a.credential != nil {
			for _, model := range a.modelIDsLocked() {
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

// ModelFirstSeen returns the earliest persisted discovery time for every
// currently exposed model. Provisional string catalogs deliberately remain
// separate from structured descriptors, but still need stable OpenAI model
// created timestamps before the first structured refresh.
func (p *Pool) ModelFirstSeen() map[string]int64 {
	result := make(map[string]int64)
	p.mu.RLock()
	for id, a := range p.accounts {
		catalog := p.catalogs[id]
		a.mu.RLock()
		if !a.disabled && a.credential != nil {
			for _, model := range a.modelIDsLocked() {
				created := catalog.FirstSeen[model]
				if created <= 0 {
					created = a.descriptors[model].Created
				}
				if created > 0 && (result[model] == 0 || created < result[model]) {
					result[model] = created
				}
			}
		}
		a.mu.RUnlock()
	}
	p.mu.RUnlock()
	return result
}

func (p *Pool) HasModel(model string) bool {
	if model == "" {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		a.mu.RLock()
		available := !a.disabled && a.credential != nil && a.hasModelLocked(model)
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
		known := a.credential != nil && a.hasModelLocked(model)
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
		updatedAt := a.catalogUpdated
		if updatedAt.IsZero() && a.credential != nil {
			updatedAt = a.credential.ModelsUpdatedAt
		}
		needs := !a.disabled && a.credential != nil && (len(a.modelIDsLocked()) == 0 || updatedAt.IsZero() || now.Sub(updatedAt) >= interval)
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
		a.mu.RLock()
		billingEligible := !a.disabled && a.credential != nil && a.credential.Tier.Billing
		due := billingEligible && (a.billing == nil || interval <= 0 || now.Sub(a.billing.UpdatedAt) >= interval)
		a.mu.RUnlock()
		if due {
			ids = append(ids, id)
		}
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	return ids
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
	wasPaid := a.billing != nil && a.billing.Paid
	a.billing = cloneBillingInfo(&info)
	a.mu.Unlock()
	p.invalidateCredentialSnapshot()
	if wasPaid != info.Paid {
		p.requestRebuild()
	}
	return nil
}

// Billing returns an independent snapshot of one account's billing metadata.
func (p *Pool) Billing(accountID string) (BillingInfo, bool) {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return BillingInfo{}, false
	}
	a.mu.RLock()
	billing := cloneBillingInfo(a.billing)
	a.mu.RUnlock()
	if billing == nil {
		return BillingInfo{}, false
	}
	return *billing, true
}
func (p *Pool) UpdateBuildRouting(accountID string, update BuildRoutingUpdate) (BuildRoutingInfo, error) {
	if update.RouteMode != nil {
		mode, err := ParseBuildRouteMode(string(*update.RouteMode))
		if err != nil {
			return BuildRoutingInfo{}, err
		}
		update.RouteMode = &mode
	}
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return BuildRoutingInfo{}, ErrCredentialNotFound
	}
	a.mu.Lock()
	changed := false
	if update.RouteMode != nil && a.buildRouteMode != *update.RouteMode {
		a.buildRouteMode = *update.RouteMode
		changed = true
	}
	if update.SuperEntitledOverride != nil && a.buildSuperEntitled != *update.SuperEntitledOverride {
		a.buildSuperEntitled = *update.SuperEntitledOverride
		changed = true
	}
	if update.APIFallback != nil && a.buildAPIFallback != *update.APIFallback {
		a.buildAPIFallback = *update.APIFallback
		changed = true
	}
	routing := buildRoutingInfoWithBilling(a.credential, a.billing, a.buildRouteMode, a.buildSuperEntitled, a.buildAPIFallback)
	mode, entitled, fallback := a.buildRouteMode, a.buildSuperEntitled, a.buildAPIFallback
	a.mu.Unlock()
	if !changed {
		return routing, nil
	}
	p.mu.Lock()
	state := p.states[accountID]
	state.BuildRouteMode = mode
	state.BuildSuperEntitled = entitled
	state.BuildAPIFallback = fallback
	if state.hasPersistentAccountState(time.Now()) {
		p.states[accountID] = state
	} else {
		delete(p.states, accountID)
	}
	p.mu.Unlock()
	p.invalidateCredentialSnapshot()
	if err := p.persistState(); err != nil {
		return BuildRoutingInfo{}, err
	}
	return routing, nil
}

func (p *Pool) MarkBuildAPIFallback(accountID string) bool {
	fallback := true
	_, err := p.UpdateBuildRouting(accountID, BuildRoutingUpdate{APIFallback: &fallback})
	return err == nil
}

func (p *Pool) UpdateModels(accountID string, models []string, updatedAt time.Time) error {
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	updatedAt = updatedAt.UTC()
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
	next.ModelsUpdatedAt = updatedAt
	// Preserve compatibility for files that already used the project's legacy
	// models extension, but never introduce it into a clean CLI auth.json.
	node := credentialNodeFor(next.Raw, cred)
	_, hadLegacyModels := node["models"]
	if hadLegacyModels {
		node["models"] = normalized
		node["models_updated_at"] = next.ModelsUpdatedAt.Format(time.RFC3339Nano)
		if err := writeCredentialAtomic(next.Path, next.Raw); err != nil {
			return err
		}
	}
	a.mu.Lock()
	a.credential = &next
	a.catalogUpdated = updatedAt.UTC()
	a.mu.Unlock()
	p.mu.Lock()
	catalog := p.catalogs[accountID]
	catalog.Provisional = normalized
	catalog.UpdatedAt = updatedAt
	seedModelFirstSeen(&catalog, normalized, updatedAt)
	p.catalogs[accountID] = catalog
	if hadLegacyModels {
		p.replaceCachedCredentialLocked(accountID, &next)
	}
	p.mu.Unlock()
	p.requestRebuild()
	p.requestStatePersist()
	return nil
}

// UpdateModelDescriptors atomically publishes a structured account catalog in
// state v2 and queues one coalesced state checkpoint. It never writes model
// metadata, endpoints, or keys into auth.json.
func (p *Pool) UpdateModelDescriptors(accountID string, descriptors []modelcatalog.ModelDescriptor, etag string, updatedAt time.Time) error {
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	p.mu.RLock()
	a := p.accounts[accountID]
	previous := p.catalogs[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	models := make(map[string]modelcatalog.ModelDescriptor, len(descriptors))
	firstSeen := make(map[string]int64, len(previous.FirstSeen)+len(descriptors))
	for model, created := range previous.FirstSeen {
		if created > 0 {
			firstSeen[model] = created
		}
	}
	for _, descriptor := range descriptors {
		descriptor = descriptor.Normalize()
		if descriptor.ID == "" {
			continue
		}
		if created := firstSeen[descriptor.ID]; created > 0 {
			descriptor.Created = created
		} else if old, exists := previous.Models[descriptor.ID]; exists && old.Created > 0 {
			descriptor.Created = old.Created
		}
		if descriptor.Created <= 0 {
			descriptor.Created = updatedAt.Unix()
		}
		firstSeen[descriptor.ID] = descriptor.Created
		models[descriptor.ID] = descriptor
	}
	if len(models) == 0 {
		return errors.New("upstream returned no valid model descriptors")
	}
	state := catalogState{
		ETag: strings.TrimSpace(etag), UpdatedAt: updatedAt.UTC(), Models: cloneDescriptors(models), FirstSeen: firstSeen,
	}
	a.mu.Lock()
	a.descriptors = cloneDescriptors(models)
	a.catalogETag = state.ETag
	a.catalogUpdated = state.UpdatedAt
	a.mu.Unlock()
	p.mu.Lock()
	p.catalogs[accountID] = state
	p.mu.Unlock()
	p.requestRebuild()
	p.requestStatePersist()
	return nil
}

func (p *Pool) TouchModelCatalog(accountID string, updatedAt time.Time) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	a.mu.Lock()
	a.catalogUpdated = updatedAt.UTC()
	a.mu.Unlock()
	p.mu.Lock()
	catalog := p.catalogs[accountID]
	catalog.UpdatedAt = updatedAt.UTC()
	p.catalogs[accountID] = catalog
	p.mu.Unlock()
	p.invalidateCredentialSnapshot()
	p.requestStatePersist()
	return nil
}

func (p *Pool) ModelCatalogETag(accountID string) string {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ""
	}
	a.mu.RLock()
	etag := a.catalogETag
	a.mu.RUnlock()
	return etag
}

// UpdateModelLimits applies authoritative inference response headers without
// waiting for a full catalog refresh.
func (p *Pool) UpdateModelLimits(accountID, model string, contextWindow uint64, maxCompletionTokens uint32) error {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return ErrNoAuth
	}
	a.mu.Lock()
	descriptor, ok := a.descriptors[model]
	changed := false
	if ok {
		if contextWindow > 0 && descriptor.ContextWindow != contextWindow {
			descriptor.ContextWindow = contextWindow
			changed = true
		}
		if maxCompletionTokens > 0 && descriptor.MaxCompletionTokens != maxCompletionTokens {
			descriptor.MaxCompletionTokens = maxCompletionTokens
			changed = true
		}
		if changed {
			a.descriptors[model] = descriptor
		}
	}
	a.mu.Unlock()
	if !ok || !changed {
		return nil
	}
	p.mu.Lock()
	catalog := p.catalogs[accountID]
	if catalog.Models == nil {
		catalog.Models = make(map[string]modelcatalog.ModelDescriptor)
	}
	catalog.Models[model] = descriptor
	p.catalogs[accountID] = catalog
	p.mu.Unlock()
	p.requestStatePersist()
	return nil
}

// AccountDescriptor is a lock-safe, non-leasing lookup used to validate
// persisted hard-affinity bindings after restart.
func (p *Pool) AccountDescriptor(accountID, model string) (modelcatalog.ModelDescriptor, bool) {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return modelcatalog.ModelDescriptor{}, false
	}
	return a.descriptor(model)
}

func (p *Pool) AggregatedModels() []modelcatalog.AggregatedModel {
	p.mu.RLock()
	descriptors := make([]modelcatalog.ModelDescriptor, 0)
	for _, a := range p.accounts {
		a.mu.RLock()
		if !a.disabled {
			for _, descriptor := range a.descriptors {
				if a.credential != nil && a.credential.AuthMode == AuthModeAPIKey && !descriptor.SupportedInAPI {
					continue
				}
				descriptors = append(descriptors, descriptor)
			}
		}
		a.mu.RUnlock()
	}
	p.mu.RUnlock()
	return modelcatalog.Aggregate(descriptors)
}

// HasProvisionalModel reports whether an enabled account can route model only
// from the legacy temporary string catalog. Callers use this to mirror the
// same provisional backend choice that inference leasing will make before the
// first structured catalog refresh.
func (p *Pool) HasProvisionalModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.accounts {
		a.mu.RLock()
		provisional := !a.disabled && a.credential != nil && len(a.descriptors) == 0 && a.hasModelLocked(model)
		a.mu.RUnlock()
		if provisional {
			return true
		}
	}
	return false
}

func cloneDescriptors(source map[string]modelcatalog.ModelDescriptor) map[string]modelcatalog.ModelDescriptor {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]modelcatalog.ModelDescriptor, len(source))
	for id, descriptor := range source {
		descriptor.ReasoningEfforts = append([]string(nil), descriptor.ReasoningEfforts...)
		result[id] = descriptor
	}
	return result
}

func (p *Pool) replaceCachedCredentialLocked(accountID string, next *credential) {
	cached, ok := p.files[next.Path]
	if !ok {
		return
	}
	if info, err := os.Stat(next.Path); err == nil {
		cached.size, cached.modTime = info.Size(), info.ModTime()
	}
	if len(cached.creds) == 0 && cached.cred != nil {
		cached.creds = []*credential{cached.cred}
	}
	for index, item := range cached.creds {
		if item.accountID() == accountID {
			cached.creds[index] = next
		}
	}
	if len(cached.creds) > 0 {
		cached.cred = cached.creds[0]
	}
	p.files[next.Path] = cached
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
	p.markCooldown(accountID, reason, duration, true)
}

// MarkCooldownIfNoOtherReason installs a subsystem cooldown only when the
// account has no active cooldown from another source. This prevents metadata
// probes from replacing stronger upstream rate-limit or quota decisions.
func (p *Pool) MarkCooldownIfNoOtherReason(accountID, reason string, duration time.Duration) bool {
	return p.markCooldown(accountID, reason, duration, false)
}

func (p *Pool) markCooldown(accountID, reason string, duration time.Duration, replaceOther bool) bool {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return false
	}
	now := time.Now()
	until := now.Add(duration)
	a.mu.Lock()
	if !replaceOther && now.Before(a.cooldownUntil) && a.cooldownCause != "" && a.cooldownCause != reason {
		a.mu.Unlock()
		return false
	}
	remaining := time.Until(a.cooldownUntil)
	extensionThreshold := remaining / 10
	if extensionThreshold < 5*time.Second {
		extensionThreshold = 5 * time.Second
	}
	changed := !now.Before(a.cooldownUntil) || until.After(a.cooldownUntil.Add(extensionThreshold))
	if changed {
		a.cooldownUntil, a.cooldownCause = until, reason
	} else {
		until = a.cooldownUntil
		reason = a.cooldownCause
	}
	a.mu.Unlock()
	if !changed {
		return false
	}
	p.mu.Lock()
	state := p.states[accountID]
	state.CooldownUntil, state.Reason = until, reason
	p.states[accountID] = state
	p.mu.Unlock()
	p.requestRebuild()
	p.rebuildWhenCooldownExpires(until)
	p.requestStatePersist()
	slog.Warn("credential account cooling", "account", accountID, "reason", reason, "until", until.UTC().Format(time.RFC3339))
	return true
}

// ClearCooldownReason clears only a cooldown created by the named subsystem.
// It cannot erase rate-limit, auth, model, or upstream quota cooldowns.
func (p *Pool) ClearCooldownReason(accountID, reason string) bool {
	p.mu.RLock()
	a := p.accounts[accountID]
	p.mu.RUnlock()
	if a == nil {
		return false
	}
	a.mu.Lock()
	if a.cooldownCause != reason {
		a.mu.Unlock()
		return false
	}
	a.cooldownUntil = time.Time{}
	a.cooldownCause = ""
	a.mu.Unlock()

	p.mu.Lock()
	state := p.states[accountID]
	if state.Reason == reason {
		state.CooldownUntil = time.Time{}
		state.Reason = ""
		if state.hasPersistentAccountState(time.Now()) {
			p.states[accountID] = state
		} else {
			delete(p.states, accountID)
		}
	}
	p.mu.Unlock()
	p.requestRebuild()
	p.notifyCapacity()
	p.requestStatePersist()
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
	p.requestStatePersist()
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
			p.requestStatePersist()
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
			p.MarkCooldown(a.id, "refresh_backoff", time.Minute)
		}
		return err
	}
	p.mu.RLock()
	catalog := p.catalogs[a.id]
	p.mu.RUnlock()
	next = credentialWithProvisionalModels(next, catalog)
	a.mu.Lock()
	if a.deleting {
		a.mu.Unlock()
		return ErrNoAuth
	}
	a.credential = next
	a.generation.Add(1)
	a.disabled = false
	a.disableReason = ""
	now := time.Now()
	keepCooldown := now.Before(a.cooldownUntil) && a.cooldownCause != "" && a.cooldownCause != "refresh_backoff"
	cooldownReason := a.cooldownCause
	if !keepCooldown {
		a.cooldownUntil = time.Time{}
		a.cooldownCause = ""
	}
	a.mu.Unlock()
	p.mu.Lock()
	state := p.states[a.id]
	criticalStateChanged := state.Disabled || state.CredentialFingerprint != ""
	stateChanged := criticalStateChanged
	state.Disabled = false
	state.CredentialFingerprint = ""
	if !keepCooldown {
		stateChanged = stateChanged || !state.CooldownUntil.IsZero() || state.Reason != ""
		state.CooldownUntil = time.Time{}
		state.Reason = ""
	} else {
		stateChanged = stateChanged || state.Reason != cooldownReason
		state.Reason = cooldownReason
	}
	if state.hasPersistentAccountState(now) {
		p.states[a.id] = state
	} else {
		delete(p.states, a.id)
	}
	if cached, ok := p.files[next.Path]; ok {
		if info, statErr := os.Stat(next.Path); statErr == nil {
			cached.modTime, cached.size = info.ModTime(), info.Size()
			if len(cached.creds) == 0 && cached.cred != nil {
				cached.creds = []*credential{cached.cred}
			}
			for index, item := range cached.creds {
				if item.accountID() == a.id {
					cached.creds[index] = next
				}
			}
			if len(cached.creds) > 0 {
				cached.cred = cached.creds[0]
			}
			p.files[next.Path] = cached
		}
	}
	p.mu.Unlock()
	if stateChanged {
		if criticalStateChanged {
			if err := p.persistState(); err != nil {
				slog.Error("credential scheduler state persistence failed", "error", err)
			}
		} else {
			p.requestStatePersist()
		}
	}
	p.requestRebuild()
	p.notifyCapacity()
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

func (p *Pool) requestStatePersist() {
	p.stateGeneration.Add(1)
	select {
	case p.stateWake <- struct{}{}:
	default:
	}
}

func (p *Pool) stateCheckpointLoop() {
	defer p.wg.Done()
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-p.stateWake:
			if timer == nil {
				delay := p.statePersistDelay
				if delay <= 0 {
					delay = time.Second
				}
				timer = time.NewTimer(delay)
				timerC = timer.C
			}
		case <-timerC:
			err := p.FlushState()
			timer = nil
			timerC = nil
			if err != nil {
				slog.Error("credential state checkpoint failed", "error", err)
				p.requestStatePersist()
			}
		case <-p.closed:
			if timer != nil {
				timer.Stop()
			}
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
	stateChanged, err := p.scanUnlocked()
	p.mutationMu.Unlock()
	if err != nil || !stateChanged {
		return err
	}
	p.requestStatePersist()
	return nil
}

func (p *Pool) scanUnlocked() (bool, error) {
	entries, err := os.ReadDir(p.cfg.Dir)
	if err != nil {
		return false, fmt.Errorf("read auths directory: %w", err)
	}
	discoveredAt := time.Now().UTC()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	seen := map[string]struct{}{}
	parsed := map[string]*credential{}
	newFiles := map[string]fileEntry{}
	scanComplete := true
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.EqualFold(filepath.Ext(name), ".json") {
			continue
		}
		path := filepath.Join(p.cfg.Dir, name)
		info, err := entry.Info()
		if err != nil {
			scanComplete = false
			continue
		}
		p.mu.RLock()
		cached, ok := p.files[path]
		p.mu.RUnlock()
		var credentials []*credential
		if ok && cached.size == info.Size() && cached.modTime.Equal(info.ModTime()) {
			credentials = cached.creds
			if len(credentials) == 0 && cached.cred != nil {
				credentials = []*credential{cached.cred}
			}
		} else {
			credentials, err = loadCredentials(path, p.cfg.Surface)
			if err != nil {
				scanComplete = false
				slog.Warn("credential file skipped", "reason", "invalid_format")
				continue
			}
		}
		for _, cred := range credentials {
			id := cred.accountID()
			if _, duplicate := seen[id]; duplicate {
				slog.Warn("duplicate credential skipped", "account", id)
				continue
			}
			seen[id] = struct{}{}
			parsed[id] = cred
		}
		var first *credential
		if len(credentials) > 0 {
			first = credentials[0]
		}
		newFiles[path] = fileEntry{size: info.Size(), modTime: info.ModTime(), cred: first, creds: credentials}
	}
	if len(parsed) == 0 {
		if !scanComplete {
			slog.Warn("credential scan incomplete", "reason", "unreadable_file")
			return false, nil
		}
		p.mu.Lock()
		poolChanged := len(p.accounts) > 0 || len(p.files) > 0
		stateChanged := len(p.states) > 0 || len(p.catalogs) > 0
		hadAccounts := len(p.accounts) > 0
		for _, a := range p.accounts {
			a.mu.Lock()
			a.disabled, a.disableReason = true, "removed"
			a.mu.Unlock()
		}
		p.accounts = map[string]*account{}
		p.files = map[string]fileEntry{}
		p.states = map[string]accountState{}
		p.catalogs = map[string]catalogState{}
		p.mu.Unlock()
		if poolChanged || stateChanged {
			p.invalidateCredentialSnapshot()
		}
		p.rebuildActive()
		if hadAccounts {
			slog.Warn("credential pool is empty")
			return stateChanged, nil
		}
		if p.cfg.AllowEmpty {
			return stateChanged, nil
		}
		return false, ErrNoAuth
	}
	p.mu.Lock()
	poolChanged := len(p.files) != len(newFiles) || len(p.accounts) != len(parsed)
	stateChanged := false
	if !poolChanged {
		for path, next := range newFiles {
			current, ok := p.files[path]
			if !ok || current.size != next.size || !current.modTime.Equal(next.modTime) {
				poolChanged = true
				break
			}
		}
	}
	var cooldowns []time.Time
	for id, cred := range parsed {
		catalog := p.catalogs[id]
		if len(catalog.Models) == 0 {
			catalogChanged := false
			node := credentialNodeFor(cred.Raw, cred)
			_, hasLegacyModels := node["models"]
			if hasLegacyModels {
				normalized := stringSlice(cred.Models)
				if !equalStrings(catalog.Provisional, normalized) {
					catalog.Provisional = normalized
					catalogChanged = true
				}
			}
			firstSeenAt := discoveredAt
			if !catalog.UpdatedAt.IsZero() {
				firstSeenAt = catalog.UpdatedAt
			}
			if seedModelFirstSeen(&catalog, catalog.Provisional, firstSeenAt) {
				catalogChanged = true
			}
			if catalogChanged {
				p.catalogs[id] = catalog
				stateChanged = true
			}
		}
		cred = credentialWithProvisionalModels(cred, catalog)
		if existing := p.accounts[id]; existing != nil {
			existing.mu.Lock()
			credentialChanged := existing.credential == nil ||
				existing.credential.Path != cred.Path || existing.credential.Scope != cred.Scope ||
				existing.credential.AccessToken != cred.AccessToken || existing.credential.RefreshToken != cred.RefreshToken ||
				existing.credential.AuthMode != cred.AuthMode || existing.credential.Subject != cred.Subject ||
				existing.credential.PrincipalType != cred.PrincipalType || existing.credential.PrincipalID != cred.PrincipalID ||
				existing.credential.ClientID != cred.ClientID || existing.credential.TokenURL != cred.TokenURL ||
				existing.credential.OIDCIssuer != cred.OIDCIssuer || !existing.credential.ExpiresAt.Equal(cred.ExpiresAt)
			existing.credential = cred
			if catalog, ok := p.catalogs[id]; ok {
				existing.descriptors = cloneDescriptors(catalog.Models)
				existing.catalogETag = catalog.ETag
				existing.catalogUpdated = catalog.UpdatedAt
			}
			if credentialChanged {
				existing.generation.Add(1)
				poolChanged = true
				if !existing.deleting {
					existing.disabled = false
					existing.disableReason = ""
					existing.cooldownUntil = time.Time{}
					existing.cooldownCause = ""
					existing.billing = nil
					state := p.states[id]
					if state.Disabled || state.CredentialFingerprint != "" || !state.CooldownUntil.IsZero() || state.Reason != "" {
						stateChanged = true
					}
					state.Disabled = false
					state.CredentialFingerprint = ""
					state.CooldownUntil = time.Time{}
					state.Reason = ""
					if state.hasPersistentAccountState(time.Now()) {
						p.states[id] = state
					} else {
						delete(p.states, id)
					}
				}
			}
			existing.mu.Unlock()
			continue
		}
		a := &account{
			id: id, credential: cred, agentID: p.globalAgentID, sessionID: randomUUID(),
			descriptors: cloneDescriptors(catalog.Models), catalogETag: catalog.ETag, catalogUpdated: catalog.UpdatedAt,
		}
		a.generation.Store(1)
		if state, ok := p.states[id]; ok {
			a.buildRouteMode = normalizeBuildRouteMode(state.BuildRouteMode)
			a.buildSuperEntitled = state.BuildSuperEntitled
			a.buildAPIFallback = state.BuildAPIFallback
			if state.Disabled {
				fingerprintChanged := state.CredentialFingerprint != "" && state.CredentialFingerprint != credentialFingerprint(cred)
				if fingerprintChanged {
					stateChanged = true
					state.Disabled = false
					state.CredentialFingerprint = ""
					state.CooldownUntil = time.Time{}
					state.Reason = ""
					if state.hasPersistentAccountState(time.Now()) {
						p.states[id] = state
					} else {
						delete(p.states, id)
					}
				} else {
					a.disabled, a.disableReason = true, state.Reason
				}
			} else if time.Now().Before(state.CooldownUntil) {
				a.cooldownUntil, a.cooldownCause = state.CooldownUntil, state.Reason
				cooldowns = append(cooldowns, state.CooldownUntil)
			}
			for model, cooldown := range state.ModelCooldowns {
				if time.Now().Before(cooldown.Until) {
					if a.modelCooldowns == nil {
						a.modelCooldowns = make(map[string]cooldownState)
					}
					a.modelCooldowns[model] = cooldown
					cooldowns = append(cooldowns, cooldown.Until)
				}
			}
		}
		p.accounts[id] = a
		poolChanged = true
	}
	for id, a := range p.accounts {
		if _, ok := parsed[id]; !ok {
			if !scanComplete {
				continue
			}
			a.mu.Lock()
			a.disabled, a.disableReason = true, "removed"
			a.mu.Unlock()
			delete(p.accounts, id)
			poolChanged = true
		}
	}
	if scanComplete {
		for id := range p.states {
			if _, ok := parsed[id]; !ok {
				delete(p.states, id)
				stateChanged = true
			}
		}
		for id := range p.catalogs {
			if _, ok := parsed[id]; !ok {
				delete(p.catalogs, id)
				stateChanged = true
			}
		}
	}
	p.files = newFiles
	count := len(p.accounts)
	p.mu.Unlock()
	if poolChanged || stateChanged {
		p.invalidateCredentialSnapshot()
	}
	for _, until := range cooldowns {
		p.rebuildWhenCooldownExpires(until)
	}
	if poolChanged {
		p.rebuildActive()
		p.notifyCapacity()
		slog.Info("credential pool loaded", "accounts", count)
	}
	return stateChanged, nil
}

func seedModelFirstSeen(catalog *catalogState, models []string, discoveredAt time.Time) bool {
	if catalog == nil || len(models) == 0 {
		return false
	}
	created := discoveredAt.Unix()
	if created <= 0 {
		created = time.Now().Unix()
	}
	changed := false
	if catalog.FirstSeen == nil {
		catalog.FirstSeen = make(map[string]int64, len(models))
	}
	for _, model := range models {
		if catalog.FirstSeen[model] <= 0 {
			catalog.FirstSeen[model] = created
			changed = true
		}
	}
	return changed
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func credentialWithProvisionalModels(credential *credential, catalog catalogState) *credential {
	if credential == nil || len(catalog.Models) > 0 || len(credential.Models) > 0 || len(catalog.Provisional) == 0 {
		return credential
	}
	next := *credential
	next.Models = append([]string(nil), catalog.Provisional...)
	next.ModelsUpdatedAt = catalog.UpdatedAt
	return &next
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

func cloneCredentialInfo(source CredentialInfo) CredentialInfo {
	cloned := source
	cloned.Models = append([]string(nil), source.Models...)
	if len(source.ModelCooldowns) > 0 {
		cloned.ModelCooldowns = make(map[string]ModelCooldownInfo, len(source.ModelCooldowns))
		for model, cooldown := range source.ModelCooldowns {
			cloned.ModelCooldowns[model] = cooldown
		}
	}
	cloned.Billing = cloneBillingInfo(source.Billing)
	if source.ExpiresAt != nil {
		value := *source.ExpiresAt
		cloned.ExpiresAt = &value
	}
	if source.CooldownUntil != nil {
		value := *source.CooldownUntil
		cloned.CooldownUntil = &value
	}
	if source.CatalogUpdated != nil {
		value := *source.CatalogUpdated
		cloned.CatalogUpdated = &value
	}
	return cloned
}

func (p *Pool) invalidateCredentialSnapshot() {
	p.credentialSnapshotGeneration.Add(1)
}

func (p *Pool) rebuildActive() {
	now := time.Now()
	p.mu.RLock()
	active := make([]*account, 0, len(p.accounts))
	byModel := map[string][]*account{}
	for _, a := range p.accounts {
		a.mu.RLock()
		available := !a.disabled && !now.Before(a.cooldownUntil) && a.credential != nil
		var models []string
		if available {
			paid := a.credential.Tier.Paid || a.billing != nil && a.billing.Paid
			a.paidTier.Store(paid)
			for _, model := range a.modelIDsLocked() {
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
	p.invalidateCredentialSnapshot()
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
		until, disabled := a.cooldownUntil, a.disabled
		if !now.Before(until) {
			until = time.Time{}
		}
		if model != "" && !disabled && a.credential != nil {
			if a.hasModelLocked(model) {
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
	if state.Version < 0 || state.Version > 2 {
		return fmt.Errorf("unsupported credential state version %d", state.Version)
	}
	if state.Version == 2 {
		p.globalAgentID = strings.TrimSpace(state.GlobalAgentID)
		p.namespaceKey = strings.TrimSpace(state.NamespaceKey)
	}
	now := time.Now()
	for id, item := range state.Accounts {
		activeModels := make(map[string]cooldownState)
		for model, cooldown := range item.ModelCooldowns {
			if now.Before(cooldown.Until) {
				activeModels[model] = cooldown
			}
		}
		item.ModelCooldowns = activeModels
		if item.hasPersistentAccountState(now) {
			p.states[id] = item
		}
	}
	if state.Version < 2 {
		return nil
	}
	for id, catalog := range state.Catalogs {
		if catalog.FirstSeen == nil {
			catalog.FirstSeen = make(map[string]int64)
		}
		models := make(map[string]modelcatalog.ModelDescriptor, len(catalog.Models))
		for model, descriptor := range catalog.Models {
			descriptor = descriptor.Normalize()
			if descriptor.ID == "" {
				descriptor.ID = model
			}
			if descriptor.WireModel == "" {
				descriptor.WireModel = descriptor.ID
			}
			if descriptor.Created <= 0 {
				descriptor.Created = catalog.FirstSeen[model]
			}
			if descriptor.Created <= 0 && !catalog.UpdatedAt.IsZero() {
				descriptor.Created = catalog.UpdatedAt.Unix()
			}
			if descriptor.Created > 0 && catalog.FirstSeen[descriptor.ID] <= 0 {
				catalog.FirstSeen[descriptor.ID] = descriptor.Created
			}
			models[descriptor.ID] = descriptor
		}
		catalog.Models = models
		catalog.Provisional = stringSlice(catalog.Provisional)
		for model, created := range catalog.FirstSeen {
			if created <= 0 {
				delete(catalog.FirstSeen, model)
			}
		}
		p.catalogs[id] = catalog
	}
	return nil
}

// FlushState synchronously persists all account and catalog state queued so far.
func (p *Pool) FlushState() error {
	return p.persistStateThrough(p.stateGeneration.Load())
}

func (p *Pool) persistState() error {
	return p.persistStateThrough(p.stateGeneration.Add(1))
}

func (p *Pool) persistStateThrough(target uint64) error {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.statePersistedGeneration.Load() >= target {
		return nil
	}
	p.mu.RLock()
	state := persistedState{
		Version: 2, GlobalAgentID: p.globalAgentID, NamespaceKey: p.namespaceKey,
		Accounts: map[string]accountState{}, Catalogs: map[string]catalogState{},
	}
	now := time.Now()
	for id, item := range p.states {
		if !now.Before(item.CooldownUntil) {
			item.CooldownUntil = time.Time{}
			if !item.Disabled {
				item.Reason = ""
			}
		}
		activeModels := make(map[string]cooldownState)
		for model, cooldown := range item.ModelCooldowns {
			if now.Before(cooldown.Until) {
				activeModels[model] = cooldown
			}
		}
		item.ModelCooldowns = activeModels
		if normalizeBuildRouteMode(item.BuildRouteMode) == BuildRouteAuto {
			item.BuildRouteMode = ""
		}
		if item.hasPersistentAccountState(now) {
			state.Accounts[id] = item
		}
	}
	for id, catalog := range p.catalogs {
		catalog.Models = cloneDescriptors(catalog.Models)
		catalog.Provisional = append([]string(nil), catalog.Provisional...)
		catalog.FirstSeen = cloneFirstSeen(catalog.FirstSeen)
		if len(catalog.Models) > 0 || len(catalog.Provisional) > 0 || len(catalog.FirstSeen) > 0 || catalog.ETag != "" || !catalog.UpdatedAt.IsZero() {
			state.Catalogs[id] = catalog
		}
	}
	snapshotGeneration := p.stateGeneration.Load()
	p.mu.RUnlock()
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	digest := sha256.Sum256(b)
	if p.stateDigestValid && digest == p.stateDigest {
		p.statePersistedGeneration.Store(snapshotGeneration)
		return nil
	}
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
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	p.stateDigest = digest
	p.stateDigestValid = true
	p.statePersistedGeneration.Store(snapshotGeneration)
	p.stateWriteCount.Add(1)
	return nil
}

func cloneFirstSeen(source map[string]int64) map[string]int64 {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]int64, len(source))
	for model, created := range source {
		result[model] = created
	}
	return result
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

func affinityModelKey(affinity Affinity, model string) string {
	if affinity.Key == "" {
		return ""
	}
	tenant := affinity.Tenant
	if tenant == "" {
		tenant = "public"
	}
	return tenant + "\x00" + modelAffinityKey(affinity.Key, model)
}

func validNamespaceKey(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validGlobalAgentID(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 16
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
