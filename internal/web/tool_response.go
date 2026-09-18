package web

import (
	"m365-copilot2api/internal/chathub"
	"net/http"
	"time"
	"unicode/utf8"
)

// toolArgsChunks splits tool-call arguments into stream-sized fragments with
// every cut on a UTF-8 rune boundary. The byte-window loop it replaces
// advanced by a fixed offset (off += size) while pushing each window's end
// forward to the next rune start, so the following fragment began inside a
// multibyte character; the orphan continuation bytes were invalid UTF-8, and
// every JSON encode/decode step (server mustJSON, client reassembly) replaces
// them with U+FFFD — goal/todo text permanently read "验�证". Reusing the
// previous window's rune-aligned end as the next start keeps every fragment
// (and their concatenation) valid UTF-8.
func toolArgsChunks(args string, size int) []string {
	if size <= 0 {
		size = 512
	}
	var chunks []string
	for off := 0; off < len(args); {
		end := off + size
		if end > len(args) {
			end = len(args)
		}
		for end < len(args) && !utf8.RuneStart(args[end]) {
			end++
		}
		chunks = append(chunks, args[off:end])
		off = end
	}
	return chunks
}

// runeSafeTruncate cuts s to at most n bytes at a UTF-8 rune boundary.
// Byte-level truncation inside a Chinese character leaves invalid trailing
// bytes that every JSON serializer turns into U+FFFD, so logs, evidence and
// previews would show a visible "�" at the cut point.
func runeSafeTruncate(s string, n int) string {
	if n < 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

func writeToolResponse(w http.ResponseWriter, id, model string, stream bool, sendUsage bool, promptTokens, cachedTokens int64, calls []detectedToolCall, res chathub.Result) error {
	toolCalls := toolCallMaps(calls)
	msg := map[string]any{"role": "assistant", "content": nil, "tool_calls": toolCalls}
	if res.Reasoning != "" {
		if reasoning := sanitizePublicReasoningText(res.Reasoning); reasoning != "" {
			msg["reasoning_content"] = reasoning
		}
	}
	// promptTokens arrives from the caller (the full logical prompt); the
	// completion is the visible answer text that produced the tool calls plus
	// the emitted arguments. Counting the answer into the prompt (the old
	// behavior) produced identical prompt/completion numbers.
	pt := promptTokens
	ct := EstimateTokens(res.Text)
	for _, tc := range calls {
		ct += EstimateTokens(string(tc.Arguments))
	}
	if stream {
		setSSEHeaders(w)
		flusher, _ := w.(http.Flusher)
		emit := func(v any) {
			if err := sseDataRaw(w, flusher, mustJSON(v)); err != nil {
				return
			}
		}
		base := func(delta map[string]any, finish any) map[string]any {
			return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		firstDelta := map[string]any{"role": "assistant", "content": nil}
		if res.Reasoning != "" {
			if reasoning := sanitizePublicReasoningText(res.Reasoning); reasoning != "" {
				firstDelta["reasoning_content"] = reasoning
			}
		}
		emit(base(firstDelta, nil))
		const chunkSize = 512
		for i, tc := range calls {
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			isLast := i == len(calls)-1
			emit(base(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc.ID, "type": typ, "function": map[string]any{"name": tc.Name, "arguments": ""}}}}, nil))
			args := string(tc.Arguments)
			argChunks := toolArgsChunks(args, chunkSize)
			for ci, argChunk := range argChunks {
				isLastArgChunk := ci == len(argChunks)-1
				var finish any
				if isLast && isLastArgChunk {
					finish = "tool_calls"
				}
				emit(base(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": argChunk}}}}, finish))
			}
			if len(args) == 0 && isLast {
				emit(base(map[string]any{}, "tool_calls"))
			}
		}
		if sendUsage {
			usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}}, "usage": usageWithCache(pt, ct, cachedTokens)}
			_ = sseSafeRaw(w, flusher, "data: "+mustJSON(usageChunk)+"\n\n")
		}
		_ = sseSafeRaw(w, flusher, "data: [DONE]\n\n")
		return nil
	}
	jsonOut(w, map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": "tool_calls"}}, "m365": compatM365Metadata(res), "usage": usageWithCache(pt, ct, cachedTokens)})
	return nil
}
