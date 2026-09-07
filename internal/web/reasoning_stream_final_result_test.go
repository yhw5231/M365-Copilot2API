package web

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"

	"github.com/gorilla/websocket"
)

// unseenSuffixOf is the guard behind the reasoning-stream final-result
// recovery: it must never duplicate delivered text and must never append
// after a non-prefix rewrite.
func TestUnseenSuffixOf(t *testing.T) {
	if s, ok := unseenSuffixOf("full answer", ""); !ok || s != "full answer" {
		t.Fatalf("empty delivered should return the whole text, got %q ok=%v", s, ok)
	}
	if s, ok := unseenSuffixOf("prefix and tail", "prefix"); !ok || s != " and tail" {
		t.Fatalf("prefix superset should return the tail, got %q ok=%v", s, ok)
	}
	if _, ok := unseenSuffixOf("same text", "same text"); ok {
		t.Fatal("fully delivered text must not re-emit")
	}
	if _, ok := unseenSuffixOf("rewritten answer", "delivered prefix"); ok {
		t.Fatal("non-prefix rewrite must not append")
	}
	if _, ok := unseenSuffixOf("short", "longer delivered"); ok {
		t.Fatal("shorter full than delivered must not append")
	}
	if _, ok := unseenSuffixOf("", ""); ok {
		t.Fatal("empty full must not emit")
	}
}

// signalRRecord is the SignalR JSON-protocol record separator.
const signalRRecord = "\x1e"

// fakeChatHubUpstream is a minimal SignalR ChatHub: handshake, one chat
// invocation, then a scripted list of server frames.
func fakeChatHubUpstream(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil { // SignalR handshake
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{}"+signalRRecord))
		if _, _, err := conn.ReadMessage(); err != nil { // chat invocation
			return
		}
		for _, f := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(f+signalRRecord)); err != nil {
				return
			}
		}
		// Close right after the frames: the queued frames are still delivered,
		// and the client's read goroutine exits immediately instead of blocking
		// the conn-pool detach wait for its full timeout.
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func newReasoningStreamTestServer(t *testing.T, frames []string) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_SESSION_CACHE", filepath.Join(dir, "conversation-sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	ws := strings.Replace(fakeChatHubUpstream(t, frames).URL, "http://", "ws://", 1) + "/m365Copilot/Chathub"
	t.Setenv("M365_CHATHUB_WS_BASE", ws)

	store, err := auth.OpenStore(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.Upsert(auth.TokenSet{
		HomeOID:  "acc-reasoning-1",
		TenantID: "tid-1",
		Email:    "test@example.com",
		AccessToken: "tok",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("upsert account: %v", err)
	}

	c := &chathub.Client{
		HTTPHeader: make(http.Header),
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		Dialer:     &websocket.Dialer{HandshakeTimeout: 15 * time.Second},
		Pool:       chathub.NewConnPool(&websocket.Dialer{HandshakeTimeout: 15 * time.Second}, make(http.Header)),
	}
	c.HTTPHeader.Set("Origin", "https://m365.cloud.microsoft")
	c.HTTPHeader.Set("User-Agent", "reasoning-stream-test")
	return &Server{
		tokens:              store,
		accountPool:         newAccountHealth(),
		accountConcurrency:  newAccountConcurrency(),
		gatewayConcurrency:  newGatewayConcurrency(),
		sessionConcurrency:  newSessionConcurrency(),
		chat:                c,
		sessions:            openSessionStore(),
		userSessions:        openUserSessionStore(30 * time.Minute),
		sessionResolver:     openSessionResolver(),
		conversationManager: openConversationManager(),
		settings:            openSettingsStore(),
		responseMessages:    map[string]map[string]respHistory{},
		usage:               &usageLog{persist: &persistStore{flush: func() error { return nil }}},
	}
}

func streamChatRequest(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-reasoning-stream-test")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.openaiChat(w, req)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("openaiChat did not return within 60s")
	}
	return w
}

func contentDeltas(t *testing.T, raw string) string {
	t.Helper()
	var joined strings.Builder
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if c, ok := delta["content"].(string); ok {
			joined.WriteString(c)
		}
	}
	return joined.String()
}

// updateFrame builds a SignalR update frame carrying one bot text message.
func updateFrame(text string) string {
	return `{"type":1,"target":"update","arguments":[{"messages":[{"author":"bot","messageType":"","text":` + jsonEsc(text) + `}]}]}`
}

// resultFrame builds the SignalR completion frame whose result.message is the
// authoritative final answer.
func resultFrame(message string) string {
	return `{"type":2,"invocationId":"1","item":{"result":{"value":"Success","message":` + jsonEsc(message) + `}}}`
}

// completionFrame closes the SignalR stream.
const completionFrame = `{"type":3}`

// TestReasoningStreamDeliversFinalResultOnlyText reproduces the production
// empty_upstream_response failure: ChatHub placed the whole answer only in the
// final result frame (zero text updates on the stream), so no inline delta was
// ever forwarded. The reasoning-stream tail must recover the authoritative
// res.Text instead of closing a "successful" blank stream.
func TestReasoningStreamDeliversFinalResultOnlyText(t *testing.T) {
	answer := "**计算：**\n\n52 × 64 + 13  \n= 3328 + 13  \n= **3341**\n\n**最终答案：3341**  \n检测编号：406095063"
	frames := []string{resultFrame(answer), completionFrame}
	s := newReasoningStreamTestServer(t, frames)
	w := streamChatRequest(t, s, `{"model":"gpt-5.6","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"52 × 64 + 13 = ?"}]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := contentDeltas(t, w.Body.String())
	if !strings.Contains(got, "3341") {
		t.Fatalf("stream must carry the final-result-only answer, got content=%q\nraw=%s", got, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("stream must end with a stop finish chunk, raw=%s", w.Body.String())
	}
}

// TestReasoningStreamNoDuplicateWhenTextStreamed guards the recovery path
// against double emission: when the text already streamed inline, the
// authoritative completion must not be appended a second time.
func TestReasoningStreamNoDuplicateWhenTextStreamed(t *testing.T) {
	answer := "你好，今天天气非常不错，适合出门散步。"
	frames := []string{updateFrame(answer), resultFrame(answer), completionFrame}
	s := newReasoningStreamTestServer(t, frames)
	w := streamChatRequest(t, s, `{"model":"gpt-5.6","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	got := contentDeltas(t, w.Body.String())
	if strings.Count(got, "今天天气非常不错") != 1 {
		t.Fatalf("answer must appear exactly once, got %d times: %q", strings.Count(got, "今天天气非常不错"), got)
	}
}

// TestReasoningStreamRecoversTruncatedPrefixTail covers the mid-stream rewrite
// case: the streamed prefix was delivered inline, but the authoritative
// completion extends it. Only the unseen tail may be appended.
func TestReasoningStreamRecoversTruncatedPrefixTail(t *testing.T) {
	streamed := "答案的开头部分已经流出，"
	full := streamed + "而这一段结尾是重写后才出现的。"
	frames := []string{updateFrame(streamed), resultFrame(full), completionFrame}
	s := newReasoningStreamTestServer(t, frames)
	w := streamChatRequest(t, s, `{"model":"gpt-5.6","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)

	got := contentDeltas(t, w.Body.String())
	if !strings.Contains(got, "而这一段结尾是重写后才出现的") {
		t.Fatalf("unseen tail must be recovered, got content=%q", got)
	}
	if strings.Count(got, streamed) != 1 {
		t.Fatalf("streamed prefix must appear exactly once, got %q", got)
	}
}

func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
