package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelTokenLimitsAreConsistent(t *testing.T) {
	t.Setenv("M365_CONTEXT_WINDOW", "128000")
	t.Setenv("M365_MAX_OUTPUT_TOKENS", "16384")
	l := configuredModelLimits()
	if l.ContextWindow != 128000 || l.MaxOutputTokens != 16384 || l.MaxInputTokens != 111616 {
		t.Fatalf("limits=%+v", l)
	}
}

func TestModelTokenLimitsNormalizeBadOutputLimit(t *testing.T) {
	t.Setenv("M365_CONTEXT_WINDOW", "100")
	t.Setenv("M365_MAX_OUTPUT_TOKENS", "500")
	l := configuredModelLimits()
	if l.MaxInputTokens <= 0 || l.MaxOutputTokens <= 0 || l.MaxInputTokens+l.MaxOutputTokens != l.ContextWindow {
		t.Fatalf("inconsistent limits=%+v", l)
	}
}

func TestModelsAdvertiseContextAndReasoning(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	s.openaiModels(w, r)
	var body struct {
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) == 0 {
		t.Fatal("empty model catalog")
	}
	if len(body.Models) != len(body.Data) {
		t.Fatalf("models alias length=%d, data length=%d", len(body.Models), len(body.Data))
	}
	for _, m := range body.Data {
		if m["owned_by"] != "gateway" || m["description"] != "Public model endpoint." {
			t.Fatalf("model catalog exposes provider details: %#v", m)
		}
		baseInstructions, ok := m["base_instructions"].(string)
		if !ok || baseInstructions == "" {
			t.Fatalf("missing Codex base instructions: %#v", m)
		}
		modelMessages, ok := m["model_messages"].(map[string]any)
		if !ok || modelMessages["instructions_template"] != baseInstructions {
			t.Fatalf("missing or inconsistent Codex model messages: %#v", m)
		}
		variables, ok := modelMessages["instructions_variables"].(map[string]any)
		if !ok || variables["personality_default"] != "" || variables["personality_friendly"] != "" || variables["personality_pragmatic"] != "" {
			t.Fatalf("invalid Codex instruction variables: %#v", modelMessages)
		}
		if modelMessages["approvals"] != nil || modelMessages["auto_review"] != nil {
			t.Fatalf("invalid optional Codex model messages: %#v", modelMessages)
		}
		if m["slug"] != m["id"] {
			t.Fatalf("missing or inconsistent slug: %#v", m)
		}
		if displayName, ok := m["display_name"].(string); !ok || displayName == "" {
			t.Fatalf("missing display_name: %#v", m)
		}
		levels, ok := m["supported_reasoning_levels"].([]any)
		if !ok || len(levels) == 0 {
			t.Fatalf("missing supported reasoning levels: %#v", m)
		}
		for _, level := range levels {
			preset, ok := level.(map[string]any)
			if !ok || preset["effort"] == "" || preset["description"] == "" {
				t.Fatalf("invalid reasoning preset: %#v", level)
			}
		}
		defaultReasoningLevel, ok := m["default_reasoning_level"].(string)
		if effort, err := normalizeReasoningEffort(defaultReasoningLevel); !ok || err != nil || effort == "" || m["description"] == "" {
			t.Fatalf("missing Codex catalog metadata: %#v", m)
		}
		if m["shell_type"] != "shell_command" || m["visibility"] != "list" || m["supported_in_api"] != true || m["priority"] != float64(1) {
			t.Fatalf("missing Codex execution metadata: %#v", m)
		}
		if _, ok := m["additional_speed_tiers"].([]any); !ok {
			t.Fatalf("missing speed tiers: %#v", m)
		}
		if _, ok := m["service_tiers"].([]any); !ok {
			t.Fatalf("missing service tiers: %#v", m)
		}
		if m["apply_patch_tool_type"] != "freeform" || m["web_search_tool_type"] != "text_and_image" || m["tool_mode"] != "code_mode_only" || m["multi_agent_version"] != "v2" {
			t.Fatalf("missing Codex tool metadata: %#v", m)
		}
		if m["max_context_window"] != m["context_window"] || m["effective_context_window_percent"] != float64(95) {
			t.Fatalf("inconsistent Codex context metadata: %#v", m)
		}
		policy, ok := m["truncation_policy"].(map[string]any)
		if !ok || policy["mode"] != "tokens" || policy["limit"] != float64(10000) {
			t.Fatalf("missing truncation policy: %#v", m)
		}
		if _, ok := m["experimental_supported_tools"].([]any); !ok || m["supports_search_tool"] != true || m["use_responses_lite"] != false {
			t.Fatalf("missing Codex capability metadata: %#v", m)
		}
		if m["context_window"].(float64) <= 0 || m["max_input_tokens"].(float64) <= 0 || m["max_output_tokens"].(float64) <= 0 {
			t.Fatalf("missing limits: %#v", m)
		}
		caps, ok := m["capabilities"].(map[string]any)
		if !ok {
			t.Fatalf("missing capabilities: %#v", m)
		}
		if caps["reasoning"] != true {
			t.Fatalf("reasoning not advertised: %#v", m)
		}
		if levels, ok := caps["supported_reasoning_levels"].([]any); !ok || len(levels) == 0 {
			t.Fatalf("capabilities missing supported reasoning levels: %#v", m)
		}
		if efforts, ok := caps["reasoning_efforts"].([]any); !ok || len(efforts) == 0 {
			t.Fatalf("capabilities missing object reasoning efforts: %#v", m)
		} else if _, ok := efforts[0].(map[string]any); !ok {
			t.Fatalf("reasoning efforts must be preset objects: %#v", efforts)
		}
	}
	for i, m := range body.Models {
		if m["slug"] != body.Data[i]["slug"] {
			t.Fatalf("models alias missing slug at %d: %#v", i, m)
		}
		if m["display_name"] != body.Data[i]["display_name"] {
			t.Fatalf("models alias missing display_name at %d: %#v", i, m)
		}
		if m["supported_reasoning_levels"] == nil {
			t.Fatalf("models alias missing supported reasoning levels at %d: %#v", i, m)
		}
		if m["base_instructions"] != body.Data[i]["base_instructions"] || m["model_messages"] == nil {
			t.Fatalf("models alias missing instruction metadata at %d: %#v", i, m)
		}
	}
}

