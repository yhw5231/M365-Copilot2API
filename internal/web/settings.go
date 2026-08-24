package web

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"m365-copilot2api/internal/outbound"
)

// upstreamMapping is a named upstream route target (previously called "上游音色 /
// upstream tone"). A public model in the routing table maps to one of these by
// name; the gateway sends the mapping's tone string to ChatHub. Mappings can be
// added, deleted or disabled from the "Model routing" console.
type upstreamMapping struct {
	Name    string `json:"name"`
	Tone    string `json:"tone"`
	Enabled *bool  `json:"enabled,omitempty"`
}

func (m upstreamMapping) enabled() bool { return m.Enabled == nil || *m.Enabled }

// proxyMode returns the normalized three-state proxy policy (direct / loose /
// strict). Unknown or empty values default to strict for backward
// compatibility with settings files written before the mode feature.
func (s runtimeSettings) proxyMode() string {
	switch strings.ToLower(strings.TrimSpace(s.ProxyMode)) {
	case outbound.ProxyModeDirect, outbound.ProxyModeLoose, outbound.ProxyModeStrict:
		return strings.ToLower(strings.TrimSpace(s.ProxyMode))
	}
	return outbound.ProxyModeStrict
}

// defaultUpstreamMappings keep the historic tone strings as the default mapping
// set, so settings files saved by earlier versions (which stored the raw tone
// in modelMapping.upstreamTone) keep resolving to the same routing targets.
var defaultUpstreamMappings = []upstreamMapping{
	{Name: "Gpt_5_2_Chat", Tone: "Gpt_5_2_Chat"},
	{Name: "Gpt_5_2_Reasoning", Tone: "Gpt_5_2_Reasoning"},
	{Name: "Gpt_5_3_Chat", Tone: "Gpt_5_3_Chat"},
	{Name: "Gpt_5_3_Reasoning", Tone: "Gpt_5_3_Reasoning"},
	{Name: "Gpt_5_4_Chat", Tone: "Gpt_5_4_Chat"},
	{Name: "Gpt_5_4_Reasoning", Tone: "Gpt_5_4_Reasoning"},
	{Name: "Gpt_5_5_Chat", Tone: "Gpt_5_5_Chat"},
	{Name: "Gpt_5_5_Reasoning", Tone: "Gpt_5_5_Reasoning"},
	{Name: "Gpt_5_6_Reasoning", Tone: "Gpt_5_6_Reasoning"},
	{Name: "Claude_Sonnet", Tone: "Claude_Sonnet"},
	{Name: "Claude_Sonnet_Reasoning", Tone: "Claude_Sonnet_Reasoning"},
}

type modelMapping struct {
	PublicModel string `json:"publicModel"`
	// UpstreamMapping is the name of the upstreamMapping target this public
	// model routes to. Persisted as "upstreamMapping"; the legacy
	// "upstreamTone" value is accepted on read for settings files written by
	// earlier versions (the raw tone string equals the default mapping name).
	UpstreamMapping       string `json:"upstreamMapping"`
	DisplayName           string `json:"displayName"`
	DefaultReasoningLevel string `json:"defaultReasoningLevel"`
	// MaxInputTokens is the optional per-route input-token budget. Nil preserves
	// compatibility with older settings and resolves to the unified 256K default.
	MaxInputTokens *int `json:"maxInputTokens,omitempty"`
	// Enabled lets the model-routing UI switch a route off without deleting the
	// mapping. A nil value means enabled (default) so settings files saved by
	// earlier versions never silently hide every configured model.
	Enabled *bool `json:"enabled,omitempty"`
}

func (m modelMapping) enabled() bool { return m.Enabled == nil || *m.Enabled }

// UnmarshalJSON keeps old settings files readable: files written before the
// "upstream mapping" rename stored the raw tone under "upstreamTone". Because
// default mapping names equal the raw tone strings, the legacy value maps
// directly onto the new field.
func (m *modelMapping) UnmarshalJSON(b []byte) error {
	type alias modelMapping
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = modelMapping(a)
	if m.UpstreamMapping == "" {
		var legacy struct {
			UpstreamTone string `json:"upstreamTone"`
		}
		if json.Unmarshal(b, &legacy) == nil && strings.TrimSpace(legacy.UpstreamTone) != "" {
			m.UpstreamMapping = strings.TrimSpace(legacy.UpstreamTone)
		}
	}
	return nil
}

