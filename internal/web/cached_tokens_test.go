package web

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newIsolatedSessionResolver returns a resolver backed by a per-test temp file
// so persisted sessions from other tests or real runs never leak into the
// assertions.
func newIsolatedSessionResolver(t *testing.T) *sessionResolver {
	t.Helper()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	t.Cleanup(func() { _ = sr.flush() })
	return sr
}

// TestCachedTokensReuseFlow verifies the diff method end to end at the
// resolver level: turn 1 stores its full input size on the session binding,
// turn 2 (history + one more user message, same prompt_cache_key) resolves the
// session and picks up that size as the cache base, so its cached share is the
// previous turn's input and only the growth is new.
func TestCachedTokensReuseFlow(t *testing.T) {
	sr := newIsolatedSessionResolver(t)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-diff-test")
	req.Header.Set("session_id", "diff-session")

	// Turn 1: system + one user message. Full logical input = 100 est tokens.
	turn1 := []oaiMsg{
		{Role: "system", Content: strings.Repeat("system rules ", 12)},
		{Role: "user", Content: strings.Repeat("first question ", 8)},
	}
	turn1Full := int64(0)
	for _, m := range turn1 {
		turn1Full += EstimateTokens(contentToString(m.Content))
	}
	if turn1Full <= 0 {
		t.Fatalf("turn1 input estimate must be positive, got %d", turn1Full)
	}
	body1 := &oaiReq{Messages: turn1, SessionID: "diff-session"}
	if res := sr.Resolve(req, body1); !res.IsNew {
		t.Fatalf("first request must start a new session, got matched=%s", res.MatchedBy)
	}
	// Bind with a cloud conversation and record the turn-1 input size.
	sr.BindWithTask("upstream-sess-1", "conv-diff-1", "acc-1", body1, "", req, nil)
	sr.RecordSessionInputTokens("conv-diff-1", turn1Full)

	// Turn 2: full replay + one more user message. The resolver must match and
	// hand back the stored base so cached = min(base, this turn's full).
	turn2 := append(append([]oaiMsg(nil), turn1...), oaiMsg{Role: "user", Content: strings.Repeat("second question ", 8)})
	turn2Full := turn1Full + EstimateTokens("second question second question second question second question second question second question second question second question ")
	body2 := &oaiReq{Messages: turn2, SessionID: "diff-session"}
	res := sr.Resolve(req, body2)
	if res.IsNew {
		t.Fatal("turn 2 must match the bound session")
	}
	if res.LastInputTokens != turn1Full {
		t.Fatalf("turn2 cache base=%d want %d", res.LastInputTokens, turn1Full)
	}
	cached := res.LastInputTokens
	if cached > turn2Full {
		cached = turn2Full
	}
	newInput := turn2Full - cached
	if cached != turn1Full || newInput <= 0 {
		t.Fatalf("diff method wrong: cached=%d new=%d base=%d turn2Full=%d", cached, newInput, res.LastInputTokens, turn2Full)
	}
	t.Logf("turn1Full=%d turn2Full=%d cached=%d new=%d", turn1Full, turn2Full, cached, newInput)
}

// TestCachedTokensNoMatchReportsZeroCache verifies the fresh-session path: no
// explicit session id, no match — the diff base stays 0 and the whole input is
// new.
func TestCachedTokensNonReuse(t *testing.T) {
	sr := newIsolatedSessionResolver(t)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "hello"}}}
	res := sr.Resolve(req, body)
	if !res.IsNew {
		t.Fatalf("unidentified request must be a new session, got matched=%s", res.MatchedBy)
	}
	if res.LastInputTokens != 0 {
		t.Fatalf("fresh session cache base=%d want 0", res.LastInputTokens)
	}
}

