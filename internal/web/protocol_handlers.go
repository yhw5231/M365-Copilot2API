package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type pipeResponseWriter struct {
	h      http.Header
	w      *io.PipeWriter
	status int
	// body retains a bounded copy of what the inner handler wrote so a failed
	// inner request can surface its real error message to the client instead of
	// the generic "inner chat request failed".
	body bytes.Buffer
}

// maxInnerErrorCapture bounds the retained inner error body. Error payloads are
// small; this only needs enough room for the diagnostic message.
const maxInnerErrorCapture = 16 << 10

func (p *pipeResponseWriter) Header() http.Header { return p.h }
func (p *pipeResponseWriter) WriteHeader(n int) {
	if p.status == 0 {
		p.status = n
	}
}
func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = 200
	}
	if room := maxInnerErrorCapture - p.body.Len(); room > 0 && len(b) > 0 {
		if len(b) > room {
			_, _ = p.body.Write(b[:room])
		} else {
			_, _ = p.body.Write(b)
		}
	}
	return p.w.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

// innerStreamError captures an error event from the inner /v1/chat/completions
// SSE stream so the Responses adapter can surface the real cause instead of
// inferring failure from the absence of content.
type innerStreamError struct {
	Code    string
	Message string
}

// streamResponsesAdapter converts the internal OpenAI SSE incrementally instead
// of buffering the entire completion in httptest.ResponseRecorder.
// newMessages carries the current request's messages (before previous_response_id
// history prepend) so the adapter can compute cached_tokens = input_tokens - new_input_tokens.
func (s *Server) streamResponsesAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string, newMessages []oaiMsg, startedAt time.Time, storeHistory *bool) {
	o.Stream = true
	// The adapter derives the downstream cached_tokens from the inner stream's
	// usage chunk (innerCachedTokens). The inner request is gateway-internal:
	// always ask for its usage regardless of what the downstream client sent,
	// otherwise the cache signal is silently lost and every /v1/responses
	// streaming row reports cached_tokens=0. The downstream-facing usage chunk
	// emitted by this adapter is NOT affected by this flag; its gating is a
	// Responses-protocol concern and handled by the protocol itself.
	o.StreamOptions = &struct {
		IncludeUsage bool `json:"include_usage"`
	}{IncludeUsage: true}
	tenant := extractAPIKey(r)
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	innerDone := make(chan struct{})
	// If the main goroutine returns early (client disconnect, first emit
	// failure), close the read side so the inner goroutine's blocked pipe
	// writes fail instead of leaking a goroutine against a dead socket.
	defer pr.CloseWithError(context.Canceled)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[responses] inner goroutine panic: %v", r)
			}
			_ = pw.Close()
			close(innerDone)
		}()
		s.openaiChat(irw, r2)
	}()

	setSSEHeaders(w)
	flusher, _ := w.(http.Flusher)
	// Keep the connection alive during long silent phases (upstream reasoning
	// can run for minutes after response.created): emit SSE comment frames on
	// a timer. All payload writes go through the keepalive mutex so frames
	// never interleave with the comments.
	keepalive := startSSEKeepalive(w, flusher, 0)
	defer keepalive.stop()
	var abortErr error
	emit := func(name string, v any) error {
		if abortErr != nil {
			return abortErr
		}
		b, _ := json.Marshal(v)
		if err := keepalive.lockedWriteCtx(r.Context(), "event: "+name+"\ndata: "+string(b)+"\n\n"); err != nil {
			abortErr = err
			return err
		}
		return nil
	}
	id := "resp_" + uuid.NewString()
	created := time.Now().Unix()
	if err := emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}}); err != nil {
		return
	}

	var text strings.Builder
	messageID := "msg_" + uuid.NewString()
	contentID := "txt_" + uuid.NewString()
	textStarted := false
	var reasoning strings.Builder
	reasoningID := "rs_" + uuid.NewString()
	reasoningContentID := "rs_summary_" + uuid.NewString()
	reasoningStarted := false
	type tcState struct {
		ID, Name, Args, Type string
		ItemID               string
	}
	calls := map[int]*tcState{}
	// The inner /v1/chat/completions stream reports failures as
	// data:{"error":{...}} chunks (upstream timeout, empty completion, repair
	// failure, ...). Surface those instead of guessing from the absence of
	// content, which currently produces a misleading empty_upstream_response.
	var innerErr *innerStreamError
	// innerCachedTokens captures the upstream conversation-reuse cache from the
	// inner stream's usage chunk (session resolver / conv-cache hit). It is the
	// authoritative cache signal for the downstream Responses usage: it works
	// even when the client never sends previous_response_id.
	var innerCachedTokens int64

	// Usage accounting for the streaming Responses path: the inner chat records
	// its own /v1/chat/completions row, but the protocol entry (/v1/responses)
	// must also be logged so the request table shows the cache share and
	// time-to-first-token as seen by the Responses client. TTFT is measured to
	// the first visible delta (reasoning, text, or tool argument).
	status := 200
	var errMsg string
	firstDeltaAt := time.Time{}
	markFirstDelta := func() {
		if firstDeltaAt.IsZero() {
			firstDeltaAt = time.Now()
		}
	}
	defer func() {
		usageOut := reasoning.String() + text.String()
		for _, call := range calls {
			usageOut += call.Name + call.Args
		}
		est := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, usageOut)
		cached := responsesHistoryCacheTokens(model, o.Messages, newMessages, o.Tools, o.ToolChoice, usageOut)
		if innerCachedTokens > cached {
			cached = innerCachedTokens
		}
		var ttft int64
		if !firstDeltaAt.IsZero() {
			ttft = firstDeltaAt.Sub(startedAt).Milliseconds()
		}
		// The model route's configured reasoning level overrides whatever the
		// client sent; resolve it once and use it for both the trace and the
		// usage row so every record shows the level the upstream actually ran.
		effectiveEffort := resolveReasoningEffort(o.ReasoningEffort, model, currentSettings().ModelMappings)
		// Keep the debug trace record in sync so the console shows the real
		// token counts instead of 0/0 for every Responses streaming request.
		usageTTFT := ttft
		if tr := traceFromRequest(r); tr != nil {
			s.trace.update(tr.ID, func(rec *traceRecord) {
				usageTTFT = applyResponsesStreamTraceUpdate(rec, model, effectiveEffort,
					int64(est.Values["input_tokens"].(int)), int64(est.Values["output_tokens"].(int)), cached,
					ttft, time.Since(startedAt).Milliseconds(), status, errMsg)
			})
		}
		s.usage.record(UsageRecord{
			Time:           time.Now(),
			APIKeyPrefix:   extractAPIKey(r),
			Model:          model,
			ReasoningLevel: effectiveEffort,
			Endpoint:       "/v1/responses",
			Stream:         true,
			InputTokens:    int64(est.Values["input_tokens"].(int)),
			OutputTokens:   int64(est.Values["output_tokens"].(int)),
			CacheTokens:    cached,
			TTFTMs:         usageTTFT,
			DurationMs:     time.Since(startedAt).Milliseconds(),
			Status:         status,
			Error:          errMsg,
		})
	}()
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		if r.Context().Err() != nil || abortErr != nil {
			return
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		if errObj, ok := chunk["error"].(map[string]any); ok {
			code, _ := errObj["code"].(string)
			msg, _ := errObj["message"].(string)
			if code == "" {
				code = "upstream_error"
			}
			innerErr = &innerStreamError{Code: code, Message: msg}
			continue
		}
		// Skip usage-only chunks (they carry no choices); the estimate is
		// computed locally at the end from the visible request and completion.
		// Capture the inner conversation-reuse cache from the usage chunk: it
		// reflects what the upstream actually held (session resolver / conv
		// cache hit), which stays valid even when the client did not send
		// previous_response_id. The inner streaming chat emits the usage chunk
		// with an empty choices array (choices:[{index:0,delta:{},finish_reason:
		// nil}]), so capture usage BEFORE branching on choices — otherwise the
		// cache signal is silently lost and the downstream Responses usage
		// always reports cached_tokens=0 on the streaming path.
		if u, ok := chunk["usage"]; ok {
			if c := cachedTokensFromUsage(u); c > 0 {
				innerCachedTokens = c
			}
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			reasoning.WriteString(rc)
			markFirstDelta()
			if !reasoningStarted {
				reasoningStarted = true
				emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "reasoning", "id": reasoningID, "status": "in_progress", "summary": []any{map[string]any{"type": "summary_text", "id": reasoningContentID, "text": "", "annotations": []any{}}}}})
			}
			emit("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "content_index": 0, "item_id": reasoningID, "delta": rc})
		}
		if content, ok := delta["content"].(string); ok && content != "" {
			text.WriteString(content)
			markFirstDelta()
			msgIndex := 0
			if reasoningStarted {
				msgIndex = 1
			}
			if !textStarted {
				textStarted = true
				emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": msgIndex, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}})
			}
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": msgIndex, "content_index": 0, "item_id": messageID, "delta": content})
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			// output_index must stay unique across the whole stream. The
			// reasoning item (0) and the message item (1 when reasoning started,
			// else 0) are already claimed once they exist, so tool items start
			// after both.
			callOffset := 0
			if reasoningStarted {
				callOffset = 1
			}
			if textStarted {
				callOffset++
			}
			for _, raw := range rawCalls {
				tc, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				markFirstDelta()
				idxFloat, ok := tc["index"].(float64)
				if !ok {
					continue
				}
				idx := int(idxFloat)
				st := calls[idx]
				typ := "function"
				if v, ok := tc["type"].(string); ok && v == "custom" {
					typ = "custom"
				}
				if st == nil {
					prefix := "fc_"
					item := map[string]any{"type": "function_call", "call_id": "", "name": "", "arguments": "", "status": "in_progress"}
					if typ == "custom" {
						prefix = "ctc_"
						item = map[string]any{"type": "custom_tool_call", "call_id": "", "name": "", "input": "", "status": "in_progress"}
					}
					st = &tcState{ItemID: prefix + uuid.NewString(), Type: typ}
					calls[idx] = st
					item["id"] = st.ItemID
					// Give the in-progress item a non-empty call_id from the
					// start: a client that replays this item before the
					// completed event would otherwise forward an empty
					// pairing key and hit the "tool call missing id" 400.
					item["call_id"] = st.ItemID
					// The first delta of a tool call normally carries the
					// function name. Surface it on the in-progress item too:
					// a client that acts on output_item.added without waiting
					// for output_item.done would otherwise see an empty name
					// and fail with "unknown tool". Only touch the item here —
					// st.Name is accumulated by the shared delta handling below.
					if fn, ok := tc["function"].(map[string]any); ok {
						if v, ok := fn["name"].(string); ok && v != "" {
							item["name"] = v
						}
					}
					emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": idx + callOffset, "item": item})
				}
				if v, ok := tc["id"].(string); ok {
					st.ID = v
				}
				fn, _ := tc["function"].(map[string]any)
				if v, ok := fn["name"].(string); ok {
					st.Name += v
				}
				if v, ok := fn["arguments"].(string); ok {
					st.Args += v
					if st.Type != "custom" {
						emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": idx + callOffset, "item_id": st.ItemID, "delta": v})
					}
				}
			}
		}
	}
	<-innerDone
	if scanner.Err() != nil || irw.status >= http.StatusBadRequest {
		status = irw.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		errMsg = errorMessage(irw.body.Bytes(), "inner chat request failed")
		// Surface the inner request's own error message (e.g. an upstream 4xx/5xx
		// or a chat protocol rejection) instead of an opaque placeholder, so the
		// client can see why the response failed.
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": strconv.Itoa(status), "message": errMsg},
			},
		})
		return
	}
	if innerErr != nil {
		// The inner stream carried an error event (upstream timeout, empty
		// completion, repair failure, ...). Report the real cause instead of
		// guessing. Partial text already streamed is kept; the failed event
		// tells the client the response is incomplete.
		status = http.StatusBadGateway
		errMsg = innerErr.Message
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": innerErr.Code, "message": innerErr.Message},
			},
		})
		return
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" && reasoning.Len() == 0 {
		// Never leave a Responses stream after response.created without a
		// terminal event: clients otherwise render this as a successful blank
		// answer and may reuse an incomplete response on the next turn.
		status = http.StatusBadGateway
		errMsg = "ChatHub returned no text or tool call"
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": "empty_upstream_response", "message": errMsg},
			},
		})
		return
	}
	output := []any{}
	if reasoningStarted {
		output = append(output, map[string]any{"type": "reasoning", "id": reasoningID, "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "id": reasoningContentID, "text": reasoning.String(), "annotations": []any{}}}})
		// OpenAI emits output_item.done in output_index order; the reasoning
		// item (index 0) completes before the message or tool items that follow.
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": output[0]})
	}
	if len(calls) > 0 {
		keys := make([]int, 0, len(calls))
		for k := range calls {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		// Keep the output_index sequence consistent with the streaming phase:
		// reasoning item holds index 0 when present, the streamed message item
		// (if any) follows, tool calls come last.
		callOffset := 0
		if reasoningStarted {
			callOffset = 1
		}
		if textStarted {
			callOffset++
		}
		// The streamed message item was added and delta'd but not yet closed —
		// emit its done events and include it in the output before the tool
		// items so clients never see an unfinished output item.
		if textStarted {
			msgIndex := 0
			if reasoningStarted {
				msgIndex = 1
			}
			item := map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": text.String(), "annotations": []any{}}}}
			output = append(output, item)
			emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": msgIndex, "content_index": 0, "item_id": messageID, "text": text.String()})
			emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": msgIndex, "item": item})
		}
		// The emitted call ids become the pending registry for the next
		// stateless Responses continuation turn.
		pending := make([]detectedToolCall, 0, len(keys))
		for _, i := range keys {
			st := calls[i]
			if st == nil {
				continue
			}
			// The upstream OpenAI-compatible stream normally carries a tool call
			// id in its first delta. If it does not, fall back to the adapter's
			// own item id so the completed item always exposes a non-empty
			// call_id — clients replay it on the next turn and an empty id here
			// would surface as a "tool call missing id" 400 later.
			callID := st.ID
			if callID == "" {
				callID = st.ItemID
			}
			if st.Name == "" {
				// Never emit a completed tool call with an empty name: the
				// client resolves the tool by name and would fail with
				// "unknown tool", then replay the broken call until the
				// tool-conversation guard 409s. Surface the upstream anomaly
				// as a failed response instead.
				log.Printf("[responses] dropping tool call with empty name at output_index=%d", i)
				status = http.StatusBadGateway
				errMsg = "upstream tool call missing name"
				emit("response.failed", map[string]any{
					"type": "response.failed",
					"response": map[string]any{
						"id": id, "object": "response", "status": "failed", "model": model,
						"error": map[string]any{"code": "invalid_tool_call", "message": errMsg},
					},
				})
				return
			}
			if st.Type == "custom" {
				input := customToolInput(st.Args)
				item := map[string]any{"type": "custom_tool_call", "id": st.ItemID, "call_id": callID, "name": st.Name, "input": input, "status": "completed"}
				output = append(output, item)
				emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i + callOffset, "item_id": item["id"], "delta": input})
				emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": i + callOffset, "item_id": item["id"], "input": input})
				emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i + callOffset, "item": item})
				continue
			}
			item := map[string]any{"type": "function_call", "id": st.ItemID, "call_id": callID, "name": st.Name, "arguments": st.Args, "status": "completed"}
			output = append(output, item)
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i + callOffset, "item_id": st.ItemID, "arguments": st.Args})
			emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i + callOffset, "item": item})
			pending = append(pending, detectedToolCall{ID: callID, Type: "function", Name: st.Name, Arguments: json.RawMessage(st.Args)})
		}
		s.recordPendingToolCalls(tenant, pending)
	} else {
		msgIndex := 0
		if reasoningStarted {
			msgIndex = 1
		}
		item := map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}
		output = append(output, item)
		if !textStarted {
			emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": msgIndex, "item": item})
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": msgIndex, "content_index": 0, "item_id": messageID, "delta": text.String()})
		}
		emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": msgIndex, "content_index": 0, "item_id": messageID, "text": text.String()})
		item["status"] = "completed"
		item["content"] = []any{map[string]any{"type": "output_text", "id": contentID, "text": text.String(), "annotations": []any{}}}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": msgIndex, "item": item})
	}
	usageOutput := reasoning.String() + text.String()
	for _, call := range calls {
		usageOutput += call.Name + call.Args
	}
	estimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, usageOutput)
	// Cache breakdown for the downstream: everything except the current
	// request's own messages (newMessages, captured before the history prepend)
	// is cached history restored through previous_response_id. The inner
	// session-reuse signal (innerCachedTokens) is a different token basis and
	// produced values inconsistent with the estimate's input_tokens, so the
	// difference-based calc is authoritative — unless the inner reuse cache is
	// the only signal available (client without previous_response_id), in which
	// case it is reported as-is.
	cached := responsesHistoryCacheTokens(model, o.Messages, newMessages, o.Tools, o.ToolChoice, usageOutput)
	if innerCachedTokens > cached {
		cached = innerCachedTokens
	}
	// Always stamp both spellings (see the non-streaming responses() path).
	withInputCacheDetails(estimate.Values, cached)
	// Reasoning-token breakdown: the reasoning transcript streamed above is
	// part of the estimate's completion; surface its share like the official
	// API's output_tokens_details.reasoning_tokens.
	if count, _ := tokenEstimator(model); count != nil {
		withOutputReasoningDetails(estimate.Values, int64(count(reasoning.String())))
	}
	resp := map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": model, "output": output, "usage": estimate.Values, "m365": localUsageMetadata(estimate.Source)}
	// Store the response for subsequent previous_response_id (same semantics as
	// the non-streaming path in responses()).
	if shouldStoreResponsesHistory(storeHistory) && (text.Len() > 0 || len(calls) > 0 || reasoning.Len() > 0) {
		stored := append([]oaiMsg(nil), o.Messages...)
		if len(calls) > 0 {
			converted := make([]map[string]any, 0, len(calls))
			for _, call := range calls {
				converted = append(converted, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Args}})
			}
			stored = append(stored, oaiMsg{Role: "assistant", ToolCalls: converted})
		} else if text.Len() > 0 {
			stored = append(stored, oaiMsg{Role: "assistant", Content: text.String(), ReasoningContent: reasoning.String()})
		} else if reasoning.Len() > 0 {
			stored = append(stored, oaiMsg{Role: "assistant", ReasoningContent: reasoning.String()})
		}
		s.responseMu.Lock()
		bucket := s.responseMessages[tenant]
		if bucket == nil {
			bucket = map[string]respHistory{}
			s.responseMessages[tenant] = bucket
		}
		for k, h := range bucket {
			if time.Since(h.At) > time.Hour {
				delete(bucket, k)
			}
		}
		if len(bucket) >= maxResponsesPerTenant {
			var oldestKey string
			var oldestAt time.Time
			for k, h := range bucket {
				if oldestKey == "" || h.At.Before(oldestAt) {
					oldestKey, oldestAt = k, h.At
				}
			}
			delete(bucket, oldestKey)
		}
		bucket[id] = respHistory{At: time.Now(), Messages: stored}
		s.responseMu.Unlock()
	}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

