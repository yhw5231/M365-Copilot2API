package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
)

func (s *Server) conversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	jsonOut(w, map[string]any{"conversations": s.sessions.list()})
}

func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || body.ID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	s.conversationManager.Delete(body.ID)
	s.sessions.delete(body.ID)
	jsonOut(w, map[string]string{"status": "deleted"})
}

func (s *Server) conversationCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body struct {
		Mode  string `json:"mode"`
		KeepN int    `json:"keep_n"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) == nil {
		if body.Mode != "" {
			s.conversationManager.SetMode(ConversationCleanupMode(body.Mode))
		}
	}
	cleaned := s.conversationManager.Cleanup()
	jsonOut(w, map[string]any{
		"status":    "cleaned",
		"mode":      string(s.conversationManager.Mode()),
		"deleted":   cleaned,
		"remaining": len(s.conversationManager.List()),
	})
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// Only return a sanitized summary. Full contextHistory contains the
		// conversation payload and must never leave the gateway through a
		// plain API-key endpoint.
		sessions := s.sessionResolver.ListSessions()
		data := make([]map[string]any, 0, len(sessions))
		for _, sess := range sessions {
			data = append(data, map[string]any{
				"id":              sess.SessionID,
				"conversation_id": sess.ConversationID,
				"account_id":      sess.AccountID,
				"created_at":      sess.CreatedAt.Unix(),
				"last_used_at":    sess.LastUsedAt.Unix(),
				"message_count":   len(sess.ContextHistory),
			})
		}
		jsonOut(w, map[string]any{
			"object": "list",
			"data":   data,
		})
	case http.MethodPost:
		var body struct {
			SessionID string `json:"session_id"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body)
		sess, ok := s.sessionResolver.GetSession(body.SessionID)
		if !ok {
			jsonOut(w, map[string]any{
				"object":     "session",
				"id":         body.SessionID,
				"created":    time.Now().Unix(),
				"expires_in": 1800,
				"status":     "created",
			})
			return
		}
		jsonOut(w, map[string]any{
			"object":          "session",
			"id":              sess.SessionID,
			"conversation_id": sess.ConversationID,
			"created":         sess.CreatedAt.Unix(),
			"status":          "active",
		})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *Server) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	stats := cacheStats.GetStats()
	jsonOut(w, map[string]any{
		"object":     "cache_stats",
		"stats":      stats,
		"conv_cache": s.convCache.Stats(),
	})
}

func (s *Server) handleCacheStatsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	cacheStats.Reset()
	jsonOut(w, map[string]any{"status": "reset"})
}

func (s *Server) handleM365Conversations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if m365CloudClient == nil && len(s.sessionResolver.ListSessions()) == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	rows := make(map[string]map[string]any)
	var cloudErr error
	if m365CloudClient != nil {
		var chats []map[string]any
		chats, cloudErr = m365CloudClient.ListConversations()
		for _, chat := range chats {
			conversationID, _ := chat["conversationId"].(string)
			if conversationID != "" {
				rows[conversationID] = chat
			}
		}
	}
	if cloudErr != nil && len(s.sessionResolver.ListSessions()) == 0 {
		err := cloudErr
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	for _, session := range s.sessionResolver.ListSessions() {
		row, ok := rows[session.ConversationID]
		if !ok {
			row = map[string]any{}
			rows[session.ConversationID] = row
		}
		row["conversationId"] = session.ConversationID
		row["sessionId"] = session.SessionID
		row["accountId"] = session.AccountID
		row["createTimeUtc"] = session.CreatedAt.UnixMilli()
		row["updateTimeUtc"] = session.LastUsedAt.UnixMilli()
		row["messageCount"] = len(session.ContextHistory)
		row["historyAvailable"] = len(session.ContextHistory) > 0
		row["source"] = "gateway"
		if account, found := s.tokens.Get(session.AccountID); found {
			row["accountEmail"] = account.Email
		}
		if name, _ := row["chatName"].(string); strings.TrimSpace(name) == "" {
			row["chatName"] = conversationTitle(session.ContextHistory)
		} else {
			// Remote-supplied titles pass through the same sanitizer so the
			// admin console never receives raw control characters.
			row["chatName"] = sanitizeChatName(name)
		}
	}

	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, row)
	}
	sort.Slice(data, func(i, j int) bool {
		return conversationTimestamp(data[i]) > conversationTimestamp(data[j])
	})
	response := map[string]any{"object": "list", "data": data, "count": len(data)}
	if cloudErr != nil {
		response["warning"] = cloudErr.Error()
	}
	jsonOut(w, response)
}

