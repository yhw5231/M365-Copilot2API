package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveExplicitSessionReusesRegardlessOfUser(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	// 首次请求用显式会话 ID 绑定云端对话。
	sr.Bind("", "conv-shared", "acc1",
		&oaiReq{User: "alice", Messages: []oaiMsg{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "你好"}}},
		"",
		resolverTestRequestWithSID("203.0.113.10", "client-a", "alice", "sid-shared"))

	// 续接请求携带同一显式会话 ID（换 user 字段仍命中，说明续用只认会话 ID）。
	res := sr.Resolve(resolverTestRequestWithSID("203.0.113.10", "client-a", "bob", "sid-shared"),
		&oaiReq{
			User: "bob",
			Messages: []oaiMsg{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "你好"},
				{Role: "user", Content: "多说点"},
			},
		})
	if res.IsNew {
		t.Fatal("同一显式会话 ID 未复用会话")
	}
	if res.ConversationID != "conv-shared" {
		t.Fatalf("expected conversation conv-shared, got %s", res.ConversationID)
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 (增量起点), got %d", res.HistoryLen)
	}

	// 携带不同的会话 ID、即使内容完全相同，也必须新建会话（不靠聊天记录相似）。
	res2 := sr.Resolve(resolverTestRequestWithSID("203.0.113.10", "client-a", "bob", "sid-other"),
		&oaiReq{
			User: "bob",
			Messages: []oaiMsg{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "你好"},
				{Role: "user", Content: "多说点"},
			},
		})
	if !res2.IsNew {
		t.Fatalf("不同会话 ID 必须新建会话，got matched=%s", res2.MatchedBy)
	}
}

func TestResolveDoesNotMatchAcrossIdentity(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	sr.Bind("", "conv-a", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 不同 IP / UA 的用户输入同样的短消息，不应串到别人的会话。
	res := sr.Resolve(resolverTestRequest("198.51.100.99", "client-b", "bob"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("跨 IP/UA 的内容必须新建会话，got matched=%s conv=%s", res.MatchedBy, res.ConversationID)
	}
}

func TestResolveExplicitSessionSingleMessageReuses(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("", "conv-short", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequestWithSID("203.0.113.10", "client-a", "alice", "sid-short"))

	// 同一显式会话 ID，即使只发单条相同消息也复用同一会话。
	res := sr.Resolve(resolverTestRequestWithSID("203.0.113.10", "client-a", "alice", "sid-short"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if res.IsNew {
		t.Fatalf("同一显式会话 ID 应复用会话，got IsNew=true")
	}
}

func TestResolveExplicitSessionNeverReusesAcrossSessions(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("", "conv-short", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequestWithSID("203.0.113.10", "client-a", "alice", "sid-a"))

	// 不同会话 ID，内容完全相同也绝不复用。
	res := sr.Resolve(resolverTestRequestWithSID("203.0.113.20", "client-b", "bob", "sid-b"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("different session id must not reuse session, got matched=%s", res.MatchedBy)
	}
}

func resolverTestRequest(ip, ua, user string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = ip + ":12345"
	r.Header.Set("User-Agent", ua)
	return r
}

func resolverTestRequestWithSID(ip, ua, user, sid string) *http.Request {
	r := resolverTestRequest(ip, ua, user)
	r.Header.Set(sessionHeaderName, sid)
	return r
}

func TestResolverIncrementalBoundary(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req1.Header.Set(sessionHeaderName, "sid-inc")
	sr.Bind("", "conv-inc", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
		}},
		"",
		req1)

	// 第二轮携带同一显式会话 ID，历史为前缀 → HistoryLen=2，只发增量。
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req2.Header.Set(sessionHeaderName, "sid-inc")
	res := sr.Resolve(req2,
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
			{Role: "user", Content: "第二轮问题"},
		}})
	if res.IsNew {
		t.Fatal("同一显式会话 ID 的增量请求应复用会话")
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2, got %d", res.HistoryLen)
	}

	// 不带会话 ID 的全新内容 → 一律新会话（不靠聊天记录相似）。
	res2 := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "全新问题完全无关"}}})
	if !res2.IsNew {
		t.Fatalf("无会话 ID 必须新建会话, got %s conv=%s", res2.MatchedBy, res2.ConversationID)
	}
}

