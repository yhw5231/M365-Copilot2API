package web

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAccountConcurrencyAcquireRelease(t *testing.T) {
	c := &accountConcurrency{limit: 2, inflight: map[string]int{}, changed: make(chan struct{})}
	if !c.Available("a") {
		t.Fatal("fresh account should be available")
	}
	rel1, err := c.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	rel2, err := c.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if c.Available("a") {
		t.Fatal("account at limit should be unavailable")
	}
	if !c.Available("b") {
		t.Fatal("other account should stay available")
	}
	rel1()
	if !c.Available("a") {
		t.Fatal("account should be available after one release")
	}
	rel2()
	rel2() // double release must be harmless (sync.Once)
	if !c.Available("a") {
		t.Fatal("account should be fully available")
	}
}

func TestAccountConcurrencyBlocksAndUnblocks(t *testing.T) {
	c := &accountConcurrency{limit: 1, inflight: map[string]int{}, changed: make(chan struct{})}
	rel1, err := c.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := c.Acquire(context.Background(), "a"); err != nil {
			t.Errorf("acquire after release failed: %v", err)
		}
	}()
	select {
	case <-done:
		t.Fatal("acquire returned while slot was still held")
	case <-time.After(50 * time.Millisecond):
	}
	rel1()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acquire did not unblock after release")
	}
}

func TestAccountConcurrencyAcquireContextCancel(t *testing.T) {
	c := &accountConcurrency{limit: 1, inflight: map[string]int{}, changed: make(chan struct{})}
	rel1, err := c.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer rel1()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Acquire(ctx, "a"); err == nil {
		t.Fatal("expected context deadline error")
	}
}

func TestAccountConcurrencySnapshot(t *testing.T) {
	c := &accountConcurrency{limit: 8, inflight: map[string]int{"x": 2}}
	snap := c.Snapshot()
	if snap["limit"].(int) != 8 {
		t.Fatalf("limit=%v want 8", snap["limit"])
	}
	inflight, ok := snap["inflight"].(map[string]int)
	if !ok || inflight["x"] != 2 {
		t.Fatalf("inflight=%v want {x:2}", snap["inflight"])
	}
	// nil receiver snapshot
	if snap := (*accountConcurrency)(nil).Snapshot(); snap["limit"].(int) != defaultAccountConcurrency {
		t.Fatalf("nil snapshot limit=%v", snap["limit"])
	}
}

func TestAccountConcurrencyNilIsNoop(t *testing.T) {
	if !(*accountConcurrency)(nil).Available("a") {
		t.Fatal("nil concurrency should allow everything")
	}
	rel, err := (*accountConcurrency)(nil).Acquire(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	rel()
}

func TestAccountConcurrencyConcurrentStress(t *testing.T) {
	c := &accountConcurrency{limit: 4, inflight: map[string]int{}, changed: make(chan struct{})}
	const workers = 20
	var wg sync.WaitGroup
	peaks := make([]int, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel, err := c.Acquire(context.Background(), "hot")
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			c.mu.Lock()
			peaks[i] = c.inflight["hot"]
			c.mu.Unlock()
			rel()
		}(i)
	}
	wg.Wait()
	for i, p := range peaks {
		if p > c.limit {
			t.Fatalf("worker %d observed inflight=%d over limit %d", i, p, c.limit)
		}
	}
	if !c.Available("hot") {
		t.Fatal("stress should end with no inflight")
	}
}

func TestAccountConcurrencyLimitsAndReleasesSlots(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "2")
	limiter := newAccountConcurrency()
	release1, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	release2, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if limiter.Available("account-a") {
		t.Fatal("account remained available at its configured limit")
	}
	if !limiter.Available("account-b") {
		t.Fatal("one full account must not block another account")
	}
	release1()
	if !limiter.Available("account-a") {
		t.Fatal("released slot was not returned")
	}
	release1()
	release2()
}

func TestAccountConcurrencyWaitHonorsCancellation(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "1")
	limiter := newAccountConcurrency()
	release, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(ctx, "account-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want deadline exceeded", err)
	}
}

func TestAccountConcurrencyUsesDocumentedDefault(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "")
	limiter := newAccountConcurrency()
	if limiter.limit != defaultAccountConcurrency {
		t.Fatalf("limit = %d, want %d", limiter.limit, defaultAccountConcurrency)
	}
}

