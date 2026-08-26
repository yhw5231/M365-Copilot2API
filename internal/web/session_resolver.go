package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// sessionBinding 璁板綍涓€娆″唴瀹归敭澶嶇敤鐨勪細璇濄€侷dentity 瀛楁锛圛P/user锛変粎浣?
// 璇婃柇鍏冩暟鎹繚鐣欙紝鍖归厤鍒ゅ畾鍙緷璧栦笂涓嬫枃鍐呭锛岃 Resolve 鐨勫唴瀹归敭閫昏緫銆?
type sessionBinding struct {
	SessionID      string    `json:"sessionId"`
	ConversationID string    `json:"conversationId"`
	AccountID      string    `json:"accountId"`
	CreatedAt      time.Time `json:"createdAt"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
	APIKey         string    `json:"apiKey,omitempty"`
	ExplicitID     string    `json:"explicitId,omitempty"` // stable downstream session ID; kept separate from the upstream SessionID
	IPFingerprint  string    `json:"ipFingerprint,omitempty"`
	UserField      string    `json:"userField,omitempty"`
	ContextFinger  string    `json:"contextFinger,omitempty"`
	// ContextHistory 鎸佷箙鍖栦繚瀛樻渶杩戜竴娆″崗璁殑瀹屾暣娑堟伅锛屼緵閲嶅惎鍚庣户缁仛
	// 鍐呭鍓嶇紑鍖归厤锛岄伩鍏嶈繘绋嬮噸鍚鑷存墍鏈変細璇濋敭鍏ㄩ儴澶辨晥銆?
	ContextHistory []oaiMsg `json:"contextHistory,omitempty"`
	// Task is the persistent server-side task ledger for this downstream
	// session. It survives context compaction and account switches, so a long
	// task never restarts from scratch.
	Task *taskLedger `json:"task,omitempty"`
}

type sessionResolver struct {
	mu          sync.Mutex
	path        string
	sessions    map[string]sessionBinding
	byExplicit  map[string]string // explicitID -> sessionID
	byUserField map[string]string // userField -> sessionID
	byIPFinger  map[string]string // ipFingerprint -> sessionID
	byContext   map[string]string // contextFingerprint -> sessionID
	ttl         time.Duration
	contextTTL  time.Duration
	maxSessions int
	persist     *persistStore
}

const defaultMaxSessions = 1000

func openSessionResolver() *sessionResolver {
	// 闂茬疆 2 灏忔椂鍗宠涓鸿繃鏈燂紙鐢ㄦ埛锛? 灏忔椂涓嶆椿璺冨凡缁忕畻涔咃級銆備細璇濊繃鏈熷悗
	// 浠?sessions.json 鍓旈櫎锛屼簯绔璇濅氦缁?auto_cleanup 鎸夌浉鍚岀獥鍙ｅ洖鏀躲€?
	ttl := 2 * time.Hour
	if v := os.Getenv("M365_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			ttl = d
		}
	}
	contextTTL := 2 * time.Hour
	if v := os.Getenv("M365_CONTEXT_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			contextTTL = d
		}
	}
	path := configuredPath("M365_SESSION_CACHE", "sessions.json")
	sr := &sessionResolver{
		path:        path,
		sessions:    map[string]sessionBinding{},
		byExplicit:  map[string]string{},
		byUserField: map[string]string{},
		byIPFinger:  map[string]string{},
		byContext:   map[string]string{},
		ttl:         ttl,
		contextTTL:  contextTTL,
		maxSessions: defaultMaxSessions,
	}
	sr.persist = &persistStore{flush: sr.flush}
	sr.loadLocked()
	return sr
}

func (sr *sessionResolver) loadLocked() {
	if b, err := os.ReadFile(sr.path); err == nil {
		var list []sessionBinding
		if err := json.Unmarshal(b, &list); err == nil {
			now := time.Now().UTC()
			for _, s := range list {
				if now.Sub(s.LastUsedAt) > sr.ttl {
					continue
				}
				sr.reindexLocked(s)
			}
		}
	}
}

// flush 在锁内生成快照，锁外写盘。
func (sr *sessionResolver) flush() error {
	sr.mu.Lock()
	list := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		list = append(list, s)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	sr.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(sr.path, b, 0o600)
}

func (sr *sessionResolver) reindexLocked(s sessionBinding) {
	sr.sessions[s.SessionID] = s
	if s.ExplicitID != "" {
		sr.byExplicit[s.ExplicitID] = s.SessionID
	}
	if s.UserField != "" {
		sr.byUserField[s.UserField] = s.SessionID
	}
	if s.IPFingerprint != "" {
		sr.byIPFinger[s.IPFingerprint] = s.SessionID
	}
	if s.ContextFinger != "" {
		sr.byContext[s.ContextFinger] = s.SessionID
	}
}

func (sr *sessionResolver) evictLocked() {
	now := time.Now().UTC()
	for id, s := range sr.sessions {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			sr.dropLocked(id, s)
		}
	}
	if len(sr.sessions) > sr.maxSessions {
		// Bound memory by dropping the least recently used sessions.
		ids := make([]string, 0, len(sr.sessions))
		last := make(map[string]time.Time, len(sr.sessions))
		for id, s := range sr.sessions {
			ids = append(ids, id)
			last[id] = s.LastUsedAt
		}
		sort.Slice(ids, func(i, j int) bool { return last[ids[i]].Before(last[ids[j]]) })
		for _, id := range ids[:len(sr.sessions)-sr.maxSessions] {
			sr.dropLocked(id, sr.sessions[id])
		}
	}
}

func (sr *sessionResolver) dropLocked(id string, s sessionBinding) {
	delete(sr.sessions, id)
	if s.ExplicitID != "" && sr.byExplicit[s.ExplicitID] == id {
		delete(sr.byExplicit, s.ExplicitID)
	}
	if sr.byUserField[s.UserField] == id {
		delete(sr.byUserField, s.UserField)
	}
	if sr.byIPFinger[s.IPFingerprint] == id {
		delete(sr.byIPFinger, s.IPFingerprint)
	}
	if sr.byContext[s.ContextFinger] == id {
		delete(sr.byContext, s.ContextFinger)
	}
}

type ResolveResult struct {
	SessionID      string
	ConversationID string
	AccountID      string
	MatchedBy      string
	IsNew          bool
	// HistoryLen is the number of leading messages in the current request that
	// are already present in the upstream conversation. It is safe to use only
	// as the start index of body.Messages[HistoryLen:].
	HistoryLen int
	// ResetUpstream signals that the downstream session is still valid, but its
	// submitted context no longer extends the stored history. The caller must
	// start a fresh upstream conversation and send the complete current context.
	ResetUpstream bool
}

func clientIPFingerprint(r *http.Request) string {
	// Use the real client IP (proxy-aware, see clientIP) so that users behind
	// a reverse proxy are not all fingerprinted as the proxy address.
	host := clientIP(r)
	ua := r.Header.Get("User-Agent")
	data := host + "|" + ua
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func contextFingerprint(messages []oaiMsg) string {
	if len(messages) == 0 {
		return ""
	}
	var parts []string
	limit := len(messages)
	if limit > 3 {
		limit = 3
	}
	for i := len(messages) - limit; i < len(messages); i++ {
		m := messages[i]
		parts = append(parts, m.Role+":"+contentToString(m.Content))
	}
	data := strings.Join(parts, "||")
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func (sr *sessionResolver) Resolve(r *http.Request, body *oaiReq) ResolveResult {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	explicitID := r.Header.Get("X-M365-Session-Id")
	key := extractAPIKey(r)

	// 瀹㈡埛绔樉寮忔寚瀹氱殑浼氳瘽 ID 鏄渶楂樹紭鍏堢殑缁帴璇箟锛氫笉鍙備笌浠讳綍韬唤鍒ゅ畾锛?
	// 鐢辫皟鐢ㄦ柟涓诲姩鍐冲畾瑕佺户缁摢涓簯绔璇濄€?
	if explicitID != "" {
		resolveExplicit := func(sessID string, sess sessionBinding) ResolveResult {
			sess.LastUsedAt = time.Now().UTC()
			sr.sessions[sessID] = sess
			sr.persist.markDirty()

			result := ResolveResult{
				SessionID:      sess.SessionID,
				ConversationID: sess.ConversationID,
				AccountID:      sess.AccountID,
				MatchedBy:      "explicit",
				IsNew:          false,
			}
			if n := contextPrefixLen(sess.ContextHistory, body.Messages); n > 0 {
				result.HistoryLen = n
				result.MatchedBy = fmt.Sprintf("explicit_prefix_%d", n)
				return result
			}
			// A request containing only the current turn is a normal incremental
			// client mode, not evidence that the client compacted its context.
			if len(body.Messages) <= 1 {
				result.MatchedBy = "explicit_incremental"
				return result
			}
			// A multi-message request that no longer extends the stored history is
			// treated as a compacted/replaced context. Preserve the downstream ID,
			// but require the caller to create a fresh upstream conversation.
			result.MatchedBy = "explicit_context_reset"
			result.ResetUpstream = true
			return result
		}

		if sessID, ok := sr.byExplicit[explicitID]; ok {
			if sess, ok := sr.sessions[sessID]; ok && sessionOwnedBy(sess, key) {
				return resolveExplicit(sessID, sess)
			}
		}
		if sess, ok := sr.sessions[explicitID]; ok && sessionOwnedBy(sess, key) {
			return resolveExplicit(explicitID, sess)
		}
	}

	// 鍐呭閿細鍗忚娑堟伅鍚嶅簭鍒椾弗鏍肩瓑浜庢煇涓凡璁板綍浼氳瘽鐨勫巻鍙叉椂鐩存帴澶嶇敤杩欎釜
	// 浜戠瀵硅瘽锛屼絾鍙湪鍚屼竴 IP/UA 鎸囩汗涓嬶紝閬垮厤鐭秷鎭湪涓嶅悓鐢ㄦ埛闂翠簰绔?
	// HistoryLen 杩斿洖璇ュ墠缂€闀垮害锛屼笂灞傛嵁姝ゅ彧鍙戦€?messages[HistoryLen:] 澧為噺銆?
	ipFinger := clientIPFingerprint(r)
	if bestID, n := sr.matchContextLocked(ipFinger, key, body.Messages); bestID != "" {
		sess := sr.sessions[bestID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_prefix_%d", n),
			IsNew:          false,
			HistoryLen:     n,
		}
	}

	// 寮辩害鏉熷厹搴曪細鍐呭涓嶆瀯鎴愪弗鏍煎墠缂€锛屼絾涓庢煇涓巻鍙查珮搴︾浉浼硷紙濡傚鎴风
	// 鏈湴鎴柇浜嗗巻鍙诧級锛屼粛澶嶇敤璇ヤ細璇濄€傛鏃跺閲忚竟鐣屾湭鐭ワ紝涓婂眰鍙戦€佸叏閲忋€?
	suffixID, suffixN := sr.matchSuffixLocked(ipFinger, key, body.Messages)
	if suffixID != "" {
		sess := sr.sessions[suffixID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[suffixID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_suffix_%d", suffixN),
			IsNew:          false,
			HistoryLen:     suffixN,
		}
	}

	return ResolveResult{IsNew: true}
}

func (sr *sessionResolver) matchSuffixLocked(ipFinger, key string, messages []oaiMsg) (string, int) {
	if len(messages) < 2 {
		return "", 0
	}
	type match struct {
		id     string
		n      int
		recent time.Time
	}
	best := match{}
	minSuffix := 2
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		if !sessionOwnedBy(sess, key) {
			continue
		}
		hist := sess.ContextHistory
		if len(hist) < minSuffix {
			continue
		}
		n := suffixMatchLen(hist, messages)
		if n >= minSuffix && (n > best.n || (n == best.n && sess.LastUsedAt.After(best.recent))) {
			best = match{id: id, n: n, recent: sess.LastUsedAt}
		}
	}
	return best.id, best.n
}

func suffixMatchLen(hist, msgs []oaiMsg) int {
	maxN := len(hist)
	if maxN > len(msgs) {
		maxN = len(msgs)
	}
	n := 0
	for i := 1; i <= maxN; i++ {
		if messagesEqual(hist[len(hist)-i], msgs[len(msgs)-i]) {
			n = i
		} else {
			break
		}
	}
	return n
}

// matchContextLocked 浠庡叏閮ㄤ細璇濅腑鎵惧埌鍏?contextHistory 涓ユ牸浣滀负娑堟伅鍓嶇紑鐨?
// 閭ｄ釜浼氳瘽锛涘彧閫夊墠缂€鏈€闀跨殑涓€涓紝閬垮厤鐭墠缂€鍦ㄤ笉鍚屼細璇濋棿浜掓挒銆傝繑鍥?
// (sessionID, 鍖归厤鍒扮殑娑堟伅鏉℃暟)銆?
func (sr *sessionResolver) matchContextLocked(ipFinger, key string, messages []oaiMsg) (string, int) {
	if len(messages) == 0 {
		return "", 0
	}
	type match struct {
		id     string
		n      int
		recent time.Time
	}
	best := match{}
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		if !sessionOwnedBy(sess, key) {
			continue
		}
		n := contextPrefixLen(sess.ContextHistory, messages)
		if n >= 1 && (n > best.n || (n == best.n && sess.LastUsedAt.After(best.recent))) {
			best = match{id: id, n: n, recent: sess.LastUsedAt}
		}
	}
	return best.id, best.n
}

// contextPrefixLen 杩斿洖 hist 鏄惁涓ユ牸鏄?msgs 鐨勫墠缂€銆俬ist 涓虹┖鎴栦笉鏄墠缂€
// 鏃惰繑鍥?0锛涘懡涓椂杩斿洖 len(hist)锛屽嵆澧為噺鍙戦€佽捣鐐广€?
func contextPrefixLen(hist, msgs []oaiMsg) int {
	if len(hist) == 0 || len(msgs) < len(hist) {
		return 0
	}
	for i := range hist {
		if !messagesEqual(hist[i], msgs[i]) {
			return 0
		}
	}
	return len(hist)
}

// messagesEqual 鍒ゅ畾涓ゆ潯娑堟伅鍦ㄤ細璇濋敭鎰忎箟涓婄瓑浠凤細role 涓庢枃鏈唴瀹逛竴鑷淬€?
// 蹇界暐 tool_calls 鐨?ID 缁嗚妭锛堜細璇濋敭鍙叧蹇冨唴瀹瑰浣曡妯″瀷娑堝寲锛夈€?
func messagesEqual(a, b oaiMsg) bool {
	if a.Role != b.Role {
		return false
	}
	ta := contentToString(a.Content)
	tb := contentToString(b.Content)
	if ta != tb {
		return false
	}
	if (a.ToolCalls == nil) != (b.ToolCalls == nil) {
		return false
	}
	for i := range a.ToolCalls {
		if i >= len(b.ToolCalls) {
			return false
		}
		if toolCallEqual(a.ToolCalls[i], b.ToolCalls[i]) {
			continue
		}
		return false
	}
	return len(a.ToolCalls) == len(b.ToolCalls)
}

// toolCallEqual 比较 name 与 arguments，忽略 ID：同一段工具调用重放时
// ID 由客户端重新生成，不应影响会话键。
func toolCallEqual(x, y map[string]any) bool {
	xFunc, _ := x["function"].(map[string]any)
	yFunc, _ := y["function"].(map[string]any)
	xn, _ := xFunc["name"].(string)
	yn, _ := yFunc["name"].(string)
	if xn != yn {
		return false
	}
	xa, _ := xFunc["arguments"].(string)
	ya, _ := yFunc["arguments"].(string)
	return xa == ya
}

func (sr *sessionResolver) Bind(sessionID, conversationID, accountID string, body *oaiReq, assistantText string, r *http.Request) {
	sr.BindWithTask(sessionID, conversationID, accountID, body, assistantText, r, nil)
}

// BindWithTask is Bind with a persistent task ledger attached to the session.
// An existing binding keeps its ledger (the original goal must not be replaced
// by a later turn), while an explicit task replaces the stored one so the task
// state follows the session across account switches.
func (sr *sessionResolver) BindWithTask(sessionID, conversationID, accountID string, body *oaiReq, assistantText string, r *http.Request, task *taskLedger) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	now := time.Now().UTC()
	// Remove only known-bad assistant workspace/tool-availability claims before
	// calculating fingerprints or persisting history. User, system, developer,
	// and tool messages are preserved even when they discuss /mnt/data,
	// containers, sandboxes, or Windows execution.
	history := cleanWorkspaceToolMisjudgments(cloneMessages(body.Messages), toolMapsFromChatHubTools(body.Tools))
	if strings.TrimSpace(assistantText) != "" && !isWorkspaceToolMisjudgmentForTools(assistantText, toolMapsFromChatHubTools(body.Tools)) {
		history = append(history, oaiMsg{Role: "assistant", Content: assistantText})
	}
	explicitID := strings.TrimSpace(r.Header.Get("X-M365-Session-Id"))
	key := extractAPIKey(r)
	// The caller-provided session ID is the stable downstream identity. Keep it
	// separate from the upstream session ID returned by ChatHub so two callers
	// can never collapse onto the same cloud conversation accidentally.
	if explicitID != "" {
		if mappedID, ok := sr.byExplicit[explicitID]; ok {
			sessionID = mappedID
		} else {
			sessionID = uuid.NewString()
			sr.byExplicit[explicitID] = sessionID
		}
	} else if sessionID == "" {
		sessionID = uuid.NewString()
	}
	// 同一云端对话只保留一条记录：内容键命中后增量轮次更新已存在会话，
	// 而不是每次 Bind 都新建一条，避免 sessions.json 膨胀。
	if sessionID != "" {
		if sess, ok := sr.sessions[sessionID]; ok {
			if task != nil {
				sess.Task = task
			}
			sess.ConversationID = conversationID
			sess.AccountID = accountID
			sess.APIKey = key
			sess.ExplicitID = explicitID
			sess.LastUsedAt = now
			sess.UserField = body.User
			sess.IPFingerprint = clientIPFingerprint(r)
			sess.ContextFinger = contextFingerprint(history)
			sess.ContextHistory = history
			sr.sessions[sessionID] = sess
			sr.reindexLocked(sess)
			sr.persist.markDirty()
			return
		}
	}
	if sessionID == "" {
		for sid, sess := range sr.sessions {
			if sess.ConversationID == conversationID {
				if task != nil {
					sess.Task = task
				}
				sess.LastUsedAt = now
				sess.AccountID = accountID
				sess.APIKey = key
				sess.UserField = body.User
				sess.IPFingerprint = clientIPFingerprint(r)
				sess.ContextFinger = contextFingerprint(history)
				sess.ContextHistory = history
				sr.sessions[sid] = sess
				sr.reindexLocked(sess)
				sr.persist.markDirty()
				return
			}
		}
		sessionID = uuid.NewString()
	}

	sess := sessionBinding{
		SessionID:      sessionID,
		ConversationID: conversationID,
		AccountID:      accountID,
		CreatedAt:      now,
		LastUsedAt:     now,
		APIKey:         key,
		ExplicitID:     explicitID,
		IPFingerprint:  clientIPFingerprint(r),
		UserField:      body.User,
		ContextFinger:  contextFingerprint(history),
		ContextHistory: history,
		Task:           task,
	}

	sr.reindexLocked(sess)
	sr.persist.markDirty()
}

// ResolveTaskLedger returns the task ledger for the downstream session matching
// the request, using the same content-key matching as Resolve (prefix first,
// then suffix to tolerate local context truncation). This lets a client that
// never sends an explicit session id (e.g. a plain OpenAI chat client) keep its
// task ledger — and therefore its goal state — across rounds, instead of
// rebuilding a fresh active ledger every request. It is a read-only fallback and
// does not mutate the session binding.
func (sr *sessionResolver) ResolveTaskLedger(r *http.Request, body *oaiReq) *taskLedger {
	if sr == nil {
		return nil
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	key := extractAPIKey(r)
	ipFinger := clientIPFingerprint(r)
	// Strict prefix match first (the request extends the stored history).
	if bestID, _ := sr.matchContextLocked(ipFinger, key, body.Messages); bestID != "" {
		if sess, ok := sr.sessions[bestID]; ok {
			return sess.Task
		}
	}
	// Suffix match tolerates the client dropping old history.
	if suffixID, _ := sr.matchSuffixLocked(ipFinger, key, body.Messages); suffixID != "" {
		if sess, ok := sr.sessions[suffixID]; ok {
			return sess.Task
		}
	}
	return nil
}

// SetTask attaches a task ledger to an existing session binding. It is a no-op
// when the session is unknown (the ledger is then attached by the next bind).
func (sr *sessionResolver) SetTask(sessionID string, task *taskLedger) {
	if sr == nil || sessionID == "" {
		return
	}
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if sess, ok := sr.sessions[sessionID]; ok {
		sess.Task = task
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[sessionID] = sess
		sr.persist.markDirty()
	}
}

// sessionOwnedBy reports whether a session recorded under one API key may be
// reused by the current request. Legacy sessions persisted before key binding
// (empty APIKey) are claimable by the first caller that binds them; newer
// sessions are strictly bound to their creating key so tenants never share
// context through the resolver.
func sessionOwnedBy(s sessionBinding, key string) bool {
	return s.APIKey == "" || s.APIKey == key
}

func (sr *sessionResolver) GetSession(sessionID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	s, ok := sr.sessions[sessionID]
	return s, ok
}

func (sr *sessionResolver) GetConversation(conversationID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for _, session := range sr.sessions {
		if session.ConversationID == conversationID {
			session.ContextHistory = cloneMessages(session.ContextHistory)
			return session, true
		}
	}
	return sessionBinding{}, false
}

func (sr *sessionResolver) ListSessions() []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	return out
}

func (sr *sessionResolver) DeleteSession(sessionID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	s, ok := sr.sessions[sessionID]
	if !ok {
		return false
	}
	sr.dropLocked(sessionID, s)
	if s.UserField != "" {
		delete(sr.byUserField, s.UserField)
	}
	if s.IPFingerprint != "" {
		delete(sr.byIPFinger, s.IPFingerprint)
	}
	if s.ContextFinger != "" {
		delete(sr.byContext, s.ContextFinger)
	}
	sr.persist.markDirty()
	return true
}

// UnbindByConversation drops every session bound to the given conversation.
// Called after an automatic cleanup deletes the cloud conversation, so the
// anti-CrossID resolver never reuses a dead conversation.
func (sr *sessionResolver) UnbindByConversation(conversationID string) int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	removed := 0
	for sid, s := range sr.sessions {
		if s.ConversationID != conversationID {
			continue
		}
		sr.dropLocked(sid, s)
		removed++
	}
	if removed > 0 {
		sr.persist.markDirty()
	}
	return removed
}

func cloneMessages(msgs []oaiMsg) []oaiMsg {
	if len(msgs) > 512 {
		msgs = msgs[len(msgs)-512:]
	}
	out := make([]oaiMsg, len(msgs))
	copy(out, msgs)
	return out
}