func TestResolverEvictsAfterTTL(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	sr.Bind("sess-old", "conv-old", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "旧问题"}}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// 把会话标记为超过默认 2h 闲置。
	sr.mu.Lock()
	old := sr.sessions["sess-old"]
	old.LastUsedAt = time.Now().UTC().Add(-3 * time.Hour)
	sr.sessions["sess-old"] = old
	sr.mu.Unlock()

	res := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "旧问题"}}})
	if !res.IsNew {
		t.Fatalf("闲置超 TTL 的会话应失效，got matched=%s", res.MatchedBy)
	}
}

func TestResolverPersistsExplicitSessionAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	t.Setenv("M365_SESSION_CACHE", path)

	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req1.Header.Set(sessionHeaderName, "sid-persist")
	sr1 := openSessionResolver()
	sr1.Bind("", "conv-persist", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "persisted question"},
			{Role: "assistant", Content: "persisted answer"},
		}},
		"",
		req1)
	if err := sr1.persist.flushNowBlocking(); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：重新打开同一缓存文件，显式会话 ID 的绑定与历史仍在 → 可续用。
	sr2 := openSessionResolver()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req2.Header.Set(sessionHeaderName, "sid-persist")
	res := sr2.Resolve(req2,
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "persisted question"},
			{Role: "assistant", Content: "persisted answer"},
			{Role: "user", Content: "follow-up"},
		}})
	if res.IsNew {
		t.Fatal("显式会话 ID 重启后仍应复用会话")
	}
	if res.ConversationID != "conv-persist" {
		t.Fatalf("unexpected conversation %s", res.ConversationID)
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 after reload, got %d", res.HistoryLen)
	}

	// 同一会话 ID 但多消息内容不再延续历史 → 保留会话 ID，但要求重建上游对话（不误续）。
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req3.Header.Set(sessionHeaderName, "sid-persist")
	res3 := sr2.Resolve(req3,
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "全新问题完全无关"},
			{Role: "assistant", Content: "全新回答"},
			{Role: "user", Content: "追问"},
		}})
	if res3.IsNew {
		t.Fatal("同一会话 ID 仍应识别为同一会话（内容变化走 reset 而非新会话）")
	}
	if !res3.ResetUpstream {
		t.Fatal("内容不延续历史时应 ResetUpstream，而不是续用旧上游对话")
	}
}

func TestAutoCleanupDefaultMaxAgeTwoHours(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	s := newTestServerForAutoCleanup(t)
	s.conversationManager.Record("conv-old", "acc1", "old")
	s.conversationManager.mu.Lock()
	old := s.conversationManager.data["conv-old"]
	old.LastUsedAt = time.Now().UTC().Add(-3 * time.Hour)
	s.conversationManager.data["conv-old"] = old
	s.conversationManager.mu.Unlock()

	active := s.activeConversationSet(2 * time.Hour)
	if active["conv-old"] {
		t.Error("3h 闲置的会话不应在 2h 保护窗口内")
	}
}

func TestConversationSessionStoreDoesNotShareBindingCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_DATA_DIR", dir)
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_SESSION_CACHE", "")

	conversationStore := openSessionStore()
	bindingStore := openSessionResolver()
	if conversationStore.path == bindingStore.path {
		t.Fatalf("conversation and binding stores share %q", conversationStore.path)
	}
}

func TestExplicitDownstreamSessionMapsToSeparateUpstreamSession(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "hello"}}}

	reqA := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqA.Header.Set(sessionHeaderName, "downstream-a")
	sr.Bind("upstream-session-a", "conversation-a", "account-a", body, "", reqA)

	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqB.Header.Set(sessionHeaderName, "downstream-b")
	sr.Bind("upstream-session-b", "conversation-b", "account-a", body, "", reqB)

	resolvedA := sr.Resolve(reqA, body)
	resolvedB := sr.Resolve(reqB, body)
	if resolvedA.IsNew || resolvedB.IsNew {
		t.Fatal("explicit downstream sessions should resolve to their persisted bindings")
	}
	if resolvedA.SessionID == "downstream-a" || resolvedB.SessionID == "downstream-b" {
		t.Fatal("downstream session IDs must not be used as upstream session IDs")
	}
	if resolvedA.SessionID == resolvedB.SessionID {
		t.Fatal("different downstream sessions must map to different upstream sessions")
	}
	if resolvedA.ConversationID != "conversation-a" || resolvedB.ConversationID != "conversation-b" {
		t.Fatalf("explicit sessions crossed conversations: A=%q B=%q", resolvedA.ConversationID, resolvedB.ConversationID)
	}
}