// applyResponsesStreamTraceUpdate reconciles the debug trace record when a
// streaming /v1/responses request completes. Two in-flight values are
// authoritative and must survive completion instead of being overwritten:
//
//   - ReasoningLevel: the caller passes the effective level (the model route's
//     configured level, which overrides the client's request). The raw client
//     effort is empty for most Responses clients and would blank the level.
//   - TTFTMs: routeUpstreamTrace recorded the upstream first-token latency.
//     The adapter only observes its first delta after the reasoning gate
//     releases the answer text, so its own measurement can land near the total
//     duration; it only fills a gap (upstream died before any delta).
//
// It returns the effective first-token latency (upstream when available) so
// the /v1/responses usage row stays consistent with the inner chat row.
func applyResponsesStreamTraceUpdate(rec *traceRecord, model, effort string, inputTokens, outputTokens, cachedTokens, adapterTTFT, durationMs int64, status int, errMsg string) int64 {
	rec.Model = model
	rec.Stream = true
	if effort != "" {
		rec.ReasoningLevel = effort
	}
	rec.InputTokens = inputTokens
	rec.OutputTokens = outputTokens
	rec.CachedTokens = cachedTokens
	ttft := adapterTTFT
	if rec.TTFTMs > 0 {
		ttft = rec.TTFTMs
	} else {
		rec.TTFTMs = adapterTTFT
	}
	rec.DurationMs = durationMs
	rec.StatusCode = status
	if errMsg != "" {
		rec.Status = "error"
		rec.Error = errMsg
	}
	return ttft
}

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

