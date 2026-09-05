package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func errorMessage(raw []byte, fallback string) string {
	var v map[string]any
	if json.Unmarshal(raw, &v) == nil {
		if e, ok := v["error"].(map[string]any); ok {
			if s, ok := e["message"].(string); ok && s != "" {
				return s
			}
		}
	}
	if s := strings.TrimSpace(string(raw)); s != "" {
		return s
	}
	return fallback
}
func writeOpenAIError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": sanitizePublicInternalText(msg), "type": typ}})
}
func writeResponsesError(w http.ResponseWriter, status int, typ, msg string) {
	writeOpenAIError(w, status, typ, msg)
}
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": sanitizePublicInternalText(msg)}})
}

// writeEndpointError writes an error in the dialect the request's endpoint
// speaks. /v1/messages clients are Anthropic SDKs: an OpenAI-shaped body there
// surfaces as an unparseable APIError, so the middleware chain (auth, gateway
// capacity, panic recovery, body limits) must answer in the Anthropic shape.
func writeEndpointError(w http.ResponseWriter, r *http.Request, status int, typ, msg string) {
	if r != nil && r.URL.Path == "/v1/messages" {
		writeAnthropicError(w, status, anthropicErrorType(status, typ), msg)
		return
	}
	writeOpenAIError(w, status, typ, msg)
}

// writeContextOverflowError rejects a request whose estimated input exceeds
// the compaction threshold, asking the client to compact its context and
// resend. The message wording mirrors what the major agent clients match on:
// OpenAI's "context_length_exceeded" code (plus "maximum context length" in
// the text) and Anthropic's "prompt is too long: X tokens > Y token maximum",
// which triggers Claude Code's auto-compact. All three protocol endpoints
// converge on openaiChat, so the dialect is selected from the request path.
func writeContextOverflowError(w http.ResponseWriter, r *http.Request, estimated, budget int) {
	const status = http.StatusBadRequest
	threshold := compactRequestThreshold(budget)
	// The stated maximum is the compaction threshold, not the full budget: a
	// client that compacts to just under the raw budget would be rejected
	// again.
	if r != nil && r.URL.Path == "/v1/messages" {
		writeAnthropicError(w, status, "invalid_request_error",
			fmt.Sprintf("prompt is too long: %d tokens > %d token maximum (%d%% of the %d-token context window). Please compact your context and resend a smaller request.", estimated, threshold, compactRequestThresholdPercent, budget))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"message": sanitizePublicInternalText(fmt.Sprintf(
			"This model's maximum context length is %d tokens (%d%% of the %d-token context window). However, your request resulted in approximately %d input tokens. Please compact your context and resend a smaller request.", threshold, compactRequestThresholdPercent, budget, estimated)),
		"type": "invalid_request_error",
		"code": "context_length_exceeded",
	}})
}

// anthropicErrorType maps an HTTP status / OpenAI-flavored type to the closest
// official Anthropic error type. Unmapped statuses keep the generic api_error.
func anthropicErrorType(status int, typ string) string {
	if status == http.StatusUnauthorized {
		return "authentication_error"
	}
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == http.StatusBadGateway {
		return "rate_limit_error"
	}
	if status == http.StatusNotFound {
		return "not_found_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}
