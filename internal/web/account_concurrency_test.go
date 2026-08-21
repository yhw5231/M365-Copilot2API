package web

import (
	"context"
	"errors"
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
