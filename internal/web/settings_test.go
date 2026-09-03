package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLimitToolCalls(t *testing.T) {
	calls := []detectedToolCall{{Name: "a"}, {Name: "b"}, {Name: "c"}}
	got := limitToolCalls(calls, 1)
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("got %#v", got)
	}
	if len(limitToolCalls(calls, 2)) != 2 {
		t.Fatal("expected two calls")
	}
	if len(limitToolCalls(calls, 99)) != 3 {
		t.Fatal("must preserve calls below limit")
	}
}
func TestSettingsPersistAndValidate(t *testing.T) {
	s := &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), accountPath: filepath.Join(t.TempDir(), "account-settings.json"), v: defaultRuntimeSettings()}
	v := s.v
	v.MaxToolCallsPerTurn = 1
	v.MaxToolRounds = 32
	v.ChatTimeoutSeconds = 60
	v.ImageTimeoutSeconds = 90
	if err := s.save(v); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path); err != nil {
		t.Fatal(err)
	}
	v.MaxToolCallsPerTurn = 0
	if err := s.save(v); err == nil {
		t.Fatal("expected validation error for MaxToolCallsPerTurn=0")
	}
	v.MaxToolCallsPerTurn = 1
	v.MaxToolRounds = 0
	if err := s.save(v); err != nil {
		t.Fatalf("MaxToolRounds=0 should be accepted as unlimited: %v", err)
	}
}

func TestAccountSettingsPersistedSeparately(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "settings.json")
	accountPath := filepath.Join(dir, "account-settings.json")

	// Seed a legacy main settings file that still carries the account keys
	// (complete valid settings, as produced by older builds).
	base, err := json.Marshal(defaultRuntimeSettings())
	if err != nil {
		t.Fatal(err)
	}
	var legacyDoc map[string]any
	if err := json.Unmarshal(base, &legacyDoc); err != nil {
		t.Fatal(err)
	}
	legacyDoc["accountConcurrency"] = 3
	legacyDoc["gatewayConcurrency"] = 7
	legacyDoc["accountRoutingRule"] = "round-robin"
	legacyDoc["accountWarmSessionSeconds"] = 120
	legacyDoc["cacheOnAccountSwitch"] = true
	legacyDoc["accountQueueTimeoutSeconds"] = 20
	legacyDoc["accountRateLimitCooldownSeconds"] = 600
	legacyRaw, err := json.MarshalIndent(legacyDoc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mainPath, legacyRaw, 0600); err != nil {
		t.Fatal(err)
	}

	s := newSettingsStore(mainPath, accountPath)
	if s.v.AccountConcurrency != 3 || s.v.GatewayConcurrency != 7 || s.v.AccountRoutingRule != "round-robin" {
		t.Fatalf("legacy account settings not loaded: %+v", s.v)
	}

	// Save must split: account keys go to the dedicated file and are stripped
	// from the main file.
	v := s.v
	v.AccountConcurrency = 5
	if err := s.save(v); err != nil {
		t.Fatal(err)
	}
	mainRaw, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range accountSettingFileKeys {
		if strings.Contains(string(mainRaw), `"`+key+`"`) {
			t.Fatalf("main settings file still carries account key %q: %s", key, mainRaw)
		}
	}
	if !strings.Contains(string(mainRaw), `"timeZone"`) {
		t.Fatalf("main settings file lost unrelated keys: %s", mainRaw)
	}
	accountRaw, err := os.ReadFile(accountPath)
	if err != nil {
		t.Fatalf("account settings file missing: %v", err)
	}
	if !strings.Contains(string(accountRaw), `"accountConcurrency": 5`) {
		t.Fatalf("account settings file wrong: %s", accountRaw)
	}

	// Reload: the dedicated file wins.
	reloaded := newSettingsStore(mainPath, accountPath)
	if reloaded.v.AccountConcurrency != 5 {
		t.Fatalf("account settings not restored, got %d", reloaded.v.AccountConcurrency)
	}
	if reloaded.v.GatewayConcurrency != 7 {
		t.Fatalf("gateway concurrency not restored, got %d", reloaded.v.GatewayConcurrency)
	}
}

func TestDefaultRuntimeSettings(t *testing.T) {
	t.Setenv("M365_CHAT_TIMEOUT_SECONDS", "")
	v := defaultRuntimeSettings()
	if v.ChatTimeoutSeconds != 600 {
		t.Fatalf("default chat timeout = %d, want 600", v.ChatTimeoutSeconds)
	}
	if v.TimeZone != "Asia/Shanghai" {
		t.Fatalf("default time zone = %q, want Asia/Shanghai", v.TimeZone)
	}
	if err := validateSettings(v); err != nil {
		t.Fatalf("default settings failed validation: %v", err)
	}
}

