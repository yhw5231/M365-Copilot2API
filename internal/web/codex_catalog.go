// Codex model catalog compatibility lives here. It is intentionally kept in
// package web because route handlers share unexported request and settings types.
package web

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type modelLimits struct{ ContextWindow, MaxInputTokens, MaxOutputTokens int }
type reasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type modelSpec struct {
	ID, Owner, DisplayName, DefaultReasoningLevel string
	Tools                                         bool
}

type reasoningEffortPreset struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

var advertisedReasoningEfforts = []reasoningEffortPreset{
	{Effort: "none", Description: "Disable additional reasoning."},
	{Effort: "minimal", Description: "Fast responses with minimal reasoning."},
	{Effort: "low", Description: "Fast responses with lighter reasoning."},
	{Effort: "medium", Description: "Balances speed and reasoning depth for everyday tasks."},
	{Effort: "high", Description: "Greater reasoning depth for complex problems."},
	{Effort: "xhigh", Description: "Extra high reasoning depth for complex problems."},
}

// gatewayCodexBaseInstructions is returned only in the Codex model catalog.
// Codex uses it to build its own request instructions; it is not interpreted
// or forwarded directly by the gateway's ChatHub adapter.
const gatewayCodexBaseInstructions = `You are a helpful AI assistant. When asked to write code, always provide the complete implementation — never truncate, abbreviate, or return only a fragment. Write full, working code with all logic included.`

func codexModelMessages() map[string]any {
	return map[string]any{
		"instructions_template": gatewayCodexBaseInstructions,
		"instructions_variables": map[string]string{
			"personality_default":   "",
			"personality_friendly":  "",
			"personality_pragmatic": "",
		},
		"approvals":   nil,
		"auto_review": nil,
	}
}

