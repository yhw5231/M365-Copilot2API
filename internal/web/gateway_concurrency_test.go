package web

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestGatewayConcurrencyUnlimitedByDefault(t *testing.T) {
	if defaultGatewayConcurrency != 0 {
		t.Fatalf("defaultGatewayConcurrency = %d, want 0 (unlimited)", defaultGatewayConcurrency)
	}
	g := &gatewayConcurrency{limit: defaultGatewayConcurrency}
	if g.Limit() != 0 {
		t.Fatalf("default limit = %d, want 0 (unlimited)", g.Limit())
	}
	rel, ok := g.TryAcquire()
	if !ok {
		t.Fatal("unlimited gateway should always admit")
	}
	rel()
	if g.Inflight() != 0 {
		t.Fatalf("inflight = %d after release, want 0", g.Inflight())
	}
}

func TestGatewayConcurrencyRejectsAtCapacity(t *testing.T) {
	g := &gatewayConcurrency{limit: 2}
	rel1, ok1 := g.TryAcquire()
	if !ok1 {
		t.Fatal("first acquire should succeed")
	}
	rel2, ok2 := g.TryAcquire()
	if !ok2 {
		t.Fatal("second acquire should succeed")
	}
	if _, ok := g.TryAcquire(); ok {
		t.Fatal("third acquire should be rejected at capacity")
	}
	if g.Inflight() != 2 {
		t.Fatalf("inflight = %d, want 2", g.Inflight())
	}
	rel1()
	rel3, ok3 := g.TryAcquire()
	if !ok3 {
		t.Fatal("acquire after one release should succeed")
	}
	rel2()
	rel3()
	if g.Inflight() != 0 {
		t.Fatalf("inflight = %d after all releases, want 0", g.Inflight())
	}
}

func TestGatewayConcurrencyReleaseIsIdempotent(t *testing.T) {
	g := &gatewayConcurrency{limit: 1}
	rel, ok := g.TryAcquire()
	if !ok {
		t.Fatal("acquire should succeed")
	}
	rel()
	rel() // double release must be harmless
	if g.Inflight() != 0 {
		t.Fatalf("inflight = %d, want 0", g.Inflight())
	}
	if _, ok := g.TryAcquire(); !ok {
		t.Fatal("gateway should admit again after release")
	}
}

func TestGatewayConcurrencySetLimit(t *testing.T) {
	g := &gatewayConcurrency{limit: 2}
	// Acquire one slot.
	rel1, ok1 := g.TryAcquire()
	if !ok1 {
		t.Fatal("first acquire should succeed")
	}
	// Lower the cap to 1; the in-flight request keeps its slot but new ones
	// must be rejected.
	g.SetLimit(1)
	if _, ok := g.TryAcquire(); ok {
		t.Fatal("at cap=1 with one in-flight, new acquire must be rejected")
	}
	rel1()
	if _, ok := g.TryAcquire(); !ok {
		t.Fatal("after release and cap=1, acquire should succeed")
	}
	if g.Snapshot()["limit"].(int) != 1 {
		t.Fatalf("limit = %v, want 1", g.Snapshot()["limit"])
	}
	g.SetLimit(0)
	if _, ok := g.TryAcquire(); !ok {
		t.Fatal("unlimited after SetLimit(0) should admit")
	}
}

func TestGatewayConcurrencyWaitForSlotBlocksUntilRelease(t *testing.T) {
	g := &gatewayConcurrency{limit: 1}
	rel, ok := g.TryAcquire()
	if !ok {
		t.Fatal("acquire should succeed")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := g.WaitForSlot(context.Background()); err != nil {
			t.Errorf("WaitForSlot after release failed: %v", err)
		}
	}()
	select {
	case <-done:
		t.Fatal("WaitForSlot returned while slot was still held")
	case <-time.After(50 * time.Millisecond):
	}
	rel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForSlot did not return after release")
	}
}

func TestGatewayConcurrencyWaitForSlotCancellation(t *testing.T) {
	g := &gatewayConcurrency{limit: 1}
	rel, ok := g.TryAcquire()
	if !ok {
		t.Fatal("acquire should succeed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := g.WaitForSlot(ctx); err == nil {
			t.Error("WaitForSlot should return an error on cancellation")
		}
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForSlot did not honor cancellation")
	}
	rel()
}

func TestGatewayConcurrencyConcurrentStress(t *testing.T) {
	g := &gatewayConcurrency{limit: 4}
	const workers = 8
	const rounds = 200
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < rounds; j++ {
				rel, ok := g.TryAcquire()
				if ok {
					// Simulate a short in-flight request.
					time.Sleep(time.Microsecond)
					rel()
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := g.Inflight(); got != 0 {
		t.Fatalf("inflight = %d after stress, want 0", got)
	}
	if g.Limit() != 4 {
		t.Fatalf("limit = %d, want 4", g.Limit())
	}
}

func TestGatewayAdmissionTracked(t *testing.T) {
	tracked := []string{
		"/api/chat",
		"/api/chat/stream",
		"/v1/chat/completions",
		"/v1/responses",
		"/v1/messages",
		"/v1/images/generations",
		"/api/conversations",
		"/api/accounts",
	}
	for _, p := range tracked {
		if !gatewayAdmissionTracked(p) {
			t.Errorf("path %q should be tracked", p)
		}
	}
	exempt := []string{
		"/api/health",
		"/api/version",
		"/api/update",
		"/api/admin/login",
		"/api/admin/settings",
		"/api/auth/start",
		"/api/auth/callback",
		"/",
		"/login",
		"/conversation",
		"/vendor/foo.js",
		"/v1/images/files/abc.png",
		"/v1/mcp/sse",
	}
	for _, p := range exempt {
		if gatewayAdmissionTracked(p) {
			t.Errorf("path %q should be exempt", p)
		}
	}
}