func TestAccountConcurrencyCountsDistinctUpstreamSessions(t *testing.T) {
	c := &accountConcurrency{
		limit:    2,
		inflight: map[string]int{},
		sessions: map[string]map[string]int{},
		changed:  make(chan struct{}),
	}

	releaseA1, err := c.Acquire(context.Background(), "account-a", "upstream-session-a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA1()

	releaseA2, err := c.Acquire(context.Background(), "account-a", "upstream-session-a")
	if err != nil {
		t.Fatalf("second request for the same upstream session should share its account slot: %v", err)
	}
	defer releaseA2()

	if got := c.inflight["account-a"]; got != 1 {
		t.Fatalf("same upstream session counted as %d concurrent sessions, want 1", got)
	}

	releaseB, err := c.Acquire(context.Background(), "account-a", "upstream-session-b")
	if err != nil {
		t.Fatalf("second distinct upstream session should fit within limit: %v", err)
	}
	defer releaseB()

	if got := c.inflight["account-a"]; got != 2 {
		t.Fatalf("two distinct upstream sessions counted as %d, want 2", got)
	}
	if c.Available("account-a") {
		t.Fatal("account should be unavailable when its distinct upstream-session limit is reached")
	}
}

func TestAccountConcurrencyBlocksNewSessionButAllowsActiveSession(t *testing.T) {
	c := &accountConcurrency{
		limit:    1,
		inflight: map[string]int{},
		sessions: map[string]map[string]int{},
		changed:  make(chan struct{}),
	}

	releaseA, err := c.Acquire(context.Background(), "account-a", "upstream-session-a")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseA()

	ctxSame, cancelSame := context.WithTimeout(context.Background(), time.Second)
	defer cancelSame()
	releaseSame, err := c.Acquire(ctxSame, "account-a", "upstream-session-a")
	if err != nil {
		t.Fatalf("active upstream session should not consume another account slot: %v", err)
	}
	releaseSame()

	ctxNew, cancelNew := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelNew()
	if _, err := c.Acquire(ctxNew, "account-a", "upstream-session-b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("new upstream session Acquire() error = %v, want deadline exceeded", err)
	}
}

// TestApplyPersistedAccountConcurrency guards against the "concurrency reset on
// every upgrade" regression: the runtime limiter must be seeded from the
// persisted settings.json value at startup, not left at the built-in default.
func TestApplyPersistedAccountConcurrency(t *testing.T) {
	st := &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), accountPath: filepath.Join(t.TempDir(), "account-settings.json"), v: defaultRuntimeSettings()}
	st.v.AccountConcurrency = 7
	limiter := newAccountConcurrency()
	if got := limiter.Snapshot()["limit"].(int); got != defaultAccountConcurrency {
		t.Fatalf("fresh limiter limit = %d, want built-in default %d", got, defaultAccountConcurrency)
	}
	s := &Server{settings: st, accountConcurrency: limiter}
	s.applyPersistedAccountConcurrency()
	if got := limiter.Snapshot()["limit"].(int); got != 7 {
		t.Fatalf("runtime limit = %d, want persisted 7 (upgrade must not reset concurrency)", got)
	}
}

func TestApplyPersistedAccountConcurrencyIgnoresBrokenState(t *testing.T) {
	limiter := newAccountConcurrency()
	s := &Server{settings: nil, accountConcurrency: limiter}
	s.applyPersistedAccountConcurrency() // must not panic
	if got := limiter.Snapshot()["limit"].(int); got != defaultAccountConcurrency {
		t.Fatalf("limit changed without settings store: %d", got)
	}
}

// waitForQueued blocks until accountID has exactly want queued waiters.
func waitForQueued(t *testing.T, c *accountConcurrency, accountID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.waiters[accountID])
		c.mu.Unlock()
		if n == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("account %q queued = not %d within 1s", accountID, want)
}

func testConcurrency(limit int) *accountConcurrency {
	return &accountConcurrency{
		limit:         limit,
		inflight:      map[string]int{},
		sessions:      map[string]map[string]int{},
		waiters:       map[string][]uint64{},
		warmSessions:  map[string]time.Time{},
		reservedCount: map[string]int{},
		changed:       make(chan struct{}),
	}
}

