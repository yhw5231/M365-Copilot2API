package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"m365-copilot2api/internal/chathub"
)

// responsesRequest is the OpenAI Responses API request subset supported by the gateway.
//
// Parameters the gateway genuinely honors are converted into the internal
// request; parameters the gateway cannot honor are rejected with an explicit
// unsupported_parameter error instead of being silently dropped, so a client
// never believes a sampling/context option took effect when it did not.
type responsesRequest struct {
	Model        string `json:"model"`
	AccountID    string `json:"accountId,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Input        any    `json:"input"`
	// Text accepts both Responses API shapes: the output-text configuration
	// object ({"format":{"type":"json_object"|"json_schema",...}}) honored via
	// the same response_format path chat/completions uses, and the plain user
	// prompt string some OpenAI-compatible clients send as a top-level alias.
	Text               any              `json:"text,omitempty"`
	Tools              []map[string]any `json:"tools,omitempty"`
	ToolChoice         any              `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool            `json:"parallel_tool_calls,omitempty"`
	Stream             bool             `json:"stream,omitempty"`
	User               string           `json:"user,omitempty"`
	Reasoning          *reasoningConfig `json:"reasoning,omitempty"`
	Include            []string         `json:"include,omitempty"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Conversation       string           `json:"conversation,omitempty"`
	NewConversation    bool             `json:"new_conversation,omitempty"`
	Temperature        *float64         `json:"temperature,omitempty"`
	TopP               *float64         `json:"top_p,omitempty"`
	MaxOutputTokens    *int             `json:"max_output_tokens,omitempty"`
	// MaxTokens is the chat/completions-era alias some OpenAI-compatible
	// clients still send to the Responses endpoint; it feeds the same budget.
	MaxTokens         *int              `json:"max_tokens,omitempty"`
	FrequencyPenalty  *float64          `json:"frequency_penalty,omitempty"`
	PresencePenalty   *float64          `json:"presence_penalty,omitempty"`
	ServiceTier       string            `json:"service_tier,omitempty"`
	ContextManagement any               `json:"context_management,omitempty"`
	Store             *bool             `json:"store,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

// unsupportedParamError marks a request parameter the gateway cannot honor.
// The Responses handler maps it to an explicit "unsupported_parameter" error
// instead of silently ignoring the client's intent.
type unsupportedParamError struct {
	Param string
	Value any
}

func (e *unsupportedParamError) Error() string {
	return fmt.Sprintf("parameter %q is not supported by the Microsoft 365 backend", e.Param)
}

// ignoredSamplingParams lists sampling controls the Microsoft 365 ChatHub
// backend cannot apply. They are accepted for client compatibility but are
// reported back in the response metadata so the client never believes the
// value took effect. Only non-default values are listed.
func ignoredSamplingParams(body *oaiReq) ([]string, bool) {
	if body == nil {
		return nil, false
	}
	var ignored []string
	if body.Temperature != nil && *body.Temperature != 1.0 {
		ignored = append(ignored, "temperature")
	}
	if body.TopP != nil && *body.TopP != 1.0 {
		ignored = append(ignored, "top_p")
	}
	if body.FrequencyPenalty != nil && *body.FrequencyPenalty != 0 {
		ignored = append(ignored, "frequency_penalty")
	}
	if body.PresencePenalty != nil && *body.PresencePenalty != 0 {
		ignored = append(ignored, "presence_penalty")
	}
	return ignored, len(ignored) > 0
}

const samplingNote = "Microsoft 365 ChatHub backend does not support sampling controls; the listed parameters were accepted for compatibility but have no effect"

// supportedResponsesIncludes lists the `include` values the gateway can
// actually produce in a Responses result. Anything else is rejected so the
// client learns it must not expect that extra field.
var supportedResponsesIncludes = map[string]bool{
	"usage":                            true, // usage is always emitted
	"reasoning":                        true, // reasoning_content is forwarded when present
	"reasoning.summary":                true,
	"reasoning.encrypted_content":      true,
	"message.output_text.annotations":  true, // annotations are emitted as an empty list
	"response.output_text.annotations": true,
}

func (r responsesRequest) validateSupportedParams() error {
	for _, inc := range r.Include {
		key := strings.ToLower(strings.TrimSpace(inc))
		if key != "" && !supportedResponsesIncludes[key] {
			return &unsupportedParamError{Param: "include", Value: inc}
		}
	}
	tier := strings.ToLower(strings.TrimSpace(r.ServiceTier))
	if tier != "" && tier != "auto" {
		return &unsupportedParamError{Param: "service_tier", Value: r.ServiceTier}
	}
	if r.ContextManagement != nil {
		cm := "auto"
		switch v := r.ContextManagement.(type) {
		case string:
			cm = strings.ToLower(strings.TrimSpace(v))
		case map[string]any:
			if s, ok := v["type"].(string); ok {
				cm = strings.ToLower(strings.TrimSpace(s))
			}
		}
		if cm != "" && cm != "auto" {
			return &unsupportedParamError{Param: "context_management", Value: r.ContextManagement}
		}
	}
	return nil
}

// applyResponsesTextFormat honors the Responses API text.format output
// configuration by converting it into the same response_format handling the
// chat/completions endpoint applies (JSON prompt injection). text.format.type
// values: "text" (default, no-op), "json_object", "json_schema".
func applyResponsesTextFormat(o *oaiReq, text map[string]any) error {
	raw, ok := text["format"]
	if !ok || raw == nil {
		return nil
	}
	format, ok := raw.(map[string]any)
	if !ok {
		return &unsupportedParamError{Param: "text.format", Value: raw}
	}
	typ, _ := format["type"].(string)
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "", "text":
		return nil
	case "json_object":
		o.ResponseFormat = &responseFormat{Type: "json_object"}
		return nil
	case "json_schema":
		schema, _ := format["schema"].(map[string]any)
		o.ResponseFormat = &responseFormat{Type: "json_schema", JSONSchema: map[string]any{"schema": schema}}
		return nil
	default:
		return &unsupportedParamError{Param: "text.format", Value: typ}
	}
}

