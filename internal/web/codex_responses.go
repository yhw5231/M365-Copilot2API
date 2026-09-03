package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// writeResponsesResult projects an internal OpenAI-style result into the
// Responses events and completion shape consumed by Codex.
func writeResponsesResult(w http.ResponseWriter, model string, stream bool, src map[string]any) {
	// The stored id is present only when history retention is on (store != false);
	// with store:false the key is absent and a naive fmt.Sprint would produce the
	// literal "<nil>", so type-assert and fall back to a fresh id.
	id, _ := src["m365_response_id"].(string)
	if id == "" {
		id = "resp_" + uuid.NewString()
	}
	msg, _ := openAIChoice(src)
	sanitizePublicAssistantMessage(msg, model)
	var output []any
	if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
		output = append(output, map[string]any{"type": "reasoning", "id": "rs_" + uuid.NewString(), "summary": []any{map[string]any{"type": "summary_text", "text": reasoning, "annotations": []any{}}}})
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			tc, _ := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if tc["type"] == "custom" {
				output = append(output, map[string]any{"type": "custom_tool_call", "id": "ctc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "input": customToolInput(fn["arguments"]), "status": "completed"})
				continue
			}
			output = append(output, map[string]any{"type": "function_call", "id": "fc_" + uuid.NewString(), "call_id": tc["id"], "name": fn["name"], "arguments": fn["arguments"], "status": "completed"})
		}
	} else {
		text, _ := msg["content"].(string)
		messageID := "msg_" + uuid.NewString()
		output = append(output, map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
	}
	usage, _ := src["usage"].(map[string]any)
	usageSource, _ := src["m365_usage_source"].(string)
	if usage == nil {
		estimate := estimateResponsesUsage(model, nil, nil, nil, fmt.Sprint(msg["content"]))
		usage = estimate.Values
		usageSource = estimate.Source
	}
	if usageSource == "" {
		usageSource = usageSourceHeuristic
	}
	m365meta := localUsageMetadata(usageSource)
	if ignored, ok := src["m365_ignored_parameters"]; ok {
		if list, ok := ignored.([]any); ok && len(list) > 0 {
			m365meta["ignored_parameters"] = list
			m365meta["sampling_note"] = samplingNote
		} else if list, ok := ignored.([]string); ok && len(list) > 0 {
			m365meta["ignored_parameters"] = list
			m365meta["sampling_note"] = samplingNote
		}
	}
	resp := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": model, "output": output, "usage": usage, "m365": m365meta}
	// Echo the effective reasoning level resolved by the inner chat request
	// (the model route's configured level when it overrides the client value).
	if eff, _ := src["reasoning_effort"].(string); eff != "" {
		resp["reasoning"] = map[string]any{"effort": eff}
	}
	if v, ok := src["store"].(bool); ok {
		resp["store"] = v
	}
	switch md := src["metadata"].(type) {
	case map[string]string:
		if len(md) > 0 {
			resp["metadata"] = md
		}
	case map[string]any:
		if len(md) > 0 {
			resp["metadata"] = md
		}
	}
	if !stream {
		jsonOut(w, resp)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	f, _ := w.(http.Flusher)
	aborted := false
	emit := func(name string, v any) {
		if aborted {
			return
		}
		if err := sseWriteFrame(w, f, name, v); err != nil {
			aborted = true
		}
	}
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})
	for i, item := range output {
		m, _ := item.(map[string]any)
		addedItem := item
		if m["type"] == "function_call" {
			// Arguments arrive in function_call_arguments.delta. Including them
			// here too would make conforming clients append duplicate JSON.
			added := make(map[string]any, len(m))
			for k, v := range m {
				added[k] = v
			}
			added["arguments"] = ""
			added["status"] = "in_progress"
			addedItem = added
		}
		emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": addedItem})
		if m["type"] == "message" {
			content, _ := m["content"].([]any)
			if len(content) > 0 {
				c, _ := content[0].(map[string]any)
				emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": i, "content_index": 0, "delta": c["text"]})
			}
		} else if m["type"] == "function_call" {
			args, _ := m["arguments"].(string)
			emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": m["id"], "delta": args})
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": m["id"], "arguments": args})
		} else if m["type"] == "custom_tool_call" {
			input, _ := m["input"].(string)
			emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i, "item_id": m["id"], "delta": input})
			emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": i, "item_id": m["id"], "input": input})
		} else if m["type"] == "reasoning" {
			summary, _ := m["summary"].([]any)
			if len(summary) > 0 {
				s, _ := summary[0].(map[string]any)
				txt, _ := s["text"].(string)
				emit("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": i, "content_index": 0, "item_id": m["id"], "delta": txt})
			}
		}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
	}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

func customToolInput(arguments any) string {
	if s, ok := arguments.(string); ok {
		var v struct {
			Input string `json:"input"`
		}
		if json.Unmarshal([]byte(s), &v) == nil {
			return v.Input
		}
	}
	return ""
}