// dropSystemInstructions removes leading system/developer instruction messages
// from a stored Responses history. OpenAI Responses semantics say
// previous_response_id does NOT inherit the previous turn's instructions: every
// request applies its own instructions afresh at top priority. The stored
// history is normalized so the current request's instructions are the only ones
// the model sees.
func dropSystemInstructions(messages []oaiMsg) []oaiMsg {
	out := messages[:0]
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeResponsesError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body responsesRequest
	// Bound the body like the chat endpoint does: without the cap these
	// handlers read unbounded client input straight into memory.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, requestBodyLimit(r))).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeResponsesError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}
		writeResponsesError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		var unsupported *unsupportedParamError
		if errors.As(err, &unsupported) {
			writeResponsesError(w, 400, "unsupported_parameter", err.Error())
			return
		}
		writeResponsesError(w, 400, "invalid_request_error", err.Error())
		return
	}
	tenant := extractAPIKey(r)
	// Stateless Responses continuation: the client may send function_call_output
	// without replaying the matching function_call and without
	// previous_response_id. Back-fill the pending function_call from the registry
	// so the tool conversation stays valid (see pending_tools.go).
	o.Messages = s.restoreStatelessToolCalls(tenant, o.Messages)
	// Capture the current request's own messages before the previous_response_id
	// history prepend: they are the "new" input for the cache breakdown, while
	// the restored history is what the downstream should report as cached.
	currentMessages := append([]oaiMsg(nil), o.Messages...)
	if body.PreviousResponseID != "" {
		s.responseMu.Lock()
		prior, ok := s.responseMessages[tenant][body.PreviousResponseID]
		messages := append([]oaiMsg(nil), prior.Messages...)
		s.responseMu.Unlock()
		if !ok || len(messages) == 0 {
			writeResponsesError(w, 400, "invalid_request_error", "unknown previous_response_id")
			return
		}
		// Responses semantics: previous_response_id carries the conversation
		// history, but the current request's `instructions` replace (not append
		// to) the previous turn's instructions. The stored history still
		// contains the prior system/developer instruction messages, so drop them
		// before prepending; otherwise the model sees stale instructions twice
		// per turn and the newest instructions lose their top priority.
		messages = dropSystemInstructions(messages)
		o.Messages = append(messages, o.Messages...)
	}
	if body.Stream {
		s.streamResponsesAdapter(w, r, o, firstNonEmpty(body.Model, "m365-copilot"), currentMessages, startedAt, body.Store)
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeResponsesError(w, status, "upstream_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream protocol error: "+err.Error())
		return
	}
	if !responsesOutputHasContent(out) {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "ChatHub returned an empty response; no reusable message was created")
		return
	}
	msg, _ := openAIChoice(out)
	// Record any emitted tool calls as pending so a later stateless Responses
	// continuation (function_call_output without previous_response_id) can be
	// validated against the actual call (see pending_tools.go).
	if msg != nil {
		if calls, ok := msg["tool_calls"].([]any); ok {
			pending := make([]detectedToolCall, 0, len(calls))
			for _, raw := range calls {
				tc, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				id, _ := tc["id"].(string)
				if id == "" {
					continue
				}
				var name string
				var args json.RawMessage
				if fn, ok := tc["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
					if a, ok := fn["arguments"].(string); ok {
						args = json.RawMessage(a)
					}
				}
				if name == "" {
					continue
				}
				pending = append(pending, detectedToolCall{ID: id, Type: "function", Name: name, Arguments: args})
			}
			s.recordPendingToolCalls(tenant, pending)
		}
	}
	outputForUsage := ""
	reasoningForUsage := ""
	if msg != nil {
		outputForUsage = fmt.Sprint(msg["content"])
		// The reasoning transcript is part of the completion, exactly like the
		// streaming path's estimate; including it keeps both Responses paths'
		// output_tokens consistent and lets the reasoning-token breakdown add
		// up to the reported completion size.
		reasoningForUsage, _ = msg["reasoning_content"].(string)
		outputForUsage += reasoningForUsage
		if calls, ok := msg["tool_calls"].([]any); ok {
			outputForUsage += fmt.Sprint(calls)
		}
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, outputForUsage)
	// Cache breakdown for the downstream: everything except the current
	// request's own messages (currentMessages, captured before the history
	// prepend) is cached history restored through previous_response_id. The
	// inner M365 conversation-reuse signal (usage.prompt_tokens_details.
	// cached_tokens) reflects what the upstream actually held (session resolver
	// / conv-cache hit) and is the only cache signal when the client never sent
	// previous_response_id, so it is preferred whenever it is larger.
	cached := responsesHistoryCacheTokens(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, currentMessages, o.Tools, o.ToolChoice, outputForUsage)
	if inner := cachedTokensFromUsage(out["usage"]); inner > cached {
		cached = inner
	}
	// Always stamp both the Responses (input_tokens_details) and Chat Completions
	// (prompt_tokens / prompt_tokens_details) spellings so downstream relays
	// (sub2api / one-api / new-api) and billing panels read the cache breakdown
	// regardless of which format they parse. cached is 0 for a fresh
	// conversation and the aliases still resolve to the same input count.
	withInputCacheDetails(estimate.Values, cached)
	// Reasoning-token breakdown (official output_tokens_details.reasoning_tokens
	// shape, with the chat-spelling alias).
	if count, _ := tokenEstimator(firstNonEmpty(body.Model, "m365-copilot")); count != nil {
		withOutputReasoningDetails(estimate.Values, int64(count(reasoningForUsage)))
	}
	out["usage"] = estimate.Values
	out["m365_usage_source"] = estimate.Source
	if body.Store != nil {
		out["store"] = *body.Store
	}
	if len(body.Metadata) > 0 {
		out["metadata"] = body.Metadata
	}
	// Sampling controls are accepted for compatibility but cannot be applied by
	// the M365 backend; surface them explicitly so nothing is silently ignored.
	if params, ok := ignoredSamplingParams(&o); ok {
		out["m365_ignored_parameters"] = params
		out["m365_sampling_note"] = samplingNote
	}
	// The model route's configured reasoning level overrides whatever the
	// client sent; resolve it once and use it for both the trace and the
	// usage row so every record shows the level the upstream actually ran.
	effectiveEffort := resolveReasoningEffort(o.ReasoningEffort, firstNonEmpty(body.Model, "m365-copilot"), currentSettings().ModelMappings)
	s.usage.record(UsageRecord{
		Time:           time.Now(),
		APIKeyPrefix:   extractAPIKey(r),
		Model:          firstNonEmpty(body.Model, "m365-copilot"),
		ReasoningLevel: effectiveEffort,
		Endpoint:       "/v1/responses",
		InputTokens:    int64(estimate.Values["input_tokens"].(int)),
		OutputTokens:   int64(estimate.Values["output_tokens"].(int)),
		CacheTokens:    cached,
		DurationMs:     time.Since(startedAt).Milliseconds(),
		Status:         200,
	})
	// Keep the debug trace record in sync so the console shows the real token
	// counts instead of 0/0 for every Responses request.
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			rec.Model = firstNonEmpty(body.Model, "m365-copilot")
			rec.Stream = false
			if effectiveEffort != "" {
				rec.ReasoningLevel = effectiveEffort
			}
			rec.InputTokens = int64(estimate.Values["input_tokens"].(int))
			rec.OutputTokens = int64(estimate.Values["output_tokens"].(int))
			rec.CachedTokens = cached
			rec.DurationMs = time.Since(startedAt).Milliseconds()
			rec.StatusCode = 200
		})
	}
	// Retain the normalized history so a subsequent previous_response_id can
	// validate its function_call_output against the original tool call. The
	// Responses API `store` parameter opts the client out of history retention.
	if shouldStoreResponsesHistory(body.Store) {
		if _, ok := out["id"].(string); ok {
			// Use the same public response id that writeResponsesResult exposes.
			publicID := "resp_" + uuid.NewString()
			out["m365_response_id"] = publicID
			stored := append([]oaiMsg(nil), o.Messages...)
			if msg, _ := openAIChoice(out); msg != nil {
				if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
					converted := make([]map[string]any, 0, len(calls))
					for _, call := range calls {
						if m, ok := call.(map[string]any); ok {
							converted = append(converted, m)
						}
					}
					stored = append(stored, oaiMsg{Role: "assistant", ToolCalls: converted})
				} else {
					if text, _ := msg["content"].(string); text != "" {
						stored = append(stored, oaiMsg{Role: "assistant", Content: text})
					}
				}
			}
			s.responseMu.Lock()
			bucket := s.responseMessages[tenant]
			if bucket == nil {
				bucket = map[string]respHistory{}
				s.responseMessages[tenant] = bucket
			}
			for k, h := range bucket {
				if time.Since(h.At) > time.Hour {
					delete(bucket, k)
				}
			}
			if len(bucket) >= maxResponsesPerTenant {
				var oldestKey string
				var oldestAt time.Time
				for k, h := range bucket {
					if oldestKey == "" || h.At.Before(oldestAt) {
						oldestKey, oldestAt = k, h.At
					}
				}
				delete(bucket, oldestKey)
			}
			bucket[publicID] = respHistory{At: time.Now(), Messages: stored}
			s.responseMu.Unlock()
		}
	}
	writeResponsesResult(w, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out)
}

