package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

func TestUpstreamAppChunksExtractsChunkNames(t *testing.T) {
	doc := []byte(`<html><head>
<script src="https://res.public.onecdn.static.microsoft/midgard/versionless-v2/runtime.eaf5d4ab.js"></script>
<link rel="preload" href="https://res.public.onecdn.static.microsoft/midgard/versionless-v2/main.4d165c30.js" as="script">
<script src="https://res.public.onecdn.static.microsoft/midgard/versionless-v2/main.4d165c30.js"></script>
<script src="https://res.public.onecdn.static.microsoft/midgard/versionless-v2/route-shared.chatproviders.2ba754a6.chunk.js"></script>
<script src="https://res.cdn.office.net/officehub/bundles/unauth-013ac4217d.js"></script>
</head></html>`)
	got := upstreamAppChunks(doc)
	want := []string{"runtime.eaf5d4ab.js", "main.4d165c30.js", "route-shared.chatproviders.2ba754a6.chunk.js"}
	if len(got) != len(want) {
		t.Fatalf("chunks=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chunks=%v, want %v", got, want)
		}
	}
}

func TestUpstreamAppChunksWithoutBundleReference(t *testing.T) {
	// The anonymous landing page names no app chunk; the refresh must report
	// that instead of pretending the list is empty.
	if got := upstreamAppChunks([]byte(`<html><head><title>OK</title></head><body></body></html>`)); len(got) != 0 {
		t.Fatalf("anonymous document yielded chunks: %v", got)
	}
}

func TestScanUpstreamTonesFiltersPlaceholderAndDedupes(t *testing.T) {
	body := []byte(`{"tone":"Gpt_5_4_Chat","alt":"Gpt_5_4_Chat","deep":"Gpt_5_6_Reasoning","fast":"Gpt_5_2_Auto",` +
		`"claude":"Claude_Sonnet_Reasoning","opus":"Claude_Opus","fallback":"Magic","decoy":"Gpt_agent"}`)
	got := scanUpstreamTones(body)
	want := []string{"Claude_Opus", "Claude_Sonnet_Reasoning", "Gpt_5_2_Auto", "Gpt_5_4_Chat", "Gpt_5_6_Reasoning"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tones=%v, want %v", got, want)
	}
}

// 文档内嵌的模型选择器清单是该账号“实际可用模型”的权威来源：脚本分片里
// 只有通用的 tone 表（GPT 5.2~5.4 / Claude），新的 GPT 5.6 只出现在清单里。
func TestParseUpstreamModelIDsFromEmbeddedPicker(t *testing.T) {
	picker := `{"state":{"data":{"bizchatAsAgentGpt":{"clientPreferences":{"modelSelectorMetadata":{` +
		`"defaultModelSelectionId":"Magic","availableModelSelectionOptions":[` +
		`{"id":"Magic","type":"item","menuItemTitle":"Auto","sectionNumber":1},` +
		`{"id":"Chat","type":"item","menuItemTitle":"Quick response","sectionNumber":1},` +
		`{"id":"Reasoning","type":"item","menuItemTitle":"Think deeper","sectionNumber":1},` +
		`{"id":"OpenAI","type":"itemGroup","menuItemTitle":"GPT","itemGroup":[` +
		`{"id":"Gpt_5_6_Reasoning","type":"item","menuItemTitle":"GPT 5.6 Think deeper","sectionNumber":2},` +
		`{"id":"Gpt_5_6_Chat","type":"item","menuItemTitle":"GPT 5.6 Quick response","sectionNumber":2}` +
		`],"sectionNumber":2}],"totalNumberOfSections":2}}}}}}`
	// The document carries the state JSON escaped inside a script string, once
	// per preloaded scenario, and unrelated "id" fields elsewhere.
	doc := []byte(`<!DOCTYPE html><html><body><script>window.__STATE__="` +
		strings.ReplaceAll(strings.ReplaceAll(picker, `"`, `\"`), `[`, `[`) +
		`"</script><script>window.__OTHER__="[{\"id\":\"not-a-model\"}]"</script></body></html>`)

	got := parseUpstreamModelIDs(doc)
	// Only tone ids ("<family>_<variant>") count: the catalog's generic modes
	// ("Chat", "Reasoning") and the "Magic" auto placeholder are not route
	// targets the gateway sends upstream.
	want := "Gpt_5_6_Chat,Gpt_5_6_Reasoning"
	if strings.Join(got, ",") != want {
		t.Fatalf("picker ids=%v, want %s", got, want)
	}
}