func TestConfiguredModelMappingsDriveCatalogAndRouting(t *testing.T) {
	mappings := []modelMapping{{PublicModel: "gpt-5.6-sol", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "low"}}
	models := configuredModelSpecs(mappings)
	// 与路由表一致：内置可服务的 gpt-5.2/5.4/5.5 + 已启用的 gpt-5.6-sol。
	if len(models) != 4 || models[3].ID != "gpt-5.6-sol" || models[3].DefaultReasoningLevel != "low" {
		t.Fatalf("configured models=%#v", models)
	}
	mapping, ok := configuredModelMapping("GPT-5.6-SOL", mappings)
	if !ok || mapping.UpstreamMapping != "Gpt_5_6_Reasoning" {
		t.Fatalf("mapping=%#v ok=%t", mapping, ok)
	}
	if tone, ok := configuredModelTone("gpt-5.6-sol", mappings); !ok || tone != "Gpt_5_6_Reasoning" {
		t.Fatalf("tone=%q ok=%t", tone, ok)
	}
	override := configuredModelSpecs([]modelMapping{{PublicModel: "gpt-5.5", UpstreamMapping: "Gpt_5_5_Reasoning", DisplayName: "GPT-5.5", DefaultReasoningLevel: "high"}})
	if len(override) != 3 {
		t.Fatalf("built-in override models=%#v", override)
	}
	found := false
	for _, m := range override {
		if m.ID == "gpt-5.5" && m.DefaultReasoningLevel == "high" && m.DisplayName == "GPT-5.5" {
			found = true
		}
	}
	if !found {
		t.Fatalf("built-in override not applied: %#v", override)
	}
}

func TestReasoningEffortRouting(t *testing.T) {
	// 使测试不依赖本机持久化的 settings 文件：临时清空模型映射，让
	// reasoningTone 走 effort 路由分支，结束后恢复原映射。
	priorMappings := openSettingsStore().v.ModelMappings
	openSettingsStore().mu.Lock()
	openSettingsStore().v.ModelMappings = nil
	openSettingsStore().mu.Unlock()
	defer func() {
		openSettingsStore().mu.Lock()
		openSettingsStore().v.ModelMappings = priorMappings
		openSettingsStore().mu.Unlock()
	}()
	cases := []struct{ model, effort, want string }{
		{"claude-sonnet", "none", "Claude_Sonnet"},
		{"claude-sonnet", "high", "Claude_Sonnet_Reasoning"},
		{"gpt-5.5", "low", "Gpt_5_5_Chat"},
		{"gpt-5.5", "medium", "Gpt_5_5_Reasoning"},
		{"gpt-5.6-reasoning", "none", "Gpt_5_6_Reasoning"},
	}
	for _, tc := range cases {
		got, err := reasoningTone(tc.model, tc.effort)
		if err != nil || got != tc.want {
			t.Fatalf("%s/%s got=%q err=%v", tc.model, tc.effort, got, err)
		}
	}
	if _, err := reasoningTone("gpt-5.6-reasoning", "extreme"); err == nil {
		t.Fatal("invalid effort accepted")
	}
}