// expireReservation waits until sessionID holds a reservation on accountID,
// then fully releases it (entry + reservedCount) so cold waiters can proceed.
// The reservation must have been created by a real acquire+release so
// reservedCount stays consistent.
func expireReservation(t *testing.T, c *accountConcurrency, accountID, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	key := accountSessionKey(accountID, sessionID)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		until, ok := c.warmSessions[key]
		if ok && until.After(time.Now()) {
			delete(c.warmSessions, key)
			if c.reservedCount[accountID] > 0 {
				c.reservedCount[accountID]--
			}
			// Wake blocked cold waiters so they re-evaluate capacity, mirroring
			// how an expired reservation surfaces inside Acquire.
			close(c.changed)
			c.changed = make(chan struct{})
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %q never reserved a slot on account %q", sessionID, accountID)
}

// TestSessionAcquireFIFOOrder ensures the session-based Acquire path respects
// FIFO ordering: the first cold session that queues gets the slot first.
func TestSessionAcquireFIFOOrder(t *testing.T) {
	c := testConcurrency(1)
	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan string, 2)
	release := make(chan string, 2)
	go func() {
		rel, err := c.Acquire(context.Background(), "a", "s1")
		if err != nil {
			acquired <- "err:s1:" + err.Error()
			return
		}
		acquired <- "s1"
		<-release
		rel()
	}()
	waitForQueued(t, c, "a", 1) // s1 must enqueue first
	go func() {
		rel, err := c.Acquire(context.Background(), "a", "s2")
		if err != nil {
			acquired <- "err:s2:" + err.Error()
			return
		}
		acquired <- "s2"
		<-release
		rel()
	}()
	waitForQueued(t, c, "a", 2)
	rel0()
	// rel0's release reserved the slot for s0; expire it so s1 can proceed
	expireReservation(t, c, "a", "s0")
	first := <-acquired
	if first != "s1" {
		t.Fatalf("first acquired = %s, want s1 (FIFO)", first)
	}
	release <- "s1"
	// s1's release reserves its slot for the warm window; expire it so the
	// cold waiter can proceed (reservation blocking is covered separately).
	expireReservation(t, c, "a", "s1")
	second := <-acquired
	if second != "s2" {
		t.Fatalf("second acquired = %s, want s2", second)
	}
	release <- "s2"
}

// TestReservationBlocksColdSessions verifies that a released slot is reserved
// for its warm session: cold sessions see no availability, cannot cut in, and
// must wait for the reservation to expire or be consumed.
func TestReservationBlocksColdSessions(t *testing.T) {
	c := testConcurrency(1)
	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	rel0() // release -> slot reserved for s0
	c.mu.Lock()
	reserved := c.reservedCount["a"]
	c.mu.Unlock()
	if reserved != 1 {
		t.Fatalf("reservedCount = %d, want 1", reserved)
	}
	if c.Available("a") {
		t.Fatal("cold session must not see the reserved slot as available")
	}
	if !c.HasReservation("a", "s0") {
		t.Fatal("s0 should hold a reservation")
	}
	if c.HasUnreservedSlot("a") {
		t.Fatal("no unreserved slot expected while reservation is active")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Acquire(ctx, "a", "s1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cold acquire err = %v, want deadline exceeded (cannot cut into reserved slot)", err)
	}
}

