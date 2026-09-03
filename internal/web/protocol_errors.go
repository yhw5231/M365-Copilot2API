package web

import (
	"encoding/json"
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