var gatewayModels = []modelSpec{
	{ID: "gpt-5.2", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.2-reasoning", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.3", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.4", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.4-reasoning", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.5", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.5-reasoning", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-5.6-reasoning", Owner: "microsoft-365", Tools: true},
	{ID: "gpt-image-2", Owner: "microsoft-365", DisplayName: "GPT Image 2"},
	{ID: "claude-sonnet", Owner: "anthropic-via-microsoft-365", Tools: true},
	{ID: "claude-sonnet-reasoning", Owner: "anthropic-via-microsoft-365", Tools: true},
}

// resolveUpstreamMapping finds the named upstream mapping and returns it.
// Lookup is case-insensitive on the mapping name.
func resolveUpstreamMapping(name string, mappings []upstreamMapping) (upstreamMapping, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, m := range mappings {
		if strings.EqualFold(strings.TrimSpace(m.Name), name) {
			return m, true
		}
	}
	return upstreamMapping{}, false
}

// validUpstreamTone reports whether the tone string is known to the live
// upstream catalog (fetched from the CDN bundle, falling back to the known
// static tones).
func validUpstreamTone(tone string) bool {
	for _, known := range liveUpstreamTones() {
		if tone == known {
			return true
		}
	}
	return false
}

// upstreamMappingEnabled reports whether the named upstream mapping exists and
// is enabled. Missing mappings count as disabled so a deleted mapping cannot
// silently keep routing its models.
func upstreamMappingEnabled(name string, mappings []upstreamMapping) bool {
	m, ok := resolveUpstreamMapping(name, mappings)
	return ok && m.enabled()
}

var (
	dynamicTones []string
	dynamicMu    sync.RWMutex
	dynamicAt    time.Time
)

func knownUpstreamTones() []string {
	return []string{"Gpt_5_2_Chat", "Gpt_5_2_Reasoning", "Gpt_5_3_Chat", "Gpt_5_3_Reasoning", "Gpt_5_4_Chat", "Gpt_5_4_Reasoning", "Gpt_5_5_Chat", "Gpt_5_5_Reasoning", "Gpt_5_6_Reasoning", "Claude_Sonnet", "Claude_Sonnet_Reasoning"}
}

func liveUpstreamTones() []string {
	dynamicMu.RLock()
	if dynamicAt.IsZero() || time.Since(dynamicAt) > 24*time.Hour {
		dynamicMu.RUnlock()
		go syncUpstreamTones()
		dynamicMu.RLock()
	}
	t := dynamicTones
	dynamicMu.RUnlock()
	if len(t) > 0 {
		return t
	}
	return knownUpstreamTones()
}

func syncUpstreamTones() {
	tones := fetchUpstreamTones()
	if len(tones) == 0 {
		return
	}
	dynamicMu.Lock()
	dynamicTones = tones
	dynamicAt = time.Now()
	dynamicMu.Unlock()
	log.Printf("synced %d upstream tones from CDN bundle", len(tones))
}

func fetchUpstreamTones() []string {
	client := &http.Client{Timeout: 30 * time.Second}
	pageURL := "https://m365.cloud.microsoft/"
	resp, err := client.Get(pageURL)
	if err != nil {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	re := regexp.MustCompile(`main\.[a-f0-9]{8}\.js`)
	m := re.FindString(string(body))
	if m == "" {
		return nil
	}
	bundleURL := "https://res.public.onecdn.static.microsoft/midgard/versionless-v2/" + m
	resp2, err := client.Get(bundleURL)
	if err != nil {
		return nil
	}
	bundle, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	toneRe := regexp.MustCompile(`(?:Gpt_[0-9]_[0-9]_[A-Za-z_]+|Claude_[A-Za-z0-9_]+|Magic)`)
	matches := toneRe.FindAllString(string(bundle), -1)
	seen := map[string]bool{}
	for _, t := range matches {
		seen[t] = true
	}
	result := make([]string, 0, len(seen))
	for t := range seen {
		result = append(result, t)
	}
	sort.Strings(result)
	return result
}

func configuredModelMapping(model string, mappings []modelMapping) (modelMapping, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, mapping := range mappings {
		if strings.EqualFold(strings.TrimSpace(mapping.PublicModel), model) {
			return mapping, true
		}
	}
	return modelMapping{}, false
}

// configuredModelTone resolves a model's effective upstream tone: the model
// mapping names an upstream mapping and that mapping's tone string is returned.
func configuredModelTone(model string, mappings []modelMapping) (string, bool) {
	mapping, ok := configuredModelMapping(model, mappings)
	if !ok {
		return "", false
	}
	return configuredMappingTone(mapping.UpstreamMapping)
}

func configuredMappingTone(mappingName string) (string, bool) {
	up, ok := resolveUpstreamMapping(mappingName, currentSettings().UpstreamMappings)
	if !ok {
		return "", false
	}
	return up.Tone, true
}

// checkModelAvailable rejects requests for models explicitly switched off in
// model route settings (either the route itself or its upstream mapping target
// is disabled). Unknown model ids stay permissive on purpose so existing
// clients that pass model aliases ("auto", "m365-copilot", ...) keep working
// unchanged.
func checkModelAvailable(model string, mappings []modelMapping) error {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return nil
	}
	if mapping, ok := configuredModelMapping(m, mappings); ok {
		if !mapping.enabled() {
			return fmt.Errorf("model %q is disabled in model routing settings", strings.TrimSpace(mapping.PublicModel))
		}
		if !upstreamMappingEnabled(mapping.UpstreamMapping, currentSettings().UpstreamMappings) {
			return fmt.Errorf("model %q routes to a disabled upstream mapping", strings.TrimSpace(mapping.PublicModel))
		}
	}
	return nil
}

// defaultReasoningLevel returns the configured default level for a model, used
// when a request omits reasoning_effort. Only mappings that explicitly set a
// level influence routing; everything else falls back to the historic permissive
// tone behavior.
func defaultReasoningLevel(model string, mappings []modelMapping) (string, bool) {
	mapping, ok := configuredModelMapping(model, mappings)
	if !ok {
		return "", false
	}
	level := strings.TrimSpace(mapping.DefaultReasoningLevel)
	return level, level != ""
}

// resolveReasoningEffort computes the effective reasoning level for a request.
// "auto" asks the backend to pick the model's default — the same semantics as
// omitting reasoning_effort — so it resolves to the model route's configured
// default when one exists and stays empty otherwise.
func resolveReasoningEffort(requested, model string, mappings []modelMapping) string {
	effort := strings.TrimSpace(requested)
	if strings.EqualFold(effort, "auto") {
		effort = ""
	}
	if effort == "" {
		if level, ok := defaultReasoningLevel(model, mappings); ok {
			return level
		}
	}
	return effort
}

func configuredModelSpecs(mappings []modelMapping) []modelSpec {
	cur := currentSettings()
	upstream := effectiveUpstreamMappings(cur.UpstreamMappings)

	// 下游模型列表与模型路由控制台表格严格一致：只暴露路由表中已启用的
	// 模型，设置里未启用、隐藏或未出现在路由表中的内置模型一律不返回。
	rows := modelRouteTable(mappings, upstream, cur.HiddenModels)
	models := make([]modelSpec, 0, len(rows))
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		spec := modelSpec{
			ID: r.Model, Owner: "microsoft-365", Tools: true,
			DisplayName: r.DisplayName, DefaultReasoningLevel: r.DefaultReasoningLevel,
		}
		for _, g := range gatewayModels {
			if strings.EqualFold(g.ID, r.Model) {
				spec.Owner = g.Owner
				spec.Tools = g.Tools
				break
			}
		}
		models = append(models, spec)
	}
	return models
}

