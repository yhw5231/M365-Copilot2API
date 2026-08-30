package web

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestResponsesToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "what time", Tools: []map[string]any{{"type": "function", "name": "clock", "parameters": map[string]any{"type": "object"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 1 || len(o.Tools) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

// TestResponsesPromptCacheKeyBecomesSessionKey verifies that the DSH/pi-ai
// prompt_cache_key (the client's stable per-session identifier) is mapped onto
// the gateway's SessionKey so the session resolver can keep the upstream
// conversation alive and detect context compaction.
func TestResponsesPromptCacheKeyBecomesSessionKey(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "hello", PromptCacheKey: "session-dsh-123"}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.SessionKey != "session-dsh-123" {
		t.Fatalf("prompt_cache_key should map to SessionKey, got %q", o.SessionKey)
	}
	if o.ConversationID != "" {
		t.Fatalf("conversation must stay empty so the resolver starts/binds a conversation, got %q", o.ConversationID)
	}
}

// TestResponsesPromptCacheKeyEmptyLeavesSessionKeyEmpty confirms that a request
// without prompt_cache_key does not invent a session identity.
func TestResponsesPromptCacheKeyEmptyLeavesSessionKeyEmpty(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "hello"}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.SessionKey != "" {
		t.Fatalf("no prompt_cache_key must not set SessionKey, got %q", o.SessionKey)
	}
}

func TestResponsesCustomExecToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "inspect", Tools: []map[string]any{{"type": "custom", "name": "exec", "description": "run a command", "format": map[string]any{"type": "grammar"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Tools) != 1 || o.Tools[0].Type != "custom" {
		t.Fatalf("tools=%+v err=%v", o.Tools, err)
	}
	if string(o.Tools[0].Function) == "" || !containsJSON(o.Tools[0].Function, "input") {
		t.Fatalf("custom exec did not receive an input schema: %s", o.Tools[0].Function)
	}
}

func TestResponsesCustomExecIsExclusiveTool(t *testing.T) {
	r := responsesRequest{Input: "edit the project", Tools: []map[string]any{
		{"type": "custom", "name": "exec", "description": "local execution"},
		{"type": "function", "name": "m365_search", "description": "native search"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Tools) != 1 || o.Tools[0].Type != "custom" {
		t.Fatalf("tools=%#v, want only custom exec", o.Tools)
	}
	if !strings.Contains(fmt.Sprint(o.Messages[0].Content), "Never use") {
		t.Fatalf("missing native-tool prohibition: %#v", o.Messages)
	}
}

func TestResponsesInstructionsAndCustomExecPolicyAreSystemMessages(t *testing.T) {
	r := responsesRequest{
		Instructions: "Use the repository selected by the caller.",
		Input:        "inspect the repository",
		Tools:        []map[string]any{{"type": "custom", "name": "exec", "description": "run a command"}},
	}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 3 {
		t.Fatalf("messages=%#v", o.Messages)
	}
	if o.Messages[0].Role != "system" {
		t.Fatalf("missing custom exec policy: %#v", o.Messages[0])
	}
	expectedInst := unifiedWorkspaceInstruction(r.Tools)
	if expectedInst == "" {
		expectedInst = customExecWorkspaceInstruction
	}
	if o.Messages[0].Content != expectedInst {
		t.Fatalf("custom exec policy mismatch:\ngot:  %q\nwant: %q", o.Messages[0].Content, expectedInst)
	}
	if o.Messages[1].Role != "system" || o.Messages[1].Content != r.Instructions {
		t.Fatalf("instructions not preserved: %#v", o.Messages[1])
	}
	if o.Messages[2].Role != "user" || o.Messages[2].Content != r.Input {
		t.Fatalf("input ordering changed: %#v", o.Messages[2])
	}
}

func TestResponsesCustomToolOutputToOpenAI(t *testing.T) {
	r := responsesRequest{Input: []any{
		map[string]any{"type": "custom_tool_call", "call_id": "call_exec", "name": "exec", "input": "uname -s"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_exec", "output": "Linux"},
	}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || o.Messages[0].Role != "assistant" || o.Messages[0].ToolCalls[0]["type"] != "custom" || o.Messages[1].Role != "tool" || o.Messages[1].ToolCallID != "call_exec" {
		t.Fatalf("messages=%+v err=%v", o.Messages, err)
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("custom tool continuation rejected: %v", err)
	}
}

// TestResponsesFunctionCallWithoutCallIDFallsBackToItemID reproduces the
// "assistant tool call missing id" 400: some Responses clients (e.g. the DSH
// harness) replay an assistant tool call item carrying only the item `id` and
// no `call_id`. The converter must fall back to the item id (and generate a
// stable id as a last resort) so the tool conversation stays valid.
func TestResponsesFunctionCallWithoutCallIDFallsBackToItemID(t *testing.T) {
	r := responsesRequest{Model: "m", Input: []any{
		map[string]any{"type": "function_call", "id": "fc_goal", "name": "create_goal", "arguments": `{"objective":"x"}`},
		map[string]any{"type": "function_call_output", "id": "fo_1", "call_id": "fc_goal", "output": "created"},
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 3 || o.Messages[0].Role != "assistant" || len(o.Messages[0].ToolCalls) != 1 {
		t.Fatalf("messages=%#v", o.Messages)
	}
	if id, _ := o.Messages[0].ToolCalls[0]["id"].(string); id == "" {
		t.Fatalf("assistant tool call id must not be empty: %#v", o.Messages[0].ToolCalls[0])
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("tool continuation rejected: %v", err)
	}
}

// TestResponsesCustomToolCallWithoutCallIDFallsBackToItemID covers the same
// replay scenario for custom_tool_call items (the custom exec bridge).
func TestResponsesCustomToolCallWithoutCallIDFallsBackToItemID(t *testing.T) {
	r := responsesRequest{Model: "m", Input: []any{
		map[string]any{"type": "custom_tool_call", "id": "ctc_exec", "name": "exec", "input": "ls"},
		map[string]any{"type": "custom_tool_call_output", "id": "cto_1", "call_id": "ctc_exec", "output": "file.txt"},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 2 || o.Messages[0].Role != "assistant" || len(o.Messages[0].ToolCalls) != 1 {
		t.Fatalf("messages=%#v", o.Messages)
	}
	if id, _ := o.Messages[0].ToolCalls[0]["id"].(string); id == "" {
		t.Fatalf("custom tool call id must not be empty: %#v", o.Messages[0].ToolCalls[0])
	}
	if err := validateToolConversation(o.Messages); err != nil {
		t.Fatalf("custom tool continuation rejected: %v", err)
	}
}

func TestResponsesParallelToolCallsAndMetadata(t *testing.T) {
	false_ := false
	true_ := true
	r := responsesRequest{
		Model:             "m",
		Input:             "hi",
		ParallelToolCalls: &false_,
		Store:             &true_,
		Metadata:          map[string]string{"session": "s-1"},
	}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.ParallelToolCalls == nil || *o.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls lost: %+v", o.ParallelToolCalls)
	}
	if len(o.Metadata) != 1 || o.Metadata["session"] != "s-1" {
		t.Fatalf("metadata lost: %+v", o.Metadata)
	}
}

func TestResponsesTextInputAlias(t *testing.T) {
	r := responsesRequest{Model: "m", Text: "inspect this repo"}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 1 || o.Messages[0].Role != "user" || o.Messages[0].Content != "inspect this repo" {
		t.Fatalf("text alias not honored: %#v", o.Messages)
	}
}

func TestResponsesRejectsUnsupportedParams(t *testing.T) {
	cases := []responsesRequest{
		{Model: "m", Input: "hi", ServiceTier: "flex"},
		{Model: "m", Input: "hi", ContextManagement: "ephemeral"},
		{Model: "m", Input: "hi", Include: []string{"file_search_call.results"}},
	}
	for _, r := range cases {
		_, err := r.openAI()
		if err == nil {
			t.Fatalf("request %+v must be rejected", r)
		}
		var unsupported *unsupportedParamError
		if !errors.As(err, &unsupported) {
			t.Fatalf("expected unsupportedParamError, got %T: %v", err, err)
		}
	}
}

func TestResponsesAcceptsSupportedIncludeAndDefaults(t *testing.T) {
	r := responsesRequest{
		Model:             "m",
		Input:             "hi",
		Include:           []string{"usage", "reasoning.summary", "message.output_text.annotations"},
		ServiceTier:       "auto",
		ContextManagement: "auto",
	}
	if _, err := r.openAI(); err != nil {
		t.Fatalf("supported include/defaults rejected: %v", err)
	}
}

func TestResponsesReasoningEffortValidation(t *testing.T) {
	// Valid efforts are preserved and forwarded.
	for _, e := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		r := responsesRequest{Model: "m", Input: "hi", Reasoning: &reasoningConfig{Effort: e}}
		o, err := r.openAI()
		if err != nil {
			t.Fatalf("effort %q rejected: %v", e, err)
		}
		if o.ReasoningEffort != e {
			t.Fatalf("effort %q lost: got %q", e, o.ReasoningEffort)
		}
	}
	// "auto" is accepted as a request for the model's default reasoning level.
	for _, e := range []string{"auto", "AUTO"} {
		r := responsesRequest{Model: "m", Input: "hi", Reasoning: &reasoningConfig{Effort: e}}
		o, err := r.openAI()
		if err != nil {
			t.Fatalf("auto effort %q rejected: %v", e, err)
		}
		if o.ReasoningEffort != e {
			t.Fatalf("auto effort %q lost: got %q", e, o.ReasoningEffort)
		}
	}
	// Invalid efforts fail fast so the stream never opens.
	for _, e := range []string{"extreme", "bogus"} {
		r := responsesRequest{Model: "m", Input: "hi", Reasoning: &reasoningConfig{Effort: e}}
		_, err := r.openAI()
		if err == nil {
			t.Fatalf("invalid effort %q must be rejected", e)
		}
	}
}

func TestDropSystemInstructionsReplacesPreviousTurn(t *testing.T) {
	hist := []oaiMsg{
		{Role: "system", Content: "OLD instructions that must not survive"},
		{Role: "developer", Content: "OLD developer rule"},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "I asked for a delay"},
	}
	hist = append(hist, oaiMsg{Role: "assistant", Content: "ok I have awaited and think about it"})
	out := dropSystemInstructions(hist)
	for _, m := range out {
		if m.Role == "system" || m.Role == "developer" {
			t.Fatalf("stale instruction survived: %#v", m)
		}
	}
	if len(out) != 4 {
		t.Fatalf("want 4 history messages, got %d: %#v", len(out), out)
	}
	if out[0].Role != "user" || out[0].Content != "first question" {
		t.Fatalf("history order corrupted: %#v", out)
	}
	if out[2].Content != "I asked for a delay" {
		t.Fatalf("history order corrupted: %#v", out)
	}
}

func TestAnthropicToOpenAI(t *testing.T) {
	r := anthropicRequest{Model: "m", System: any("be concise"), Messages: []anthropicMessage{{Role: "user", Content: any("weather")}}, Tools: []anthropicTool{{Name: "weather", InputSchema: map[string]any{"type": "object"}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || len(o.Tools) != 1 {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestAnthropicToolResult(t *testing.T) {
	r := anthropicRequest{Messages: []anthropicMessage{{Role: "assistant", Content: []any{map[string]any{"type": "tool_use", "id": "x", "name": "f", "input": map[string]any{}}}}, {Role: "user", Content: []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "ok"}}}}}
	o, err := r.openAI()
	if err != nil || len(o.Messages) != 2 || o.Messages[1].ToolCallID != "x" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestResponsesConversationAndNewConversation(t *testing.T) {
	base := responsesRequest{Model: "m", Input: "hi", Conversation: "conv-123"}
	o, err := base.openAI()
	if err != nil || o.ConversationID != "conv-123" || o.NewConversation {
		t.Fatalf("conversation not honored: %+v err=%v", o, err)
	}
	fresh := responsesRequest{Model: "m", Input: "hi", Conversation: "conv-123", NewConversation: true}
	o, err = fresh.openAI()
	if err != nil || !o.NewConversation || o.ConversationID != "" {
		t.Fatalf("new_conversation not honored: %+v err=%v", o, err)
	}
}

func TestResponsesTextFormatJSONObject(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "hi", Text: map[string]any{"format": map[string]any{"type": "json_object"}}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.ResponseFormat == nil || o.ResponseFormat.Type != "json_object" {
		t.Fatalf("json_object format lost: %+v", o.ResponseFormat)
	}
}

func TestResponsesTextFormatJSONSchema(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "hi", Text: map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}}}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.ResponseFormat == nil || o.ResponseFormat.Type != "json_schema" {
		t.Fatalf("json_schema format lost: %+v", o.ResponseFormat)
	}
	if _, ok := o.ResponseFormat.JSONSchema["schema"]; !ok {
		t.Fatalf("json_schema payload lost: %+v", o.ResponseFormat.JSONSchema)
	}
}

func TestResponsesTextFormatRejectsUnknown(t *testing.T) {
	r := responsesRequest{Model: "m", Input: "hi", Text: map[string]any{"format": map[string]any{"type": "bogus"}}}
	_, err := r.openAI()
	if err == nil {
		t.Fatal("unknown text.format.type must be rejected")
	}
	var unsupported *unsupportedParamError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected unsupportedParamError, got %T: %v", err, err)
	}
}

func TestResponsesTextStringStillActsAsInputAlias(t *testing.T) {
	// An object `text` config plus a string `input` must not clobber the prompt.
	r := responsesRequest{Model: "m", Input: "real prompt", Text: map[string]any{"format": map[string]any{"type": "json_object"}}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 1 || o.Messages[0].Content != "real prompt" {
		t.Fatalf("object text replaced the input: %#v", o.Messages)
	}
	if o.ResponseFormat == nil {
		t.Fatal("format must still apply alongside string input")
	}
}

func TestResponsesMaxTokensAlias(t *testing.T) {
	mt := 128
	r := responsesRequest{Model: "m", Input: "hi", MaxTokens: &mt}
	o, err := r.openAI()
	if err != nil || o.MaxTokens == nil || *o.MaxTokens != 128 {
		t.Fatalf("max_tokens alias lost: %+v err=%v", o.MaxTokens, err)
	}
	// max_output_tokens takes precedence over the max_tokens alias.
	mo := 64
	r2 := responsesRequest{Model: "m", Input: "hi", MaxTokens: &mt, MaxOutputTokens: &mo}
	o2, err := r2.openAI()
	if err != nil || o2.MaxCompletionTokens == nil || *o2.MaxCompletionTokens != 64 {
		t.Fatalf("max_output_tokens precedence lost: %+v err=%v", o2.MaxCompletionTokens, err)
	}
}

func TestResponsesSamplingPenaltiesAreNoted(t *testing.T) {
	fp := 0.5
	pp := 0.2
	r := responsesRequest{Model: "m", Input: "hi", FrequencyPenalty: &fp, PresencePenalty: &pp}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.FrequencyPenalty == nil || *o.FrequencyPenalty != fp || o.PresencePenalty == nil || *o.PresencePenalty != pp {
		t.Fatalf("penalties lost: %+v", o)
	}
	ignored, ok := ignoredSamplingParams(&o)
	if !ok || len(ignored) != 2 {
		t.Fatalf("expected both penalties reported as ignored, got %v", ignored)
	}
}

func TestResponsesStoreHistoryGuard(t *testing.T) {
	if !shouldStoreResponsesHistory(nil) {
		t.Fatal("nil store must retain history")
	}
	tr := true
	if !shouldStoreResponsesHistory(&tr) {
		t.Fatal("store=true must retain history")
	}
	fa := false
	if shouldStoreResponsesHistory(&fa) {
		t.Fatal("store=false must not retain history")
	}
}
