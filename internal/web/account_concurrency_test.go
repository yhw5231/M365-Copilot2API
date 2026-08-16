package web

import (
	"context"
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
