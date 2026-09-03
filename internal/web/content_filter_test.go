package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func setTestContentFilterRules(t *testing.T, rules []contentFilterRule) {
	t.Helper()
	prev := openSettingsStore
	t.Cleanup(func() { openSettingsStore = prev })
	store := newSettingsStore("", "")
	store.v.ContentFilterEnabled = true
	store.v.ContentFilterRules = rules
	store.v.ContentFilterOpeningBuffer = 0
	openSettingsStore = func() *settingsStore { return store }
}

func TestFilterContentFullReplacesWholeText(t *testing.T) {
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: "Microsoft", Replacement: "替换文本"}})
	repl, hit := filterContentFull("Hello from Microsoft 365 Copilot!")
	if !hit {
		t.Fatalf("expected keyword hit")
	}
	if repl != "替换文本" {
		t.Fatalf("expected replacement text, got %q", repl)
	}
	if _, hit := filterContentFull("nothing here"); hit {
		t.Fatalf("unexpected hit on clean text")
	}
}

func TestFilterContentFullCaseInsensitive(t *testing.T) {
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: "MicroSoft", Replacement: "X"}})
	if _, hit := filterContentFull("welcome to microsoft.com"); !hit {
		t.Fatalf("expected case-insensitive hit")
	}
}

func TestFilterContentDisabled(t *testing.T) {
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: "Microsoft", Replacement: "X"}})
	// Disable the switch explicitly.
	store := openSettingsStore()
	store.v.ContentFilterEnabled = false
	if _, hit := filterContentFull("microsoft"); hit {
		t.Fatalf("filter must be inert when disabled")
	}
}

func TestFilterContentDisabledWithoutRules(t *testing.T) {
	setTestContentFilterRules(t, nil)
	if _, hit := filterContentFull("anything"); hit {
		t.Fatalf("filter must be inert without rules")
	}
}

// TestContentStreamFilterSplitKeyword walks every possible split point of a
// keyword-carrying stream: no matter how the upstream chunks the text, the
// keyword must be detected and never released.
func TestContentStreamFilterSplitKeyword(t *testing.T) {
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: "Microsoft", Replacement: "REPL"}})
	text := "The quick brown fox jumps over Microsoft today."
	for split := 1; split < len(text); split++ {
		f := newContentStreamFilter()
		var sent strings.Builder
		for i := 0; i < len(text); i += split {
			end := i + split
			if end > len(text) {
				end = len(text)
			}
			out, hit, repl := f.push(text[i:end])
			if hit {
				if repl != "REPL" {
					t.Fatalf("split=%d: wrong replacement %q", split, repl)
				}
				// Nothing further may be released after a hit.
				if out2, hit2, _ := f.push(" more"); !hit2 || out2 != "" {
					t.Fatalf("split=%d: post-hit push must drop text", split)
				}
				sent.WriteString("<HIT>")
				break
			}
			sent.WriteString(out)
		}
		if !strings.Contains(sent.String(), "<HIT>") {
			t.Fatalf("split=%d: keyword never detected (streamed %q)", split, sent.String())
		}
		if strings.Contains(sent.String(), "Microsoft") || strings.Contains(strings.ToLower(sent.String()), "microsof") {
			t.Fatalf("split=%d: keyword fragment leaked before replacement: %q", split, sent.String())
		}
	}
}

func TestContentStreamFilterCleanStreamReleasesAll(t *testing.T) {
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: "Microsoft", Replacement: "X"}})
	f := newContentStreamFilter()
	text := "just an ordinary answer with no banned words at all."
	var sent strings.Builder
	for i := 0; i < len(text); i += 7 {
		end := i + 7
		if end > len(text) {
			end = len(text)
		}
		out, hit, _ := f.push(text[i:end])
		if hit {
			t.Fatalf("unexpected hit on clean text")
		}
		sent.WriteString(out)
	}
	tail, hit, _ := f.flush()
	if hit {
		t.Fatalf("unexpected hit at flush")
	}
	sent.WriteString(tail)
	if sent.String() != text {
		t.Fatalf("clean stream must be released verbatim: got %q want %q", sent.String(), text)
	}
}