func TestExplicitSessionDetectsCompactedContext(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, "downstream-compacted")

	original := &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
		{Role: "assistant", Content: "second answer"},
	}}
	sr.Bind("upstream-session-old", "conversation-old", "account-a", original, "", req)

	compacted := &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "Summary of the earlier conversation"},
		{Role: "user", Content: "continue from the summary"},
	}}
	resolved := sr.Resolve(req, compacted)
	if resolved.IsNew {
		t.Fatal("stable downstream session should still resolve after compaction")
	}
	if !resolved.ResetUpstream {
		t.Fatalf("compacted context should reset the upstream conversation, matched=%q", resolved.MatchedBy)
	}
	if resolved.HistoryLen != 0 {
		t.Fatalf("compacted context must be sent in full, HistoryLen=%d", resolved.HistoryLen)
	}
	if resolved.MatchedBy != "explicit_context_reset" {
		t.Fatalf("unexpected match mode %q", resolved.MatchedBy)
	}
}

func TestExplicitSessionTreatsSingleMessageAsIncremental(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, "downstream-incremental")

	original := &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
	}}
	sr.Bind("upstream-session", "conversation-existing", "account-a", original, "", req)

	incremental := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "next question"}}}
	resolved := sr.Resolve(req, incremental)
	if resolved.IsNew || resolved.ResetUpstream {
		t.Fatalf("single-message incremental mode must reuse the upstream conversation: %+v", resolved)
	}
	if resolved.HistoryLen != 0 {
		t.Fatalf("single incremental message must be sent as-is, HistoryLen=%d", resolved.HistoryLen)
	}
	if resolved.MatchedBy != "explicit_incremental" {
		t.Fatalf("unexpected match mode %q", resolved.MatchedBy)
	}
	if resolved.ConversationID != "conversation-existing" {
		t.Fatalf("incremental request resolved conversation %q", resolved.ConversationID)
	}
}

func TestExplicitSessionUsesStrictPrefixForIncrementalSlice(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, "downstream-prefix")

	original := &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}}
	sr.Bind("upstream-session", "conversation-existing", "account-a", original, "", req)

	continued := &oaiReq{Messages: []oaiMsg{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "tell me more"},
	}}
	resolved := sr.Resolve(req, continued)
	if resolved.IsNew || resolved.ResetUpstream {
		t.Fatalf("strict history prefix should reuse the upstream conversation: %+v", resolved)
	}
	if resolved.HistoryLen != 2 {
		t.Fatalf("expected two already-sent prefix messages, got %d", resolved.HistoryLen)
	}
	if resolved.MatchedBy != "explicit_prefix_2" {
		t.Fatalf("unexpected match mode %q", resolved.MatchedBy)
	}
}

// TestBodySessionKeyDrivesCompactionDetection verifies the DSH/pi-ai flow where
// the client sends NO session_id header but carries prompt_cache_key, which the
// Responses adapter maps onto body.SessionKey. The resolver must treat that as
// the stable downstream identity: bind once, reuse while history extends the
// stored context, and ResetUpstream (fresh conversation) after compaction.
func TestBodySessionKeyDrivesCompactionDetection(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-dsh-tenant")

	// First turn: no session anywhere yet -> IsNew, then Bind via SessionKey.
	first := &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "You are an agent."},
		{Role: "user", Content: "first question"},
	}}
	first.SessionKey = "session-dsh-abc"
	if res := sr.Resolve(req, first); !res.IsNew {
		t.Fatalf("first turn with no binding must be IsNew, got %+v", res)
	}
	sr.BindWithTask("upstream-session", "conversation-v1", "account-a", first, "first answer", req, nil)

	// Second turn replays the same history plus one new message -> prefix reuse.
	second := &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "You are an agent."},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
	}}
	second.SessionKey = "session-dsh-abc"
	res := sr.Resolve(req, second)
	if res.IsNew {
		t.Fatalf("second turn must reuse the session bound by prompt_cache_key")
	}
	if res.ResetUpstream {
		t.Fatalf("extending history must not reset upstream, matched=%q", res.MatchedBy)
	}
	if res.ConversationID != "conversation-v1" {
		t.Fatalf("second turn must keep conversation-v1, got %q", res.ConversationID)
	}

	// Third turn: client compacted its context (summary replaces history).
	compacted := &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "Summary of the earlier conversation"},
		{Role: "user", Content: "continue from the summary"},
	}}
	compacted.SessionKey = "session-dsh-abc"
	res3 := sr.Resolve(req, compacted)
	if res3.IsNew {
		t.Fatalf("stable downstream session must still resolve after compaction")
	}
	if !res3.ResetUpstream {
		t.Fatalf("compaction must reset the upstream conversation, matched=%q", res3.MatchedBy)
	}
	if res3.MatchedBy != "explicit_context_reset" {
		t.Fatalf("unexpected match mode %q", res3.MatchedBy)
	}
}