// TestCachedTokensCompactedContextResetsCache covers the compaction contract:
// a client that shrinks/replaces its context must be treated as a new upstream
// conversation — cache 0, and never cached > input.
func TestCachedTokensCompactedContextResetsCache(t *testing.T) {
	sr := newIsolatedSessionResolver(t)

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-diff-test")
	req.Header.Set("session_id", "diff-compact")

	// Turn 1: a long conversation gets bound with its full input size.
	turn1 := []oaiMsg{
		{Role: "system", Content: strings.Repeat("system rules ", 20)},
		{Role: "user", Content: strings.Repeat("question one ", 10)},
		{Role: "assistant", Content: strings.Repeat("answer one ", 10)},
		{Role: "user", Content: strings.Repeat("question two ", 10)},
	}
	turn1Full := int64(0)
	for _, m := range turn1 {
		turn1Full += EstimateTokens(contentToString(m.Content))
	}
	body1 := &oaiReq{Messages: turn1, SessionID: "diff-compact"}
	sr.BindWithTask("upstream-sess-c", "conv-compact-1", "acc-1", body1, "", req, nil)
	sr.RecordSessionInputTokens("conv-compact-1", turn1Full)

	// Turn 2: client compacted the context — fewer, replaced messages. This
	// must NOT extend the stored history: ResetUpstream with cache base 0.
	compacted := []oaiMsg{
		{Role: "user", Content: "summary of the earlier conversation"},
		{Role: "user", Content: strings.Repeat("new question ", 10)},
	}
	body2 := &oaiReq{Messages: compacted, SessionID: "diff-compact"}
	res := sr.Resolve(req, body2)
	if !res.ResetUpstream {
		t.Fatalf("compacted context must reset the upstream conversation, got matched=%s history=%d", res.MatchedBy, res.HistoryLen)
	}
	if res.LastInputTokens != 0 {
		t.Fatalf("compacted context cache base=%d want 0", res.LastInputTokens)
	}
	// Diff-method invariant with base 0: cached 0, everything new.
	full := int64(0)
	for _, m := range compacted {
		full += EstimateTokens(contentToString(m.Content))
	}
	if full <= 0 {
		t.Fatalf("compacted input estimate must be positive, got %d", full)
	}
}

// TestContextPrefixSnapshotDriftStillMatches covers the snapshot-drift
// compatibility: DSH regenerates its runtime-context snapshot every turn and
// may inject extra copies or drop old ones. None of that may reset the
// upstream conversation — only a real content divergence does.
func TestContextPrefixSnapshotDriftStillMatches(t *testing.T) {
	snap := func(policy string) oaiMsg {
		return oaiMsg{Role: "user", Content: "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\n" + policy}
	}
	hist := []oaiMsg{
		{Role: "system", Content: "system prompt"},
		snap("old policy text that drifted away"),
		{Role: "user", Content: "question one"},
		{Role: "assistant", Content: "answer one"},
	}

	// 同位置内容漂移：快照文本变了但位置不变 → 命中。
	sameSpot := []oaiMsg{
		{Role: "system", Content: "system prompt"},
		snap("brand new policy text for this turn"),
		{Role: "user", Content: "question one"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "question two"},
	}
	if n := contextPrefixLen(hist, sameSpot); n != 4 {
		t.Fatalf("drifted snapshot at same position: history=%d want 4", n)
	}

	// 快照数量变化：客户端多注入一份快照（压缩后常见）→ 仍命中。返回值是
	// 增量起始下标：前 5 条（含多出的快照）都已消化，增量只剩最后一条用户消息。
	extraSnap := []oaiMsg{
		{Role: "system", Content: "system prompt"},
		snap("policy A"),
		snap("policy B — newly injected after client-side compaction"),
		{Role: "user", Content: "question one"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "question two"},
	}
	if n := contextPrefixLen(hist, extraSnap); n != 5 {
		t.Fatalf("extra injected snapshot: history=%d want 5", n)
	}

	// 快照消失：存储侧有快照、客户端不再发送 → 仍命中。
	noSnap := []oaiMsg{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "question one"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "question two"},
	}
	if n := contextPrefixLen(hist, noSnap); n != 3 {
		t.Fatalf("dropped snapshot: history=%d want 3", n)
	}

	// 真实内容分叉（非快照消息被替换）→ 必须判 0。
	diverged := []oaiMsg{
		{Role: "system", Content: "system prompt"},
		snap("policy"),
		{Role: "user", Content: "TOTALLY DIFFERENT REPLACED HISTORY"},
		{Role: "assistant", Content: "answer one"},
		{Role: "user", Content: "question two"},
	}
	if n := contextPrefixLen(hist, diverged); n != 0 {
		t.Fatalf("genuine divergence: history=%d want 0", n)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