// UnmarshalJSON keeps settings files written by versions that only had the
// boolean "proxyEnabled" switch readable: proxyEnabled=false maps to the loose
// mode, true (or absent) maps to strict.
func (s *runtimeSettings) UnmarshalJSON(b []byte) error {
	type alias runtimeSettings
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*s = runtimeSettings(a)
	if strings.TrimSpace(s.ProxyMode) == "" {
		var legacy struct {
			ProxyEnabled *bool `json:"proxyEnabled"`
		}
		if json.Unmarshal(b, &legacy) == nil && legacy.ProxyEnabled != nil && !*legacy.ProxyEnabled {
			s.ProxyMode = outbound.ProxyModeLoose
		}
	}
	return nil
}

var defaultModelMappings = []modelMapping{
	{PublicModel: "gpt-5.6-sol", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "low"},
	{PublicModel: "gpt-5.6-terra", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Terra", DefaultReasoningLevel: "medium"},
	{PublicModel: "gpt-5.6-luna", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Luna", DefaultReasoningLevel: "medium"},
}

var publicModelID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// routeTargetID constrains upstream mapping names and tones: letters, digits,
// dots, underscores, spaces and hyphens (historic tone strings like
// "Gpt_5_4_Chat" fit, as do human-friendly names).
var routeTargetID = regexp.MustCompile(`^[A-Za-z0-9._ -]{1,128}$`)

var configurableCodexModels = []string{
	"gpt-5.2",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.5",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"codex-auto-review",
}

// modelRouteConfig is the row shape the "Model routing" console section edits:
// one entry per configurable model with the enabled switch, the upstream
// mapping target and the default reasoning level the gateway applies when a
// request omits reasoning_effort.
type modelRouteConfig struct {
	Model                 string `json:"model"`
	DisplayName           string `json:"displayName"`
	UpstreamMapping       string `json:"upstreamMapping"`
	DefaultReasoningLevel string `json:"defaultReasoningLevel"`
	MaxInputTokens        *int   `json:"maxInputTokens,omitempty"`
	Enabled               bool   `json:"enabled"`
}

const defaultRouteTone = "Gpt_5_4_Chat"

// effectiveUpstreamMappings returns the configured mapping set, falling back to
// the defaults when the persisted settings carry none (legacy files or a fresh
// install).
func effectiveUpstreamMappings(configured []upstreamMapping) []upstreamMapping {
	if len(configured) == 0 {
		return append([]upstreamMapping(nil), defaultUpstreamMappings...)
	}
	return configured
}

func modelRouteTable(mappings []modelMapping, upstream []upstreamMapping, hidden []string) []modelRouteConfig {
	upstream = effectiveUpstreamMappings(upstream)
	hiddenSet := make(map[string]bool, len(hidden))
	for _, id := range hidden {
		hiddenSet[strings.ToLower(strings.TrimSpace(id))] = true
	}
	seen := make(map[string]bool, len(configurableCodexModels)+len(mappings))
	rows := make([]modelRouteConfig, 0, len(configurableCodexModels)+len(mappings))
	servable := func(id string) bool {
		for _, spec := range gatewayModels {
			if strings.EqualFold(spec.ID, id) {
				return true
			}
		}
		return false
	}
	add := func(id string) {
		id = strings.TrimSpace(id)
		key := strings.ToLower(id)
		if id == "" || seen[key] || hiddenSet[key] {
			return
		}
		seen[key] = true
		var mappingName, display, level string
		var maxInputTokens *int
		enabled := servable(id) // routes not served today stay off until enabled
		if m, ok := configuredModelMapping(id, mappings); ok {
			mappingName = strings.TrimSpace(m.UpstreamMapping)
			display = strings.TrimSpace(m.DisplayName)
			level = strings.TrimSpace(m.DefaultReasoningLevel)
			maxInputTokens = m.MaxInputTokens
			enabled = m.enabled()
		}
		if mappingName == "" {
			mappingName = modelTone(id)
		}
		if mappingName == "" || mappingName == "magic" {
			mappingName = defaultRouteTone
		}
		// A model routes only while its upstream mapping target exists and is
		// enabled too.
		if up, ok := resolveUpstreamMapping(mappingName, upstream); ok {
			enabled = enabled && up.enabled()
		}
		if display == "" {
			display = id
		}
		if level == "" {
			level = "medium"
		}
		rows = append(rows, modelRouteConfig{Model: id, DisplayName: display, UpstreamMapping: mappingName, DefaultReasoningLevel: level, MaxInputTokens: maxInputTokens, Enabled: enabled})
	}
	for _, id := range configurableCodexModels {
		add(id)
	}
	for _, m := range mappings {
		add(m.PublicModel)
	}
	return rows
}

type runtimeSettings struct {
	MaxToolCallsPerTurn int      `json:"maxToolCallsPerTurn"`
	MaxToolRounds       int      `json:"maxToolRounds"`
	ContextWindow       int      `json:"contextWindow"`
	MaxOutputTokens     int      `json:"maxOutputTokens"`
	ChatTimeoutSeconds  int      `json:"chatTimeoutSeconds"`
	ImageTimeoutSeconds int      `json:"imageTimeoutSeconds"`
	LogLevel            string   `json:"logLevel"`
	DebugLogPath        string   `json:"debugLogPath"`
	ListenAddress       string   `json:"listenAddress"`
	ConfigPath          string   `json:"configPath"`
	TokenCachePath      string   `json:"tokenCachePath"`
	SessionCachePath    string   `json:"sessionCachePath"`
	OutboundProxy       string   `json:"outboundProxy"`
	ProxyPool           []string `json:"proxyPool,omitempty"`
	// ProxyMode is the three-state proxy policy: "direct" (never use the
	// pool), "loose" (prefer the pool, fall back to direct), "strict"
	// (mandatory once a pool is configured; the default when absent).
	ProxyMode     string         `json:"proxyMode,omitempty"`
	ClientID      string         `json:"clientId"`
	Authority     string         `json:"authority"`
	RedirectURI   string         `json:"redirectUri"`
	Scope         string         `json:"scope"`
	ModelMappings []modelMapping `json:"modelMappings"`
	// UpstreamMappings is the set of named upstream route targets ("上游映射")
	// the routing table can point public models at. Missing on legacy files; the
	// defaults are applied on load.
	UpstreamMappings []upstreamMapping `json:"upstreamMappings,omitempty"`
	// HiddenModels lists public model ids removed from the routing console.
	// Built-in models reappear when a matching mapping is re-added.
	HiddenModels     []string `json:"hiddenModels,omitempty"`
	ToolPlanningMode string   `json:"toolPlanningMode"`
	// TraceEnabled turns on the request-tracing debug mode that keeps the most
	// recent TraceMaxRecords full request/response captures (default 50).
	TraceEnabled    bool `json:"traceEnabled,omitempty"`
	TraceMaxRecords int  `json:"traceMaxRecords,omitempty"`
	// AccountConcurrency is the maximum number of simultaneous requests allowed
	// for each account. It defaults to 1 and can be updated at runtime.
	AccountConcurrency int `json:"accountConcurrency"`
	// AccountRoutingRule controls selection for new sessions. Supported values
	// are "available-first" and "round-robin". Existing sessions remain sticky
	// to their assigned account while that account is available.
	AccountRoutingRule string `json:"accountRoutingRule"`
	// TimeZone controls server-side calendar boundaries and frontend time display.
	// It defaults to Asia/Shanghai and must be a valid IANA time zone name.
	TimeZone string `json:"timeZone"`
}

type settingsStore struct {
	mu   sync.RWMutex
	path string
	v    runtimeSettings
}

func envInt(name string, fallback int) int {
	n, e := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if e == nil && n > 0 {
		return n
	}
	return fallback
}
func defaultRuntimeSettings() runtimeSettings {
	return runtimeSettings{
		MaxToolCallsPerTurn: envInt("M365_MAX_TOOL_CALLS_PER_TURN", 32), MaxToolRounds: envInt("M365_MAX_TOOL_ROUNDS", 512),
		ContextWindow: envInt("M365_CONTEXT_WINDOW", 128000), MaxOutputTokens: envInt("M365_MAX_OUTPUT_TOKENS", 16384),
		ChatTimeoutSeconds: envInt("M365_CHAT_TIMEOUT_SECONDS", 600), ImageTimeoutSeconds: envInt("M365_IMAGE_TIMEOUT_SECONDS", 150), LogLevel: firstNonEmptySetting(os.Getenv("M365_LOG_LEVEL"), "warn"),
		DebugLogPath: envPath("M365_DEBUG_LOG"), ListenAddress: os.Getenv("M365_LISTEN"), ConfigPath: envPath("M365_CONFIG"),
		TokenCachePath: envPath("M365_TOKEN_CACHE"), SessionCachePath: envPath("M365_SESSION_CACHE"), OutboundProxy: os.Getenv(outbound.EnvProxy), ClientID: os.Getenv("M365_CLIENT_ID"),
		Authority: os.Getenv("M365_AUTHORITY"), RedirectURI: os.Getenv("M365_REDIRECT_URI"), Scope: os.Getenv("M365_SCOPE"),
		ModelMappings:      append([]modelMapping(nil), defaultModelMappings...),
		UpstreamMappings:   append([]upstreamMapping(nil), defaultUpstreamMappings...),
		ToolPlanningMode:   toolPlanningMode(os.Getenv("M365_TOOL_PLANNING_MODE")),
		TraceMaxRecords:    defaultTraceMaxRecords,
		AccountConcurrency: envInt("M365_ACCOUNT_DEFAULT_CONCURRENCY", defaultAccountConcurrency),
		AccountRoutingRule: "available-first",
		TimeZone:           firstNonEmptySetting(os.Getenv("M365_TIME_ZONE"), "Asia/Shanghai"),
	}
}
func settingsPath() string {
	if p := envPath("M365_SETTINGS_FILE"); p != "" {
		return p
	}
	return defaultDataPath("settings.json")
}

var openSettingsStore = sync.OnceValue(func() *settingsStore {
	s := &settingsStore{path: settingsPath(), v: defaultRuntimeSettings()}
	if b, e := os.ReadFile(s.path); e == nil {
		_ = json.Unmarshal(b, &s.v)
	}
	// Legacy files predating the upstream-mapping feature carry no
	// upstreamMappings key; fall back to the defaults so every model keeps a
	// working route target.
	if len(s.v.UpstreamMappings) == 0 {
		s.v.UpstreamMappings = append([]upstreamMapping(nil), defaultUpstreamMappings...)
	}
	// Migrate settings files created before account scheduling was configurable.
	// Missing JSON fields decode to zero values, so restore the documented defaults.
	if s.v.AccountConcurrency < 1 {
		s.v.AccountConcurrency = defaultAccountConcurrency
	}
	if s.v.AccountRoutingRule == "" {
		s.v.AccountRoutingRule = "available-first"
	}
	if strings.TrimSpace(s.v.TimeZone) == "" {
		s.v.TimeZone = "Asia/Shanghai"
	}
	if e := validateSettings(s.v); e != nil {
		log.Printf("[settings] invalid persisted settings: %v", e)
	}
	return s
})

func firstNonEmptySetting(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// traceConfig returns the effective request-tracing configuration from the
// runtime settings store.
func traceConfig() (enabled bool, max int) {
	s := currentSettings()
	enabled = s.TraceEnabled
	max = traceMaxNorm(s.TraceMaxRecords)
	return enabled, max
}

func validateSettings(v runtimeSettings) error {
	if v.MaxToolCallsPerTurn < 1 || v.MaxToolCallsPerTurn > 64 {
		return fmt.Errorf("每轮工具调用数必须为 1-64")
	}
	if v.MaxToolRounds < 1 || v.MaxToolRounds > 512 {
		return fmt.Errorf("最大工具轮次必须为 1-512")
	}
	if v.ContextWindow < 1024 {
		return fmt.Errorf("上下文窗口不能小于 1024")
	}
	if v.MaxOutputTokens < 1 || v.MaxOutputTokens >= v.ContextWindow {
		return fmt.Errorf("最大输出必须大于 0 且小于上下文窗口")
	}
	if v.ChatTimeoutSeconds < 5 || v.ChatTimeoutSeconds > 3600 {
		return fmt.Errorf("聊天超时必须为 5-3600 秒")
	}
	if v.ImageTimeoutSeconds < 5 || v.ImageTimeoutSeconds > 3600 {
		return fmt.Errorf("图片超时必须为 5-3600 秒")
	}
	if v.LogLevel != "silent" && v.LogLevel != "error" && v.LogLevel != "warn" && v.LogLevel != "info" && v.LogLevel != "debug" {
		return fmt.Errorf("日志等级必须为 silent、error、warn、info 或 debug")
	}
	if v.TraceMaxRecords < 0 || v.TraceMaxRecords > 2000 {
		return fmt.Errorf("调试记录条数必须为 0-2000")
	}
	if v.AccountConcurrency < 1 || v.AccountConcurrency > 256 {
		return fmt.Errorf("账号并发必须为 1-256")
	}
	if v.AccountRoutingRule != "available-first" && v.AccountRoutingRule != "round-robin" {
		return fmt.Errorf("账号轮询规则必须为 available-first 或 round-robin")
	}
	if strings.TrimSpace(v.TimeZone) == "" {
		return fmt.Errorf("时区不能为空")
	}
	if _, err := time.LoadLocation(strings.TrimSpace(v.TimeZone)); err != nil {
		return fmt.Errorf("时区必须是有效的 IANA 时区名称: %w", err)
	}
	if raw := strings.ToLower(strings.TrimSpace(v.ProxyMode)); raw != "" && raw != outbound.ProxyModeDirect && raw != outbound.ProxyModeLoose && raw != outbound.ProxyModeStrict {
		return fmt.Errorf("代理模式必须为 direct、loose 或 strict")
	}
	if err := outbound.ValidateProxyURL(v.OutboundProxy); err != nil {
		return err
	}
	for _, proxyURL := range v.ProxyPool {
		if err := outbound.ValidateProxyURL(strings.TrimSpace(proxyURL)); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(v.ModelMappings))
	upstream := effectiveUpstreamMappings(v.UpstreamMappings)
	for _, mapping := range upstream {
		name := strings.TrimSpace(mapping.Name)
		if !routeTargetID.MatchString(name) {
			return fmt.Errorf("上游映射名称只能包含字母、数字、点、下划线、空格或连字符，且长度为 1-128")
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("上游映射名称 %q 重复", name)
		}
		seen[key] = struct{}{}
		if tone := strings.TrimSpace(mapping.Tone); !routeTargetID.MatchString(tone) {
			return fmt.Errorf("上游映射 %q 的 tone 只能包含字母、数字、点、下划线、空格或连字符，且长度为 1-128", name)
		}
	}
	mappingNames := make(map[string]struct{}, len(upstream))
	for _, mapping := range upstream {
		mappingNames[strings.ToLower(strings.TrimSpace(mapping.Name))] = struct{}{}
	}
	seen = make(map[string]struct{}, len(v.ModelMappings))
	for _, mapping := range v.ModelMappings {
		model := strings.TrimSpace(mapping.PublicModel)
		if !publicModelID.MatchString(model) {
			return fmt.Errorf("公开模型 ID 只能包含字母、数字、点、下划线或连字符，且长度为 1-128")
		}
		key := strings.ToLower(model)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("公开模型 ID %q 重复", model)
		}
		seen[key] = struct{}{}
		target := strings.TrimSpace(mapping.UpstreamMapping)
		if target == "" {
			return fmt.Errorf("公开模型 %q 未选择上游映射", model)
		}
		if _, exists := mappingNames[strings.ToLower(target)]; !exists {
			return fmt.Errorf("公开模型 %q 引用了不存在的上游映射 %q", model, target)
		}
		if strings.TrimSpace(mapping.DisplayName) == "" {
			return fmt.Errorf("公开模型 %q 缺少显示名称", model)
		}
		if _, err := normalizeReasoningEffort(mapping.DefaultReasoningLevel); err != nil || strings.TrimSpace(mapping.DefaultReasoningLevel) == "" {
			return fmt.Errorf("公开模型 %q 的默认推理级别无效", model)
		}
		if mapping.MaxInputTokens != nil && *mapping.MaxInputTokens < 1 {
			return fmt.Errorf("公开模型 %q 的最大输入 token 必须为空或为正整数", model)
		}
	}
	for _, id := range v.HiddenModels {
		if !publicModelID.MatchString(strings.TrimSpace(id)) {
			return fmt.Errorf("隐藏模型 ID 只能包含字母、数字、点、下划线或连字符，且长度为 1-128")
		}
	}
	return nil
}
func (s *settingsStore) get() runtimeSettings { s.mu.RLock(); defer s.mu.RUnlock(); return s.v }
func (s *settingsStore) save(v runtimeSettings) error {
	if e := validateSettings(v); e != nil {
		return e
	}
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e := writeFileAtomic(s.path, b, 0600); e != nil {
		return e
	}
	s.mu.Lock()
	s.v = v
	s.mu.Unlock()
	return nil
}
func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cur := s.settings.get()
		upstream := effectiveUpstreamMappings(cur.UpstreamMappings)
		names := make([]string, 0, len(upstream))
		for _, m := range upstream {
			names = append(names, m.Name)
		}
		jsonOut(w, map[string]any{"settings": cur, "codexModels": configurableCodexModels, "upstreamTones": names, "upstreamMappings": upstream, "catalogModels": modelRouteTable(cur.ModelMappings, cur.UpstreamMappings, cur.HiddenModels), "reasoningLevels": advertisedReasoningEfforts, "restartRequiredFields": []string{"listenAddress", "configPath", "tokenCachePath", "sessionCachePath", "outboundProxy", "proxyPool", "clientId", "authority", "redirectUri", "scope", "debugLogPath"}})
	case http.MethodPut:
		// 前端可能只修改一个字段（如监听地址），其余字段以零值提交。
		// 逐字段合并到当前设置再校验，避免"改一个字段弄丢其他配置"。
		cur := s.settings.get()
		base, _ := json.Marshal(cur)
		var merged map[string]any
		if json.Unmarshal(base, &merged) != nil {
			writeOpenAIError(w, 500, "internal_error", "marshal settings")
			return
		}
		var patch map[string]any
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&patch) != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
			return
		}
		for k, v := range patch {
			merged[k] = v
		}
		mergedJSON, _ := json.Marshal(merged)
		var v runtimeSettings
		if json.Unmarshal(mergedJSON, &v) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		if e := validateSettings(v); e != nil {
			writeOpenAIError(w, 400, "invalid_request_error", e.Error())
			return
		}
		if e := s.settings.save(v); e != nil {
			writeOpenAIError(w, 500, "storage_error", "settings could not be saved; check the persistent data directory permissions")
			return
		}
		if e := outbound.ConfigurePool(v.ProxyPool); e != nil {
			writeOpenAIError(w, 400, "invalid_request_error", e.Error())
			return
		}
		outbound.SetProxyMode(v.proxyMode())
		if s.accountConcurrency != nil {
			s.accountConcurrency.SetLimit(v.AccountConcurrency)
		}
		jsonOut(w, map[string]any{"ok": true, "settings": v})
	default:
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
	}
}
func configuredToolCallLimit(s *settingsStore) int {
	if raw, ok := os.LookupEnv("M365_MAX_TOOL_CALLS_PER_TURN"); ok {
		if n, e := strconv.Atoi(strings.TrimSpace(raw)); e == nil && n >= 1 && n <= 64 {
			return n
		}
		return 1
	}
	return s.get().MaxToolCallsPerTurn
}

