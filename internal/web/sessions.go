package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

type conversation struct {
	ID             string    `json:"id"`
	AccountID      string    `json:"accountId"`
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	Title          string    `json:"title,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type sessionStore struct {
	mu         sync.Mutex
	path       string
	data       map[string]conversation
	maxEntries int
	persist    *persistStore
}

// Capacity and key-sanity limits for the session stores (P2 hardening):
// bounded memory, no unbounded key growth, and no control characters that
// could corrupt the JSON cache file or admin console output.
const (
	sessionStoreMaxEntries = 10000
	userSessionMaxEntries  = 4096
	maxSessionKeyLength    = 256
)

// validSessionKey rejects keys that are empty, overlong, or carry control
// characters. It is used for sessionKey/user/ID keys entering the stores.
func validSessionKey(k string) bool {
	if k == "" || len(k) > maxSessionKeyLength {
		return false
	}
	for _, r := range k {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func openSessionStore() *sessionStore {
	path := configuredPath("M365_CONVERSATION_SESSION_CACHE", "conversation-sessions.json")
	s := &sessionStore{path: path, data: map[string]conversation{}, maxEntries: sessionStoreMaxEntries}
	s.persist = &persistStore{flush: s.flush}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	s.evictLocked()
	return s
}

// evictLocked drops the least recently updated entries beyond maxEntries.
func (s *sessionStore) evictLocked() {
	if len(s.data) <= s.maxEntries {
		return
	}
	for len(s.data) > s.maxEntries {
		var oldestID string
		var oldest time.Time
		for id, v := range s.data {
			if oldestID == "" || v.UpdatedAt.Before(oldest) {
				oldestID, oldest = id, v.UpdatedAt
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.data, oldestID)
	}
}

// flush 在锁内生成快照，锁外写盘。
func (s *sessionStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func (s *sessionStore) list() []conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]conversation, 0, len(s.data))
	for _, v := range s.data {
		out = append(out, v)
	}
	return out
}

func (s *sessionStore) get(id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok
}

func (s *sessionStore) upsert(v conversation) conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	if !validSessionKey(v.ID) {
		return conversation{}
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	s.data[v.ID] = v
	s.evictLocked()
	s.persist.markDirty()
	return v
}

func (s *sessionStore) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; !ok {
		return false
	}
	delete(s.data, id)
	s.persist.markDirty()
	return true
}

type userSession struct {
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	AccountID      string    `json:"accountId"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
}

type userSessionStore struct {
	mu         sync.Mutex
	path       string
	data       map[string]userSession
	ttl        time.Duration
	maxEntries int
	persist    *persistStore
}

func openUserSessionStore(ttl time.Duration) *userSessionStore {
	path := configuredPath("M365_USER_SESSION_CACHE", "user-sessions.json")
	s := &userSessionStore{path: path, data: map[string]userSession{}, ttl: ttl, maxEntries: userSessionMaxEntries}
	s.persist = &persistStore{flush: s.flush}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.data)
	}
	s.evictLocked()
	return s
}

// flush 在锁内生成快照，锁外写盘。
func (s *userSessionStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func (s *userSessionStore) evictLocked() {
	if s.ttl <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.ttl)
	for k, v := range s.data {
		if v.LastUsedAt.Before(cutoff) {
			delete(s.data, k)
		}
	}
	// Bound memory: drop the least recently used entries beyond maxEntries.
	for len(s.data) > s.maxEntries {
		var oldestKey string
		var oldest time.Time
		for k, v := range s.data {
			if oldestKey == "" || v.LastUsedAt.Before(oldest) {
				oldestKey, oldest = k, v.LastUsedAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.data, oldestKey)
	}
}

func (s *userSessionStore) Get(user string) (userSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	v, ok := s.data[user]
	if ok {
		v.LastUsedAt = time.Now().UTC()
		s.data[user] = v
		s.persist.markDirty()
	}
	return v, ok
}

func (s *userSessionStore) Put(user, conversationID, sessionID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validSessionKey(user) {
		return
	}
	s.data[user] = userSession{
		ConversationID: conversationID,
		SessionID:      sessionID,
		AccountID:      accountID,
		LastUsedAt:     time.Now().UTC(),
	}
	s.evictLocked()
	s.persist.markDirty()
}

func (s *userSessionStore) Delete(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, user)
	s.persist.markDirty()
}

// ActiveConversations returns conversation IDs whose owning user used the
// session within the given window. The auto-cleanup skips these so a user's
// in-flight conversation is never removed while still in use.
func (s *userSessionStore) ActiveConversations(window time.Duration) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().UTC().Add(-window)
	out := map[string]bool{}
	for _, v := range s.data {
		if v.LastUsedAt.After(cutoff) {
			out[v.ConversationID] = true
		}
	}
	return out
}
