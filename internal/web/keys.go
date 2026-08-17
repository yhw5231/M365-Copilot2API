package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type apiKeyRecord struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	Hash   string `json:"hash"`
	// LegacyHash keeps the hash of a key that was created during the brief
	// hash-only era (plaintext never stored, so it cannot be re-displayed).
	// On migration the record is rotated to a fresh plaintext key and the old
	// hash is preserved here so existing clients keep working until the
	// operator deletes the record.
	LegacyHash string     `json:"legacyHash,omitempty"`
	Raw        string     `json:"raw,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Revoked    bool       `json:"revoked"`
}
type apiKeyStore struct {
	mu      sync.Mutex
	Path    string
	Keys    []apiKeyRecord `json:"keys"`
	persist *persistStore
}

func newAPIKeyStore(path string) *apiKeyStore {
	s := &apiKeyStore{Path: path}
	s.persist = &persistStore{flush: s.flush}
	return s
}

func openAPIKeys() *apiKeyStore {
	p := envPath("M365_API_KEYS")
	if p == "" {
		p = configuredPath("M365_API_KEYS", "api-keys.json")
	}
	s := newAPIKeyStore(p)
	b, e := os.ReadFile(p)
	if e == nil && json.Unmarshal(b, s) == nil {
		migrated := false
		for i := range s.Keys {
			k := &s.Keys[i]
			// Keys created before the hash migration carry a raw plaintext but
			// no hash: backfill the hash and keep the plaintext so the console
			// can still redisplay the full key (see list()).
			if k.Raw != "" && k.Hash == "" {
				k.Hash = keyHash(k.Raw)
				migrated = true
				continue
			}
			// Keys created during the hash-only era have a hash but no stored
			// plaintext, which SHA-256 cannot recover. Rotate them in place:
			// persist a fresh plaintext key so the console can display and copy
			// it again, while the old hash stays valid (LegacyHash) so existing
			// clients keep working until the operator deletes the record.
			if k.Raw == "" && k.Hash != "" {
				newRaw := newKeyRaw()
				k.LegacyHash = k.Hash
				k.Hash = keyHash(newRaw)
				k.Prefix = newRaw[:12]
				k.Raw = newRaw
				migrated = true
				log.Printf("[api-keys] rotated key %q (%s): plaintext was never stored under the old hash-only scheme; new value is persisted and shown in the console, old value remains valid", k.Name, k.ID)
			}
		}
		if migrated {
			_ = s.flush()
		}
	}
	return s
}
func (s *apiKeyStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(s.Path, b, 0600)
}
func keyHash(k string) string { h := sha256.Sum256([]byte(k)); return hex.EncodeToString(h[:]) }
func newKeyRaw() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic("api key rng failure: " + e.Error())
	}
	return "m365_" + hex.EncodeToString(b)
}
func (s *apiKeyStore) create(name string) (apiKeyRecord, string, error) {
	raw := newKeyRaw()
	// 明文 Raw 与 Hash 一同持久化：控制台可以随时重新显示并复制完整密钥
	// （前端 openUseKey 依赖 list() 返回的 raw 字段）。
	r := apiKeyRecord{ID: raw[5:21], Name: name, Prefix: raw[:12], Hash: keyHash(raw), Raw: raw, CreatedAt: time.Now()}
	s.mu.Lock()
	s.Keys = append(s.Keys, r)
	s.mu.Unlock()
	if err := s.persist.flushNowBlocking(); err != nil {
		s.mu.Lock()
		s.Keys = s.Keys[:len(s.Keys)-1]
		s.mu.Unlock()
		return apiKeyRecord{}, "", err
	}
	r.Hash = ""
	return r, raw, nil
}
func (s *apiKeyStore) list() []apiKeyRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]apiKeyRecord, len(s.Keys))
	copy(out, s.Keys)
	for i := range out {
		// Hash 永不下发；Raw 保留以便控制台重复显示密钥。
		out[i].Hash = ""
		out[i].LegacyHash = ""
	}
	return out
}
func (s *apiKeyStore) revoke(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID == id && !s.Keys[i].Revoked {
			s.Keys[i].Revoked = true
			s.mu.Unlock()
			if err := s.persist.flushNowBlocking(); err != nil {
				s.mu.Lock()
				s.Keys[i].Revoked = false
				s.mu.Unlock()
				return false, err
			}
			return true, nil
		}
	}
	s.mu.Unlock()
	return false, nil
}

// delete physically removes a key record, rolling back on persistence failure.
func (s *apiKeyStore) delete(id string) (bool, error) {
	s.mu.Lock()
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		removed := s.Keys[i]
		s.Keys = append(s.Keys[:i], s.Keys[i+1:]...)
		s.mu.Unlock()
		if err := s.persist.flushNowBlocking(); err != nil {
			s.mu.Lock()
			s.Keys = append(s.Keys[:i], append([]apiKeyRecord{removed}, s.Keys[i:]...)...)
			s.mu.Unlock()
			return false, err
		}
		return true, nil
	}
	s.mu.Unlock()
	return false, nil
}

func (s *apiKeyStore) update(id, name string, revoked *bool) (bool, error) {
	s.mu.Lock()
	found := false
	for i := range s.Keys {
		if s.Keys[i].ID != id {
			continue
		}
		if name != "" {
			s.Keys[i].Name = name
		}
		if revoked != nil {
			s.Keys[i].Revoked = *revoked
		}
		found = true
		break
	}
	s.mu.Unlock()
	if !found {
		return false, nil
	}
	if err := s.persist.flushNowBlocking(); err != nil {
		return false, err
	}
	return true, nil
}
func (s *apiKeyStore) valid(raw string) bool {
	s.mu.Lock()
	h := keyHash(raw)
	found := false
	for i := range s.Keys {
		if s.Keys[i].Revoked {
			continue
		}
		if s.Keys[i].Hash == h || (s.Keys[i].LegacyHash != "" && s.Keys[i].LegacyHash == h) {
			now := time.Now()
			s.Keys[i].LastUsedAt = &now
			found = true
			break
		}
	}
	s.mu.Unlock()
	if found {
		s.persist.markDirty()
	}
	return found
}