func TestIsReasoningTone(t *testing.T) {
	reasoning := []string{"Gpt_5_2_Reasoning", "Gpt_5_3_Reasoning", "Gpt_5_4_Reasoning", "Gpt_5_5_Reasoning", "Gpt_5_6_Reasoning", "Claude_Sonnet_Reasoning"}
	for _, tone := range reasoning {
		if !isReasoningTone(tone) {
			t.Fatalf("isReasoningTone(%q) = false, want true", tone)
		}
	}
	plain := []string{"Gpt_5_2_Chat", "Gpt_5_3_Chat", "Gpt_5_4_Chat", "Gpt_5_5_Chat", "Claude_Sonnet", "magic", ""}
	for _, tone := range plain {
		if isReasoningTone(tone) {
			t.Fatalf("isReasoningTone(%q) = true, want false", tone)
		}
	}
}

func TestResolveReasoningEffort(t *testing.T) {
	mappings := []modelMapping{{PublicModel: "gpt-5.6-sol", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "xhigh"}}
	// The route's configured level overrides any requested level.
	if got := resolveReasoningEffort("high", "gpt-5.6-sol", mappings); got != "xhigh" {
		t.Fatalf("explicit effort got=%q want route default xhigh", got)
	}
	// "auto" resolves to the model's configured default.
	if got := resolveReasoningEffort("auto", "gpt-5.6-sol", mappings); got != "xhigh" {
		t.Fatalf("auto got=%q want xhigh", got)
	}
	// Case-insensitive.
	if got := resolveReasoningEffort("AUTO", "gpt-5.6-sol", mappings); got != "xhigh" {
		t.Fatalf("AUTO got=%q want xhigh", got)
	}
	// An omitted effort also takes the default.
	if got := resolveReasoningEffort("", "gpt-5.6-sol", mappings); got != "xhigh" {
		t.Fatalf("empty got=%q want xhigh", got)
	}
	// Unknown models keep the request's own value (defaults only come from
	// mappings that actually declare one).
	if got := resolveReasoningEffort("auto", "no-such-model", mappings); got != "" {
		t.Fatalf("unknown model auto got=%q want empty", got)
	}
	if got := resolveReasoningEffort("medium", "no-such-model", mappings); got != "medium" {
		t.Fatalf("unknown model explicit got=%q", got)
	}
}

func TestChatRejectsInvalidReasoningBeforeUpstream(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-reasoning","reasoning_effort":"extreme","messages":[{"role":"user","content":"hello"}]}`))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "unsupported reasoning effort") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDisabledMappingRemovesModelFromCatalog(t *testing.T) {
	off := false
	mappings := []modelMapping{{PublicModel: "gpt-5.4", UpstreamMapping: "Gpt_5_4_Chat", DisplayName: "GPT-5.4", DefaultReasoningLevel: "low", Enabled: &off}}
	models := configuredModelSpecs(mappings)
	for _, m := range models {
		if strings.EqualFold(m.ID, "gpt-5.4") {
			t.Fatalf("disabled gpt-5.4 still advertised: %#v", m)
		}
	}
	// 与路由表一致：只剩无映射但内置可服务的 gpt-5.2 / gpt-5.5。
	if len(models) != 2 || models[0].ID != "gpt-5.2" || models[1].ID != "gpt-5.5" {
		t.Fatalf("expected [gpt-5.2 gpt-5.5], got %#v", models)
	}
}

func TestModelAvailabilityAndDefaults(t *testing.T) {
	off := false
	mappings := []modelMapping{
		{PublicModel: "gpt-5.2", UpstreamMapping: "Gpt_5_2_Chat", DisplayName: "GPT-5.2", DefaultReasoningLevel: "high", Enabled: &off},
		{PublicModel: "gpt-5.3", UpstreamMapping: "Gpt_5_3_Chat", DisplayName: "GPT-5.3", DefaultReasoningLevel: "low"},
	}
	if err := checkModelAvailable("gpt-5.2", mappings); err == nil {
		t.Fatal("disabled model must be rejected")
	}
	if err := checkModelAvailable("gpt-5.3", mappings); err != nil {
		t.Fatalf("enabled model rejected: %v", err)
	}
	if err := checkModelAvailable("auto", mappings); err != nil {
		t.Fatalf("unknown permissive model rejected: %v", err)
	}
	if lvl, ok := defaultReasoningLevel("gpt-5.3", mappings); !ok || lvl != "low" {
		t.Fatalf("default=%q ok=%t", lvl, ok)
	}
	if _, ok := defaultReasoningLevel("does-not-exist", mappings); ok {
		t.Fatal("unknown model must not claim a default level")
	}
}