func TestParseUpstreamModelIDsIgnoresDocumentWithoutPicker(t *testing.T) {
	if got := parseUpstreamModelIDs([]byte(`<html><body>no state here</body></html>`)); len(got) != 0 {
		t.Fatalf("unexpected ids: %v", got)
	}
}

func TestSyncUpstreamMappingsAppendsAndPrunes(t *testing.T) {
	off := false
	current := []upstreamMapping{
		{Name: "Gpt_5_4_Chat", Tone: "Gpt_5_4_Chat"},
		{Name: "My Chat", Tone: "Gpt_5_4_Reasoning", Enabled: &off},
		{Name: "Retired_Tone", Tone: "Retired_Tone"},
	}
	available := []string{"Gpt_5_4_Chat", "Claude_Opus", "Claude_Opus"}
	referenced := map[string]bool{"my chat": true, "retired_tone": true}

	merged, added, removed, unavailable := syncUpstreamMappings(current, available, referenced)
	names := make([]string, 0, len(merged))
	for _, m := range merged {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "Gpt_5_4_Chat,My Chat,Retired_Tone,Claude_Opus" {
		t.Fatalf("merged=%v", names)
	}
	if len(added) != 1 || added[0] != "Claude_Opus" {
		t.Fatalf("added=%v", added)
	}
	if len(removed) != 0 {
		t.Fatalf("referenced mappings must not be pruned: %v", removed)
	}
	// Both kept-because-referenced mappings point at a tone the upstream no
	// longer offers, so both are reported as unavailable.
	if strings.Join(unavailable, ",") != "My Chat,Retired_Tone" {
		t.Fatalf("unavailable=%v", unavailable)
	}
	if merged[1].enabled() {
		t.Fatal("kept mapping lost its disabled switch")
	}
	if merged[1].Tone != "Gpt_5_4_Reasoning" {
		t.Fatalf("kept mapping lost its tone: %#v", merged[1])
	}
	if merged[3].Tone != "Claude_Opus" || !merged[3].enabled() {
		t.Fatalf("new mapping must default to tone = name and enabled: %#v", merged[3])
	}
	// The caller's slice is left alone.
	if len(current) != 3 {
		t.Fatalf("sync mutated the caller's mapping slice: %#v", current)
	}

	// Without a route pointing at it, a mapping the upstream dropped is gone.
	merged, added, removed, unavailable = syncUpstreamMappings(current, available, map[string]bool{"my chat": true})
	names = names[:0]
	for _, m := range merged {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "Gpt_5_4_Chat,My Chat,Claude_Opus" {
		t.Fatalf("merged=%v", names)
	}
	if strings.Join(removed, ",") != "Retired_Tone" || strings.Join(unavailable, ",") != "My Chat" {
		t.Fatalf("removed=%v unavailable=%v", removed, unavailable)
	}
	if len(added) != 1 {
		t.Fatalf("added=%v", added)
	}
}

func TestAdminModelSyncMergesUpstreamModels(t *testing.T) {
	st := &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), accountPath: filepath.Join(t.TempDir(), "account-settings.json"), v: defaultRuntimeSettings()}
	s := &Server{settings: st}
	prior := upstreamModelFetcher
	upstreamModelFetcher = func(context.Context, *Server) ([]string, error) {
		return []string{"Gpt_5_6_Reasoning", "Gpt_5_6_Chat"}, nil
	}
	defer func() { upstreamModelFetcher = prior }()
	priorTones, priorAt := dynamicTones, dynamicAt
	defer func() {
		dynamicMu.Lock()
		dynamicTones, dynamicAt = priorTones, priorAt
		dynamicMu.Unlock()
	}()

	w := httptest.NewRecorder()
	s.adminModelSync(w, httptest.NewRequest(http.MethodPost, "/api/admin/models/sync", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("sync=%d %s", w.Code, w.Body.String())
	}
	var body struct {
		Synced           bool              `json:"synced"`
		UpstreamTones    []string          `json:"upstream_tones"`
		Count            int               `json:"count"`
		Added            []string          `json:"added"`
		Removed          []string          `json:"removed"`
		Unavailable      []string          `json:"unavailable"`
		UpstreamMappings []upstreamMapping `json:"upstreamMappings"`
		SyncedAt         string            `json:"synced_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Synced || body.Count != 2 || body.SyncedAt == "" {
		t.Fatalf("unexpected response: %s", w.Body.String())
	}
	if strings.Join(body.Added, ",") != "Gpt_5_6_Chat" {
		t.Fatalf("added=%v", body.Added)
	}
	// A refresh is a force update: the default mappings this account can no
	// longer pick are deleted, and only the offered models remain.
	if len(body.Removed) != len(defaultUpstreamMappings)-1 {
		t.Fatalf("removed=%v, want %d entries", body.Removed, len(defaultUpstreamMappings)-1)
	}
	if len(body.Unavailable) != 0 {
		t.Fatalf("unavailable=%v", body.Unavailable)
	}
	names := make([]string, 0, len(body.UpstreamMappings))
	for _, m := range body.UpstreamMappings {
		names = append(names, m.Name)
	}
	if strings.Join(names, ",") != "Gpt_5_6_Reasoning,Gpt_5_6_Chat" {
		t.Fatalf("upstreamMappings=%v", names)
	}
	// The rewritten set is persisted (the picker keeps it after a reload) and
	// the offline fallback list reflects the refresh too.
	persisted := st.get().UpstreamMappings
	if len(persisted) != 2 {
		t.Fatalf("persisted=%#v", persisted)
	}
	if _, ok := resolveUpstreamMapping("gpt_5_6_chat", persisted); !ok {
		t.Fatalf("new upstream mapping not persisted: %#v", persisted)
	}
	if !strings.Contains(strings.Join(liveUpstreamTones(), ","), "Gpt_5_6_Chat") {
		t.Fatalf("liveUpstreamTones=%v", liveUpstreamTones())
	}
}

// 路由仍引用的映射即使已从上游列表消失也不能删除，否则设置无法保存。
func TestAdminModelSyncKeepsReferencedMappingAndFlagsIt(t *testing.T) {
	v := defaultRuntimeSettings()
	v.ModelMappings = []modelMapping{{PublicModel: "legacy-model", UpstreamMapping: "Gpt_5_4_Chat", DisplayName: "Legacy", DefaultReasoningLevel: "medium"}}
	st := &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), accountPath: filepath.Join(t.TempDir(), "account-settings.json"), v: v}
	s := &Server{settings: st}
	prior := upstreamModelFetcher
	upstreamModelFetcher = func(context.Context, *Server) ([]string, error) {
		return []string{"Gpt_5_6_Reasoning"}, nil
	}
	defer func() { upstreamModelFetcher = prior }()

	w := httptest.NewRecorder()
	s.adminModelSync(w, httptest.NewRequest(http.MethodPost, "/api/admin/models/sync", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("sync=%d %s", w.Code, w.Body.String())
	}
	var body struct {
		Removed          []string          `json:"removed"`
		Unavailable      []string          `json:"unavailable"`
		UpstreamMappings []upstreamMapping `json:"upstreamMappings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if strings.Join(body.Unavailable, ",") != "Gpt_5_4_Chat" {
		t.Fatalf("unavailable=%v", body.Unavailable)
	}
	for _, name := range body.Removed {
		if name == "Gpt_5_4_Chat" {
			t.Fatal("a mapping a route still references must not be pruned")
		}
	}
	if _, ok := resolveUpstreamMapping("Gpt_5_4_Chat", st.get().UpstreamMappings); !ok {
		t.Fatalf("referenced mapping missing from the persisted set: %#v", st.get().UpstreamMappings)
	}
}

func TestAdminModelSyncReportsUpstreamFailure(t *testing.T) {
	st := &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), accountPath: filepath.Join(t.TempDir(), "account-settings.json"), v: defaultRuntimeSettings()}
	s := &Server{settings: st}
	prior := upstreamModelFetcher
	upstreamModelFetcher = func(context.Context, *Server) ([]string, error) {
		return nil, errors.New("没有可用账号")
	}
	defer func() { upstreamModelFetcher = prior }()

	w := httptest.NewRecorder()
	s.adminModelSync(w, httptest.NewRequest(http.MethodPost, "/api/admin/models/sync", nil))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("failed sync=%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "没有可用账号") {
		t.Fatalf("error detail missing: %s", w.Body.String())
	}
	if len(st.get().UpstreamMappings) != len(defaultUpstreamMappings) {
		t.Fatal("a failed refresh must not rewrite the mapping set")
	}
}