func positiveEnvInt(name string, fallback int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err == nil && v > 0 {
		return v
	}
	return fallback
}
func configuredModelLimits() modelLimits {
	cfg := currentSettings()
	contextWindow := cfg.ContextWindow
	maxOutput := cfg.MaxOutputTokens
	if maxOutput >= contextWindow {
		maxOutput = contextWindow / 8
		if maxOutput < 1 {
			maxOutput = 1
		}
	}
	return modelLimits{ContextWindow: contextWindow, MaxInputTokens: contextWindow - maxOutput, MaxOutputTokens: maxOutput}
}
func normalizeReasoningEffort(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if e == "" {
		return "", nil
	}
	switch e {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return e, nil
	}
	return "", fmt.Errorf("unsupported reasoning effort %q; use none, minimal, low, medium, high, or xhigh", e)
}
func reasoningTone(model, effort string) (string, error) {
	e, err := normalizeReasoningEffort(effort)
	if err != nil {
		return "", err
	}
	if tone, ok := configuredModelTone(model, currentSettings().ModelMappings); ok {
		return tone, nil
	}
	base := modelTone(model)
	// Explicit reasoning aliases are never silently downgraded by a generic client default.
	if strings.Contains(strings.ToLower(model), "reasoning") {
		return base, nil
	}
	if e == "" || e == "none" || e == "minimal" || e == "low" {
		return base, nil
	}
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "claude", "claude-sonnet":
		return "Claude_Sonnet_Reasoning", nil
	case "gpt-5.2":
		return "Gpt_5_2_Reasoning", nil
	case "gpt-5.3":
		return "Gpt_5_3_Reasoning", nil
	case "gpt-5.4":
		return "Gpt_5_4_Reasoning", nil
	case "gpt-5.5":
		return "Gpt_5_5_Reasoning", nil
	case "gpt-5.6":
		return "Gpt_5_6_Reasoning", nil
	default:
		return "Gpt_5_5_Reasoning", nil
	}
}

// compatibilityAliasModel reports whether a public model id is a compatibility
// alias that funnels into an existing Microsoft 365 ChatHub tone rather than a
// distinct upstream model with independent capabilities. gpt-5.6-sol (and the
// sibling gpt-5.6-terra / gpt-5.6-luna names) all route to the same
// Gpt_5_6_Reasoning tone; their advertised capabilities are exactly the M365
// backend's, not the capabilities of any hypothetical OpenAI model with the
// same name.
func compatibilityAliasModel(id string) bool {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return true
	}
	return false
}

// isReasoningTone reports whether a ChatHub tone drives the multi-step
// ChainOfThought transcript that ChatHub surfaces as reasoning_content. Every
// built-in reasoning tone ends with "_Reasoning" (Gpt_5_2_Reasoning ...,
// Claude_Sonnet_Reasoning); a custom reasoning tone must keep that suffix for
// streaming reasoning_content to be delivered.
func isReasoningTone(tone string) bool {
	return strings.HasSuffix(tone, "_Reasoning")
}

// reasoningGateWindow returns how long to hold the leading answer text on a
// reasoning stream so a late ChainOfThought frame can still be emitted before
// the answer. M365_REASONING_GATE_MS (integer milliseconds) overrides the
// built-in default of 1500ms; 0 disables the gate.
func reasoningGateWindow() time.Duration {
	if ms := envInt("M365_REASONING_GATE_MS", 0); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 1500 * time.Millisecond
}