func TestChatRejectsDisabledModel(t *testing.T) {
	off := false
	prior := openSettingsStore().v.ModelMappings
	openSettingsStore().mu.Lock()
	openSettingsStore().v.ModelMappings = []modelMapping{{PublicModel: "gpt-5.2", UpstreamMapping: "Gpt_5_2_Chat", DisplayName: "GPT-5.2", Enabled: &off}}
	openSettingsStore().mu.Unlock()
	defer func() {
		openSettingsStore().mu.Lock()
		openSettingsStore().v.ModelMappings = prior
		openSettingsStore().mu.Unlock()
	}()

	s := &Server{}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.2","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	s.openaiChat(w, r)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDefaultModelMappingStillAdvertised(t *testing.T) {
	// The stock gpt-5.6-* mappings keep their defaults and stay enabled.
	mappings := defaultModelMappings
	for _, m := range mappings {
		if !m.enabled() {
			t.Fatalf("default mapping %q unexpectedly disabled", m.PublicModel)
		}
	}
	models := configuredModelSpecs(mappings)
	// 默认安装：内置可服务的 gpt-5.2/5.4/5.5 + 三个默认 gpt-5.6-* 映射。
	if len(models) != 6 {
		t.Fatalf("expected 6 advertised models, got %d: %#v", len(models), models)
	}
	for _, id := range []string{"gpt-5.2", "gpt-5.4", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		found := false
		for _, m := range models {
			if m.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("default model %q not advertised: %#v", id, models)
		}
	}
}

// 下游模型列表必须与模型路由设置严格一致：只含已启用路由的模型。
func TestCatalogMatchesEnabledRoutesOnly(t *testing.T) {
	off, on := false, true
	mappings := []modelMapping{
		{PublicModel: "gpt-5.4", UpstreamMapping: "Gpt_5_4_Chat", DisplayName: "GPT-5.4", DefaultReasoningLevel: "medium", Enabled: &on},
		{PublicModel: "gpt-5.5", UpstreamMapping: "Gpt_5_5_Chat", DisplayName: "GPT-5.5", DefaultReasoningLevel: "medium", Enabled: &off},
		{PublicModel: "claude-sonnet", UpstreamMapping: "Claude_Sonnet", DisplayName: "Claude Sonnet", DefaultReasoningLevel: "medium", Enabled: &on},
	}
	ids := map[string]bool{}
	for _, m := range configuredModelSpecs(mappings) {
		ids[strings.ToLower(m.ID)] = true
	}
	want := map[string]bool{
		"gpt-5.4": true, "gpt-5.2": true, "claude-sonnet": true,
		"gpt-5.5": false, "gpt-5.4-mini": false, "gpt-5.6-sol": false, "codex-auto-review": false,
		"gpt-5.2-reasoning": false, "gpt-5.3": false, "gpt-5.4-reasoning": false,
		"gpt-5.5-reasoning": false, "gpt-5.6-reasoning": false, "claude-sonnet-reasoning": false,
	}
	for id, expect := range want {
		if ids[id] != expect {
			t.Fatalf("model %q advertised=%t want=%t (catalog=%v)", id, ids[id], expect, ids)
		}
	}
}

func TestCatalogMarksBackendAndCompatibilityAlias(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	s.openaiModels(w, r)
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, m := range body.Data {
		byID[strings.ToLower(fmt.Sprint(m["id"]))] = m
	}
	for _, id := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		m, ok := byID[id]
		if !ok {
			continue // model not enabled in this environment
		}
		if m["backend"] != "microsoft-365" {
			t.Fatalf("%s backend=%v want microsoft-365", id, m["backend"])
		}
		if m["compatibility_alias"] != true {
			t.Fatalf("%s must be flagged compatibility_alias=true: %#v", id, m)
		}
		if m["context_window"].(float64) > float64(m365EffectiveContextWindow()) {
			t.Fatalf("%s advertises context_window=%v above the effective budget %d", id, m["context_window"], m365EffectiveContextWindow())
		}
		if m["max_context_window"] != m["context_window"] {
			t.Fatalf("%s max_context_window != context_window: %#v", id, m)
		}
	}
	// A genuine built-in model must NOT be flagged as a compatibility alias.
	if m, ok := byID["gpt-5.2"]; ok && m["compatibility_alias"] == true {
		t.Fatalf("gpt-5.2 is not a compatibility alias: %#v", m)
	}
}

func TestResponsesReasoningConvertsToOpenAI(t *testing.T) {
	r := responsesRequest{Model: "gpt-5.6-reasoning", Input: "hello", Reasoning: &reasoningConfig{Effort: "high"}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if o.ReasoningEffort != "high" {
		t.Fatalf("effort=%q", o.ReasoningEffort)
	}
}