// adaptiveToolCallLimit permits parallel calls only when every call is a
// read-only, independently addressable operation. Any write, execution,
// mutation, or ambiguous tool is serialized conservatively.
func adaptiveToolCallLimit(c []detectedToolCall, configured int) int {
	if len(c) < 2 || configured < 2 {
		return 1
	}
	for _, call := range c {
		name := strings.ToLower(strings.TrimSpace(call.Name))
		if name == "" || toolLooksMutating(name) || !toolLooksReadOnly(name) {
			return 1
		}
	}
	return configured
}

func toolLooksMutating(name string) bool {
	for _, word := range []string{"exec", "shell", "command", "write", "edit", "update", "delete", "remove", "move", "rename", "create", "patch", "apply", "install", "run"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func toolLooksReadOnly(name string) bool {
	for _, word := range []string{"read", "list", "search", "find", "get", "fetch", "browser", "lookup", "inspect", "stat", "status", "describe", "info"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func limitToolCalls(c []detectedToolCall, n int) []detectedToolCall {
	if n < 1 {
		n = 1
	}
	if len(c) > n {
		return c[:n]
	}
	return c
}

func currentSettings() runtimeSettings { return openSettingsStore().get() }

// ApplyStartupSettingsEnv loads persisted restart-required fields before the
// rest of the application initializes. Explicit process environment variables
// always win over values saved from the web console.
func ApplyStartupSettingsEnv() {
	s := openSettingsStore().get()
	values := map[string]string{"M365_LISTEN": s.ListenAddress, "M365_CONFIG": s.ConfigPath, "M365_TOKEN_CACHE": s.TokenCachePath, "M365_SESSION_CACHE": s.SessionCachePath, outbound.EnvProxy: s.OutboundProxy, "M365_PROXY_POOL": strings.Join(s.ProxyPool, "\n"), "M365_CLIENT_ID": s.ClientID, "M365_AUTHORITY": s.Authority, "M365_REDIRECT_URI": s.RedirectURI, "M365_SCOPE": s.Scope, "M365_DEBUG_LOG": s.DebugLogPath, outbound.EnvProxyMode: s.proxyMode()}
	for k, v := range values {
		if _, exists := os.LookupEnv(k); !exists && strings.TrimSpace(v) != "" {
			_ = os.Setenv(k, v)
		}
	}
}
