package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"time"
)

func (s *Server) runOpenAIAdapter(r *http.Request, o oaiReq) (map[string]any, []byte, int, error) {
	o.Stream = false
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	rr := httptest.NewRecorder()
	s.openaiChat(rr, r2)
	var out map[string]any
	err := json.Unmarshal(rr.Body.Bytes(), &out)
	return out, rr.Body.Bytes(), rr.Code, err
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeAnthropicError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxChatRequestBody)
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeAnthropicError(w, status, "api_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream protocol error: "+err.Error())
		return
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, "")
	s.usage.record(UsageRecord{
		Time:           time.Now(),
		APIKeyPrefix:   apiKeyPrefix(r),
		Model:          firstNonEmpty(body.Model, "m365-copilot"),
		ReasoningLevel: o.ReasoningEffort,
		Endpoint:       "/v1/messages",
		Stream:         o.Stream,
		InputTokens:    int64(estimate.Values["input_tokens"].(int)),
		OutputTokens:   int64(estimate.Values["output_tokens"].(int)),
		DurationMs:     time.Since(startedAt).Milliseconds(),
		Status:         200,
	})
	writeAnthropicResult(w, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out)
}