func TestContentStreamFilterHitAtFlush(t *testing.T) {
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: "秘密句子", Replacement: "已替换"}})
	f := newContentStreamFilter()
	// The scan runs on every push, so a keyword completed inside the buffer is
	// detected immediately with nothing released before it.
	out, hit, repl := f.push("回答中包含秘密句子")
	if !hit || repl != "已替换" {
		t.Fatalf("expected hit at push, got hit=%v repl=%q", hit, repl)
	}
	if out != "" {
		t.Fatalf("nothing may be released once a hit is detected, got %q", out)
	}
	// After the hit the filter keeps reporting the replacement and drops text.
	if out2, hit2, repl2 := f.push(" more"); !hit2 || out2 != "" || repl2 != "已替换" {
		t.Fatalf("post-hit push must drop text, got out=%q hit=%v repl=%q", out2, hit2, repl2)
	}
}

func TestContentStreamFilterOpeningBufferCleanReplacementOnly(t *testing.T) {
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: "Microsoft", Replacement: "REPL"}})
	f := newContentStreamFilter()
	f.openingBuffer = 512
	var sent strings.Builder
	// Keyword inside the opening window: nothing may be sent before the hit.
	out, hit, _ := f.push("I am Microsoft Copilot and I")
	if hit {
		sent.WriteString(out)
	}
	if strings.Contains(sent.String(), "I am") {
		t.Fatalf("opening window leaked text before hit: %q", sent.String())
	}
	_, hit, repl := f.flush()
	if !hit || repl != "REPL" {
		t.Fatalf("expected replacement %q, got hit=%v repl=%q", "REPL", hit, repl)
	}
}

func TestContentStreamFilterMultiByteKeywordSplit(t *testing.T) {
	kw := "微软公司"
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: kw, Replacement: "R"}})
	text := "这是关于微软公司的一段话。"
	for split := 1; split < len(text); split++ {
		f := newContentStreamFilter()
		var sent strings.Builder
		detected := false
		for i := 0; i < len(text); i += split {
			end := i + split
			if end > len(text) {
				end = len(text)
			}
			out, hit, _ := f.push(text[i:end])
			if hit {
				detected = true
				break
			}
			sent.WriteString(out)
		}
		if !detected {
			t.Fatalf("split=%d: multi-byte keyword never detected", split)
		}
		if strings.Contains(sent.String(), "微软公") {
			t.Fatalf("split=%d: keyword fragment leaked: %q", split, sent.String())
		}
		if !validUTF8Continuation(sent.String()) {
			t.Fatalf("split=%d: released text broke a UTF-8 sequence: %q", split, sent.String())
		}
	}
}