// shouldStoreResponsesHistory reports whether a completed response should be
// retained for previous_response_id continuation. The Responses API `store`
// parameter opts a client out of history retention; absent or true retains it.
func shouldStoreResponsesHistory(store *bool) bool {
	return store == nil || *store
}

const customExecWorkspaceInstruction = `You are operating through the caller's local OpenCode execution bridge. Never use, request, or mention Microsoft 365/Copilot native tools. The only permitted execution tool is the caller-provided custom exec tool. Modify only files within the scope explicitly specified by the caller. Every file or directory path used for reading, writing, editing, deleting, or verification must be explicitly provided by the caller; never infer, guess, discover, or substitute a path. A caller-provided path may be either an absolute project path or a path relative to the caller-selected project workspace. Do not assume paths such as /root, /workspace, /tmp, or /mnt/data. Never claim a file was created, modified, or verified until custom exec returns a successful result. After every execution, use custom exec to verify the result.`

func (r responsesRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, AccountID: r.AccountID, Stream: r.Stream, ToolChoice: r.ToolChoice, User: r.User, ParallelToolCalls: r.ParallelToolCalls, Metadata: r.Metadata}
	if err := r.validateSupportedParams(); err != nil {
		return o, err
	}
	// Conversation continuity: `conversation` names the upstream conversation to
	// resume (mirroring chat/completions conversation_id), while
	// `new_conversation` forces a fresh conversation regardless of any stored
	// session binding. Both were previously parsed but silently dropped.
	switch {
	case r.NewConversation:
		o.NewConversation = true
		o.ConversationID = ""
	default:
		o.ConversationID = r.Conversation
	}
	if r.Temperature != nil {
		o.Temperature = r.Temperature
	}
	if r.TopP != nil {
		o.TopP = r.TopP
	}
	o.FrequencyPenalty = r.FrequencyPenalty
	o.PresencePenalty = r.PresencePenalty
	if r.MaxOutputTokens != nil {
		o.MaxCompletionTokens = r.MaxOutputTokens
	} else if r.MaxTokens != nil {
		o.MaxTokens = r.MaxTokens
	}
	if instructions := strings.TrimSpace(r.Instructions); instructions != "" {
		o.Messages = append(o.Messages, oaiMsg{Role: "system", Content: instructions})
	}
	if r.Reasoning != nil {
		effort := strings.TrimSpace(r.Reasoning.Effort)
		// Validate the requested reasoning effort before the stream opens. An
		// invalid value must fail with a plain 400 (like chat/completions) rather
		// than a streamed response.failed after response.created. "auto" is
		// accepted here and resolved to the model's default by the chat adapter.
		if effort != "" && !strings.EqualFold(effort, "auto") {
			if _, err := normalizeReasoningEffort(effort); err != nil {
				return o, err
			}
		}
		o.Reasoning = r.Reasoning
		o.ReasoningEffort = effort
	}
	// `text` has two accepted shapes. OpenAI Responses clients send it as the
	// output-text configuration object (e.g. {"format":{"type":"json_object"}});
	// some OpenAI-compatible clients send it as a plain user-prompt string. A
	// string acts as an input alias when `input` is absent; an object is
	// honored as the text.format output configuration (JSON mode / schema).
	input := r.Input
	switch t := r.Text.(type) {
	case string:
		if input == nil && strings.TrimSpace(t) != "" {
			input = t
		}
	case map[string]any:
		if err := applyResponsesTextFormat(&o, t); err != nil {
			return o, err
		}
	}
	switch v := input.(type) {
	case string:
		if v == "" {
			return o, fmt.Errorf("input required")
		}
		o.Messages = append(o.Messages, oaiMsg{Role: "user", Content: v})
	case []any:
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call_progress":
				// Progress is deliberately not converted into an assistant/tool
				// message. It is transport metadata from a long-running client-side
				// executor and must not trigger a model turn or tool completion.
				if _, ok := parseToolProgress(m); !ok {
					return o, fmt.Errorf("invalid function_call_progress")
				}
				continue
			case "function_call_output":
				id, _ := m["call_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: m["output"]})
			case "custom_tool_call_output":
				id, _ := m["call_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: m["output"]})
			case "function_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args := m["arguments"]
				if s, ok := args.(string); ok {
					var x any
					if json.Unmarshal([]byte(s), &x) == nil {
						args = x
					}
				}
				o.Messages = append(o.Messages, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": mustJSON(args)}}}})
			case "custom_tool_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				input, _ := m["input"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "custom", "function": map[string]any{"name": name, "arguments": mustJSON(map[string]any{"input": input})}}}})
			default:
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				// Responses input items use input_text/input_image/input_file/
				// input_audio blocks. Keep the blocks intact so flattenPromptMessages
				// can extract every attachment into the ChatHub payload.
				content := m["content"]
				if content == nil {
					content = []any{m}
				}
				o.Messages = append(o.Messages, oaiMsg{Role: role, Content: content})
			}
		}
	default:
		if input == nil {
			return o, fmt.Errorf("input required")
		}
		return o, fmt.Errorf("input must be string or array")
	}
	hasCustomExec := false
	for _, t := range r.Tools {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
		if typ == "custom" && name == "exec" {
			hasCustomExec = true
			break
		}
	}
	for _, t := range r.Tools {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
		if hasCustomExec && !(typ == "custom" && name == "exec") {
			continue
		}
		f := map[string]any{"name": t["name"], "description": t["description"], "parameters": t["parameters"]}
		if typ == "custom" && name == "exec" {
			// ChatHub accepts JSON function arguments while Codex exec accepts a
			// grammar-constrained raw input string. Preserve the distinction in
			// Tool.Type and bridge the input through a single string field.
			f["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
			hasCustomExec = true
		} else if typ != "function" {
			continue
		}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: typ, Function: b})
	}
	if hasCustomExec {
		wsInst := customExecWorkspaceInstruction // fallback if unified builder returns ""
		if inst := unifiedWorkspaceInstruction(r.Tools); inst != "" {
			wsInst = inst
		}
		o.Messages = append([]oaiMsg{{Role: "system", Content: wsInst}}, o.Messages...)
	}
	return o, nil
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}
type anthropicRequest struct {
	Model         string             `json:"model"`
	System        any                `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
}

func (r anthropicRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, Stream: r.Stream}
	if r.MaxTokens > 0 {
		mt := r.MaxTokens
		o.MaxCompletionTokens = &mt
	}
	if len(r.StopSequences) > 0 {
		o.Stop = r.StopSequences
	}
	if r.System != nil {
		o.Messages = append(o.Messages, oaiMsg{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		if s, ok := m.Content.(string); ok {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: s})
			continue
		}
		blocks, ok := m.Content.([]any)
		if !ok {
			return o, fmt.Errorf("invalid anthropic content")
		}
		var text []any
		var calls []map[string]any
		for _, raw := range blocks {
			b, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := b["type"].(string)
			switch typ {
			case "text":
				text = append(text, b)
			case "image":
				source, _ := b["source"].(map[string]any)
				if source != nil {
					srcType, _ := source["type"].(string)
					switch srcType {
					case "base64":
						data, _ := source["data"].(string)
						media, _ := source["media_type"].(string)
						if data != "" {
							if media == "" {
								media = "application/octet-stream"
							}
							text = append(text, map[string]any{
								"type":      "input_image",
								"image_url": "data:" + media + ";base64," + data,
							})
						}
					case "url":
						url, _ := source["url"].(string)
						if url != "" {
							text = append(text, map[string]any{
								"type":      "input_image",
								"image_url": url,
							})
						}
					}
				}
			case "tool_use":
				calls = append(calls, map[string]any{"id": b["id"], "type": "function", "function": map[string]any{"name": b["name"], "arguments": mustJSON(b["input"])}})
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: b["content"]})
			}
		}
		if len(text) > 0 || len(calls) > 0 {
			o.Messages = append(o.Messages, oaiMsg{Role: m.Role, Content: text, ToolCalls: calls})
		}
	}
	for _, t := range r.Tools {
		f := map[string]any{"name": t.Name, "description": t.Description, "parameters": t.InputSchema}
		b, _ := json.Marshal(f)
		o.Tools = append(o.Tools, chathub.Tool{Type: "function", Function: b})
	}
	if c, ok := r.ToolChoice.(map[string]any); ok {
		switch c["type"] {
		case "auto":
			o.ToolChoice = "auto"
		case "any":
			o.ToolChoice = "required"
		case "none":
			o.ToolChoice = "none"
		case "tool":
			o.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": c["name"]}}
		}
	}
	return o, nil
}
