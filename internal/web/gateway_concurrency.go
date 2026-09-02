package web

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultGatewayConcurrency is the default maximum number of concurrent
// requests the whole gateway accepts at once, independent of the per-account
// limit. 0 disables the gateway-wide cap (unlimited), which preserves the
// behavior of older builds where only per-account concurrency applied. Set
// M365_GATEWAY_CONCURRENCY (or the web-console setting gatewayConcurrency) to
// a positive number to protect the server from being overwhelmed: once the
// in-flight count reaches the cap, new requests are rejected immediately with
// an error instead of being queued.
const defaultGatewayConcurrency = 0

// gatewayConcurrency is the gateway-wide admission limiter. Unlike the
// per-account limiter (accountConcurrency) it does not queue: a request that
// arrives while the cap is reached is rejected right away so the server can
// shed load instead of piling up work.
type gatewayConcurrency struct {
	limit    int32
	inflight int32
}

func newGatewayConcurrency() *gatewayConcurrency {
	limit := int32(defaultGatewayConcurrency)
	if raw := strings.TrimSpace(os.Getenv("M365_GATEWAY_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 0 {
			limit = int32(parsed)
		}
	}
	return &gatewayConcurrency{limit: limit}
}

// TryAcquire attempts to reserve one gateway slot without blocking. It returns
// ok=false when the gateway is at capacity, in which case the caller should
// reject the request with an error immediately. When ok is true the returned
// release function must be called exactly once when the request completes.
func (c *gatewayConcurrency) TryAcquire() (release func(), ok bool) {
	if c == nil {
		return func() {}, true
	}
	limit := atomic.LoadInt32(&c.limit)
	if limit <= 0 {
		return func() {}, true // unlimited
	}
	for {
		cur := atomic.LoadInt32(&c.inflight)
		if cur >= limit {
			return nil, false
		}
		if atomic.CompareAndSwapInt32(&c.inflight, cur, cur+1) {
			var once sync.Once
			return func() {
				once.Do(func() {
					atomic.AddInt32(&c.inflight, -1)
				})
			}, true
		}
	}
}

// SetLimit hot-updates the gateway-wide concurrency cap. A value of 0 disables
// the cap (unlimited). Lowering the cap below the current in-flight count
// takes effect for new requests only; in-flight requests are never cut off.
func (c *gatewayConcurrency) SetLimit(limit int) {
	if c == nil || limit < 0 {
		return
	}
	atomic.StoreInt32(&c.limit, int32(limit))
}

// WaitForSlot blocks until the gateway has a free admission slot or ctx is
// done, then immediately releases it. Streaming handlers call this BEFORE
// emitting any stream preamble so a queued request fails with a clean HTTP
// error instead of a half-written SSE stream.
func (c *gatewayConcurrency) WaitForSlot(ctx context.Context) error {
	if c == nil {
		return nil
	}
	limit := atomic.LoadInt32(&c.limit)
	if limit <= 0 {
		return nil // unlimited
	}
	for {
		cur := atomic.LoadInt32(&c.inflight)
		if cur < limit {
			if atomic.CompareAndSwapInt32(&c.inflight, cur, cur+1) {
				atomic.AddInt32(&c.inflight, -1)
				return nil
			}
			continue
		}
		// At capacity: poll with a short sleep, checking for context
		// cancellation on each tick.
		timer := time.NewTimer(gatewayWaitPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// gatewayWaitPollInterval is how long WaitForSlot sleeps between re-checks
// when the gateway is at capacity. A short interval keeps latency low while
// avoiding a busy loop.
const gatewayWaitPollInterval = 100 * time.Millisecond

// Limit returns the current gateway-wide concurrency cap (0 = unlimited).
func (c *gatewayConcurrency) Limit() int {
	if c == nil {
		return defaultGatewayConcurrency
	}
	return int(atomic.LoadInt32(&c.limit))
}

// Inflight returns the number of currently admitted gateway requests.
func (c *gatewayConcurrency) Inflight() int {
	if c == nil {
		return 0
	}
	return int(atomic.LoadInt32(&c.inflight))
}

// Snapshot exposes the cap and current in-flight count for the admin console.
func (c *gatewayConcurrency) Snapshot() map[string]any {
	return map[string]any{
		"limit":    c.Limit(),
		"inflight": c.Inflight(),
	}
}
