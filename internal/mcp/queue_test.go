package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestToolCallQueueRejectsWhenFull(t *testing.T) {
	q := NewToolCallQueue()
	q.capacity = 2
	q.ttl = time.Hour

	if call := q.Enqueue("first", nil); call == nil {
		t.Fatal("first enqueue unexpectedly failed")
	}
	if call := q.Enqueue("second", nil); call == nil {
		t.Fatal("second enqueue unexpectedly failed")
	}
	if call := q.Enqueue("third", nil); call != nil {
		t.Fatal("queue accepted a call beyond its configured capacity")
	}
	if got := q.PendingCount(); got != 2 {
		t.Fatalf("PendingCount()=%d, want 2", got)
	}
}

func TestToolCallQueueRemovesExpiredCallsBeforeEnqueue(t *testing.T) {
	q := NewToolCallQueue()
	q.capacity = 1
	q.ttl = time.Millisecond

	expired := q.Enqueue("expired", nil)
	if expired == nil {
		t.Fatal("initial enqueue unexpectedly failed")
	}
	expired.CreatedAt = time.Now().Add(-time.Second)

	replacement := q.Enqueue("replacement", nil)
	if replacement == nil {
		t.Fatal("expired entry was not removed before accepting replacement")
	}
	select {
	case err := <-expired.ErrCh:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired call error=%v, want context deadline exceeded", err)
		}
	default:
		t.Fatal("expired call waiter was not notified")
	}
	if got := q.PendingCount(); got != 1 {
		t.Fatalf("PendingCount()=%d, want 1", got)
	}
}

func TestToolCallQueueRemoveCancelledCall(t *testing.T) {
	q := NewToolCallQueue()
	call := q.Enqueue("cancelled", nil)
	if call == nil {
		t.Fatal("enqueue unexpectedly failed")
	}
	if !q.Remove(call) {
		t.Fatal("Remove returned false for a queued call")
	}
	if q.Remove(call) {
		t.Fatal("Remove returned true for an already removed call")
	}
	if got := q.PendingCount(); got != 0 {
		t.Fatalf("PendingCount()=%d, want 0", got)
	}
}

func TestMCPToolProviderAppliesQueueBackpressure(t *testing.T) {
	q := NewToolCallQueue()
	q.capacity = 1
	q.ttl = time.Hour
	if call := q.Enqueue("occupied", nil); call == nil {
		t.Fatal("initial enqueue unexpectedly failed")
	}

	provider := NewMCPToolProvider(nil, q)
	_, err := provider.CallTool(context.Background(), "overflow", nil)
	if !errors.Is(err, ErrToolCallQueueFull) {
		t.Fatalf("CallTool() error=%v, want ErrToolCallQueueFull", err)
	}
}

func TestMCPToolProviderExecutesCallsConcurrently(t *testing.T) {
	q := NewToolCallQueue()
	provider := NewMCPToolProvider(nil, q)

	type outcome struct {
		name   string
		result CallResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, name := range []string{"first", "second"} {
		name := name
		go func() {
			result, err := provider.CallTool(context.Background(), name, map[string]any{"name": name})
			outcomes <- outcome{name: name, result: result, err: err}
		}()
	}

	deadline := time.Now().Add(time.Second)
	for q.PendingCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := q.PendingCount(); got != 2 {
		t.Fatalf("PendingCount()=%d, want 2 concurrent calls", got)
	}

	first := q.DequeueNonBlocking()
	second := q.DequeueNonBlocking()
	if first == nil || second == nil {
		t.Fatal("concurrent calls were not independently queued")
	}

	q.Resolve(second, CallResult{Content: []map[string]any{{"type": "text", "text": second.Name}}}, nil)
	q.Resolve(first, CallResult{Content: []map[string]any{{"type": "text", "text": first.Name}}}, nil)

	for i := 0; i < 2; i++ {
		select {
		case got := <-outcomes:
			if got.err != nil {
				t.Fatalf("CallTool(%s) error=%v", got.name, got.err)
			}
			if len(got.result.Content) != 1 {
				t.Fatalf("CallTool(%s) returned %d content items, want 1", got.name, len(got.result.Content))
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent tool call did not complete")
		}
	}
}

func TestMCPToolProviderTimeoutDoesNotBlockOtherCalls(t *testing.T) {
	q := NewToolCallQueue()
	q.ttl = 50 * time.Millisecond
	provider := NewMCPToolProvider(nil, q)

	type outcome struct {
		name   string
		result CallResult
		err    error
	}
	outcomes := make(chan outcome, 2)

	go func() {
		result, err := provider.CallTool(context.Background(), "fast", nil)
		outcomes <- outcome{name: "fast", result: result, err: err}
	}()

	deadline := time.Now().Add(time.Second)
	for q.PendingCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := q.PendingCount(); got != 1 {
		t.Fatalf("PendingCount()=%d, want fast call queued", got)
	}

	go func() {
		result, err := provider.CallTool(context.Background(), "stalled", nil)
		outcomes <- outcome{name: "stalled", result: result, err: err}
	}()

	deadline = time.Now().Add(time.Second)
	for q.PendingCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := q.PendingCount(); got != 2 {
		t.Fatalf("PendingCount()=%d, want two independent calls", got)
	}

	fast := q.DequeueNonBlocking()
	if fast == nil || fast.Name != "fast" {
		t.Fatalf("first dequeued call=%v, want fast", fast)
	}
	q.Resolve(fast, CallResult{Content: []map[string]any{{"type": "text", "text": "done"}}}, nil)

	first := <-outcomes
	if first.name != "fast" {
		t.Fatalf("first completed call=%q, want fast", first.name)
	}
	if first.err != nil {
		t.Fatalf("fast CallTool() error=%v", first.err)
	}

	select {
	case stalled := <-outcomes:
		if stalled.name != "stalled" {
			t.Fatalf("second completed call=%q, want stalled", stalled.name)
		}
		if stalled.err == nil || !strings.Contains(stalled.err.Error(), "timed out") {
			t.Fatalf("stalled CallTool() error=%v, want explicit timeout error", stalled.err)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled call did not time out")
	}

	if got := q.PendingCount(); got != 0 {
		t.Fatalf("PendingCount()=%d after timeout, want 0", got)
	}
}