// TestResolveAcceptsAltSessionHeaderName verifies the client compatibility
// contract: the session identity is the explicit ID VALUE, regardless of
// whether the client sent it via the canonical "session_id" header or the
// alternative "x-session-id" header (pi-ai openrouter format). Both names must
// resolve to the same persisted session, and a different value under either
// name must stay a fresh session.
func TestResolveAcceptsAltSessionHeaderName(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "hello"}}}

	// Bind via the canonical session_id header.
	reqA := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqA.Header.Set(sessionHeaderName, "shared-session")
	sr.Bind("upstream-a", "conversation-a", "account-a", body, "", reqA)

	// Resolve via the alternative x-session-id header with the same value.
	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqB.Header.Set(sessionHeaderAlt, "shared-session")
	resolvedB := sr.Resolve(reqB, body)
	if resolvedB.IsNew {
		t.Fatal("x-session-id header must resolve the same session as session_id")
	}
	if resolvedB.ConversationID != "conversation-a" {
		t.Fatalf("alt header resolved conversation %q", resolvedB.ConversationID)
	}

	// A different value under either header is a different session.
	reqC := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqC.Header.Set(sessionHeaderAlt, "other-session")
	if resolvedC := sr.Resolve(reqC, body); !resolvedC.IsNew {
		t.Fatalf("different session value must stay new, got %+v", resolvedC)
	}
}

// TestResolveAcceptsClientRequestIDHeader verifies that pi-ai's
// "x-client-request-id" header (sent alongside session_id for the openai
// affinity format) is also accepted as a session identity fallback. It must
// resolve the same persisted session as session_id and must be isolated from
// other values under the same header.
func TestResolveAcceptsClientRequestIDHeader(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "hello"}}}

	// Bind via the canonical session_id header.
	reqA := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqA.Header.Set(sessionHeaderName, "shared-session")
	sr.Bind("upstream-a", "conversation-a", "account-a", body, "", reqA)

	// Resolve via pi-ai's x-client-request-id header with the same value.
	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqB.Header.Set(sessionHeaderClientRequestID, "shared-session")
	resolvedB := sr.Resolve(reqB, body)
	if resolvedB.IsNew {
		t.Fatal("x-client-request-id header must resolve the same session as session_id")
	}
	if resolvedB.ConversationID != "conversation-a" {
		t.Fatalf("x-client-request-id header resolved conversation %q", resolvedB.ConversationID)
	}

	// A different value under the same header is a different session.
	reqC := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqC.Header.Set(sessionHeaderClientRequestID, "other-session")
	if resolvedC := sr.Resolve(reqC, body); !resolvedC.IsNew {
		t.Fatalf("different x-client-request-id value must stay new, got %+v", resolvedC)
	}

	// A per-request correlation id that never repeats (e.g. a random UUID used
	// by some HTTP clients) must NOT silently hijack a session: it is only a
	// fallback, so it must resolve to a fresh session, never a stale one.
	reqD := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqD.Header.Set(sessionHeaderClientRequestID, "uuid-request-0001")
	if resolvedD := sr.Resolve(reqD, body); !resolvedD.IsNew {
		t.Fatalf("unmatched x-client-request-id must stay new, got %+v", resolvedD)
	}
}