func TestTimeZoneValidation(t *testing.T) {
	v := defaultRuntimeSettings()
	v.TimeZone = "Asia/Shanghai"
	if err := validateSettings(v); err != nil {
		t.Fatalf("valid IANA time zone rejected: %v", err)
	}
	v.TimeZone = "not/a-time-zone"
	if err := validateSettings(v); err == nil {
		t.Fatal("invalid IANA time zone accepted")
	}
}

func TestSettingsFileOverrideWinsOverDataDir(t *testing.T) {
	dataDir := t.TempDir()
	explicit := filepath.Join(t.TempDir(), "settings.json")
	t.Setenv("M365_DATA_DIR", dataDir)
	t.Setenv("M365_SETTINGS_FILE", explicit)
	if got := settingsPath(); got != explicit {
		t.Fatalf("settingsPath()=%q, want %q", got, explicit)
	}
}

func TestModelMappingsValidate(t *testing.T) {
	v := defaultRuntimeSettings()
	v.ModelMappings = []modelMapping{{PublicModel: "gpt-5.6-sol", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "low"}}
	if err := validateSettings(v); err != nil {
		t.Fatal(err)
	}
	v.ModelMappings[0].UpstreamMapping = "unknown"
	if err := validateSettings(v); err == nil {
		t.Fatal("accepted unknown upstream tone")
	}
	v.ModelMappings[0].UpstreamMapping = "Gpt_5_6_Reasoning"
	v.ModelMappings = append(v.ModelMappings, v.ModelMappings[0])
	if err := validateSettings(v); err == nil {
		t.Fatal("accepted duplicate public model")
	}
	v.ModelMappings = []modelMapping{{PublicModel: "custom-codex-route", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "Custom Codex Route", DefaultReasoningLevel: "medium"}}
	if err := validateSettings(v); err != nil {
		t.Fatalf("rejected custom public model: %v", err)
	}
}

func TestModelMappingsValidateWithEnabledFlag(t *testing.T) {
	off := false
	v := defaultRuntimeSettings()
	v.ModelMappings = []modelMapping{
		{PublicModel: "gpt-5.6-sol", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "low", Enabled: &off},
		{PublicModel: "gpt-5.4", UpstreamMapping: "Gpt_5_4_Chat", DisplayName: "GPT-5.4", DefaultReasoningLevel: "high"},
	}
	if err := validateSettings(v); err != nil {
		t.Fatalf("routing mappings rejected: %v", err)
	}
	if v.ModelMappings[0].enabled() || !v.ModelMappings[1].enabled() {
		t.Fatalf("enabled semantics wrong: explicit-off=%t nil-default=%t", v.ModelMappings[0].enabled(), v.ModelMappings[1].enabled())
	}
	v.ModelMappings[1].DefaultReasoningLevel = "ultra"
	if err := validateSettings(v); err == nil {
		t.Fatal("accepted invalid default reasoning level")
	}
}

func TestModelRouteTableCoversConfigurableModels(t *testing.T) {
	off := false
	mappings := []modelMapping{
		{PublicModel: "gpt-5.6-sol", UpstreamMapping: "Gpt_5_6_Reasoning", DisplayName: "GPT-5.6-Sol", DefaultReasoningLevel: "low", Enabled: &off},
		{PublicModel: "custom-route", UpstreamMapping: "Gpt_5_4_Chat", DisplayName: "Custom", DefaultReasoningLevel: "high"},
	}
	rows := modelRouteTable(mappings, defaultUpstreamMappings, nil)
	byID := make(map[string]modelRouteConfig, len(rows))
	for _, r := range rows {
		byID[strings.ToLower(r.Model)] = r
	}
	if row, ok := byID["gpt-5.6-sol"]; !ok || row.Enabled || row.DefaultReasoningLevel != "low" {
		t.Fatalf("sol row=%+v ok=%t", row, ok)
	}
	if row, ok := byID["custom-route"]; !ok || !row.Enabled || row.UpstreamMapping != "Gpt_5_4_Chat" || row.DefaultReasoningLevel != "high" {
		t.Fatalf("custom row=%+v ok=%t", row, ok)
	}
	if row, ok := byID["gpt-5.4"]; !ok || row.UpstreamMapping != "Gpt_5_4_Chat" || row.DefaultReasoningLevel != "medium" {
		t.Fatalf("built-in default row=%+v ok=%t", row, ok)
	}
	for _, id := range configurableCodexModels {
		if _, ok := byID[strings.ToLower(id)]; !ok {
			t.Fatalf("configurable model %q missing from route table", id)
		}
	}
}

func TestOutboundProxySettingValidation(t *testing.T) {
	v := defaultRuntimeSettings()
	v.OutboundProxy = "socks5://proxy.example:1080"
	if err := validateSettings(v); err != nil {
		t.Fatalf("rejected SOCKS5 proxy: %v", err)
	}
	v.OutboundProxy = "https://proxy.example:8443"
	if err := validateSettings(v); err != nil {
		t.Fatalf("rejected HTTPS proxy: %v", err)
	}
	v.OutboundProxy = "ftp://proxy.example:21"
	if err := validateSettings(v); err == nil {
		t.Fatal("accepted unsupported proxy scheme")
	}
}

