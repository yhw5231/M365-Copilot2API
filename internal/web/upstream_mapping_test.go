package web

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestModelMappingLegacyUpstreamToneUnmarshal(t *testing.T) {
	raw := `{"publicModel":"gpt-5.4","upstreamTone":"Gpt_5_4_Chat","displayName":"GPT-5.4","defaultReasoningLevel":"medium"}`
	var m modelMapping
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.UpstreamMapping != "Gpt_5_4_Chat" {
		t.Fatalf("legacy upstreamTone not mapped to UpstreamMapping: %#v", m)
	}
	// New-format files keep working.
	raw = `{"publicModel":"gpt-5.4","upstreamMapping":"Custom Chat","displayName":"GPT-5.4","defaultReasoningLevel":"medium"}`
	m = modelMapping{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.UpstreamMapping != "Custom Chat" {
		t.Fatalf("new upstreamMapping not read: %#v", m)
	}
}

func TestEffectiveUpstreamMappingsFallbackToDefaults(t *testing.T) {
	if len(effectiveUpstreamMappings(nil)) != len(defaultUpstreamMappings) {
		t.Fatalf("empty configured mappings must fall back to defaults")
	}
	custom := []upstreamMapping{{Name: "my-map", Tone: "My_Tone"}}
	if got := effectiveUpstreamMappings(custom); len(got) != 1 || got[0].Name != "my-map" {
		t.Fatalf("configured mappings must win: %#v", got)
	}
}

func TestConfiguredModelToneResolvesMappingName(t *testing.T) {
	prior := openSettingsStore().v.UpstreamMappings
	openSettingsStore().mu.Lock()
	openSettingsStore().v.UpstreamMappings = []upstreamMapping{{Name: "My Chat", Tone: "Gpt_5_4_Chat"}}
	openSettingsStore().mu.Unlock()
	defer func() {
		openSettingsStore().mu.Lock()
		openSettingsStore().v.UpstreamMappings = prior
		openSettingsStore().mu.Unlock()
	}()

	mappings := []modelMapping{{PublicModel: "custom-model", UpstreamMapping: "my chat", DisplayName: "Custom", DefaultReasoningLevel: "medium"}}
	tone, ok := configuredModelTone("custom-model", mappings)
	if !ok || tone != "Gpt_5_4_Chat" {
		t.Fatalf("tone=%q ok=%t", tone, ok)
	}
	if !upstreamMappingEnabled("MY CHAT", openSettingsStore().v.UpstreamMappings) {
		t.Fatal("case-insensitive mapping lookup failed")
	}
	if upstreamMappingEnabled("missing", openSettingsStore().v.UpstreamMappings) {
		t.Fatal("missing mapping must count as disabled")
	}
}

func TestDisabledUpstreamMappingRemovesModel(t *testing.T) {
	off := false
	priorMappings := openSettingsStore().v.ModelMappings
	priorUpstream := openSettingsStore().v.UpstreamMappings
	openSettingsStore().mu.Lock()
	openSettingsStore().v.ModelMappings = []modelMapping{{PublicModel: "gpt-5.4", UpstreamMapping: "My Chat", DisplayName: "GPT-5.4", DefaultReasoningLevel: "low"}}
	openSettingsStore().v.UpstreamMappings = []upstreamMapping{{Name: "My Chat", Tone: "Gpt_5_4_Chat", Enabled: &off}}
	openSettingsStore().mu.Unlock()
	defer func() {
		openSettingsStore().mu.Lock()
		openSettingsStore().v.ModelMappings = priorMappings
		openSettingsStore().v.UpstreamMappings = priorUpstream
		openSettingsStore().mu.Unlock()
	}()

	models := configuredModelSpecs(openSettingsStore().v.ModelMappings)
	for _, m := range models {
		if strings.EqualFold(m.ID, "gpt-5.4") {
			t.Fatalf("gpt-5.4 still advertised with disabled upstream mapping")
		}
	}
	if err := checkModelAvailable("gpt-5.4", openSettingsStore().v.ModelMappings); err == nil {
		t.Fatal("model with disabled upstream mapping must be rejected")
	}
	rows := modelRouteTable(openSettingsStore().v.ModelMappings, openSettingsStore().v.UpstreamMappings, nil)
	for _, r := range rows {
		if strings.EqualFold(r.Model, "gpt-5.4") && r.Enabled {
			t.Fatalf("route row reported enabled despite disabled upstream mapping: %#v", r)
		}
	}
}

func TestHiddenModelsRemovedFromRouteTableAndCatalog(t *testing.T) {
	priorMappings := openSettingsStore().v.ModelMappings
	priorHidden := openSettingsStore().v.HiddenModels
	openSettingsStore().mu.Lock()
	openSettingsStore().v.ModelMappings = nil
	openSettingsStore().v.HiddenModels = []string{"gpt-5.5", "gpt-5.6-sol"}
	openSettingsStore().mu.Unlock()
	defer func() {
		openSettingsStore().mu.Lock()
		openSettingsStore().v.ModelMappings = priorMappings
		openSettingsStore().v.HiddenModels = priorHidden
		openSettingsStore().mu.Unlock()
	}()

	rows := modelRouteTable(nil, defaultUpstreamMappings, openSettingsStore().v.HiddenModels)
	for _, r := range rows {
		if strings.EqualFold(r.Model, "gpt-5.5") || strings.EqualFold(r.Model, "gpt-5.6-sol") {
			t.Fatalf("hidden model %q still in route table", r.Model)
		}
	}
	models := configuredModelSpecs(nil)
	for _, m := range models {
		if strings.EqualFold(m.ID, "gpt-5.5") {
			t.Fatalf("hidden model gpt-5.5 still advertised")
		}
	}
}

func TestValidateSettingsRejectsBrokenMappings(t *testing.T) {
	v := defaultRuntimeSettings()
	// Duplicate mapping names.
	v.UpstreamMappings = append(v.UpstreamMappings, upstreamMapping{Name: "Gpt_5_4_Chat", Tone: "X"})
	if err := validateSettings(v); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("duplicate mapping name accepted: %v", err)
	}
	// Model referencing a mapping that does not exist.
	v = defaultRuntimeSettings()
	v.ModelMappings = []modelMapping{{PublicModel: "custom", UpstreamMapping: "Nope_Nope", DisplayName: "Custom", DefaultReasoningLevel: "medium"}}
	if err := validateSettings(v); err == nil || !strings.Contains(err.Error(), "不存在的上游映射") {
		t.Fatalf("model referencing unknown mapping accepted: %v", err)
	}
	// Model without any mapping target.
	v = defaultRuntimeSettings()
	v.ModelMappings = []modelMapping{{PublicModel: "custom", DisplayName: "Custom", DefaultReasoningLevel: "medium"}}
	if err := validateSettings(v); err == nil {
		t.Fatal("model without upstream mapping accepted")
	}
	// Custom mapping with an invalid tone.
	v = defaultRuntimeSettings()
	v.UpstreamMappings = []upstreamMapping{{Name: "ok-name", Tone: "../evil"}}
	if err := validateSettings(v); err == nil {
		t.Fatal("mapping with invalid tone accepted")
	}
	// Valid custom mapping + model round-trips.
	v = defaultRuntimeSettings()
	v.UpstreamMappings = []upstreamMapping{{Name: "My Custom Map", Tone: "Gpt_5_6_Reasoning"}}
	v.ModelMappings = []modelMapping{{PublicModel: "custom-route", UpstreamMapping: "MY CUSTOM MAP", DisplayName: "Custom Route", DefaultReasoningLevel: "high"}}
	if err := validateSettings(v); err != nil {
		t.Fatalf("valid custom config rejected: %v", err)
	}
}

func TestDefaultModelMappingsUseDefaultMappingNames(t *testing.T) {
	for _, m := range defaultModelMappings {
		if _, ok := resolveUpstreamMapping(m.UpstreamMapping, defaultUpstreamMappings); !ok {
			t.Fatalf("default mapping %q references unknown upstream mapping %q", m.PublicModel, m.UpstreamMapping)
		}
	}
}