// TestWarmSessionReturnsImmediately verifies the core "对话保留优先级" guarantee:
// a session within its warm window gets its reserved slot back at once even
// when the account is otherwise full, while the cold waiter stays queued.
func TestWarmSessionReturnsImmediately(t *testing.T) {
	c := testConcurrency(2)
	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	rel0() // s0 reserved
	rel1, err := c.Acquire(context.Background(), "a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	// Account now: inflight=1 (s1) + reserved=1 (s0) = full for cold sessions.
	doneCold := make(chan string, 1)
	go func() {
		rel, err := c.Acquire(context.Background(), "a", "s2")
		if err != nil {
			doneCold <- "err:s2:" + err.Error()
			return
		}
		rel()
		doneCold <- "s2"
	}()
	waitForQueued(t, c, "a", 1)
	// Warm s0 returns -> immediate acquire, no queueing, cold s2 cannot cut in.
	doneWarm := make(chan string, 1)
	go func() {
		rel, err := c.Acquire(context.Background(), "a", "s0")
		if err != nil {
			doneWarm <- "err:s0:" + err.Error()
			return
		}
		rel()
		doneWarm <- "ok"
	}()
	select {
	case r := <-doneWarm:
		if r != "ok" {
			t.Fatalf("warm acquire: %s", r)
		}
	case <-time.After(time.Second):
		t.Fatal("warm session did not acquire its reserved slot immediately")
	}
	c.mu.Lock()
	queued := len(c.waiters["a"])
	c.mu.Unlock()
	if queued != 1 {
		t.Fatalf("cold waiter count = %d, want 1 (cold must not cut in)", queued)
	}
	rel1()
	// rel1's release reserved s1's slot for the warm window; lapse it so the
	// cold waiter can proceed once the occupying session truly moves on.
	expireReservation(t, c, "a", "s1")
	select {
	case <-doneCold:
	case <-time.After(time.Second):
		t.Fatal("cold session did not acquire after the occupying session released")
	}
}

// TestReservationExpiryFreesCapacity verifies that an expired reservation
// returns capacity to the pool for cold sessions.
func TestReservationExpiryFreesCapacity(t *testing.T) {
	c := testConcurrency(1)
	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	rel0()
	c.mu.Lock()
	c.warmSessions[accountSessionKey("a", "s0")] = time.Now().Add(-time.Second) // expire
	c.mu.Unlock()
	if c.HasReservation("a", "s0") {
		t.Fatal("expired reservation should be gone")
	}
	if !c.HasUnreservedSlot("a") {
		t.Fatal("capacity should be free after reservation expiry")
	}
	rel1, err := c.Acquire(context.Background(), "a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	rel1()
}

// TestExpiredReservationAutoSweptUnblocksWaiter verifies the automatic expiry
// sweep: once a warm reservation lapses, unreserved capacity returns AND a
// blocked cold waiter is woken immediately (no queue-timeout wait), so new
// sessions enter the freed slot and re-arm the warm window.
func TestExpiredReservationAutoSweptUnblocksWaiter(t *testing.T) {
	c := testConcurrency(1)
	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	rel0() // release -> slot reserved for s0 for the warm window
	// Expire s0's reservation in the background shortly after the waiter blocks.
	go func() {
		time.Sleep(30 * time.Millisecond)
		c.mu.Lock()
		c.warmSessions[accountSessionKey("a", "s0")] = time.Now().Add(-time.Second)
		c.lastSweep = map[string]time.Time{} // force the next sweep to run
		c.mu.Unlock()
	}()
	acquired := make(chan error, 1)
	go func() {
		_, err := c.Acquire(context.Background(), "a", "s1") // cold, must queue then proceed
		acquired <- err
	}()
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("cold session should acquire after reservation expiry, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cold waiter never woken after reservation expired (sweep did not run)")
	}
}

// TestSessionAcquireExpiredWarmTreatedAsCold verifies that an expired
// reservation does not get priority.
func TestSessionAcquireExpiredWarmTreatedAsCold(t *testing.T) {
	c := testConcurrency(1)
	// s1 held a slot recently but its reservation has already expired (created
	// through a real acquire+release so the accounting stays consistent).
	relWarm, err := c.Acquire(context.Background(), "a", "s1")
	if err != nil {
		t.Fatal(err)
	}
	relWarm()
	expireReservation(t, c, "a", "s1")
	// lazily purge the expired entry so capacity returns before s0 acquires
	c.mu.Lock()
	c.reservationActiveLocked("a", "s1")
	c.mu.Unlock()

	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan string, 2)
	release := make(chan string, 2)
	// s1 (expired reservation → cold) queues first
	go func() {
		rel, err := c.Acquire(context.Background(), "a", "s1")
		if err != nil {
			acquired <- "err:s1:" + err.Error()
			return
		}
		acquired <- "s1"
		<-release
		rel()
	}()
	waitForQueued(t, c, "a", 1)
	// s2 (cold) queues second
	go func() {
		rel, err := c.Acquire(context.Background(), "a", "s2")
		if err != nil {
			acquired <- "err:s2:" + err.Error()
			return
		}
		acquired <- "s2"
		<-release
		rel()
	}()
	waitForQueued(t, c, "a", 2)
	// Expired reservation is treated as cold, so both are cold and FIFO applies:
	// s1 queued first and must go first.
	rel0()
	// rel0's release reserved the slot for s0; expire it so s1 can proceed
	expireReservation(t, c, "a", "s0")
	first := <-acquired
	if first != "s1" {
		t.Fatalf("first acquired = %s, want s1 (expired warm is cold, still FIFO)", first)
	}
	release <- "s1"
	expireReservation(t, c, "a", "s1")
	second := <-acquired
	if second != "s2" {
		t.Fatalf("second acquired = %s, want s2", second)
	}
	release <- "s2"
}