func TestAdaptiveToolCallLimitSerializesDependentOrMutatingCalls(t *testing.T) {
	calls := []detectedToolCall{{Name: "read_file"}, {Name: "exec_command"}}
	if got := adaptiveToolCallLimit(calls, 4); got != 1 {
		t.Fatalf("got %d, want 1", got)
	}
}
func TestAdaptiveToolCallLimitAllowsIndependentReadOnlyCalls(t *testing.T) {
	calls := []detectedToolCall{{Name: "read_file"}, {Name: "search_code"}}
	if got := adaptiveToolCallLimit(calls, 4); got != 4 {
		t.Fatalf("got %d, want 4", got)
	}
}

func TestAccountWarmSessionSecondsDefaultResolution(t *testing.T) {
	t.Setenv("M365_ACCOUNT_WARM_SESSION_SECONDS", "")
	t.Setenv("M365_ACCOUNT_WARM_SESSION_WINDOW", "")
	if got := accountWarmSessionSecondsDefault(); got != defaultAccountWarmSessionSeconds {
		t.Fatalf("default = %d, want %d", got, defaultAccountWarmSessionSeconds)
	}
	t.Setenv("M365_ACCOUNT_WARM_SESSION_SECONDS", "45")
	if got := accountWarmSessionSecondsDefault(); got != 45 {
		t.Fatalf("integer env = %d, want 45", got)
	}
	t.Setenv("M365_ACCOUNT_WARM_SESSION_SECONDS", "")
	t.Setenv("M365_ACCOUNT_WARM_SESSION_WINDOW", "90s")
	if got := accountWarmSessionSecondsDefault(); got != 90 {
		t.Fatalf("duration env = %d, want 90", got)
	}
}

func TestAccountWarmSessionSettingsDefaultsAndValidation(t *testing.T) {
	t.Setenv("M365_ACCOUNT_WARM_SESSION_SECONDS", "")
	t.Setenv("M365_ACCOUNT_WARM_SESSION_WINDOW", "")
	v := defaultRuntimeSettings()
	if v.AccountWarmSessionSeconds != defaultAccountWarmSessionSeconds {
		t.Fatalf("default warm seconds = %d, want %d", v.AccountWarmSessionSeconds, defaultAccountWarmSessionSeconds)
	}
	if v.CacheOnAccountSwitch {
		t.Fatal("cacheOnAccountSwitch must default to false")
	}
	if err := validateSettings(v); err != nil {
		t.Fatalf("defaults failed validation: %v", err)
	}
	v.AccountWarmSessionSeconds = 0
	if err := validateSettings(v); err == nil {
		t.Fatal("zero warm seconds accepted")
	}
	v.AccountWarmSessionSeconds = 86401
	if err := validateSettings(v); err == nil {
		t.Fatal("warm seconds above 86400 accepted")
	}
}

func TestAccountQueueTimeoutSettingsDefaultsAndValidation(t *testing.T) {
	t.Setenv("M365_ACCOUNT_QUEUE_TIMEOUT_SECONDS", "")
	v := defaultRuntimeSettings()
	if v.AccountQueueTimeoutSeconds != defaultAccountQueueTimeoutSeconds {
		t.Fatalf("default queue timeout = %d, want %d", v.AccountQueueTimeoutSeconds, defaultAccountQueueTimeoutSeconds)
	}
	if err := validateSettings(v); err != nil {
		t.Fatalf("defaults failed validation: %v", err)
	}
	v.AccountQueueTimeoutSeconds = 0
	if err := validateSettings(v); err == nil {
		t.Fatal("zero queue timeout accepted")
	}
	v.AccountQueueTimeoutSeconds = 86401
	if err := validateSettings(v); err == nil {
		t.Fatal("queue timeout above 86400 accepted")
	}
}

func TestGatewayConcurrencySettingsDefaultsAndValidation(t *testing.T) {
	t.Setenv("M365_GATEWAY_CONCURRENCY", "")
	v := defaultRuntimeSettings()
	if v.GatewayConcurrency != defaultGatewayConcurrency {
		t.Fatalf("default gateway concurrency = %d, want %d", v.GatewayConcurrency, defaultGatewayConcurrency)
	}
	if err := validateSettings(v); err != nil {
		t.Fatalf("defaults failed validation: %v", err)
	}
	// 0 = unlimited, must be accepted.
	v.GatewayConcurrency = 0
	if err := validateSettings(v); err != nil {
		t.Fatalf("0 (unlimited) should be accepted: %v", err)
	}
	// Negative values must be rejected.
	v.GatewayConcurrency = -1
	if err := validateSettings(v); err == nil {
		t.Fatal("negative gateway concurrency accepted")
	}
	// Above 65536 must be rejected.
	v.GatewayConcurrency = 65537
	if err := validateSettings(v); err == nil {
		t.Fatal("gateway concurrency above 65536 accepted")
	}
}