func responsesOutputHasContent(src map[string]any) bool {
	msg, _ := openAIChoice(src)
	if msg == nil {
		return false
	}
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	if text, _ := msg["content"].(string); strings.TrimSpace(text) != "" {
		return true
	}
	if reasoning, _ := msg["reasoning_content"].(string); strings.TrimSpace(reasoning) != "" {
		return true
	}
	return false
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAnthropicError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	// Bound the body like the chat endpoint does: without the cap these
	// handlers read unbounded client input straight into memory.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, requestBodyLimit(r))).Decode(&body); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}
		writeAnthropicError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	// The adapter buffers the whole upstream generation, so a streaming client
	// would see zero bytes for the entire "thinking" phase and idle proxies
	// would cut the connection. Start the SSE ping keepalive BEFORE the
	// upstream call; failures surface as an in-stream Anthropic error event
	// (the SSE headers are already sent, so an HTTP status is no longer
	// possible, and OpenAI-shaped JSON would break Anthropic SDK parsing).
	var sender *anthropicStreamSender
	if body.Stream {
		sender = startAnthropicStream(w)
		defer sender.stop()
	}
	emitAnthropicUpstreamError := func(status int, message string) {
		if sender == nil {
			writeAnthropicError(w, status, anthropicErrorType(status, ""), message)
			return
		}
		sender.emitError(anthropicErrorType(status, ""), message)
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		emitAnthropicUpstreamError(status, errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		emitAnthropicUpstreamError(http.StatusBadGateway, "upstream protocol error: "+err.Error())
		return
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, "")
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		Model:        firstNonEmpty(body.Model, "m365-copilot"),
		Endpoint:     "/v1/messages",
		InputTokens:  int64(estimate.Values["input_tokens"].(int)),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
	if sender != nil {
		sender.emitMessage(buildAnthropicMessage(firstNonEmpty(body.Model, "m365-copilot"), out))
		return
	}
	writeAnthropicResult(w, firstNonEmpty(body.Model, "m365-copilot"), false, out)
}