// TestAcquireQueueTimeoutSession ensures a cold session that waits past the
// configured queue bound fails with errQueueTimeout instead of waiting forever.
func TestAcquireQueueTimeoutSession(t *testing.T) {
	c := testConcurrency(1)
	c.queueTimeoutOverride = 50 * time.Millisecond
	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err2 := c.Acquire(ctx, "a", "s1") // cold, no unreserved slot -> queues
	if !IsQueueTimeout(err2) {
		t.Fatalf("want errQueueTimeout, got %v", err2)
	}
	rel0()
}

// TestAcquireQueueTimeoutNoSession covers the request-based (no session ID)
// wait path with the same queue-timeout bound.
func TestAcquireQueueTimeoutNoSession(t *testing.T) {
	c := testConcurrency(1)
	c.queueTimeoutOverride = 50 * time.Millisecond
	rel0, err := c.Acquire(context.Background(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err2 := c.Acquire(ctx, "a", "")
	if !IsQueueTimeout(err2) {
		t.Fatalf("want errQueueTimeout, got %v", err2)
	}
	rel0()
}

// TestAcquireQueueTimeoutSlotFreedInTime verifies a waiter acquires normally
// when capacity frees well before the queue bound expires.
func TestAcquireQueueTimeoutSlotFreedInTime(t *testing.T) {
	c := testConcurrency(1)
	c.queueTimeoutOverride = 2 * time.Second
	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() {
		_, err := c.Acquire(context.Background(), "a", "s1")
		acquired <- err
	}()
	waitForQueued(t, c, "a", 1)
	rel0()
	// rel0's release reserved the slot for s0; expire it so s1 can proceed.
	expireReservation(t, c, "a", "s0")
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("s1 should have acquired before the queue timeout, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("s1 never acquired")
	}
}

// TestWaitForSlotQueueTimeout ensures WaitForSlot (used before a stream
// preamble) surfaces the queue timeout as errQueueTimeout.
func TestWaitForSlotQueueTimeout(t *testing.T) {
	c := testConcurrency(1)
	c.queueTimeoutOverride = 50 * time.Millisecond
	rel0, err := c.Acquire(context.Background(), "a", "s0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.WaitForSlot(ctx, "a", "s1"); !IsQueueTimeout(err) {
		t.Fatalf("want errQueueTimeout, got %v", err)
	}
	rel0()
}

// TestWaitForSlotAcquiresWhenIdle verifies WaitForSlot succeeds immediately on
// an account with free unreserved capacity.
func TestWaitForSlotAcquiresWhenIdle(t *testing.T) {
	c := testConcurrency(1)
	c.queueTimeoutOverride = 2 * time.Second
	if err := c.WaitForSlot(context.Background(), "a", "s1"); err != nil {
		t.Fatalf("WaitForSlot on idle account failed: %v", err)
	}
}

// TestQueueTimeoutErrorMapping ensures errQueueTimeout is recognized and mapped
// to HTTP 503 (without triggering rate-limit/failover classification).
func TestQueueTimeoutErrorMapping(t *testing.T) {
	if !IsQueueTimeout(errQueueTimeout) {
		t.Fatal("IsQueueTimeout must recognize errQueueTimeout")
	}
	if IsQueueTimeout(context.DeadlineExceeded) {
		t.Fatal("IsQueueTimeout must not match unrelated errors")
	}
	if IsRateLimited(errQueueTimeout) {
		t.Fatal("errQueueTimeout must not be classified as rate limited (it must not trigger failover)")
	}
	if upstreamStatus(errQueueTimeout) != 503 {
		t.Fatalf("upstreamStatus(errQueueTimeout) = %d, want 503", upstreamStatus(errQueueTimeout))
	}
}
