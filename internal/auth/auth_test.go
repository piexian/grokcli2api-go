package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOfficialSubscriptionTierMapping(t *testing.T) {
	tests := []struct {
		tier    int64
		key     string
		display string
		paid    bool
		billing bool
	}{
		{tier: 0, key: "free", display: "Free"},
		{tier: 1, key: "supergrok", display: "SuperGrok", paid: true, billing: true},
		{tier: 2, key: "x_basic", display: "X Basic", paid: true},
		{tier: 3, key: "x_premium", display: "X Premium", paid: true, billing: true},
		{tier: 4, key: "x_premium_plus", display: "X Premium+", paid: true, billing: true},
		{tier: 5, key: "supergrok_heavy", display: "SuperGrok Heavy", paid: true, billing: true},
		{tier: 6, key: "supergrok_lite", display: "SuperGrok Lite", paid: true, billing: true},
		{tier: 99, key: "99", display: "99", paid: true, billing: true},
	}
	for _, test := range tests {
		t.Run(fmt.Sprint(test.tier), func(t *testing.T) {
			tier := subscriptionTierFromClaims(jwtClaims(testJWTWithTier(test.tier)))
			if tier.Key != test.key || tier.Display != test.display || tier.Paid != test.paid || tier.Billing != test.billing {
				t.Fatalf("tier %d = %#v", test.tier, tier)
			}
		})
	}
	if tier := subscriptionTierFromClaims(nil); tier != (subscriptionTier{}) {
		t.Fatalf("missing tier claim = %#v", tier)
	}
}

func TestLoadFlatOAuthCredential(t *testing.T) {
	dir := t.TempDir()
	path := writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	cred, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	if cred.AccessToken != "token-a" || cred.Subject != "subject-a" || cred.ClientID != "client-id" {
		t.Fatalf("unexpected credential metadata: token=%t subject=%q client=%q", cred.AccessToken != "", cred.Subject, cred.ClientID)
	}
	if cred.session().Token != "token-a" || cred.session().UserID != "subject-a" {
		t.Fatalf("unexpected session: %#v", cred.session())
	}
}

