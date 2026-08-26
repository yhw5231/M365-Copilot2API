package web

import (
	"context"
	"encoding/json"
	"log"
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

// defaultAccountWarmSessionSeconds is how long a session keeps its concurrency
// slot reserved after releasing it. During the window the returning warm
// session gets its slot back immediately and new/cold sessions cannot use the
// reserved slot ("对话保留 3 分钟优先级"). Override at startup with
// M365_ACCOUNT_WARM_SESSION_SECONDS, or at runtime from the web console
// (accountWarmSessionSeconds).
const defaultAccountWarmSessionSeconds = 180

// defaultAccountQueueTimeoutSeconds bounds how long a cold session may sit in
// a per-account FIFO queue before the request fails with HTTP 429 ("排队也要
// 有时间，默认超过 10 秒返回 429"). Override at startup with
// M365_ACCOUNT_QUEUE_TIMEOUT_SECONDS, or at runtime from the web console
// (accountQueueTimeoutSeconds).
const defaultAccountQueueTimeoutSeconds = 10

// warmSessionWindow returns the effective warm/reservation window. The
// persisted web-console setting wins; the legacy M365_ACCOUNT_WARM_SESSION_WINDOW
// (Go duration) environment variable is honored as a fallback for older
// deployments.
func warmSessionWindow() time.Duration {
	if sec := currentSettings().AccountWarmSessionSeconds; sec > 0 {
		return time.Duration(sec) * time.Second
	}
	if raw := strings.TrimSpace(os.Getenv("M365_ACCOUNT_WARM_SESSION_WINDOW")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return time.Duration(defaultAccountWarmSessionSeconds) * time.Second
}

type accountConcurrency struct {
	mu         sync.Mutex
	limit      int
	inflight   map[string]int
	sessions   map[string]map[string]int // accountID -> upstream session ID -> active request references
	waiters    map[string][]uint64       // accountID -> FIFO waiter tickets (cold sessions only)
	nextTicket uint64
	// warmSessions reserves the just-released slot for a returning session
	// ("accountID\x00sessionID" -> reserved until). While reserved, the session
	// acquires immediately and other (cold) sessions cannot use that slot.
	warmSessions map[string]time.Time
	// reservedCount is the per-account number of active slot reservations, so
	// unreserved capacity = limit - inflight - reservedCount.
	reservedCount map[string]int
	changed       chan struct{}
	// queueTimeoutOverride lets tests bound the queue wait without touching the
	// global settings store; zero means "use the configured setting".
	queueTimeoutOverride time.Duration
}

func newAccountConcurrency() *accountConcurrency {
	limit := defaultAccountConcurrency
	if raw := strings.TrimSpace(os.Getenv("M365_ACCOUNT_DEFAULT_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
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

// Available reports whether the account currently has a free unreserved slot
// for a brand-new session. A returning warm session is not gated by this check
// — Acquire honors its reservation directly.
func (c *accountConcurrency) Available(accountID string) bool {
	if c == nil || accountID == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unreservedLocked(accountID) > 0
}

// HasUnreservedSlot reports whether the account has capacity that is neither in
// flight nor reserved for a returning warm session.
func (c *accountConcurrency) HasUnreservedSlot(accountID string) bool {
	if c == nil || accountID == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unreservedLocked(accountID) > 0
}

// HasReservation reports whether the session currently holds a reserved slot on
// the account (i.e. it is still within its warm window).
func (c *accountConcurrency) HasReservation(accountID, sessionID string) bool {
	if c == nil || accountID == "" || sessionID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reservationActiveLocked(accountID, sessionID)
}

// queueTimeout returns how long a queued cold session may wait for a slot
// before Acquire fails with errQueueTimeout. Zero means wait indefinitely.
// The web-console setting wins; the override field exists for tests.
func (c *accountConcurrency) queueTimeout() time.Duration {
	if c != nil && c.queueTimeoutOverride > 0 {
		return c.queueTimeoutOverride
	}
	if sec := currentSettings().AccountQueueTimeoutSeconds; sec > 0 {
		return time.Duration(sec) * time.Second
	}
	return 0
}

// WaitForSlot blocks until the session may start on the account (its reserved
// warm slot, an existing in-flight request, or unreserved capacity), bounded by
// the queue timeout. It acquires and immediately releases the slot, so
// streaming handlers can decide BEFORE emitting any stream preamble whether the
// request would otherwise just queue; on timeout it returns errQueueTimeout
// (surfaced to the client as HTTP 429).
func (c *accountConcurrency) WaitForSlot(ctx context.Context, accountID, sessionID string) error {
	if c == nil || accountID == "" {
		return nil
	}
	release, err := c.Acquire(ctx, accountID, sessionID)
	if err != nil {
		return err
	}
	release()
	return nil
}

// queueWaitTimer arms a timer that fires when the queued waiter's budget runs
// out. It returns a nil timer when there is nothing to wait for.
func queueWaitTimer(timeout time.Duration, ticket uint64, queuedAt time.Time) (*time.Timer, <-chan time.Time) {
	if ticket == 0 || timeout <= 0 || queuedAt.IsZero() {
		return nil, nil
	}
	remaining := timeout - time.Since(queuedAt)
	if remaining < 0 {
		remaining = 0
	}
	t := time.NewTimer(remaining)
	return t, t.C
}

// stopTimer stops a timer without leaking its channel.
func stopTimer(t *time.Timer) {
	if t == nil {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

func (c *accountConcurrency) unreservedLocked(accountID string) int {
	return c.limit - c.inflight[accountID] - c.reservedCount[accountID]
}

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

func (c *accountConcurrency) enqueueWaiterLocked(accountID string) uint64 {
	if c.waiters == nil {
		c.waiters = map[string][]uint64{}
	}
	c.nextTicket++
	ticket := c.nextTicket
	c.waiters[accountID] = append(c.waiters[accountID], ticket)
	return ticket
}

func (c *accountConcurrency) waiterIsFirstLocked(accountID string, ticket uint64) bool {
	queue := c.waiters[accountID]
	return len(queue) > 0 && queue[0] == ticket
}

func (c *accountConcurrency) removeWaiterLocked(accountID string, ticket uint64) {
	queue := c.waiters[accountID]
	for i, queuedTicket := range queue {
		if queuedTicket != ticket {
			continue
		}
		queue = append(queue[:i], queue[i+1:]...)
		if len(queue) == 0 {
			delete(c.waiters, accountID)
		} else {
			c.waiters[accountID] = queue
		}
		return
	}
}

// accountSessionKey builds the reservation map key for one account+session.
func accountSessionKey(accountID, sessionID string) string {
	return accountID + "\x00" + sessionID
}

// reservationActiveLocked reports whether the session still holds a reserved
// slot. Expired reservations are lazily released (reservedCount decremented).
func (c *accountConcurrency) reservationActiveLocked(accountID, sessionID string) bool {
	key := accountSessionKey(accountID, sessionID)
	until, ok := c.warmSessions[key]
	if !ok {
		return false
	}
	if until.After(time.Now()) {
		return true
	}
	delete(c.warmSessions, key)
	if c.reservedCount[accountID] > 0 {
		c.reservedCount[accountID]--
	}
	return false
}

// clearReservationLocked consumes a reservation when the warm session re-acquires.
func (c *accountConcurrency) clearReservationLocked(accountID, sessionID string) {
	key := accountSessionKey(accountID, sessionID)
	if _, ok := c.warmSessions[key]; ok {
		delete(c.warmSessions, key)
		if c.reservedCount[accountID] > 0 {
			c.reservedCount[accountID]--
		}
	}
}

// reserveSlotLocked marks the just-released slot as reserved for the session.
func (c *accountConcurrency) reserveSlotLocked(accountID, sessionID string) {
	if c.warmSessions == nil {
		c.warmSessions = map[string]time.Time{}
	}
	if c.reservedCount == nil {
		c.reservedCount = map[string]int{}
	}
	c.warmSessions[accountSessionKey(accountID, sessionID)] = time.Now().Add(warmSessionWindow())
	c.reservedCount[accountID]++
	c.pruneWarmSessionsLocked()
}

// pruneWarmSessionsLocked bounds reservation-map growth on long-running
// processes; expired entries older than twice the window are evicted.
func (c *accountConcurrency) pruneWarmSessionsLocked() {
	if len(c.warmSessions) < 1024 {
		return
	}
	cutoff := time.Now().Add(-2 * warmSessionWindow())
	for key, until := range c.warmSessions {
		if until.Before(cutoff) {
			delete(c.warmSessions, key)
			if idx := strings.Index(key, "\x00"); idx > 0 {
				if c.reservedCount[key[:idx]] > 0 {
					c.reservedCount[key[:idx]]--
				}
			}
		}
	}
}

// Acquire blocks until the account has a free upstream-session slot (or ctx
// is done). Calls without a session ID retain request-based behavior for
// operations that have not created an upstream session yet.
//
// Scheduling model:
//   - refs > 0  (session with an in-flight request)  -> bypasses the limit.
//   - reserved  (session within its warm window)     -> immediate acquire of
//     the slot that was set aside for it; cold sessions cannot cut in.
//   - otherwise (cold session / new request)         -> FIFO queue, only uses
//     unreserved capacity.
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
		var ticket uint64
		var queuedAt time.Time
		for {
			timeout := c.queueTimeout()
			c.mu.Lock()
			if ticket == 0 && c.unreservedLocked(accountID) <= 0 {
				ticket = c.enqueueWaiterLocked(accountID)
				queuedAt = time.Now()
			}
			// A waiter that has spent its whole queue budget fails with 429,
			// even if a slot happens to free at the same instant.
			if ticket != 0 && !queuedAt.IsZero() && timeout > 0 && time.Since(queuedAt) >= timeout {
				c.removeWaiterLocked(accountID, ticket)
				close(c.changed)
				c.changed = make(chan struct{})
				c.mu.Unlock()
				return nil, errQueueTimeout
			}
			canAcquire := c.unreservedLocked(accountID) > 0 && (ticket == 0 || c.waiterIsFirstLocked(accountID, ticket))
			if canAcquire {
				if ticket != 0 {
					c.removeWaiterLocked(accountID, ticket)
				}
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
			timer, timerC := queueWaitTimer(timeout, ticket, queuedAt)
			changed := c.changed
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				stopTimer(timer)
				c.mu.Lock()
				if ticket != 0 {
					c.removeWaiterLocked(accountID, ticket)
				}
				close(c.changed)
				c.changed = make(chan struct{})
				c.mu.Unlock()
				return nil, ctx.Err()
			case <-changed:
				stopTimer(timer)
			case <-timerC:
				c.mu.Lock()
				if ticket != 0 {
					c.removeWaiterLocked(accountID, ticket)
					close(c.changed)
					c.changed = make(chan struct{})
				}
				c.mu.Unlock()
				return nil, errQueueTimeout
			}
		}
	}
	var ticket uint64
	var queuedAt time.Time
	for {
		timeout := c.queueTimeout()
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
		reserved := c.reservationActiveLocked(accountID, sessionID)
		if ticket == 0 && !(refs > 0 || reserved || c.unreservedLocked(accountID) > 0) {
			ticket = c.enqueueWaiterLocked(accountID)
			queuedAt = time.Now()
		}
		// A waiter that has spent its whole queue budget fails with 429, even
		// if a slot happens to free at the same instant.
		if ticket != 0 && !queuedAt.IsZero() && timeout > 0 && time.Since(queuedAt) >= timeout {
			c.removeWaiterLocked(accountID, ticket)
			close(c.changed)
			c.changed = make(chan struct{})
			c.mu.Unlock()
			return nil, errQueueTimeout
		}
		canAcquire := (refs > 0 || reserved || c.unreservedLocked(accountID) > 0) && (ticket == 0 || c.waiterIsFirstLocked(accountID, ticket))
		if canAcquire {
			if ticket != 0 {
				c.removeWaiterLocked(accountID, ticket)
			}
			if refs == 0 {
				c.inflight[accountID]++
				if reserved {
					c.clearReservationLocked(accountID, sessionID)
				}
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
							c.reserveSlotLocked(accountID, sessionID)
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
		timer, timerC := queueWaitTimer(timeout, ticket, queuedAt)
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			stopTimer(timer)
			c.mu.Lock()
			if ticket != 0 {
				c.removeWaiterLocked(accountID, ticket)
			}
			close(c.changed)
			c.changed = make(chan struct{})
			c.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
			stopTimer(timer)
		case <-timerC:
			c.mu.Lock()
			if ticket != 0 {
				c.removeWaiterLocked(accountID, ticket)
				close(c.changed)
				c.changed = make(chan struct{})
			}
			c.mu.Unlock()
			return nil, errQueueTimeout
		}
	}
}

// Snapshot exposes the current limit and per-account in-flight / waiting /
// reserved counts for the admin console.
func (c *accountConcurrency) Snapshot() map[string]any {
	if c == nil {
		return map[string]any{
			"limit":    defaultAccountConcurrency,
			"inflight": map[string]int{},
			"waiting":  map[string]int{},
			"reserved": map[string]int{},
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	inflight := make(map[string]int, len(c.inflight))
	for accountID, count := range c.inflight {
		inflight[accountID] = count
	}
	waiting := make(map[string]int, len(c.waiters))
	for accountID, queue := range c.waiters {
		waiting[accountID] = len(queue)
	}
	reserved := make(map[string]int, len(c.reservedCount))
	for accountID, count := range c.reservedCount {
		reserved[accountID] = count
	}
	return map[string]any{"limit": c.limit, "inflight": inflight, "waiting": waiting, "reserved": reserved}
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
	if IsEmptyCompletion(err) {
		// ChatHub occasionally emits a successful completion frame without any
		// assistant text after arbitrary model turns, including ordinary reads and
		// final status updates. Retry the generation once centrally instead of
		// limiting recovery to a particular tool or endpoint.
		log.Printf("[empty-completion] account=%s retrying completed response with no content", accountID)
		if s.accountPool != nil {
			s.accountPool.MarkCall(accountID)
		}
		result, err = s.accountClient(accountID).Chat(ctx, account, request)
	}
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

// chatWithAccountRawEvents runs one streaming chat request under the account's
// concurrency slot, forwarding every raw upstream SignalR frame to onRaw as it
// arrives (no buffering until completion).
func (s *Server) chatWithAccountRawEvents(ctx context.Context, accountID string, account chathub.Account, request chathub.Request, onRaw func(json.RawMessage) error) (chathub.Result, error) {
	release, err := s.accountConcurrency.Acquire(ctx, accountID, request.SessionID)
	if err != nil {
		return chathub.Result{}, err
	}
	defer release()
	if s.accountPool != nil {
		s.accountPool.MarkCall(accountID)
	}
	result, err := s.accountClient(accountID).ChatWithRawEvents(ctx, account, request, onRaw)
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