// conversationTimestamp prefers the update time so a touched conversation
// bubbles to the top of the list.
func conversationTimestamp(row map[string]any) int64 {
	updated, ok := row["updateTimeUtc"].(int64)
	if ok {
		return updated
	}
	if f, ok := row["updateTimeUtc"].(float64); ok {
		return int64(f)
	}
	created, ok := row["createTimeUtc"].(int64)
	if ok {
		return created
	}
	if f, ok := row["createTimeUtc"].(float64); ok {
		return int64(f)
	}
	return 0
}

// handleM365ConversationDetail returns the locally tracked context history for
// one cloud conversation so the admin UI can show a per-conversation detail
// view without re-fetching remote chats (upstream PR #24).
func (s *Server) handleM365ConversationDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	conversationID := strings.TrimSpace(r.URL.Query().Get("id"))
	if conversationID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "conversation id is required")
		return
	}
	session, found := s.sessionResolver.GetConversation(conversationID)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "conversation_not_found", "conversation history is not available")
		return
	}
	accountEmail := ""
	if account, ok := s.tokens.Get(session.AccountID); ok {
		accountEmail = account.Email
	}
	jsonOut(w, map[string]any{
		"object":         "conversation",
		"conversationId": session.ConversationID,
		"sessionId":      session.SessionID,
		"accountId":      session.AccountID,
		"accountEmail":   accountEmail,
		"chatName":       conversationTitle(session.ContextHistory),
		"createdAt":      session.CreatedAt,
		"updatedAt":      session.LastUsedAt,
		"messageCount":   len(session.ContextHistory),
		"messages":       session.ContextHistory,
	})
}

// conversationTitle derives a short human title from the first user message.
// The output is length-bounded (40 runes) and free of control characters so
// the admin console can render it as a plain DOM text node without injection.
func conversationTitle(messages []oaiMsg) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		text := strings.TrimSpace(contentToString(message.Content))
		text = strings.Join(strings.Fields(text), " ")
		if text == "" {
			continue
		}
		runes := []rune(stripControlChars(text))
		if len(runes) > 40 {
			return string(runes[:40]) + "…"
		}
		return string(runes)
	}
	return "未命名会话"
}

// sanitizeChatName cleans a chat name coming from a remote source (cloud
// conversation list) before it reaches the admin console: control characters
// are removed and the length is capped.
func sanitizeChatName(name string) string {
	name = stripControlChars(strings.TrimSpace(name))
	runes := []rune(name)
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return name
}

// stripControlChars removes C0/C1 control characters (which can break out of
// DOM text nodes or log lines) while collapsing common whitespace to a space.
func stripControlChars(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		if r == 0x2028 || r == 0x2029 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func (s *Server) handleM365Delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if m365CloudClient == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || body.ConversationID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if err := m365CloudClient.DeleteConversation(body.ConversationID); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	s.dropConversation(body.ConversationID)
	jsonOut(w, map[string]any{"status": "deleted", "conversation_id": body.ConversationID})
}

func (s *Server) handleM365Cleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if m365CloudClient == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "m365_not_configured", "M365 cloud client not configured. Please add an M365 account first via PKCE authorization.")
		return
	}
	var body struct {
		MaxAgeHours int `json:"max_age_hours"`
		KeepN       int `json:"keep_n"`
	}
	json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body)

	maxAge := time.Duration(body.MaxAgeHours) * time.Hour
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	keepN := body.KeepN
	if keepN <= 0 {
		keepN = 5
	}

	deleted, err := m365CloudClient.CleanupOldConversations(maxAge, keepN)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "m365_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "cleaned", "deleted": deleted})
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	if sessionID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "session_id required")
		return
	}
	if s.sessionResolver.DeleteSession(sessionID) {
		jsonOut(w, map[string]any{"status": "deleted", "session_id": sessionID})
	} else {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "session not found")
	}
}

type conversationWhitelistRequest struct {
	ConversationID string `json:"conversation_id"`
	Add            bool   `json:"add"`
}

func (s *Server) conversationWhitelist(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body conversationWhitelistRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body) != nil || body.ConversationID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if body.Add {
		s.conversationManager.Whitelist(body.ConversationID)
	} else {
		s.conversationManager.Unwhitelist(body.ConversationID)
	}
	jsonOut(w, map[string]any{"status": "updated", "conversation_id": body.ConversationID, "whitelisted": body.Add})
}