func TestAdminModelSyncRejectsNonPost(t *testing.T) {
	s := &Server{settings: &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), accountPath: filepath.Join(t.TempDir(), "account-settings.json"), v: defaultRuntimeSettings()}}
	w := httptest.NewRecorder()
	s.adminModelSync(w, httptest.NewRequest(http.MethodGet, "/api/admin/models/sync", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestUpstreamAccountsPreferOnlineAndSkipTokenless(t *testing.T) {
	accounts := upstreamAccounts([]auth.AccountToken{
		{ID: "1", Email: "cold@example.com", Status: "cooldown", RefreshToken: "r1"},
		{ID: "2", Email: "no-token@example.com", Status: "online"},
		{ID: "3", Email: "hot@example.com", Status: "online", RefreshToken: "r3"},
	})
	if len(accounts) != 2 {
		t.Fatalf("accounts=%#v", accounts)
	}
	if accounts[0].Email != "hot@example.com" || accounts[1].Email != "cold@example.com" {
		t.Fatalf("online accounts must come first: %#v", accounts)
	}
}

func TestUnionUpstreamModelsKeepsFirstSpellingAndSorts(t *testing.T) {
	got := unionUpstreamModels([]string{"Gpt_5_6_Chat", "gpt_5_6_chat", " "}, []string{"Claude_Sonnet", "Gpt_5_6_Chat"})
	if strings.Join(got, ",") != "Claude_Sonnet,Gpt_5_6_Chat" {
		t.Fatalf("union=%v", got)
	}
}

// 账号选择器清单只列当前可选项，ChatHub 仍接受更早的 tone（例如 Gpt_5_5_Chat），
// 因此拉取结果必须是多来源并集，不能只取选择器清单。
func TestFetchUpstreamModelsUnionsCatalogAndBuiltinList(t *testing.T) {
	store, err := auth.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(auth.TokenSet{Email: "picker@example.com", AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	s := &Server{tokens: store}
	prior := upstreamAccountFetcher
	upstreamAccountFetcher = func(context.Context, *Server, auth.AccountToken) ([]string, error) {
		// What the picker catalog + chunk tone table yielded for this account.
		return []string{"Gpt_5_6_Chat", "Gpt_5_6_Reasoning", "Gpt_5_4_Chat"}, nil
	}
	defer func() { upstreamAccountFetcher = prior }()

	got, err := s.fetchUpstreamModels(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	joined := strings.Join(got, ",")
	for _, want := range []string{"Gpt_5_6_Chat", "Gpt_5_6_Reasoning", "Gpt_5_4_Chat", "Gpt_5_5_Chat", "Claude_Sonnet"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("union missed %s: %v", want, got)
		}
	}
}