// TestExplicitIncrementalCarriesStoredContext verifies that a single-message
// incremental reuse (explicit_incremental, HistoryLen == 0) surfaces the
// upstream conversation's persisted history through ResolveResult.StoredContext.
// The caller uses it to report cached_tokens even though the request body does
// not echo the history; without it the client would see a false zero even
// though the upstream conversation was genuinely reused.
func TestExplicitIncrementalCarriesStoredContext(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, "sid-inc-context")
	sr.Bind("upstream-conv", "conv-inc-context", "account-a",
		&oaiReq{Messages: []oaiMsg{
			{Role: "system", Content: "You are concise."},
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
		}},
		"",
		req)

	// Second turn: only the current user message, no history echo.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req2.Header.Set(sessionHeaderName, "sid-inc-context")
	res := sr.Resolve(req2, &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "第二轮问题"}}})
	if res.IsNew {
		t.Fatal("single-message request with the same explicit session id must reuse the session")
	}
	if res.MatchedBy != "explicit_incremental" {
		t.Fatalf("expected explicit_incremental, got %q", res.MatchedBy)
	}
	if res.HistoryLen != 0 {
		t.Fatalf("expected HistoryLen=0 for single-message incremental, got %d", res.HistoryLen)
	}
	if len(res.StoredContext) != 3 {
		t.Fatalf("expected StoredContext to carry the 3 persisted messages, got %d", len(res.StoredContext))
	}
	if res.StoredContext[2].Content != "第一轮回答" {
		t.Fatalf("stored context content mismatch: %q", res.StoredContext[2].Content)
	}

	// A prefix-echo request must NOT populate StoredContext (the history is
	// already visible in the request body).
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req3.Header.Set(sessionHeaderName, "sid-inc-context")
	res3 := sr.Resolve(req3, &oaiReq{Messages: []oaiMsg{
		{Role: "system", Content: "You are concise."},
		{Role: "user", Content: "第一轮问题"},
		{Role: "assistant", Content: "第一轮回答"},
		{Role: "user", Content: "第三轮问题"},
	}})
	if res3.MatchedBy != "explicit_prefix_3" {
		t.Fatalf("expected explicit_prefix_3, got %q", res3.MatchedBy)
	}
	if len(res3.StoredContext) != 0 {
		t.Fatalf("prefix echo must not populate StoredContext, got %d", len(res3.StoredContext))
	}
}

// TestDSHRuntimeContextSnapshotDrift reproduces the DSH harness's per-request
// runtime-context snapshot: the client re-injects a "Current runtime context."
// user message on every turn whose content drifts ("This snapshot supersedes
// earlier runtime-context snapshots"). The snapshot is session metadata, not
// conversation content, so its drift must not break the prefix match —
// otherwise every request is judged a context reset (full replay, cache 0).
func TestDSHRuntimeContextSnapshotDrift(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, "dsh-snapshot-session")

	systemMsg := oaiMsg{Role: "system", Content: "You are an AI agent powered by DeepSeek Harness."}
	userMsg1 := oaiMsg{Role: "user", Content: "实现站点自定义协议头功能"}
	snapshotV1 := oaiMsg{Role: "user", Content: "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nCurrent DSH file policy: danger-full-access. The DSH file sandbox does not restrict file modifications."}
	snapshotV2 := oaiMsg{Role: "user", Content: "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nCurrent DSH file policy: danger-full-access. Approval prompts are disabled in this session."}

	// First turn: bind [system, user1, snapshotV1] as client history, then the
	// server appends the assistant reply (exactly like bindConversation does).
	bindLikeServer(t, sr, "dsh-snapshot-session", "conv-snap", "account-a",
		[]oaiMsg{systemMsg, userMsg1, snapshotV1},
		"第一轮回答")

	// Second turn: the harness re-drifts the snapshot, so the stored history
	// now diverges at the snapshot position. The prefix must still match the
	// stored history (HistoryLen=4) and only the new user message is the
	// increment; without the snapshot tolerance this is explicit_context_reset.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req2.Header.Set(sessionHeaderName, "dsh-snapshot-session")
	res := sr.Resolve(req2, &oaiReq{Messages: []oaiMsg{
		systemMsg,
		userMsg1,
		snapshotV2,
		{Role: "assistant", Content: "第一轮回答"},
		{Role: "user", Content: "第二轮问题"},
	}})
	if res.IsNew {
		t.Fatal("snapshot drift must not turn the session into a new session")
	}
	if res.ResetUpstream {
		t.Fatalf("snapshot drift must not reset the upstream conversation; matched=%q HistoryLen=%d", res.MatchedBy, res.HistoryLen)
	}
	if res.HistoryLen != 4 {
		t.Fatalf("expected HistoryLen=4 (history plus drifted snapshot), got %d", res.HistoryLen)
	}
	if res.MatchedBy != "explicit_prefix_4" {
		t.Fatalf("expected explicit_prefix_4, got %q", res.MatchedBy)
	}
	if res.ConversationID != "conv-snap" {
		t.Fatalf("expected conversation reuse, got %q", res.ConversationID)
	}
}

