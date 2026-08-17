package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/storage"
)

type AccountToken struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"displayName,omitempty"`
	Status       string    `json:"status"`
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	OID          string    `json:"oid,omitempty"`
	TID          string    `json:"tid,omitempty"`
	ClientID     string    `json:"clientId,omitempty"`
}

type Cache struct {
	Accounts []AccountToken `json:"accounts"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	data     Cache
	nextIdx  int
	inflight map[string]*inflightRefresh
}

// inflightRefresh coalesces concurrent EnsureValid refreshes for the same
// account: AAD refresh tokens can only be redeemed once, so a stampede of
// concurrent requests must not each try Refresh().
type inflightRefresh struct {
	done chan struct{}
	acc  AccountToken
	err  error
}

func CachePath() string {
	// Explicit file paths win over a derived data directory. This matters when
	// a deployment keeps general state in a mounted directory but puts account
	// tokens on a separate volume.
	for _, name := range []string{"M365_CONFIG", "M365_TOKEN_CACHE", "M365_TOKEN_FILE"} {
		if p := storage.EnvPath(name); p != "" {
			return p
		}
	}
	return storage.Path("", "accounts.json")
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		path = CachePath()
	}
	s := &Store{path: path, data: Cache{Accounts: []AccountToken{}}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, err
	}
	// Normalize oid/tid for older cache entries.
	legacyPlain := false
	for i := range s.data.Accounts {
		a := &s.data.Accounts[i]
		if a.OID == "" {
			a.OID = a.ID
		}
		if a.ID == "" {
			a.ID = a.OID
		}
		if (a.AccessToken != "" && !strings.HasPrefix(a.AccessToken, encryptedTokenPrefix)) ||
			(a.RefreshToken != "" && !strings.HasPrefix(a.RefreshToken, encryptedTokenPrefix)) {
			legacyPlain = true
		}
	}
	aead, err := tokenCipher()
	if err != nil {
		return nil, err
	}
	if aead != nil {
		if err := decryptAccountsLocked(&s.data.Accounts, aead); err != nil {
			return nil, err
		}
		// Migrate legacy plaintext storage: the next save rewrites every
		// account with encrypted tokens.
		if legacyPlain && len(s.data.Accounts) > 0 {
			_ = s.saveLocked()
		}
	}
	return s, nil
}

// tokenCipher derives the AES-256-GCM AEAD from M365_TOKEN_ENC_KEY. A missing
// key disables encryption (nil AEAD); a malformed key is a hard error so the
// server never silently falls back to plaintext storage.
func tokenCipher() (cipher.AEAD, error) {
	raw := strings.TrimSpace(os.Getenv("M365_TOKEN_ENC_KEY"))
	if raw == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, errors.New("M365_TOKEN_ENC_KEY must be a hex-encoded 32-byte key (64 hex characters)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptedTokenPrefix marks stored values that were encrypted at rest.
const encryptedTokenPrefix = "enc:v1:"

func encryptToken(aead cipher.AEAD, plain string) (string, error) {
	if aead == nil || plain == "" {
		return plain, nil
	}
	// Idempotent: a value already encrypted (e.g. an in-memory account that
	// was read back from an encrypted file) is never re-encrypted.
	if strings.HasPrefix(plain, encryptedTokenPrefix) {
		return plain, nil
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plain), nil)
	return encryptedTokenPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func decryptToken(aead cipher.AEAD, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !strings.HasPrefix(stored, encryptedTokenPrefix) {
		return stored, nil // legacy plaintext value
	}
	if aead == nil {
		return "", errors.New("M365_TOKEN_ENC_KEY is not configured but accounts.json contains encrypted tokens")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encryptedTokenPrefix))
	if err != nil || len(raw) < aead.NonceSize() {
		return "", errors.New("stored token is not valid encrypted data")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("stored token failed to decrypt (wrong M365_TOKEN_ENC_KEY?)")
	}
	return string(plain), nil
}

// decryptAccountsLocked restores in-memory plaintext for every account. The
// JSON file itself keeps ciphertext; only this process holds plaintext.
func decryptAccountsLocked(accounts *[]AccountToken, aead cipher.AEAD) error {
	for i := range *accounts {
		a := &(*accounts)[i]
		at, err := decryptToken(aead, a.AccessToken)
		if err != nil {
			return fmt.Errorf("account %s accessToken: %w", a.ID, err)
		}
		rt, err := decryptToken(aead, a.RefreshToken)
		if err != nil {
			return fmt.Errorf("account %s refreshToken: %w", a.ID, err)
		}
		a.AccessToken = at
		a.RefreshToken = rt
	}
	return nil
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) saveLocked() error {
	aead, err := tokenCipher()
	if err != nil {
		return err
	}
	// Serialize a snapshot: in-memory tokens must stay plaintext for callers,
	// only the on-disk copy carries ciphertext.
	snap := Cache{Accounts: make([]AccountToken, len(s.data.Accounts))}
	copy(snap.Accounts, s.data.Accounts)
	if aead != nil {
		for i := range snap.Accounts {
			at, err := encryptToken(aead, snap.Accounts[i].AccessToken)
			if err != nil {
				return err
			}
			rt, err := encryptToken(aead, snap.Accounts[i].RefreshToken)
			if err != nil {
				return err
			}
			snap.Accounts[i].AccessToken = at
			snap.Accounts[i].RefreshToken = rt
		}
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.path, b, 0o600)
}

func atomicWrite(path string, b []byte, perm os.FileMode) error {
	return storage.WriteFileAtomic(path, b, perm)
}

func (s *Store) List() []AccountToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountToken, len(s.data.Accounts))
	copy(out, s.data.Accounts)
	return out
}

