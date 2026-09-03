package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

func openAIChoice(v map[string]any) (map[string]any, string) {
	choices, _ := v["choices"].([]any)
	if len(choices) == 0 {
		return nil, ""
	}
	c, _ := choices[0].(map[string]any)
	m, _ := c["message"].(map[string]any)
	finish, _ := c["finish_reason"].(string)
	return m, finish
}

// anthropicMessageParts carries the projected Anthropic message plus the
// pieces the SSE emitter needs to replay it as content blocks.
type anthropicMessageParts struct {
	out          map[string]any
	blocks       []any
	stop         string
	outputTokens int64
}

// buildAnthropicMessage projects an internal OpenAI-style result into the
// Anthropic message envelope (id/type/role/content blocks/stop_reason/usage).
func buildAnthropicMessage(model string, src map[string]any) anthropicMessageParts {
	id := "msg_" + uuid.NewString()
	msg, finish := openAIChoice(src)
	sanitizePublicAssistantMessage(msg, model)
	blocks := []any{}
	stop := "end_turn"
	if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reasoning, "signature": ""})
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		stop = "tool_use"
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			var input any = map[string]any{}
			if a, ok := fn["arguments"].(string); ok {
				_ = json.Unmarshal([]byte(a), &input)
			}
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": tc["id"], "name": fn["name"], "input": input})
		}
	} else {
		switch content := msg["content"].(type) {
		case []any:
			for _, raw := range content {
				part, _ := raw.(map[string]any)
				switch part["type"] {
				case "text":
					if t, _ := part["text"].(string); t != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": t})
					}
				case "image_url":
					img, _ := part["image_url"].(map[string]any)
					if u, _ := img["url"].(string); u != "" {
						if strings.HasPrefix(u, "data:") {
							parts := strings.SplitN(u, ",", 2)
							meta := parts[0]
							b64 := ""
							if len(parts) == 2 {
								b64 = parts[1]
							}
							media := strings.TrimPrefix(meta, "data:")
							media = strings.SplitN(media, ";", 2)[0]
							blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": b64}})
						} else {
							blocks = append(blocks, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}})
						}
					}
				}
			}
		default:
			blocks = append(blocks, map[string]any{"type": "text", "text": fmt.Sprint(content)})
		}
		if len(blocks) == 0 {
			blocks = append(blocks, map[string]any{"type": "text", "text": ""})
		}
	}
	_ = finish
	inputTokens := int64(0)
	outputTokens := int64(0)
	if u, ok := src["usage"].(map[string]any); ok {
		if v, ok := u["prompt_tokens"]; ok {
			if n, ok := v.(int64); ok {
				inputTokens = n
			}
			if n, ok := v.(float64); ok {
				inputTokens = int64(n)
			}
		}
		if v, ok := u["completion_tokens"]; ok {
			if n, ok := v.(int64); ok {
				outputTokens = n
			}
			if n, ok := v.(float64); ok {
				outputTokens = int64(n)
			}
		}
	}
	out := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens}}
	// Add cached-token fields: Anthropic native cache_read_input_tokens and the
	// OpenAI-style input_tokens_details.cached_tokens (sub2api reads both).
	if u, ok := src["usage"].(map[string]any); ok {
		if cached := cachedTokensFromUsage(u); cached > 0 {
			outUsage := out["usage"].(map[string]any)
			outUsage["cache_read_input_tokens"] = cached
			withInputCacheDetails(outUsage, cached)
		}
	}
	return anthropicMessageParts{out: out, blocks: blocks, stop: stop, outputTokens: outputTokens}
}

func writeAnthropicResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	parts := buildAnthropicMessage(model, src)
	if !stream {
		jsonOut(w, parts.out)
		return
	}
	sender := startAnthropicStream(w)
	defer sender.stop()
	sender.emitMessage(parts)
}

// anthropicStreamSender serializes Anthropic SSE frames against the ping
// keepalive goroutine. The upstream generation is buffered by the adapter, so
// without pings a long "thinking" phase produces zero bytes and idle proxies
// kill the connection before the first content block.
type anthropicStreamSender struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	f       http.Flusher
	done    chan struct{}
	once    sync.Once
	aborted bool
}

// startAnthropicStream sets the SSE headers and begins the ping keepalive.
func startAnthropicStream(w http.ResponseWriter) *anthropicStreamSender {
	setSSEHeaders(w)
	f, _ := w.(http.Flusher)
	s := &anthropicStreamSender{w: w, f: f, done: make(chan struct{})}
	go s.pingLoop()
	return s
}

func (s *anthropicStreamSender) pingLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if err := s.frame("ping", map[string]any{"type": "ping"}); err != nil {
				return
			}
		}
	}
}

func (s *anthropicStreamSender) frame(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted {
		return fmt.Errorf("anthropic stream aborted")
	}
	if err := sseWriteFrame(s.w, s.f, name, v); err != nil {
		s.aborted = true
		return err
	}
	return nil
}

// stop ends the keepalive goroutine. Safe to call multiple times.
func (s *anthropicStreamSender) stop() {
	s.once.Do(func() { close(s.done) })
}

// emitError reports a mid-stream failure as an Anthropic error event (the SSE
// headers are already sent, so an HTTP status is no longer possible).
func (s *anthropicStreamSender) emitError(errType, message string) {
	_ = s.frame("error", map[string]any{"type": "error", "error": map[string]any{"type": errType, "message": sanitizePublicInternalText(message)}})
}

// emitMessage replays the built message as the official Anthropic event
// sequence: message_start → content_block_start/delta/stop → message_delta →
// message_stop.
func (s *anthropicStreamSender) emitMessage(parts anthropicMessageParts) {
	emit := func(name string, v any) {
		_ = s.frame(name, v)
	}
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": parts.out["id"], "type": "message", "role": "assistant", "model": parts.out["model"], "content": []any{}, "stop_reason": nil, "usage": parts.out["usage"]}})
	for i, b := range parts.blocks {
		m, _ := b.(map[string]any)
		startBlock := b
		blockType := ""
		if t, _ := m["type"].(string); t != "" {
			blockType = t
		}
		switch blockType {
		case "tool_use":
			startBlock = map[string]any{"type": "tool_use", "id": m["id"], "name": m["name"], "input": map[string]any{}}
		case "thinking":
			startBlock = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
		case "image":
			startBlock = map[string]any{"type": "image", "source": m["source"]}
		}
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": startBlock})
		switch blockType {
		case "text":
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "text_delta", "text": m["text"]}})
		case "tool_use":
			partial, _ := json.Marshal(m["input"])
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(partial)}})
		case "thinking":
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": m["thinking"]}})
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": i})
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": parts.stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": parts.outputTokens}})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

// sseWriteFrame writes one SSE frame and flushes; a write error (client gone,
// deadline exceeded) aborts the stream instead of leaving the handler blocked.
func sseWriteFrame(w http.ResponseWriter, f http.Flusher, name string, value any) error {
	b, _ := json.Marshal(value)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseDataRaw writes a raw "data: ..." frame with the same write deadline.
func sseDataRaw(w http.ResponseWriter, f http.Flusher, data string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// sseSafeRaw writes a pre-formatted frame (e.g. ": connected" or "[DONE]").
func sseSafeRaw(w http.ResponseWriter, f http.Flusher, payload string) error {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}
