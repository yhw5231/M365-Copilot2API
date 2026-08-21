package web

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/chathub"
)

// defaultAccountConcurrency bounds how many in-flight chat requests one M365
// account may carry at the same time. Upstream throttles accounts that are
// hammered concurrently, so the gateway queues excess requests per account
// instead of racing them (upstream PR #25).
const defaultAccountConcurrency = 1

type accountConcurrency struct {
	mu       sync.Mutex
	limit    int
	inflight map[string]int
	sessions map[string]map[string]int // accountID -> upstream session ID -> active request references
	changed  chan struct{}
}

func newAccountConcurrency() *accountConcurrency {
	limit := defaultAccountConcurrency
	if raw := strings.TrimSpace(os.Getenv("M365_ACCOUNT_DEFAULT_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	return &accountConcurrency{
		limit:    limit,
		inflight: map[string]int{},
		sessions: map[string]map[string]int{},
		changed:  make(chan struct{}),
	}
}

// Available reports whether the account currently has a free concurrency slot.
// SetLimit hot-updates the per-account concurrency limit and wakes blocked
// callers so they can re-evaluate availability immediately.
func (c *accountConcurrency) SetLimit(limit int) {
	if c == nil || limit < 1 {
		return
	}
	c.mu.Lock()
	if c.limit != limit {
		c.limit = limit
		close(c.changed)
		c.changed = make(chan struct{})
	}
	c.mu.Unlock()
}

func (c *accountConcurrency) Available(accountID string) bool {
	if c == nil || accountID == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[accountID] < c.limit
}

// Acquire blocks until the account has a free upstream-session slot (or ctx
// is done). Calls without a session ID retain request-based behavior for
// operations that have not created an upstream session yet.
func (c *accountConcurrency) Acquire(ctx context.Context, accountID string, sessionIDs ...string) (func(), error) {
	if c == nil || accountID == "" {
		return func() {}, nil
	}
	sessionID := ""
	if len(sessionIDs) > 0 {
		sessionID = strings.TrimSpace(sessionIDs[0])
	}
	// Requests without an established upstream session retain the original
	// request-based accounting semantics. They must not all share the empty
	// session key, because each such request may create a distinct session.
	if sessionID == "" {
		for {
			c.mu.Lock()
			if c.inflight[accountID] < c.limit {
				c.inflight[accountID]++
				c.mu.Unlock()
				var once sync.Once
				return func() {
					once.Do(func() {
						c.mu.Lock()
						if c.inflight[accountID] <= 1 {
							delete(c.inflight, accountID)
						} else {
							c.inflight[accountID]--
						}
						close(c.changed)
						c.changed = make(chan struct{})
						c.mu.Unlock()
					})
				}, nil
			}
			changed := c.changed
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-changed:
			}
		}
	}
	for {
		c.mu.Lock()
		if c.sessions == nil {
			c.sessions = map[string]map[string]int{}
		}
		accountSessions := c.sessions[accountID]
		if accountSessions == nil {
			accountSessions = map[string]int{}
			c.sessions[accountID] = accountSessions
		}
		refs := accountSessions[sessionID]
		canAcquire := refs > 0 || c.inflight[accountID] < c.limit
		if canAcquire {
			if refs == 0 {
				c.inflight[accountID]++
			}
			accountSessions[sessionID] = refs + 1
			c.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					c.mu.Lock()
					accountSessions := c.sessions[accountID]
					if accountSessions != nil {
						if accountSessions[sessionID] <= 1 {
							delete(accountSessions, sessionID)
							if c.inflight[accountID] <= 1 {
								delete(c.inflight, accountID)
							} else {
								c.inflight[accountID]--
							}
						} else {
							accountSessions[sessionID]--
						}
						if len(accountSessions) == 0 {
							delete(c.sessions, accountID)
						}
					}
					close(c.changed)
					c.changed = make(chan struct{})
					c.mu.Unlock()
				})
			}, nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

// Snapshot exposes the current limit and per-account in-flight counts for the
// admin console.
func (c *accountConcurrency) Snapshot() map[string]any {
	if c == nil {
		return map[string]any{"limit": defaultAccountConcurrency, "inflight": map[string]int{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	inflight := make(map[string]int, len(c.inflight))
	for accountID, count := range c.inflight {
		inflight[accountID] = count
	}
	return map[string]any{"limit": c.limit, "inflight": inflight}
}

// accountAvailable is the combined health gate: the account must be scheduled
// (not disabled), out of cooldown/auth-failure AND below its per-account
// concurrency limit.
func (s *Server) accountAvailable(accountID string) bool {
	if s.tokens != nil && !s.tokens.ScheduleEnabled(accountID) {
		return false
	}
	return s.accountPool.Available(accountID) && s.accountConcurrency.Available(accountID)
}

// accountClient selects the bound-proxy client when the account pins a proxy,
// falling back to the shared pool client. One account, one egress IP.
func (s *Server) accountClient(accountID string) *chathub.Client {
	if acc, ok := s.tokens.Get(accountID); ok && acc.BoundProxy != "" {
		return s.clientForProxy(acc.BoundProxy)
	}
	return s.chat
}

// chatWithAccount runs one chat request under the account's concurrency slot.
// When upstream supplies a Retry-After delay for a rate-limit response, the
// request waits for that delay and is replayed once unless its context ends.
func (s *Server) chatWithAccount(ctx context.Context, accountID string, account chathub.Account, request chathub.Request) (chathub.Result, error) {
	release, err := s.accountConcurrency.Acquire(ctx, accountID, request.SessionID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).Chat(ctx, account, request)
	if err != nil && IsRateLimited(err) {
		if retryAfter := RetryAfterSeconds(err); retryAfter > 0 {
			timer := time.NewTimer(time.Duration(retryAfter) * time.Second)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				s.markAccountResult(accountID, err)
				return result, ctx.Err()
			case <-timer.C:
			}
			if s.accountPool != nil {
				s.accountPool.MarkCall(accountID)
			}
			result, err = s.accountClient(accountID).Chat(ctx, account, request)
		}
	}
	s.markAccountResult(accountID, err)
	return result, err
}

// chatWithAccountEvents runs one streaming chat request under the account's
// concurrency slot.
func (s *Server) chatWithAccountEvents(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onEvent func(chathub.StreamEvent) error) (chathub.Result, error) {
	release, err := s.accountConcurrency.Acquire(ctx, accountID, request.SessionID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).ChatWithEvents(ctx, account, request, onEvent)
	s.markAccountResult(accountID, err)
	return result, err
}

// chatWithAccountReasoning runs one reasoning-stream chat request under the
// account's concurrency slot.
func (s *Server) chatWithAccountReasoning(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onDelta, onReasoning func(string) error) (chathub.Result, error) {
	release, err := s.accountConcurrency.Acquire(ctx, accountID, request.SessionID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).ChatWithReasoning(ctx, account, request, onDelta, onReasoning)
	s.markAccountResult(accountID, err)
	return result, err
}
