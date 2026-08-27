package web

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Execution environment header so clients can identify the deployment boundary
// and reject hosted/container fallback results.
const executionEnvironmentHeader = "X-M365-Execution-Environment"
const executionEnvironmentValue = "m365-copilot2api-relay"

// setSSEHeaders sets the common SSE response headers, including the execution
// environment header.
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set(executionEnvironmentHeader, executionEnvironmentValue)
}

// sseKeepalive periodically writes SSE comment frames during long silent
// phases (upstream reasoning, backpressure) so client and proxy idle timers
// do not expire. All SSE payload writes that may race with the keepalive
// goroutine must go through lockedWrite/lockedWriteCtx to share the mutex.
type sseKeepalive struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	f        http.Flusher
	interval time.Duration
	done     chan struct{}
	once     sync.Once
}

// startSSEKeepalive starts a background goroutine that sends SSE comment
// frames every interval. Call stop() when the stream is complete.
func startSSEKeepalive(w http.ResponseWriter, f http.Flusher, interval time.Duration) *sseKeepalive {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	k := &sseKeepalive{w: w, f: f, interval: interval, done: make(chan struct{})}
	go k.loop()
	return k
}

func (k *sseKeepalive) loop() {
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-ticker.C:
			k.mu.Lock()
			_ = writeDeadline(writeContext{k.w, k.f}, ": keep-alive\n\n")
			k.mu.Unlock()
		}
	}
}

// stop stops the keepalive goroutine. Safe to call multiple times.
func (k *sseKeepalive) stop() {
	k.once.Do(func() { close(k.done) })
}

// lockedWrite writes a complete SSE frame under the same mutex the keepalive
// goroutine uses, so frames never interleave with keep-alive comments.
func (k *sseKeepalive) lockedWrite(payload string) error {
	if k == nil {
		return writeDeadline(writeContext{k.w, k.f}, payload)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return writeDeadline(writeContext{k.w, k.f}, payload)
}

// lockedWriteCtx checks the request context before writing, mirroring the
// sseRaw behavior while sharing the keepalive mutex.
func (k *sseKeepalive) lockedWriteCtx(ctx context.Context, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if k == nil {
		return writeDeadline(writeContext{k.w, k.f}, payload)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return writeDeadline(writeContext{k.w, k.f}, payload)
}

// writeContext bundles the writer and flusher for helper functions.
type writeContext struct {
	w http.ResponseWriter
	f http.Flusher
}

// writeDeadline writes payload and flushes, with a 30-second write deadline.
func writeDeadline(wc writeContext, payload string) error {
	rc := http.NewResponseController(wc.w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(wc.w, payload); err != nil {
		return err
	}
	if wc.f != nil {
		wc.f.Flush()
	}
	return nil
}