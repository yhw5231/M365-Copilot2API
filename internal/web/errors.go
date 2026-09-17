package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func logOAuthError(stage string, err error) {
	var oauthErr *auth.OAuthError
	if errors.As(err, &oauthErr) {
		log.Printf("oauth_error stage=%s error=%q aadsts=%q http_status=%d correlation_id=%q trace_id=%q", stage, oauthErr.Code, oauthErr.AADSTS, oauthErr.HTTPStatus, oauthErr.CorrelationID, oauthErr.TraceID)
		return
	}
	log.Printf("oauth_error stage=%s error=%q", stage, "request_failed")
}

// isTransportFailure reports whether err is a transport-class upstream
// failure — the request reached the WebSocket layer and died on the network
// (dial error, silent drop, read timeout) or as an unknown protocol error;
// the same class upstreamStatus maps to HTTP 502. Classes with dedicated
// recovery (rate-limit failover, auth, empty-completion and rate-limit
// replays, queue timeouts, tone fallback, content-filter termination) are
// excluded so their own handling is not double-triggered, and so is a spent
// caller context: replaying into an already-expired deadline is pointless.
// These are the only errors a single same-account replay can fix, because
// nothing was produced for them — the M365 edge occasionally accepts a
// submit and then goes completely silent (no frames, no SignalR pings) until
// the ws read deadline kills the turn.
func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	if IsRateLimited(err) || IsAuthFailure(err) || IsEmptyCompletion(err) || IsQueueTimeout(err) || IsLocalCapacity(err) {
		return false
	}
	if errors.Is(err, errContentFilterHit) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// isRouterProbeTransportFailure reports whether a tool-router probe failed in
// the transport layer before the model produced any decision (dial error,
// silent drop, read timeout, spent probe window, no healthy proxy node).
// These failures never yielded a model answer, so a required tool_choice must
// not hard-fail on them: the answer stream below carries the same tool
// definitions and can still surface calls through the native/tool-shaped
// detection paths. Unlike isTransportFailure, a spent context counts as
// transport here — the probe runs on its own bounded window (routerProbeWindow),
// and the router fall-through re-derives a fresh deadline from the client
// connection, so a probe-window expiry is precisely the transient network
// stall the answer stream can recover from. Model-level failures (empty
// completion, rate limit, auth, queue, capacity) keep their dedicated
// handling and still hard-fail a required round.
func isRouterProbeTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	if IsRateLimited(err) || IsAuthFailure(err) || IsEmptyCompletion(err) || IsQueueTimeout(err) || IsLocalCapacity(err) {
		return false
	}
	if errors.Is(err, errContentFilterHit) {
		return false
	}
	return true
}

// upstreamError keeps transport details, including URLs and credentials, out
// of client-visible responses while retaining a server-side diagnostic.
func upstreamError(err error) string {
	if err == nil {
		return "upstream request failed"
	}
	log.Printf("upstream request failed: %v", err)
	return "upstream request failed"
}

// errQueueTimeout is returned by accountConcurrency.Acquire when a cold
// session has been queued past the configured queue-wait bound. It is
// deliberately NOT an UpstreamHTTPError{Status: 429}, so it never triggers
// failover or rate-limit handling: the client just receives HTTP 503 telling it
// to retry shortly.
var errQueueTimeout = errors.New("concurrency queue wait timed out")

// IsQueueTimeout reports whether err is a concurrency queue-wait timeout.
func IsQueueTimeout(err error) bool {
	return errors.Is(err, errQueueTimeout)
}

// upstreamStatus maps a failed upstream call to the client-visible HTTP status:
// gateway-local overload (queue timeout or no free concurrency) becomes 503,
// genuine upstream rate limits stay 429 (with Retry-After when known), auth
// failures become 401, everything else is 502. Unknown upstream failures must
// never leak internals.
func upstreamStatus(err error) int {
	if IsQueueTimeout(err) {
		return http.StatusServiceUnavailable
	}
	if IsLocalCapacity(err) {
		return http.StatusServiceUnavailable
	}
	if IsRateLimited(err) {
		return http.StatusTooManyRequests
	}
	if IsAuthFailure(err) {
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

// IsEmptyCompletion reports whether the upstream returned an empty completion
// because the requested tone is unavailable for this tenant.
func IsEmptyCompletion(err error) bool {
	return errors.Is(err, chathub.ErrEmptyCompletion)
}

// writeUpstreamError renders a failed upstream call as an HTTP response,
// surfacing the Retry-After hint so clients can back off. Gateway-local
// overload (queue timeout, no free concurrency) surfaces as HTTP 503; genuine
// upstream rate limits stay HTTP 429.
func writeUpstreamError(w http.ResponseWriter, err error) {
	if IsQueueTimeout(err) {
		w.Header().Set("Retry-After", "1")
		writeOpenAIError(w, http.StatusServiceUnavailable, "queue_timeout", "account is at capacity; the request queued too long, please retry shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	if IsLocalCapacity(err) {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", "1")
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "rate_limit_error", "gateway concurrency is at capacity; no account is currently available, please retry shortly")
		return
	}
	status := upstreamStatus(err)
	if status == http.StatusTooManyRequests {
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(rateLimitCooldown().Seconds())))
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	writeOpenAIError(w, status, "upstream_error", upstreamError(err))
}