func validUTF8Continuation(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

func TestContentStreamFilterFirstRuleWins(t *testing.T) {
	setTestContentFilterRules(t, []contentFilterRule{
		{Keyword: "beta", Replacement: "FIRST"},
		{Keyword: "alpha", Replacement: "SECOND"},
	})
	f := newContentStreamFilter()
	if _, hit, repl := f.push("alpha beta"); !hit || repl != "SECOND" {
		t.Fatalf("earliest-position match must win, got repl=%q hit=%v", repl, hit)
	}
}

func TestContentStreamFilterNilPassThrough(t *testing.T) {
	var f *contentStreamFilter
	out, hit, _ := f.push("anything")
	if hit || out != "anything" {
		t.Fatalf("nil filter must pass text through unchanged")
	}
}

func TestValidateSettingsContentFilter(t *testing.T) {
	v := defaultRuntimeSettings()
	v.ContentFilterEnabled = true
	v.ContentFilterRules = []contentFilterRule{{Keyword: "  ", Replacement: "x"}}
	if err := validateSettings(v); err == nil || !strings.Contains(err.Error(), "关键词") {
		t.Fatalf("expected keyword validation error, got %v", err)
	}
	v.ContentFilterRules = []contentFilterRule{{Keyword: "Microsoft", Replacement: ""}}
	if err := validateSettings(v); err != nil {
		t.Fatalf("empty replacement must be allowed: %v", err)
	}
	if v.ContentFilterOpeningBuffer != contentFilterOpeningBufferDefault() {
		t.Fatalf("default opening buffer = %d, want %d", v.ContentFilterOpeningBuffer, contentFilterOpeningBufferDefault())
	}
	v.ContentFilterOpeningBuffer = -1
	if err := validateSettings(v); err == nil {
		t.Fatalf("negative opening buffer must be rejected")
	}
}

func TestContentStreamFilterLongKeywordHoldback(t *testing.T) {
	kw := "this is a very long banned sentence keyword"
	setTestContentFilterRules(t, []contentFilterRule{{Keyword: kw, Replacement: "R"}})
	f := newContentStreamFilter()
	if f.holdbackRunes != len([]rune(kw))-1 {
		t.Fatalf("holdback = %d, want %d", f.holdbackRunes, len([]rune(kw))-1)
	}
	// The keyword split mid-way must still be caught before its prefix leaks.
	var sent strings.Builder
	detected := false
	text := "prefix text " + kw + " suffix"
	for i := 0; i < len(text); i += 5 {
		end := i + 5
		if end > len(text) {
			end = len(text)
		}
		out, hit, _ := f.push(text[i:end])
		if hit {
			detected = true
			break
		}
		sent.WriteString(out)
	}
	if !detected {
		t.Fatalf("long keyword never detected")
	}
	if strings.Contains(sent.String(), kw[:10]) {
		t.Fatalf("long keyword prefix leaked: %q", sent.String())
	}
}

func TestAdminSettingsContentFilterRoundTrip(t *testing.T) {
	st := &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), accountPath: filepath.Join(t.TempDir(), "account-settings.json"), v: defaultRuntimeSettings()}
	s := &Server{settings: st}

	body := `{"contentFilterEnabled":true,"contentFilterOpeningBuffer":512,"contentFilterRules":[{"keyword":"Microsoft","replacement":"自定义文本"},{"keyword":"secret sentence","replacement":""}]}`
	r := httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()
	s.adminSettings(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("content filter PUT=%d %s", w.Code, w.Body.String())
	}
	got := st.get()
	if !got.ContentFilterEnabled || got.ContentFilterOpeningBuffer != 512 || len(got.ContentFilterRules) != 2 {
		t.Fatalf("content filter settings not persisted: %+v", got)
	}
	if got.ContentFilterRules[0].Keyword != "Microsoft" || got.ContentFilterRules[0].Replacement != "自定义文本" {
		t.Fatalf("rule 0 mismatch: %+v", got.ContentFilterRules[0])
	}
	if got.ContentFilterRules[1].Replacement != "" {
		t.Fatalf("empty replacement must be preserved: %+v", got.ContentFilterRules[1])
	}

	// GET must expose the fields for the admin console.
	gr := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	gw := httptest.NewRecorder()
	s.adminSettings(gw, gr)
	if gw.Code != http.StatusOK {
		t.Fatalf("GET=%d", gw.Code)
	}
	var payload struct {
		Settings runtimeSettings `json:"settings"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &payload); err != nil {
		t.Fatalf("GET body decode: %v", err)
	}
	if !payload.Settings.ContentFilterEnabled || len(payload.Settings.ContentFilterRules) != 2 {
		t.Fatalf("GET does not expose content filter settings: %s", gw.Body.String())
	}
}

func TestAdminSettingsContentFilterRejectsBadRule(t *testing.T) {
	st := &settingsStore{path: filepath.Join(t.TempDir(), "settings.json"), accountPath: filepath.Join(t.TempDir(), "account-settings.json"), v: defaultRuntimeSettings()}
	s := &Server{settings: st}

	r := httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader([]byte(`{"contentFilterEnabled":true,"contentFilterRules":[{"keyword":"  ","replacement":"x"}]}`)))
	w := httptest.NewRecorder()
	s.adminSettings(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("blank keyword must be rejected with 400, got %d %s", w.Code, w.Body.String())
	}
}
