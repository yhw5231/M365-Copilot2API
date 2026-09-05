package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"m365-copilot2api/internal/chathub"

	"github.com/google/uuid"
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
	// PromptCacheKey is the session-scoped cache key some OpenAI Responses
	// clients (e.g. the DSH harness through pi-ai) send to bind prompt-cache
	// affinity. The Microsoft 365 backend cannot honor prompt caching, but the
	// value is the client's stable per-session identifier. The gateway maps it
	// onto SessionKey so the session resolver can keep the upstream
	// conversation alive across turns and detect context compaction.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
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
	// Generation-shape controls the upstream cannot honor. They are accepted
	// for compatibility but naming them in the note keeps the response honest
	// instead of silently dropping a parameter the client asked for.
	if body.Stop != nil {
		ignored = append(ignored, "stop")
	}
	if body.N != nil && *body.N > 1 {
		ignored = append(ignored, "n")
	}
	if body.Seed != nil {
		ignored = append(ignored, "seed")
	}
	if body.Logprobs != nil && *body.Logprobs {
		ignored = append(ignored, "logprobs")
	}
	if body.TopLogprobs != nil && *body.TopLogprobs > 0 {
		ignored = append(ignored, "top_logprobs")
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

const customExecWorkspaceInstruction = `You are operating through the caller's local OpenCode execution bridge. Use only the tools declared in this session; do not attempt Microsoft 365/Copilot native tools. The execution tool is the caller-provided custom exec tool, which runs directly on the caller's local machine. You have full access to the caller's local machine and its files. Modify files within the scope of the caller's project workspace. Work with the paths in the workspace: use absolute project paths or paths relative to the caller-selected project workspace. Only report a file as created, modified, or verified after custom exec returns a successful result. After every execution, use custom exec to verify the result.`

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
	// The DSH harness (through pi-ai) sends prompt_cache_key as its per-session
	// identifier on /v1/responses. The Microsoft 365 backend cannot honor prompt
	// caching, but the value is a stable downstream session id. Mapping it onto
	// SessionKey lets the session resolver keep the upstream conversation alive
	// across turns (explicit_incremental) and detect context compaction
	// (explicit_context_reset -> ResetUpstream), which is what keeps the model
	// from hallucinating in long goal sessions.
	if r.PromptCacheKey != "" {
		o.SessionKey = r.PromptCacheKey
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
	// additionalTools holds tool definitions declared inline as
	// "additional_tools" input items (see the case below). They are merged
	// into the top-level tool list after the input loop.
	var additionalTools []map[string]any
	switch v := input.(type) {
	case string:
		if v == "" {
			return o, fmt.Errorf("input required")
		}
		o.Messages = append(o.Messages, oaiMsg{Role: "user", Content: v})
	case []any:
		for i := 0; i < len(v); {
			raw := v[i]
			i++
			// Plain string items are official Responses input ("hi" ≡
			// {"type":"message","role":"user","content":"hi"}); dropping them
			// silently emptied the whole request for clients like `input: ["hi"]`.
			if s, ok := raw.(string); ok {
				if strings.TrimSpace(s) != "" {
					o.Messages = append(o.Messages, oaiMsg{Role: "user", Content: s})
				}
				continue
			}
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "reasoning":
				// OpenAI Responses clients (e.g. the DSH harness) replay a prior
				// assistant turn's reasoning as a standalone "reasoning" item in
				// the input array. It carries no "role" field, so the default
				// branch below would otherwise turn it into a spurious user
				// message, shifting the message sequence and defeating the
				// session resolver's prefix match: every turn would then look
				// like a context reset, force a new upstream conversation, and
				// destroy prompt caching. Reasoning is thinking metadata, not
				// conversation content — skip it. The following message item
				// carries the assistant's actual output, and the resumed
				// upstream conversation already holds its own reasoning state.
				continue
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
				if id == "" {
					id, _ = m["id"].(string)
				}
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: m["output"]})
			case "custom_tool_call_output":
				id, _ := m["call_id"].(string)
				if id == "" {
					id, _ = m["id"].(string)
				}
				o.Messages = append(o.Messages, oaiMsg{Role: "tool", ToolCallID: id, Content: m["output"]})
			case "additional_tools":
				// Codex Desktop 0.153+ declares session tools inside the input
				// array as an additional_tools item instead of the top-level
				// `tools` field. The item is a tool declaration, not
				// conversation content: dropping it left the model with zero
				// tools, so it answered file/command requests in plain chat
				// ("file not found / upload it to the conversation"). Collect
				// the definitions and merge them into the tool list below.
				additionalTools = append(additionalTools, collectAdditionalTools(m)...)
				continue
			case "function_call", "custom_tool_call":
				// DSH (and other OpenAI Responses clients) replay parallel tool
				// calls from a single assistant turn as consecutive separate
				// function_call/custom_tool_call items in the input array.
				// Emitting one oaiMsg per item would produce multiple
				// consecutive assistant messages, breaking the tool protocol
				// ("tool results missing before assistant message" 400).
				// Group consecutive call items into ONE assistant message
				// with multiple tool calls.
				var calls []map[string]any
				for j := i - 1; j < len(v); j++ {
					cm, cok := v[j].(map[string]any)
					if !cok {
						break
					}
					ctyp, _ := cm["type"].(string)
					if ctyp != "function_call" && ctyp != "custom_tool_call" {
						break
					}
					i = j + 1 // advance the outer loop past this item
					id := responsesToolCallID(cm)
					name, _ := cm["name"].(string)
					switch ctyp {
					case "function_call":
						args := cm["arguments"]
						if s, ok := args.(string); ok {
							var x any
							if json.Unmarshal([]byte(s), &x) == nil {
								args = x
							}
						}
						calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": mustJSON(args)}})
					case "custom_tool_call":
						input, _ := cm["input"].(string)
						calls = append(calls, map[string]any{"id": id, "type": "custom", "function": map[string]any{"name": name, "arguments": mustJSON(map[string]any{"input": input})}})
					}
				}
				if len(calls) > 0 {
					o.Messages = append(o.Messages, oaiMsg{Role: "assistant", ToolCalls: calls})
				}
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
	// additionalTools names are exempt from the exec-only filter below: a
	// client that deliberately declares wait/request_user_input/MCP tools
	// next to exec (Codex Desktop code mode) expects all of them honored,
	// unlike the OpenCode bridge pattern where exec replaces generic tools.
	additionalNames := map[string]bool{}
	if len(additionalTools) > 0 {
		for _, t := range additionalTools {
			if n, _ := t["name"].(string); n != "" {
				additionalNames[n] = true
			}
		}
		r.Tools = append(r.Tools, additionalTools...)
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
		if hasCustomExec && !(typ == "custom" && name == "exec") && !additionalNames[name] {
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
	// PromptCacheKey and metadata.user_id carry the relay's stable per-session
	// identity (relays forward session_id/prompt_cache_key; Claude Code clients
	// send metadata.user_id). Mapping them onto SessionKey lets the session
	// resolver reuse the upstream conversation across turns, which is what
	// produces a non-zero cache_read_input_tokens for Anthropic clients.
	PromptCacheKey string                `json:"prompt_cache_key,omitempty"`
	Metadata       *anthropicReqMetadata `json:"metadata,omitempty"`
}

// anthropicReqMetadata carries the Anthropic-standard request metadata subset
// the gateway uses for session affinity.
type anthropicReqMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

func (r anthropicRequest) openAI() (oaiReq, error) {
	o := oaiReq{Model: r.Model, Stream: r.Stream}
	// Session affinity: prompt_cache_key wins (explicit cache key), then the
	// Anthropic-standard metadata.user_id. Without a stable identity the
	// resolver starts a fresh upstream conversation every turn and the cache
	// fields in the downstream usage can never become non-zero.
	if key := strings.TrimSpace(r.PromptCacheKey); key != "" {
		o.SessionKey = key
	} else if r.Metadata != nil {
		if uid := strings.TrimSpace(r.Metadata.UserID); uid != "" {
			o.SessionKey = uid
		}
	}
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
			case "additional_tools":
				// Codex Desktop 0.153+ relays may bridge the Responses-style
				// additional_tools declaration onto the Anthropic endpoint as a
				// content block. Flatten namespace groups and merge the defs into
				// the request tools; the block itself is not conversation content.
				for _, t := range flattenAdditionalToolGroups(blockList(b["tools"])) {
					name, _ := t["name"].(string)
					if name == "" {
						continue
					}
					schema, _ := t["parameters"].(map[string]any)
					if tt, _ := t["type"].(string); tt == "custom" && name == "exec" {
						schema = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
					}
					if schema == nil {
						schema = map[string]any{"type": "object", "properties": map[string]any{}}
					}
					desc, _ := t["description"].(string)
					r.Tools = append(r.Tools, anthropicTool{Name: name, Description: desc, InputSchema: schema})
				}
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

// responsesToolCallID resolves the id used for a replayed Responses tool call.
// The canonical pairing key is `call_id`; when a client replays the item
// without one, fall back to the item's own `id`, and finally to a generated
// id so the assistant tool call is never forwarded with an empty id (which
// the tool-conversation validator rejects with a 400).
func responsesToolCallID(m map[string]any) string {
	if id, _ := m["call_id"].(string); id != "" {
		return id
	}
	if id, _ := m["id"].(string); id != "" {
		return id
	}
	return "call_" + uuid.NewString()
}

// collectAdditionalTools flattens a Responses "additional_tools" input item
// into Responses tool definitions. Codex Desktop 0.153+ nests the real tool
// defs one level deep inside namespace groups:
//
//	{"type":"additional_tools","tools":[
//	  {"type":"namespace","name":"functions","tools":[tooldef...]},
//	  {"type":"namespace","name":"mcp__x","tools":[tooldef...]}]}
//
// A flat tooldef array (no namespace wrapper) is accepted the same way: an
// entry carrying a nested "tools" array is treated as a namespace group,
// anything else as a direct tool definition. Entries without a recognizable
// name are skipped rather than turned into broken tool schemas.
func collectAdditionalTools(item map[string]any) []map[string]any {
	raw, ok := item["tools"].([]any)
	if !ok {
		return nil
	}
	return flattenAdditionalToolGroups(raw)
}

// flattenAdditionalToolGroups expands a list of additional-tools entries:
// entries with a nested "tools" array are namespace groups whose children are
// collected, plain entries pass through when they carry a name.
func flattenAdditionalToolGroups(raw []any) []map[string]any {
	var out []map[string]any
	for _, e := range raw {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if nested, ok := em["tools"].([]any); ok {
			out = append(out, flattenAdditionalToolGroups(nested)...)
			continue
		}
		if n, _ := em["name"].(string); n != "" {
			out = append(out, em)
		}
	}
	return out
}

// blockList coerces a tools field that may arrive as []any or []map[string]any
// into a uniform []any for flattenAdditionalToolGroups.
func blockList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []map[string]any:
		out := make([]any, 0, len(t))
		for _, m := range t {
			out = append(out, m)
		}
		return out
	default:
		return nil
	}
}

// collectChatAdditionalTools unwraps additional_tools definitions from a
// chat/completions message. Relays bridging Codex Desktop onto the chat
// endpoint embed the Responses-style item as the message content (either the
// raw map or a content block list containing it). The returned definitions
// are Responses tool shapes ready for the shared conversion loop.
func collectChatAdditionalTools(content any) []map[string]any {
	switch v := content.(type) {
	case map[string]any:
		if t, _ := v["type"].(string); t == "additional_tools" {
			return collectAdditionalTools(v)
		}
	case []any:
		var out []map[string]any
		for _, b := range v {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := bm["type"].(string); t == "additional_tools" {
				out = append(out, collectAdditionalTools(bm)...)
			}
		}
		return out
	}
	return nil
}

// responsesToolDefsToChatHub converts flattened Responses tool definitions
// into internal ChatHub tools, applying the same custom-exec argument bridge
// (grammar-constrained raw input -> single "input" string property) the
// Responses path uses.
func responsesToolDefsToChatHub(defs []map[string]any) []chathub.Tool {
	var out []chathub.Tool
	for _, t := range defs {
		typ, _ := t["type"].(string)
		name, _ := t["name"].(string)
		f := map[string]any{"name": name, "description": t["description"], "parameters": t["parameters"]}
		if typ == "custom" && name == "exec" {
			f["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
		} else if typ != "function" && typ != "" {
			continue
		}
		b, _ := json.Marshal(f)
		out = append(out, chathub.Tool{Type: typ, Function: b})
	}
	return out
}

// chatHubToolNames lists the names already declared in a chathub.Tool list so
// additional_tools definitions can be merged without duplicates.
func chatHubToolNames(tools []chathub.Tool) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		var f map[string]any
		if json.Unmarshal(t.Function, &f) != nil {
			continue
		}
		if n, _ := f["name"].(string); n != "" {
			out[n] = true
		}
	}
	return out
}

// stripAdditionalToolsBlocks removes additional_tools markers from message
// content while preserving every other block (text, images, ...). A content
// that was exactly the marker becomes nil so no empty message is forwarded.
func stripAdditionalToolsBlocks(content any) any {
	switch v := content.(type) {
	case map[string]any:
		if t, _ := v["type"].(string); t == "additional_tools" {
			return nil
		}
	case []any:
		var kept []any
		for _, b := range v {
			bm, ok := b.(map[string]any)
			if ok {
				if t, _ := bm["type"].(string); t == "additional_tools" {
					continue
				}
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			return nil
		}
		return kept
	}
	return content
}