func TestCredentialInfoUsesOfficialSubscriptionTierDisplay(t *testing.T) {
	raw, err := json.Marshal(map[string]any{
		"access_token": testJWTWithTier(4),
		"sub":          "subject-a",
		"models":       []string{"grok-4.5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cred, err := parseCredential(raw, "", "tui")
	if err != nil {
		t.Fatal(err)
	}
	a := &account{id: accountID(cred.Subject), credential: cred}
	info := credentialInfo(a.id, a, time.Now())
	if info.SubscriptionTier != "x_premium_plus" || info.SubscriptionTierDisplay != "X Premium+" {
		t.Fatalf("credential tier = %#v", info)
	}
	encoded, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"subscription_tier":"x_premium_plus"`) || !strings.Contains(string(encoded), `"subscription_tier_display":"X Premium+"`) {
		t.Fatalf("credential JSON = %s", encoded)
	}
}

func TestRefreshRotatesAndPersistsCredential(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("client_id") != "client-id" || r.Form.Get("refresh_token") != "refresh-a" {
			t.Errorf("unexpected refresh form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	path := writeTestCredential(t, dir, "a.json", "subject-a", "token-old", time.Now().Add(-time.Minute), server.URL)
	cred, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	next, err := cred.refresh(context.Background(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || next.AccessToken != "token-new" || next.RefreshToken != "refresh-new" {
		t.Fatalf("refresh result calls=%d token=%q refresh=%q", calls.Load(), next.AccessToken, next.RefreshToken)
	}
	reloaded, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.AccessToken != "token-new" || !reloaded.ExpiresAt.After(time.Now()) {
		t.Fatalf("refresh was not persisted: %#v", reloaded)
	}
}

func TestRefreshUpdatesOfficialSubscriptionTier(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": testJWTWithTier(5),
			"expires_in":   3600,
		})
	}))
	defer server.Close()
	dir := t.TempDir()
	path := writeTestCredentialModels(t, dir, "a.json", "subject-a", testJWTWithTier(0), time.Now().Add(-time.Minute), server.URL, []string{"grok-4.5"})
	cred, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	next, err := cred.refresh(context.Background(), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if next.Tier.Key != "supergrok_heavy" || next.Tier.Display != "SuperGrok Heavy" || !next.Tier.Paid || !next.Tier.Billing {
		t.Fatalf("refreshed tier = %#v", next.Tier)
	}
	reloaded, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Tier != next.Tier {
		t.Fatalf("reloaded tier = %#v, want %#v", reloaded.Tier, next.Tier)
	}
}

func TestConcurrentRefreshIsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	path := writeTestCredential(t, dir, "a.json", "subject-a", "token-old", time.Now().Add(-time.Minute), server.URL)
	cred, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	a := &account{id: accountID(cred.Subject), credential: cred, agentID: "agent", sessionID: "session"}
	p := &Pool{
		cfg: PoolConfig{Dir: dir, RefreshConcurrency: 4}, http: server.Client(), accounts: map[string]*account{a.id: a},
		files: map[string]fileEntry{path: {cred: cred}}, states: map[string]accountState{},
		affinity: newAffinityCache(time.Hour, 100), refreshSem: make(chan struct{}, 4), closed: make(chan struct{}),
	}
	p.active.Store([]*account{a})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.ensureFresh(context.Background(), a, false); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls.Load())
	}
}

func TestConcurrentForcedRefreshUsesCredentialGeneration(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	path := writeTestCredential(t, dir, "a.json", "subject-a", "token-old", time.Now().Add(time.Hour), server.URL)
	cred, err := loadCredential(path, "tui")
	if err != nil {
		t.Fatal(err)
	}
	a := &account{id: accountID(cred.Subject), credential: cred, agentID: "agent", sessionID: "session"}
	a.generation.Store(1)
	p := &Pool{
		cfg: PoolConfig{Dir: dir, RefreshConcurrency: 4}, http: server.Client(), accounts: map[string]*account{a.id: a},
		files: map[string]fileEntry{path: {cred: cred}}, states: map[string]accountState{},
		affinity: newAffinityCache(time.Hour, 100), refreshSem: make(chan struct{}, 4), closed: make(chan struct{}),
	}
	p.active.Store([]*account{a})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.RefreshIfUnchanged(context.Background(), a.id, 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 || a.currentGeneration() != 2 {
		t.Fatalf("refresh calls=%d generation=%d, want 1 and 2", calls.Load(), a.currentGeneration())
	}
}

func TestRefreshLockWaitHonorsContext(t *testing.T) {
	a := &account{}
	if err := a.acquireRefresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.releaseRefresh()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := a.acquireRefresh(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquireRefresh() error = %v, want deadline exceeded", err)
	}
	if time.Since(started) > 250*time.Millisecond {
		t.Fatalf("canceled refresh wait took too long: %s", time.Since(started))
	}
}

func TestRefreshPreservesQuotaCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-old", time.Now().Add(time.Hour), server.URL)
	pool := newTestPool(t, dir)
	defer pool.Close()
	id := accountID("subject-a")
	pool.MarkCooldown(id, "quota_exhausted", time.Hour)
	if err := pool.Refresh(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if lease, err := pool.AcquireAccount(context.Background(), id); err == nil {
		lease.Release()
		t.Fatal("OAuth refresh cleared the quota cooldown")
	}
}

func TestRefreshPreservesModelCooldown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token-new","refresh_token":"refresh-new","expires_in":3600}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	writeTestCredentialModels(t, dir, "a.json", "subject-a", "token-old", time.Now().Add(time.Hour), server.URL, []string{"grok-alpha", "grok-beta"})
	pool := newTestPool(t, dir)
	defer pool.Close()
	id := accountID("subject-a")
	pool.MarkModelCooldown(id, "grok-alpha", "model_free_quota_exhausted", time.Hour)
	if err := pool.Refresh(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var unavailable *UnavailableError
	if _, err := pool.Acquire(context.Background(), Affinity{}, "grok-alpha", nil); !errors.As(err, &unavailable) || !unavailable.Cooling {
		t.Fatalf("model cooldown was cleared by OAuth refresh: %v", err)
	}
	lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-beta", nil)
	if err != nil {
		t.Fatalf("unrelated model was unavailable after refresh: %v", err)
	}
	lease.Release()
}

func TestPoolRoundRobinAffinityAndConcurrentLease(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		writeTestCredential(t, dir, fmt.Sprintf("%d.json", i), fmt.Sprintf("subject-%d", i), fmt.Sprintf("token-%d", i), time.Now().Add(time.Hour), "")
	}
	pool := newTestPool(t, dir)
	defer pool.Close()
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-4", nil)
		if err != nil {
			t.Fatal(err)
		}
		seen[lease.AccountID()] = true
		lease.Release()
	}
	if len(seen) != 3 {
		t.Fatalf("round robin selected %d accounts", len(seen))
	}
	first, err := pool.Acquire(context.Background(), Affinity{Key: "session:one", Mode: AffinityHard}, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	id := first.AccountID()
	first.Release()
	pool.BindResponseID("resp-one", "grok-4", id)
	byResponse, err := pool.Acquire(context.Background(), Affinity{Key: "previous:resp-one", Mode: AffinityHard}, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if byResponse.AccountID() != id {
		t.Fatalf("response affinity moved from %s to %s", id, byResponse.AccountID())
	}
	byResponse.Release()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := pool.Acquire(context.Background(), Affinity{Key: "session:one", Mode: AffinityHard}, "grok-4", nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer lease.Release()
			if lease.AccountID() != id {
				t.Errorf("affinity moved from %s to %s", id, lease.AccountID())
			}
		}()
	}
	wg.Wait()
}

func TestPoolWaitsForPerAccountCapacity(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	pool.cfg.AccountMaxInflight = 2

	first, err := pool.Acquire(context.Background(), Affinity{}, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Acquire(context.Background(), Affinity{}, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan *Lease, 1)
	errorsCh := make(chan error, 1)
	go func() {
		lease, acquireErr := pool.Acquire(ctx, Affinity{}, "grok-4", nil)
		if acquireErr != nil {
			errorsCh <- acquireErr
			return
		}
		result <- lease
	}()
	select {
	case lease := <-result:
		lease.Release()
		t.Fatal("third request bypassed the per-account in-flight limit")
	case err := <-errorsCh:
		t.Fatal(err)
	case <-time.After(30 * time.Millisecond):
	}
	first.Release()
	select {
	case lease := <-result:
		lease.Release()
	case err := <-errorsCh:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("waiting request was not notified when account capacity became available")
	}
	second.Release()
}

func TestSoftAffinitySpillsWhileHardAffinityWaits(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	pool.cfg.AccountMaxInflight = 1

	soft := Affinity{Key: "cache:shared", Mode: AffinitySoft}
	first, err := pool.Acquire(context.Background(), soft, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.Acquire(context.Background(), soft, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID() == second.AccountID() {
		t.Fatal("soft affinity did not spill to an idle account")
	}
	first.Release()
	second.Release()

	hard := Affinity{Key: "session:strict", Mode: AffinityHard}
	pinned, err := pool.Acquire(context.Background(), hard, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease *Lease
		err   error
	}
	results := make(chan result, 1)
	go func() {
		lease, acquireErr := pool.Acquire(context.Background(), hard, "grok-4", nil)
		results <- result{lease: lease, err: acquireErr}
	}()
	select {
	case got := <-results:
		if got.lease != nil {
			got.lease.Release()
		}
		pinned.Release()
		t.Fatal("hard affinity bypassed its busy account")
	case <-time.After(30 * time.Millisecond):
	}
	pinnedID := pinned.AccountID()
	pinned.Release()
	select {
	case got := <-results:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.lease.Release()
		if got.lease.AccountID() != pinnedID {
			t.Fatalf("hard affinity moved from %s to %s", pinnedID, got.lease.AccountID())
		}
	case <-time.After(time.Second):
		t.Fatal("hard affinity waiter was not released")
	}
}

func TestPoolPrefersLeastInflightAccount(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	active := pool.schedulingSnapshot("grok-4")
	active[0].inflight.Store(3)
	defer active[0].inflight.Store(0)
	lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.AccountID() == active[0].id {
		t.Fatal("scheduler selected the more loaded account")
	}
}

func TestPoolPrefersPaidAccountsThenFallsBackToFree(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentialModelsWithTier(t, dir, "free.json", "free-subject", 0, []string{"grok-4.5"})
	writeTestCredentialModelsWithTier(t, dir, "paid-a.json", "paid-a-subject", 4, []string{"grok-4.5"})
	writeTestCredentialModelsWithTier(t, dir, "paid-b.json", "paid-b-subject", 1, []string{"grok-4.5"})
	pool := newTestPool(t, dir)
	defer pool.Close()

	freeID := accountID("free-subject")
	paidIDs := []string{accountID("paid-a-subject"), accountID("paid-b-subject")}
	pool.cfg.AccountMaxInflight = 1
	firstPaid, err := pool.Acquire(context.Background(), Affinity{}, "grok-4.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondPaid, err := pool.Acquire(context.Background(), Affinity{}, "grok-4.5", nil)
	if err != nil {
		firstPaid.Release()
		t.Fatal(err)
	}
	spill, err := pool.Acquire(context.Background(), Affinity{}, "grok-4.5", nil)
	if err != nil {
		firstPaid.Release()
		secondPaid.Release()
		t.Fatal(err)
	}
	if spill.AccountID() != freeID {
		spill.Release()
		firstPaid.Release()
		secondPaid.Release()
		t.Fatal("scheduler did not fall back to free after all paid accounts reached capacity")
	}
	spill.Release()
	firstPaid.Release()
	secondPaid.Release()

	seenPaid := map[string]bool{}
	for i := 0; i < 6; i++ {
		lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-4.5", nil)
		if err != nil {
			t.Fatal(err)
		}
		if lease.AccountID() == freeID {
			lease.Release()
			t.Fatal("free account selected while a paid account was available")
		}
		seenPaid[lease.AccountID()] = true
		lease.Release()
	}
	if len(seenPaid) != len(paidIDs) {
		t.Fatalf("paid account rotation used %d accounts, want %d", len(seenPaid), len(paidIDs))
	}

	for _, id := range paidIDs {
		pool.MarkCooldown(id, "quota_exhausted", time.Hour)
	}
	lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-4.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.AccountID() != freeID {
		t.Fatalf("fallback account = %s, want free account %s", lease.AccountID(), freeID)
	}
}

func TestCooldownUpdatesAreCoalescedAndExpireWithoutDirectoryScan(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	id := accountID("subject-a")

	pool.MarkCooldown(id, "rate_limited", 100*time.Millisecond)
	pool.mu.RLock()
	firstUntil := pool.states[id].CooldownUntil
	pool.mu.RUnlock()
	time.Sleep(10 * time.Millisecond)
	pool.MarkCooldown(id, "rate_limited", 100*time.Millisecond)
	pool.mu.RLock()
	secondUntil := pool.states[id].CooldownUntil
	pool.mu.RUnlock()
	if !secondUntil.Equal(firstUntil) {
		t.Fatalf("duplicate cooldown extended from %s to %s", firstUntil, secondUntil)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for len(pool.schedulingSnapshot("grok-4")) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(pool.schedulingSnapshot("grok-4")); got != 1 {
		t.Fatalf("active model snapshot contains %d accounts during cooldown, want 1", got)
	}
	deadline = time.Now().Add(time.Second)
	for len(pool.schedulingSnapshot("grok-4")) != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(pool.schedulingSnapshot("grok-4")); got != 2 {
		t.Fatalf("active model snapshot contains %d accounts after cooldown expiry, want 2", got)
	}
}

func TestAccountCooldownReasonsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentialModelsWithTier(t, dir, "paid.json", "paid-subject", 4, []string{"grok-4.5"})
	pool := newTestPool(t, dir)
	defer pool.Close()
	id := accountID("paid-subject")
	pool.MarkCooldown(id, "rate_limited", 25*time.Millisecond)
	pool.MarkCooldown(id, billingExhaustedReason, time.Hour)
	time.Sleep(40 * time.Millisecond)
	pool.RebuildSchedulingSnapshot()
	var unavailable *UnavailableError
	if _, err := pool.Acquire(context.Background(), Affinity{}, "grok-4.5", nil); !errors.As(err, &unavailable) || !unavailable.Cooling {
		t.Fatalf("billing cooldown did not survive rate-limit expiry: %v", err)
	}
	if !pool.ClearCooldownReason(id, billingExhaustedReason) {
		t.Fatal("billing cooldown was not cleared")
	}
	lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-4.5", nil)
	if err != nil {
		t.Fatalf("expired rate-limit cooldown remained after billing recovery: %v", err)
	}
	lease.Release()
}

func TestAccountCooldownReasonsPersistIndependently(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentialModelsWithTier(t, dir, "paid.json", "paid-subject", 4, []string{"grok-4.5"})
	id := accountID("paid-subject")
	pool := newTestPool(t, dir)
	pool.MarkCooldown(id, "rate_limited", time.Hour)
	pool.MarkCooldown(id, billingExhaustedReason, 2*time.Hour)
	pool.Close()

	reloaded := newTestPool(t, dir)
	defer reloaded.Close()
	if !reloaded.ClearCooldownReason(id, billingExhaustedReason) {
		t.Fatal("persisted billing cooldown was not restored")
	}
	var unavailable *UnavailableError
	if _, err := reloaded.Acquire(context.Background(), Affinity{}, "grok-4.5", nil); !errors.As(err, &unavailable) || !unavailable.Cooling {
		t.Fatalf("clearing billing cooldown also cleared rate limit: %v", err)
	}
	if !reloaded.ClearCooldownReason(id, "rate_limited") {
		t.Fatal("persisted rate-limit cooldown was not restored")
	}
	lease, err := reloaded.Acquire(context.Background(), Affinity{}, "grok-4.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestBillingRefreshEligibilityFollowsOfficialUsageGate(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentialModelsWithTier(t, dir, "free.json", "free-subject", 0, []string{"grok-4.5"})
	writeTestCredentialModelsWithTier(t, dir, "basic.json", "basic-subject", 2, []string{"grok-4.5"})
	writeTestCredentialModelsWithTier(t, dir, "premium.json", "premium-subject", 4, []string{"grok-4.5"})
	pool := newTestPool(t, dir)
	defer pool.Close()

	got := pool.AccountsNeedingBillingRefresh(time.Hour)
	want := []string{accountID("premium-subject")}
	if !slices.Equal(got, want) {
		t.Fatalf("billing refresh accounts = %v, want %v", got, want)
	}
}

func TestRefreshClearsBillingStateWhenTierLosesEligibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": testJWTWithTier(0),
			"expires_in":   3600,
		})
	}))
	defer server.Close()
	dir := t.TempDir()
	writeTestCredentialModels(t, dir, "paid.json", "paid-subject", testJWTWithTier(4), time.Now().Add(time.Hour), server.URL, []string{"grok-4.5"})
	pool := newTestPool(t, dir)
	defer pool.Close()
	id := accountID("paid-subject")
	usage := 100.0
	if err := pool.UpdateBilling(id, BillingInfo{UsagePercent: &usage, Exhausted: true, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pool.MarkCooldown(id, billingExhaustedReason, time.Hour)
	if err := pool.Refresh(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	info, ok := pool.Credential(id)
	if !ok {
		t.Fatal("credential disappeared after refresh")
	}
	if info.SubscriptionTier != "free" || info.Billing != nil || info.CooldownUntil != nil {
		t.Fatalf("refreshed credential retained stale billing state: %#v", info)
	}
	lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-4.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
}

func TestRefreshReprobesBillingWhenEligibleTierChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": testJWTWithTier(5),
			"expires_in":   3600,
		})
	}))
	defer server.Close()
	dir := t.TempDir()
	writeTestCredentialModels(t, dir, "paid.json", "paid-subject", testJWTWithTier(4), time.Now().Add(time.Hour), server.URL, []string{"grok-4.5"})
	pool := newTestPool(t, dir)
	defer pool.Close()
	id := accountID("paid-subject")
	usage := 25.0
	if err := pool.UpdateBilling(id, BillingInfo{UsagePercent: &usage, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pool.EnableBillingPreflight()
	select {
	case <-pool.BillingRefreshSignal():
	default:
	}
	if err := pool.Refresh(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	info, ok := pool.Credential(id)
	if !ok {
		t.Fatal("credential disappeared after refresh")
	}
	if info.SubscriptionTier != "supergrok_heavy" || info.Billing != nil || info.Status != "pending_billing" || info.Usable {
		t.Fatalf("tier change did not invalidate billing state: %#v", info)
	}
	select {
	case <-pool.BillingRefreshSignal():
	case <-time.After(time.Second):
		t.Fatal("tier change did not request a billing refresh")
	}
	pool.CompleteBillingRefresh(id)
}

func TestModelCooldownIsScopedAndPersists(t *testing.T) {
	dir := t.TempDir()
	writeTestCredentialModels(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "", []string{"grok-alpha", "grok-beta"})
	pool := newTestPool(t, dir)
	id := accountID("subject-a")
	pool.MarkModelCooldown(id, "grok-alpha", "model_free_quota_exhausted", time.Hour)

	var unavailable *UnavailableError
	if _, err := pool.Acquire(context.Background(), Affinity{}, "grok-alpha", nil); !errors.As(err, &unavailable) || !unavailable.Cooling {
		t.Fatalf("cooled model error=%v, want cooling unavailable error", err)
	}
	if unavailable.RetryAfter < 59*time.Minute {
		t.Fatalf("model retry-after=%s, want approximately one hour", unavailable.RetryAfter)
	}
	lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-beta", nil)
	if err != nil {
		t.Fatalf("unrelated model was cooled: %v", err)
	}
	lease.Release()
	if !pool.HasModel("grok-alpha") || !slices.Contains(pool.Models(), "grok-alpha") {
		t.Fatal("cooled model disappeared from the model catalog")
	}
	pool.Close()

	stateBytes, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	stateText := string(stateBytes)
	if !strings.Contains(stateText, `"model_cooldowns"`) || !strings.Contains(stateText, `"grok-alpha"`) {
		t.Fatalf("model cooldown was not persisted: %s", stateText)
	}
	if strings.Contains(stateText, "token-a") || strings.Contains(stateText, "subject-a") {
		t.Fatal("persisted model cooldown contains credential data")
	}

	reloaded := newTestPool(t, dir)
	defer reloaded.Close()
	unavailable = nil
	if _, err := reloaded.Acquire(context.Background(), Affinity{}, "grok-alpha", nil); !errors.As(err, &unavailable) || !unavailable.Cooling {
		t.Fatalf("persisted model cooldown was not restored: %v", err)
	}
	lease, err = reloaded.Acquire(context.Background(), Affinity{}, "grok-beta", nil)
	if err != nil {
		t.Fatalf("persisted cooldown affected unrelated model: %v", err)
	}
	lease.Release()
}

func TestModelCooldownExpiresWithoutDirectoryScan(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	id := accountID("subject-a")
	pool.MarkModelCooldown(id, "grok-4", "model_free_quota_exhausted", 30*time.Millisecond)
	var unavailable *UnavailableError
	if _, err := pool.Acquire(context.Background(), Affinity{}, "grok-4", nil); !errors.As(err, &unavailable) {
		t.Fatalf("model was not cooled: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		lease, err := pool.Acquire(context.Background(), Affinity{}, "grok-4", nil)
		if err == nil {
			lease.Release()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("model cooldown did not expire: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPoolDeduplicatesAccountsBySubject(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "same-subject", "token-a", time.Now().Add(time.Hour), "")
	writeTestCredential(t, dir, "b.json", "same-subject", "token-b", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	if got := len(pool.AccountIDs()); got != 1 {
		t.Fatalf("deduplicated account count = %d, want 1", got)
	}
}

func TestPoolAggregatesPersistsAndSchedulesModels(t *testing.T) {
	dir := t.TempDir()
	pathA := writeTestCredentialModels(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "", []string{"grok-alpha", "grok-shared"})
	writeTestCredentialModels(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "", []string{"grok-beta", "grok-shared"})
	pool := newTestPool(t, dir)
	defer pool.Close()

	if got, want := strings.Join(pool.Models(), ","), "grok-alpha,grok-beta,grok-shared"; got != want {
		t.Fatalf("aggregated models = %q, want %q", got, want)
	}
	lease, err := pool.Acquire(context.Background(), Affinity{Key: "session:beta", Mode: AffinityHard}, "grok-beta", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := lease.AccountID(), accountID("subject-b"); got != want {
		t.Fatalf("grok-beta scheduled to account %q, want %q", got, want)
	}
	lease.Release()

	_, err = pool.Acquire(context.Background(), Affinity{}, "grok-unknown", nil)
	var unavailable *ModelUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Model != "grok-unknown" {
		t.Fatalf("unknown model error = %v, want ModelUnavailableError", err)
	}

	updatedAt := time.Now().UTC().Truncate(time.Nanosecond)
	if err := pool.UpdateModels(accountID("subject-a"), []string{"grok-new", "grok-new", " grok-shared "}, updatedAt); err != nil {
		t.Fatal(err)
	}
	credential, err := loadCredential(pathA, "tui")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(credential.Models, ","), "grok-new,grok-shared"; got != want {
		t.Fatalf("persisted models = %q, want %q", got, want)
	}
	if !credential.ModelsUpdatedAt.Equal(updatedAt) {
		t.Fatalf("persisted models_updated_at = %s, want %s", credential.ModelsUpdatedAt, updatedAt)
	}
}

func TestAffinityCacheTTLAndCapacity(t *testing.T) {
	expiring := newAffinityCache(10*time.Millisecond, 64)
	expiring.Set("session", "account")
	time.Sleep(20 * time.Millisecond)
	if _, ok := expiring.Get("session"); ok {
		t.Fatal("expired affinity entry was returned")
	}

	bounded := newAffinityCache(time.Hour, 64)
	for i := 0; i < 1000; i++ {
		bounded.Set(fmt.Sprintf("session-%d", i), "account")
	}
	total := 0
	for i := range bounded.shards {
		bounded.shards[i].Lock()
		total += len(bounded.shards[i].entries)
		bounded.shards[i].Unlock()
	}
	if total > 64 {
		t.Fatalf("affinity cache size = %d, limit 64", total)
	}
}

func TestCooldownPersistsAndAffinityMigrates(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	lease, err := pool.Acquire(context.Background(), Affinity{Key: "session:one", Mode: AffinityHard}, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	cooledID := lease.AccountID()
	lease.Release()
	pool.MarkCooldown(cooledID, "quota_exhausted", time.Hour)
	migrated, err := pool.Acquire(context.Background(), Affinity{Key: "session:one", Mode: AffinityHard}, "grok-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.AccountID() == cooledID {
		t.Fatal("affinity did not migrate away from cooled account")
	}
	migrated.Release()
	pool.Close()
	stateBytes, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), "subject-a") || strings.Contains(string(stateBytes), "token-a") {
		t.Fatal("scheduler state persisted credential data")
	}

	reloaded := newTestPool(t, dir)
	defer reloaded.Close()
	if lease, err := reloaded.AcquireAccount(context.Background(), cooledID); err == nil {
		lease.Release()
		t.Fatal("persisted cooldown was not restored")
	}
}

func TestLegacyAccountCooldownStateLoads(t *testing.T) {
	dir := t.TempDir()
	writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	id := accountID("subject-a")
	legacy := persistedState{Version: 1, Accounts: map[string]accountState{
		id: {CooldownUntil: time.Now().Add(time.Hour), Reason: "quota_exhausted"},
	}}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stateFileName), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	pool := newTestPool(t, dir)
	defer pool.Close()
	var unavailable *UnavailableError
	if _, err := pool.Acquire(context.Background(), Affinity{}, "grok-4", nil); !errors.As(err, &unavailable) || !unavailable.Cooling {
		t.Fatalf("legacy cooldown was not restored: %v", err)
	}
	if !pool.ClearCooldownReason(id, "quota_exhausted") {
		t.Fatal("legacy cooldown was not converted to a reason-specific cooldown")
	}
}

func TestDisabledCredentialPersistsUntilTokensChange(t *testing.T) {
	t.Run("unchanged credential stays disabled across restart", func(t *testing.T) {
		dir := t.TempDir()
		writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
		writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")

		pool := newTestPool(t, dir)
		id := accountID("subject-a")
		pool.Disable(id, "chat_endpoint_denied")
		pool.Close()

		stateBytes, err := os.ReadFile(filepath.Join(dir, stateFileName))
		if err != nil {
			t.Fatal(err)
		}
		stateText := string(stateBytes)
		if !strings.Contains(stateText, `"credential_fingerprint"`) {
			t.Fatalf("persisted state has no credential fingerprint: %s", stateText)
		}
		if strings.Contains(stateText, "token-a") || strings.Contains(stateText, "refresh-a") || strings.Contains(stateText, "subject-a") {
			t.Fatal("persisted state contains credential data")
		}

		reloaded := newTestPool(t, dir)
		defer reloaded.Close()
		if lease, err := reloaded.AcquireAccount(context.Background(), id); err == nil {
			lease.Release()
			t.Fatal("unchanged disabled credential became available after restart")
		}
	})

	t.Run("token replacement restores during hot reload", func(t *testing.T) {
		dir := t.TempDir()
		writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
		pool := newTestPool(t, dir)
		defer pool.Close()
		id := accountID("subject-a")
		pool.Disable(id, "chat_endpoint_denied")

		writeTestCredential(t, dir, "a.json", "subject-a", "replacement-token", time.Now().Add(time.Hour), "")
		if err := pool.scan(); err != nil {
			t.Fatal(err)
		}
		lease, err := pool.AcquireAccount(context.Background(), id)
		if err != nil {
			t.Fatalf("replacement credential was not restored: %v", err)
		}
		lease.Release()
	})

	t.Run("token replacement restores after restart", func(t *testing.T) {
		dir := t.TempDir()
		writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
		writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")
		id := accountID("subject-a")

		pool := newTestPool(t, dir)
		pool.Disable(id, "chat_endpoint_denied")
		pool.Close()
		writeTestCredential(t, dir, "a.json", "subject-a", "replacement-token", time.Now().Add(time.Hour), "")

		reloaded := newTestPool(t, dir)
		defer reloaded.Close()
		lease, err := reloaded.AcquireAccount(context.Background(), id)
		if err != nil {
			t.Fatalf("replacement credential was not restored after restart: %v", err)
		}
		lease.Release()
	})
}

func TestHotReloadAddsAndRemovesCredentials(t *testing.T) {
	dir := t.TempDir()
	pathA := writeTestCredential(t, dir, "a.json", "subject-a", "token-a", time.Now().Add(time.Hour), "")
	pool := newTestPool(t, dir)
	defer pool.Close()
	writeTestCredential(t, dir, "b.json", "subject-b", "token-b", time.Now().Add(time.Hour), "")
	if err := pool.scan(); err != nil {
		t.Fatal(err)
	}
	pool.mu.RLock()
	if len(pool.accounts) != 2 {
		t.Fatalf("account count after add = %d", len(pool.accounts))
	}
	pool.mu.RUnlock()
	if err := os.Remove(pathA); err != nil {
		t.Fatal(err)
	}
	if err := pool.scan(); err != nil {
		t.Fatal(err)
	}
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	if len(pool.accounts) != 1 {
		t.Fatalf("account count after remove = %d", len(pool.accounts))
	}
}

func TestPoolAllowEmptyImportReplaceAndDelete(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewPool(context.Background(), PoolConfig{Dir: dir}, nil); !errors.Is(err, ErrNoAuth) {
		t.Fatalf("NewPool without AllowEmpty error = %v, want ErrNoAuth", err)
	}
	pool, err := NewPool(context.Background(), PoolConfig{
		Dir: dir, Surface: "tui", AllowEmpty: true, ReloadInterval: time.Hour,
		RefreshConcurrency: 1, AffinityTTL: time.Hour, AffinityMaxEntries: 128,
	}, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	first := remoteCredentialJSON(t, "remote-subject", "token-old", []string{"grok-4"})
	info, created, err := pool.ImportCredential(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if !created || info.Status != "ready" || !info.Usable || info.ID == "" {
		t.Fatalf("first import = %#v, created=%v", info, created)
	}
	if len(pool.Credentials()) != 1 {
		t.Fatalf("credential count = %d", len(pool.Credentials()))
	}
	path := filepath.Join(dir, info.ID+".json")
	if stat, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if os.PathSeparator != '\\' && stat.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o", stat.Mode().Perm())
	}

	replacement := remoteCredentialJSON(t, "remote-subject", "token-new", []string{"grok-4", "grok-new"})
	updated, created, err := pool.ImportCredential(context.Background(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	if created || updated.ID != info.ID || !slices.Equal(updated.Models, []string{"grok-4", "grok-new"}) {
		t.Fatalf("replacement = %#v, created=%v", updated, created)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), "token-new") || strings.Contains(string(stored), "token-old") {
		t.Fatal("replacement was not persisted")
	}

	if err := pool.DeleteCredential(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	if len(pool.Credentials()) != 0 {
		t.Fatalf("credential count after delete = %d", len(pool.Credentials()))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("credential file still exists: %v", err)
	}
	if _, err := pool.Acquire(context.Background(), Affinity{}, "grok-4", nil); err == nil {
		t.Fatal("empty pool Acquire unexpectedly succeeded")
	} else {
		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("empty pool Acquire error = %T %v", err, err)
		}
	}
}

func TestImportCredentialValidation(t *testing.T) {
	pool, err := NewPool(context.Background(), PoolConfig{Dir: t.TempDir(), AllowEmpty: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, _, err := pool.ImportCredential(context.Background(), []byte(`{"access_token":`)); !errors.Is(err, ErrInvalidCredentialJSON) {
		t.Fatalf("invalid JSON error = %v", err)
	}
	if _, _, err := pool.ImportCredential(context.Background(), []byte(`{"access_token":"token"}`)); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("missing subject error = %v", err)
	}
}

func TestImportWaitsForRefreshAndWins(t *testing.T) {
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(refreshStarted)
		<-releaseRefresh
		_, _ = io.WriteString(w, `{"access_token":"token-refreshed","refresh_token":"refresh-new","expires_in":3600}`)
	}))
	defer server.Close()
	pool, err := NewPool(context.Background(), PoolConfig{
		Dir: t.TempDir(), Surface: "tui", AllowEmpty: true, ReloadInterval: time.Hour,
		RefreshConcurrency: 1, AffinityTTL: time.Hour, AffinityMaxEntries: 128,
	}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	initial, err := json.Marshal(map[string]any{
		"access_token": "token-old", "refresh_token": "refresh-old", "client_id": "client",
		"sub": "refresh-race", "expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		"token_endpoint": server.URL, "models": []string{"grok-4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _, err := pool.ImportCredential(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- pool.Refresh(context.Background(), info.ID) }()
	<-refreshStarted
	replacement := remoteCredentialJSON(t, "refresh-race", "token-uploaded", []string{"grok-4"})
	importDone := make(chan error, 1)
	go func() {
		_, _, err := pool.ImportCredential(context.Background(), replacement)
		importDone <- err
	}()
	select {
	case err := <-importDone:
		t.Fatalf("import completed before refresh lock was released: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseRefresh)
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
	if err := <-importDone; err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pool.cfg.Dir, info.ID+".json")
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stored), "token-uploaded") || strings.Contains(string(stored), "token-refreshed") {
		t.Fatalf("upload did not win refresh race: %s", stored)
	}
}

func remoteCredentialJSON(t *testing.T, subject, token string, models []string) []byte {
	t.Helper()
	raw := map[string]any{
		"access_token": token, "refresh_token": "refresh", "client_id": "client",
		"sub": subject, "expired": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		"models": models, "models_updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func BenchmarkPoolAcquireTenThousandAccounts(b *testing.B) {
	p := &Pool{
		accounts: map[string]*account{}, states: map[string]accountState{},
		affinity: newAffinityCache(time.Hour, 100000), refreshSem: make(chan struct{}, 4), closed: make(chan struct{}),
	}
	active := make([]*account, 10000)
	for i := range active {
		id := fmt.Sprintf("%024d", i)
		a := &account{id: id, credential: &credential{AccessToken: "token", Subject: id, ExpiresAt: time.Now().Add(time.Hour), Models: []string{"grok-4"}}, agentID: "agent", sessionID: "session"}
		p.accounts[id], active[i] = a, a
	}
	p.active.Store(active)
	p.activeByModel.Store(map[string][]*account{"grok-4": active})
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lease, err := p.Acquire(context.Background(), Affinity{}, "grok-4", nil)
			if err != nil {
				b.Fatal(err)
			}
			lease.Release()
		}
	})
}

func newTestPool(t *testing.T, dir string) *Pool {
	t.Helper()
	pool, err := NewPool(context.Background(), PoolConfig{Dir: dir, Surface: "tui", ReloadInterval: time.Hour, RefreshConcurrency: 2, AffinityTTL: time.Hour, AffinityMaxEntries: 1024}, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func writeTestCredential(t *testing.T, dir, name, subject, token string, expires time.Time, tokenURL string) string {
	t.Helper()
	return writeTestCredentialModels(t, dir, name, subject, token, expires, tokenURL, []string{"grok-4"})
}

func writeTestCredentialModels(t *testing.T, dir, name, subject, token string, expires time.Time, tokenURL string, models []string) string {
	t.Helper()
	if tokenURL == "" {
		tokenURL = "https://auth.x.ai/oauth2/token"
	}
	raw := map[string]any{
		"type": "xai", "auth_kind": "oauth", "access_token": token,
		"refresh_token": "refresh-a", "client_id": "client-id", "sub": subject,
		"expired": expires.UTC().Format(time.RFC3339Nano), "expires_in": 3600,
		"token_endpoint": tokenURL,
		"models":         models, "models_updated_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTestCredentialModelsWithTier(t *testing.T, dir, name, subject string, tier int64, models []string) string {
	t.Helper()
	return writeTestCredentialModels(t, dir, name, subject, testJWTWithTier(tier), time.Now().Add(time.Hour), "", models)
}

func testJWTWithTier(tier int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"tier":%d}`, tier)))
	return header + "." + payload + ".signature"
}
