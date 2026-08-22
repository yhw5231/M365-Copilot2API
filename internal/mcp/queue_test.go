package mcp

import (
	"context"
	"errors"
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