// TestMessagesEqualToleratesRuntimeSnapshotDrift verifies the message-level
// equality rule: two messages whose role matches but whose text differs only by
// the drifting runtime-context snapshot are session-equivalent.
func TestMessagesEqualToleratesRuntimeSnapshotDrift(t *testing.T) {
	snapshotA := oaiMsg{Role: "user", Content: "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nPolicy A."}
	snapshotB := oaiMsg{Role: "user", Content: "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nPolicy B: approvals disabled."}
	if !messagesEqual(snapshotA, snapshotB) {
		t.Fatal("two runtime-context snapshots with drifting content must compare equal")
	}
	// A real conversation message must still differ from a snapshot.
	if messagesEqual(snapshotA, oaiMsg{Role: "user", Content: "Current runtime context rocks"}) {
		t.Fatal("a plain user message must not compare equal to a snapshot")
	}
	if messagesEqual(snapshotA, oaiMsg{Role: "assistant", Content: snapshotB.Content}) {
		t.Fatal("role mismatch must still fail even when both sides look like snapshots")
	}
}

// TestDSHToolRoundThinkingReplay reproduces the observed DSH tool-round replay
// shape: the harness carries the previous turn's tool calls and wraps the
// reasoning summary as an assistant output_text message with <thinking> tags.
// bindConversation stores the assistant reply with Content=res.Reasoning when
// the tool round produced no visible text (res.Text == ""), and messagesEqual
// strips the <thinking> wrapper, so the stored history must still be a prefix
// of the next request instead of a context reset.
func TestDSHToolRoundThinkingReplay(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(sessionHeaderName, "dsh-tool-round")

	systemMsg := oaiMsg{Role: "system", Content: "You are an AI agent powered by DeepSeek Harness."}
	userMsg1 := oaiMsg{Role: "user", Content: "阅读项目代码并给出修改方案"}
	thinking := "**Selecting next tool**\nIt seems like the next step is to use the \"create_goal\" tool."

	// First turn (tool round): bind client messages plus an assistant reply
	// whose Content is the reasoning (res.Text was empty for the tool round).
	bindLikeServer(t, sr, "dsh-tool-round", "conv-tool", "account-a",
		[]oaiMsg{systemMsg, userMsg1},
		thinking)

	// Second turn: the harness replays the thinking wrapped in <thinking> tags
	// as an assistant output_text item, plus the function call and its result.
	res := sr.Resolve(req, &oaiReq{Messages: []oaiMsg{
		systemMsg,
		userMsg1,
		{Role: "assistant", Content: []any{map[string]any{"type": "output_text", "text": "<thinking>" + thinking + "</thinking>"}}},
		{Role: "assistant", Content: "第一轮工具调用"},
		{Role: "user", Content: "工具结果"},
	}})
	if res.IsNew {
		t.Fatal("tool-round replay must reuse the session bound by session id")
	}
	if res.ResetUpstream {
		t.Fatalf("thinking replay must not reset the upstream conversation; matched=%q HistoryLen=%d", res.MatchedBy, res.HistoryLen)
	}
	if res.HistoryLen != 3 {
		t.Fatalf("expected HistoryLen=3 (system,user,thinking), got %d", res.HistoryLen)
	}
	if res.MatchedBy != "explicit_prefix_3" {
		t.Fatalf("expected explicit_prefix_3, got %q", res.MatchedBy)
	}
}

// TestMessagesEqualStripsThinkingWrapper verifies that DSH's <thinking> wrapper
// on replayed reasoning is stripped before comparing assistant messages, while
// genuinely different assistant content still fails.
func TestMessagesEqualStripsThinkingWrapper(t *testing.T) {
	plain := oaiMsg{Role: "assistant", Content: "**Selecting next tool**\nIt seems like the next step is to use the create_goal tool."}
	wrapped := oaiMsg{Role: "assistant", Content: []any{map[string]any{"type": "output_text", "text": "<thinking>**Selecting next tool**\nIt seems like the next step is to use the create_goal tool.</thinking>"}}}
	if !messagesEqual(plain, wrapped) {
		t.Fatal("assistant reasoning with <thinking> wrapper must compare equal to the stored plain reasoning")
	}
	if messagesEqual(plain, oaiMsg{Role: "assistant", Content: "**Different answer**\nSomething else entirely."}) {
		t.Fatal("different assistant content must still fail")
	}
}
