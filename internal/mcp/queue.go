package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// PendingToolCall represents a tool call that is waiting to be executed by the client.
type PendingToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	ResultCh  chan CallResult
	ErrCh     chan error
	CreatedAt time.Time
}

// ToolCallQueue manages pending MCP tool calls and their results.
// It allows the MCP server's onCall handler to block until the client
// executes the tool and returns the result.
const (
	defaultToolCallQueueCapacity = 128
	defaultToolCallQueueTTL      = 30 * time.Second
)

var ErrToolCallQueueFull = errors.New("MCP tool call queue is full")

// ToolCallQueue manages a bounded set of pending calls. Entries that are
// cancelled or exceed the configured TTL are removed before new work is
// accepted, preventing an unavailable client from causing unbounded growth.
type ToolCallQueue struct {
	mu       sync.Mutex
	pending  []*PendingToolCall
	nextID   int64
	capacity int
	ttl      time.Duration
}

// NewToolCallQueue creates a bounded tool call queue using secure defaults.
func NewToolCallQueue() *ToolCallQueue {
	return &ToolCallQueue{
		capacity: defaultToolCallQueueCapacity,
		ttl:      defaultToolCallQueueTTL,
	}
}

// Enqueue adds a tool call to the bounded queue. It returns nil when the
// queue remains at capacity after expired entries have been removed.
func (q *ToolCallQueue) Enqueue(name string, arguments map[string]any) *PendingToolCall {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.removeExpiredLocked(time.Now())
	if len(q.pending) >= q.capacity {
		return nil
	}

	q.nextID++
	call := &PendingToolCall{
		ID:        fmt.Sprintf("mcp-tool-%d", q.nextID),
		Name:      name,
		Arguments: arguments,
		ResultCh:  make(chan CallResult, 1),
		ErrCh:     make(chan error, 1),
		CreatedAt: time.Now(),
	}
	q.pending = append(q.pending, call)
	return call
}

func (q *ToolCallQueue) removeExpiredLocked(now time.Time) {
	if q.ttl <= 0 || len(q.pending) == 0 {
		return
	}

	kept := q.pending[:0]
	for _, call := range q.pending {
		if now.Sub(call.CreatedAt) < q.ttl {
			kept = append(kept, call)
			continue
		}
		select {
		case call.ErrCh <- context.DeadlineExceeded:
		default:
		}
	}
	q.pending = kept
}

// Remove deletes a queued call that timed out or whose caller was cancelled.
func (q *ToolCallQueue) Remove(call *PendingToolCall) bool {
	if call == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, pending := range q.pending {
		if pending != call {
			continue
		}
		copy(q.pending[i:], q.pending[i+1:])
		q.pending[len(q.pending)-1] = nil
		q.pending = q.pending[:len(q.pending)-1]
		return true
	}
	return false
}

// Dequeue waits for and returns the next pending tool call.
// Returns nil if the context is cancelled or no call arrives before the context deadline.
func (q *ToolCallQueue) Dequeue(ctx context.Context) *PendingToolCall {
	for {
		q.mu.Lock()
		if len(q.pending) > 0 {
			call := q.pending[0]
			q.pending = q.pending[1:]
			q.mu.Unlock()
			return call
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// DequeueNonBlocking returns the next pending tool call without waiting.
// Returns nil if no pending calls.
func (q *ToolCallQueue) DequeueNonBlocking() *PendingToolCall {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.pending) == 0 {
		return nil
	}
	call := q.pending[0]
	q.pending = q.pending[1:]
	return call
}

// Resolve sends the result for a pending tool call, unblocking the onCall handler.
func (q *ToolCallQueue) Resolve(call *PendingToolCall, result CallResult, err error) {
	if err != nil {
		select {
		case call.ErrCh <- err:
		default:
		}
	} else {
		select {
		case call.ResultCh <- result:
		default:
		}
	}
}

// PendingCount returns the number of pending tool calls.
func (q *ToolCallQueue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// NewMCPToolProvider creates a ToolProvider that uses the ToolCallQueue for async tool execution.
func NewMCPToolProvider(tools []Tool, queue *ToolCallQueue) *MCPToolProvider {
	return &MCPToolProvider{
		tools: tools,
		queue: queue,
	}
}

// MCPToolProvider is a ToolProvider that enqueues tool calls for async execution.
type MCPToolProvider struct {
	mu    sync.RWMutex
	tools []Tool
	queue *ToolCallQueue
}

func (p *MCPToolProvider) ListTools(ctx context.Context) ([]Tool, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]Tool(nil), p.tools...), nil
}

func (p *MCPToolProvider) CallTool(ctx context.Context, name string, arguments map[string]any) (CallResult, error) {
	// Enqueue the tool call for the main flow to pick up. Reject new work
	// explicitly when the bounded queue is full instead of dereferencing nil
	// or allowing unbounded memory growth.
	call := p.queue.Enqueue(name, arguments)
	if call == nil {
		return CallResult{}, ErrToolCallQueueFull
	}

	// Each call gets its own timer, so a stalled tool cannot block unrelated
	// calls. Remove the timed-out call from the queue before returning.
	timer := time.NewTimer(p.queue.ttl)
	defer timer.Stop()

	select {
	case result := <-call.ResultCh:
		return result, nil
	case err := <-call.ErrCh:
		return CallResult{}, err
	case <-timer.C:
		p.queue.Remove(call)
		return CallResult{}, fmt.Errorf("MCP tool call %q timed out after %s", name, p.queue.ttl)
	case <-ctx.Done():
		p.queue.Remove(call)
		return CallResult{}, ctx.Err()
	}
}

// UpdateTools replaces the tool list for the provider.
func (p *MCPToolProvider) UpdateTools(tools []Tool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools = append([]Tool(nil), tools...)
}