func (s *Store) Upsert(tok TokenSet) (AccountToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := tok.HomeOID
	if id == "" {
		id = tok.Email
	}
	if id == "" {
		id = "account-" + time.Now().Format("150405")
	}
	acc := AccountToken{
		ID:           id,
		Email:        tok.Email,
		DisplayName:  tok.DisplayName,
		Status:       "online",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		UpdatedAt:    time.Now(),
		OID:          firstNonEmpty(tok.HomeOID, id),
		TID:          tok.TenantID,
		ClientID:     ClientID(),
	}
	found := false
	for i, existing := range s.data.Accounts {
		if existing.ID == acc.ID || (acc.Email != "" && existing.Email == acc.Email) {
			if acc.RefreshToken == "" {
				acc.RefreshToken = existing.RefreshToken
			}
			if acc.TID == "" {
				acc.TID = existing.TID
			}
			if acc.OID == "" {
				acc.OID = existing.OID
			}
			s.data.Accounts[i] = acc
			found = true
			break
		}
	}
	if !found {
		s.data.Accounts = append(s.data.Accounts, acc)
	}
	return acc, s.saveLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data.Accounts[:0]
	for _, a := range s.data.Accounts {
		if a.ID != id {
			next = append(next, a)
		}
	}
	s.data.Accounts = next
	return s.saveLocked()
}

// UpdateRefreshToken persists a rotated refresh token (e.g. one returned by a
// separate-scope refresh used for Designer image downloads).
func (s *Store) UpdateRefreshToken(id, refreshToken string) error {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id {
			s.data.Accounts[i].RefreshToken = refreshToken
			s.data.Accounts[i].UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return errors.New("account not found")
}

func (s *Store) Get(id string) (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.data.Accounts {
		if a.ID == id || a.OID == id || a.Email == id {
			return a, true
		}
	}
	return AccountToken{}, false
}

func (s *Store) First() (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Accounts) == 0 {
		return AccountToken{}, false
	}
	return s.data.Accounts[0], true
}

// Next returns the next account in round-robin order.
func (s *Store) Next() (AccountToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.data.Accounts)
	if n == 0 {
		return AccountToken{}, false
	}
	acc := s.data.Accounts[s.nextIdx%n]
	s.nextIdx = (s.nextIdx + 1) % n
	return acc, true
}

func (s *Store) EnsureValid(id string) (AccountToken, error) {
	acc, ok := s.Get(id)
	if !ok {
		return AccountToken{}, os.ErrNotExist
	}
	if time.Now().Before(acc.ExpiresAt.Add(-30 * time.Second)) {
		return acc, nil
	}
	if acc.RefreshToken == "" {
		acc.Status = "expired"
		s.mu.Lock()
		for i, a := range s.data.Accounts {
			if a.ID == acc.ID {
				s.data.Accounts[i] = acc
				_ = s.saveLocked()
				break
			}
		}
		s.mu.Unlock()
		return acc, fmtExpired()
	}
	return s.refreshInflight(acc)
}

// refreshInflight runs the AAD token refresh exactly once per account; waiters
// block on the shared flight instead of redeeming the one-time refresh token
// themselves. The winner's outcome is broadcast to all waiters.
func (s *Store) refreshInflight(acc AccountToken) (AccountToken, error) {
	s.mu.Lock()
	if s.inflight == nil {
		s.inflight = map[string]*inflightRefresh{}
	}
	if f, ok := s.inflight[acc.ID]; ok {
		s.mu.Unlock()
		<-f.done
		return f.acc, f.err
	}
	f := &inflightRefresh{done: make(chan struct{})}
	s.inflight[acc.ID] = f
	s.mu.Unlock()

	tok, err := Refresh(acc.RefreshToken)
	if err != nil {
		acc.Status = "expired"
		s.mu.Lock()
		for i, a := range s.data.Accounts {
			if a.ID == acc.ID {
				s.data.Accounts[i] = acc
				_ = s.saveLocked()
				break
			}
		}
		s.mu.Unlock()
		f.acc, f.err = acc, err
	} else {
		if tok.Email == "" {
			tok.Email = acc.Email
		}
		if tok.DisplayName == "" {
			tok.DisplayName = acc.DisplayName
		}
		if tok.HomeOID == "" {
			tok.HomeOID = firstNonEmpty(acc.OID, acc.ID)
		}
		if tok.TenantID == "" {
			tok.TenantID = acc.TID
		}
		f.acc, f.err = s.Upsert(tok)
	}
	close(f.done)
	s.mu.Lock()
	delete(s.inflight, acc.ID)
	s.mu.Unlock()
	return f.acc, f.err
}

func fmtExpired() error {
	return errors.New("token_expired: refresh token missing or expired")
}