// reasoningQuietWindow returns the trailing-quiet interval used to decide when
// a reasoning stream has finished. When reasoning arrives, the answer text is
// held until no new reasoning is seen within this window, so the client sees
// the complete "think" block before the answer. M365_REASONING_QUIET_MS
// (integer milliseconds) overrides the built-in default of 800ms; 0 falls back
// to releasing the answer as soon as the first reasoning event arrives.
func reasoningQuietWindow() time.Duration {
	if ms := envInt("M365_REASONING_QUIET_MS", 0); ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 800 * time.Millisecond
}

func modelCatalog() []map[string]any {
	l := configuredModelLimits()
	models := configuredModelSpecs(currentSettings().ModelMappings)
	// Honesty over capability inflation: the Microsoft 365 backend only
	// consumes the effective input budget this gateway actually manages, so the
	// catalog advertises that budget — never a larger number the gateway cannot
	// deliver (a claimed 1.05M window with ~96K of real handling is a lie).
	eff := m365EffectiveContextWindow()
	advertisedWindow := l.ContextWindow
	if advertisedWindow > eff {
		advertisedWindow = eff
	}
	advertisedInput := l.MaxInputTokens
	if advertisedInput > eff {
		advertisedInput = eff
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		// Keep capability fields both at the top level and under capabilities:
		// different OpenAI-compatible clients inspect different locations.
		alias := compatibilityAliasModel(m.ID)
		features := []string{"tools", "function_calling", "streaming", "reasoning", "vision"}
		if alias {
			features = []string{"tools", "function_calling", "streaming", "reasoning"}
		}
		modalities := []string{"text", "image"}
		if alias {
			modalities = []string{"text"}
		}
		caps := map[string]any{
			"chat_completions": true, "responses": true, "streaming": true,
			"tools": true, "reasoning": true,
			"reasoning_efforts": advertisedReasoningEfforts, "supported_reasoning_levels": advertisedReasoningEfforts,
			"reasoning_mode": "gateway_tone_routing", "supports_tools": true, "tool_calls": true,
			"function_calling": true, "supports_function_calling": true, "supports_vision": !alias,
			"vision": !alias, "modalities": modalities, "input_modalities": modalities,
			"output_modalities": []string{"text"}, "supported_features": features,
		}
		displayName := m.DisplayName
		if displayName == "" {
			displayName = m.ID
		}
		defaultReasoningLevel := m.DefaultReasoningLevel
		if defaultReasoningLevel == "" {
			defaultReasoningLevel = "medium"
		}
		out = append(out, map[string]any{
			"id": m.ID, "slug": m.ID, "display_name": displayName, "description": "Public model endpoint.",
			"base_instructions": gatewayCodexBaseInstructions, "model_messages": codexModelMessages(),
			"default_reasoning_level": defaultReasoningLevel, "object": "model", "owned_by": "gateway",
			"shell_type": "shell_command", "visibility": "list", "supported_in_api": true, "priority": 1,
			"additional_speed_tiers": []string{}, "service_tiers": []any{},
			"availability_nux": nil, "upgrade": nil, "include_skills_usage_instructions": false,
			"supports_reasoning_summaries": true, "default_reasoning_summary": "none",
			"support_verbosity": true, "default_verbosity": "low", "apply_patch_tool_type": "freeform",
			"web_search_tool_type": "text_and_image", "truncation_policy": map[string]any{"mode": "tokens", "limit": 10000},
			"supports_parallel_tool_calls": true, "supports_image_detail_original": !alias,
			// Explicit capability declaration: which backend actually serves this
			// model and whether the id is a compatibility alias of that backend.
			"backend": m.Owner, "compatibility_alias": alias,
			"max_context_window": advertisedWindow, "effective_context_window": advertisedWindow, "effective_context_window_percent": 95,
			"experimental_supported_tools": []any{}, "supports_search_tool": true, "use_responses_lite": false,
			"tool_mode": "code_mode_only", "multi_agent_version": "v2",
			"context_window": advertisedWindow, "max_input_tokens": advertisedInput, "max_output_tokens": l.MaxOutputTokens,
			"capabilities": caps, "supports_tools": true, "tool_calls": true,
			"supported_reasoning_levels": advertisedReasoningEfforts,
			"function_calling":           true, "supports_function_calling": true, "supports_vision": !alias,
			"vision": !alias, "modalities": modalities, "input_modalities": modalities,
			"output_modalities": []string{"text"}, "supported_features": features,
		})
	}
	return out
}
