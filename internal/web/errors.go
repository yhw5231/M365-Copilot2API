package web

import (
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
// failover or rate-limit handling: the client just receives HTTP 429 telling it
// to retry shortly.
var errQueueTimeout = errors.New("concurrency queue wait timed out")

// IsQueueTimeout reports whether err is a concurrency queue-wait timeout.
func IsQueueTimeout(err error) bool {
	return errors.Is(err, errQueueTimeout)
}

// upstreamStatus maps a failed upstream call to the client-visible HTTP status:
// rate limits stay 429 (with Retry-After when known), auth failures become 401,
// everything else is 502. Unknown upstream failures must never leak internals.
func upstreamStatus(err error) int {
	if IsQueueTimeout(err) {
		return http.StatusTooManyRequests
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
// surfacing the Retry-After hint for rate limits so clients can back off.
func writeUpstreamError(w http.ResponseWriter, err error) {
	if IsQueueTimeout(err) {
		w.Header().Set("Retry-After", "1")
		writeOpenAIError(w, http.StatusTooManyRequests, "queue_timeout", "account is at capacity; the request queued too long, please retry shortly")
		return
	}
	if IsEmptyCompletion(err) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "upstream returned empty completion; the requested model may be unavailable for this tenant")
		return
	}
	if retry := RetryAfterSeconds(err); retry > 0 {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	}
	status := upstreamStatus(err)
	if status == http.StatusTooManyRequests {
		if IsLocalCapacity(err) {
			if w.Header().Get("Retry-After") == "" {
				w.Header().Set("Retry-After", "1")
			}
			writeOpenAIError(w, status, "rate_limit_error", "gateway concurrency is at capacity; no account is currently available, please retry shortly")
			return
		}
		if w.Header().Get("Retry-After") == "" {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(rateLimitCooldown().Seconds())))
		}
		writeOpenAIError(w, status, "rate_limit_error", "upstream is rate limiting; try again shortly")
		return
	}
	writeOpenAIError(w, status, "upstream_error", upstreamError(err))
}
