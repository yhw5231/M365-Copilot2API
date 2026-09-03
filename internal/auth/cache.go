package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/storage"
)

type AccountToken struct {
	ID               string    `json:"id"`
	Email            string    `json:"email"`
	DisplayName      string    `json:"displayName,omitempty"`
	Status           string    `json:"status"`
	ScheduleDisabled bool      `json:"scheduleDisabled,omitempty"`
	AccessToken      string    `json:"accessToken"`
	RefreshToken     string    `json:"refreshToken,omitempty"`
	ExpiresAt        time.Time `json:"expiresAt"`
	ImportedAt       time.Time `json:"importedAt,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
	OID              string    `json:"oid,omitempty"`
	TID              string    `json:"tid,omitempty"`
	ClientID         string    `json:"clientId,omitempty"`
	BoundProxy       string    `json:"boundProxy,omitempty"`
}

// Cache is the in-memory account set. It doubles as the reader for the legacy
// single-file accounts.json that older versions wrote.
type Cache struct {
	Accounts []AccountToken `json:"accounts"`
}

// settingsSuffix marks the interim per-account settings file some builds of
// the per-account layout wrote (<name>.settings.json). Those files are merged
// back into the account file and removed on load.
const settingsSuffix = ".settings.json"

// accountSettingsFile is the interim on-disk format of one account's settings
// (<accountsDir>/<account-name>.settings.json).
type accountSettingsFile struct {
	ScheduleDisabled bool   `json:"scheduleDisabled,omitempty"`
	BoundProxy       string `json:"boundProxy,omitempty"`
}

// accountFile is the on-disk format of one account
// (<accountsDir>/<account-name>.json). It holds both the credentials and the
// per-account settings (scheduling toggle, bound proxy); the shared account
// scheduling settings live in a separate store managed by the web layer.
type accountFile struct {
	ID               string    `json:"id"`
	Email            string    `json:"email"`
	DisplayName      string    `json:"displayName,omitempty"`
	Status           string    `json:"status"`
	ScheduleDisabled bool      `json:"scheduleDisabled,omitempty"`
	AccessToken      string    `json:"accessToken"`
	RefreshToken     string    `json:"refreshToken,omitempty"`
	ExpiresAt        time.Time `json:"expiresAt"`
	ImportedAt       time.Time `json:"importedAt,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
	OID              string    `json:"oid,omitempty"`
	TID              string    `json:"tid,omitempty"`
	ClientID         string    `json:"clientId,omitempty"`
	BoundProxy       string    `json:"boundProxy,omitempty"`
}

type Store struct {
	mu  sync.Mutex
	dir string
	// files maps account ID -> base file name (without extension) currently
	// backing it on disk; taken maps lowercased base names -> account ID so a
	// case-insensitive filesystem never lets two accounts share one file.
	files    map[string]string
	taken    map[string]string
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

func cryptoRandUint16() uint16 {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

// AccountsDir returns the directory holding one JSON file per authorized
// account. It defaults to <data dir>/accounts and can be relocated with
// M365_ACCOUNTS_DIR.
func AccountsDir() string {
	if p := storage.EnvPath("M365_ACCOUNTS_DIR"); p != "" {
		return p
	}
	return filepath.Join(storage.DataDir(), "accounts")
}

// LegacyCachePath returns the location of the pre-per-account single-file
// store (accounts.json). It is only read once to import accounts into
// AccountsDir; explicit file-path environment variables from older
// deployments are honored as the migration source.
func LegacyCachePath() string {
	for _, name := range []string{"M365_CONFIG", "M365_TOKEN_CACHE", "M365_TOKEN_FILE"} {
		if p := storage.EnvPath(name); p != "" {
			return p
		}
	}
	return storage.Path("", "accounts.json")
}

// OpenStore opens (or creates) the per-account store. A non-empty path
// overrides the accounts directory (used by tests); an empty path resolves
// AccountsDir(). A legacy single-file cache next to the data directory is
// imported automatically on first run.
func OpenStore(path string) (*Store, error) {
	dir := path
	if dir == "" {
		dir = AccountsDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create accounts directory %q: %w", dir, err)
	}
	s := &Store{
		dir:   dir,
		files: map[string]string{},
		taken: map[string]string{},
		data:  Cache{Accounts: []AccountToken{}},
	}
	aead, err := tokenCipher()
	if err != nil {
		return nil, err
	}
	if err := s.loadPerAccount(aead); err != nil {
		return nil, err
	}
	if err := s.importLegacy(aead); err != nil {
		return nil, err
	}
	return s, nil
}

// normalizeLocked backfills oid/id for older records where only one was set.
func normalizeLocked(a *AccountToken) {
	if a.OID == "" {
		a.OID = a.ID
	}
	if a.ID == "" {
		a.ID = a.OID
	}
}

// loadPerAccount reads every <name>.json (plus its <name>.settings.json) from
// the accounts directory in deterministic file-name order. Unparseable files
// are skipped with a warning so one damaged account cannot take the whole
// gateway down; decrypt errors still fail hard (wrong M365_TOKEN_ENC_KEY must
// never degrade into silently missing accounts).
func (s *Store) loadPerAccount(aead cipher.AEAD) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, settingsSuffix) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for _, name := range names {
		base := strings.TrimSuffix(name, ".json")
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return err
		}
		var rec accountFile
		if err := json.Unmarshal(b, &rec); err != nil {
			log.Printf("[accounts] WARNING: skipping unparseable account file %s: %v", name, err)
			continue
		}
		normalizeAccountFile(&rec)
		if rec.ID == "" || seen[rec.ID] {
			log.Printf("[accounts] WARNING: skipping account file %s without a usable id (or duplicate)", name)
			continue
		}
		seen[rec.ID] = true
		acc := AccountToken{
			ID:               rec.ID,
			Email:            rec.Email,
			DisplayName:      rec.DisplayName,
			Status:           rec.Status,
			ScheduleDisabled: rec.ScheduleDisabled,
			AccessToken:      rec.AccessToken,
			RefreshToken:     rec.RefreshToken,
			ExpiresAt:        rec.ExpiresAt,
			ImportedAt:       rec.ImportedAt,
			UpdatedAt:        rec.UpdatedAt,
			OID:              rec.OID,
			TID:              rec.TID,
			ClientID:         rec.ClientID,
			BoundProxy:       rec.BoundProxy,
		}
		at, err := decryptToken(aead, acc.AccessToken)
		if err != nil {
			return fmt.Errorf("account %s accessToken: %w", acc.ID, err)
		}
		rt, err := decryptToken(aead, acc.RefreshToken)
		if err != nil {
			return fmt.Errorf("account %s refreshToken: %w", acc.ID, err)
		}
		acc.AccessToken, acc.RefreshToken = at, rt
		normalizeLocked(&acc)
		// Merge the interim per-account settings file (<name>.settings.json)
		// written by an intermediate build back into the account record and
		// remove the file; settings now live inside the account file itself.
		var st accountSettingsFile
		if sb, err := os.ReadFile(filepath.Join(s.dir, base+settingsSuffix)); err == nil {
			if err := json.Unmarshal(sb, &st); err != nil {
				log.Printf("[accounts] WARNING: ignoring unparseable interim settings file for %s: %v", base, err)
			}
			acc.ScheduleDisabled = st.ScheduleDisabled
			acc.BoundProxy = st.BoundProxy
			if err := os.Remove(filepath.Join(s.dir, base+settingsSuffix)); err != nil {
				log.Printf("[accounts] WARNING: could not remove interim settings file for %s: %v", base, err)
			}
		}
		s.data.Accounts = append(s.data.Accounts, acc)
		s.files[acc.ID] = base
		s.taken[strings.ToLower(base)] = acc.ID
		// Encryption upgrade: a plaintext record on disk with a configured key
		// is rewritten encrypted right away.
		if aead != nil && (hasPlainToken(rec.AccessToken) || hasPlainToken(rec.RefreshToken)) {
			if err := s.persistTokenFileLocked(acc); err != nil {
				log.Printf("[accounts] WARNING: could not re-encrypt %s: %v", name, err)
			}
		} else if st.ScheduleDisabled || st.BoundProxy != "" {
			// Fold the merged interim settings into the account file.
			if err := s.persistTokenFileLocked(acc); err != nil {
				log.Printf("[accounts] WARNING: could not merge settings into %s: %v", name, err)
			}
		}
	}
	return nil
}

func hasPlainToken(v string) bool {
	return v != "" && !strings.HasPrefix(v, encryptedTokenPrefix)
}

func normalizeAccountFile(rec *accountFile) {
	if rec.OID == "" {
		rec.OID = rec.ID
	}
	if rec.ID == "" {
		rec.ID = rec.OID
	}
}

// importLegacy merges the legacy single-file accounts.json into the per-account
// directory. Only accounts missing from the directory are imported, so an
// interrupted migration resumes cleanly on the next start. After a successful
// import the legacy file is renamed out of the way (never deleted) so removed
// accounts cannot resurrect from it on later restarts.
func (s *Store) importLegacy(aead cipher.AEAD) error {
	legacy := LegacyCachePath()
	if strings.TrimSpace(legacy) == "" {
		return nil
	}
	b, err := os.ReadFile(legacy)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read legacy account cache %q: %w", legacy, err)
	}
	var old Cache
	if err := json.Unmarshal(b, &old); err != nil {
		return fmt.Errorf("parse legacy account cache %q: %w", legacy, err)
	}
	existing := map[string]bool{}
	for _, a := range s.data.Accounts {
		existing[a.ID] = true
	}
	imported := 0
	for _, a := range old.Accounts {
		normalizeLocked(&a)
		if a.ID == "" || existing[a.ID] {
			continue
		}
		at, err := decryptToken(aead, a.AccessToken)
		if err != nil {
			return fmt.Errorf("account %s accessToken: %w", a.ID, err)
		}
		rt, err := decryptToken(aead, a.RefreshToken)
		if err != nil {
			return fmt.Errorf("account %s refreshToken: %w", a.ID, err)
		}
		a.AccessToken, a.RefreshToken = at, rt
		if err := s.persistTokenFileLocked(a); err != nil {
			return err
		}
		s.data.Accounts = append(s.data.Accounts, a)
		existing[a.ID] = true
		imported++
	}
	sealed := legacy + ".migrated"
	if _, err := os.Stat(sealed); err == nil {
		sealed = legacy + ".migrated-" + time.Now().Format("20060102-150405")
	}
	if err := os.Rename(legacy, sealed); err != nil {
		// Without the rename a later restart would re-import removed accounts,
		// but the per-account data itself is already consistent; keep serving.
		log.Printf("[accounts] WARNING: legacy cache %q could not be renamed after import: %v", legacy, err)
		return nil
	}
	if imported > 0 {
		log.Printf("[accounts] imported %d account(s) from legacy %q into %q (one JSON per account)", imported, legacy, s.dir)
	} else {
		log.Printf("[accounts] sealed legacy cache %q (no new accounts to import)", legacy)
	}
	return nil
}

// sanitizeFileName converts an account name into a cross-platform safe file
// name: Windows-reserved characters become underscores, control characters are
// dropped, and reserved device names / trailing dots+spaces are neutralized.
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop control characters
		case strings.ContainsRune(`\/:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), " .")
	if out == "" {
		out = "account"
	}
	if reservedDeviceName(strings.SplitN(strings.ToUpper(out), ".", 2)[0]) {
		out = "_" + out
	}
	return out
}

func reservedDeviceName(n string) bool {
	switch n {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

func shortID(id string) string {
	r := []rune(sanitizeFileName(id))
	if len(r) > 8 {
		r = r[:8]
	}
	return string(r)
}

// reserveBaseLocked returns the base file name backing this account, updating
// the id->file maps. When an account's email changes, the files saved under
// the previous name are removed so exactly one file pair per account remains.
func (s *Store) reserveBaseLocked(a AccountToken) (string, error) {
	want := sanitizeFileName(firstNonEmpty(a.Email, a.ID))
	if prev, ok := s.files[a.ID]; ok && prev != "" {
		if strings.EqualFold(prev, want) {
			return prev, nil
		}
		for _, suffix := range []string{".json", settingsSuffix} {
			if err := os.Remove(filepath.Join(s.dir, prev+suffix)); err != nil && !os.IsNotExist(err) {
				return "", err
			}
		}
		if s.taken[strings.ToLower(prev)] == a.ID {
			delete(s.taken, strings.ToLower(prev))
		}
	}
	if owner, ok := s.taken[strings.ToLower(want)]; ok && owner != a.ID {
		// Case-insensitive filename collision (e.g. two emails differing only
		// in case): disambiguate deterministically with the account id.
		want += "-" + shortID(a.ID)
	}
	s.files[a.ID] = want
	s.taken[strings.ToLower(want)] = a.ID
	return want, nil
}

// persistTokenFileLocked writes the account file (credentials + per-account
// settings) with encrypted tokens. In-memory accounts always carry plaintext;
// only the on-disk copy is sealed.
func (s *Store) persistTokenFileLocked(a AccountToken) error {
	aead, err := tokenCipher()
	if err != nil {
		return err
	}
	base, err := s.reserveBaseLocked(a)
	if err != nil {
		return err
	}
	rec := accountFile{
		ID:               a.ID,
		Email:            a.Email,
		DisplayName:      a.DisplayName,
		Status:           a.Status,
		ScheduleDisabled: a.ScheduleDisabled,
		ExpiresAt:        a.ExpiresAt,
		ImportedAt:       a.ImportedAt,
		UpdatedAt:        a.UpdatedAt,
		OID:              a.OID,
		TID:              a.TID,
		ClientID:         a.ClientID,
		BoundProxy:       a.BoundProxy,
	}
	if rec.AccessToken, err = encryptToken(aead, a.AccessToken); err != nil {
		return err
	}
	if rec.RefreshToken, err = encryptToken(aead, a.RefreshToken); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, base+".json"), b, 0o600)
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
		return "", errors.New("M365_TOKEN_ENC_KEY is not configured but the account file contains encrypted tokens")
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

// Dir returns the per-account storage directory (for diagnostics and the
// health endpoint).
func (s *Store) Dir() string {
	return s.dir
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

func (s *Store) SetScheduleEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id {
			s.data.Accounts[i].ScheduleDisabled = !enabled
			s.data.Accounts[i].UpdatedAt = time.Now()
			return s.persistTokenFileLocked(s.data.Accounts[i])
		}
	}
	return errors.New("account not found")
}

func (s *Store) ScheduleEnabled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, account := range s.data.Accounts {
		if account.ID == id {
			return !account.ScheduleDisabled
		}
	}
	return false
}

func (s *Store) Upsert(tok TokenSet) (AccountToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := tok.HomeOID
	if id == "" {
		id = tok.Email
	}
	if id == "" {
		id = fmt.Sprintf("account-%s-%04x", time.Now().Format("150405"), cryptoRandUint16())
	}
	acc := AccountToken{
		ID:           id,
		Email:        tok.Email,
		DisplayName:  tok.DisplayName,
		Status:       "online",
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.ExpiresAt,
		ImportedAt:   time.Now(),
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
			// Re-authorizing or refreshing an existing account must not change its
			// original import timestamp. Legacy records without importedAt use their
			// earliest available persisted timestamp as a stable migration fallback.
			acc.ImportedAt = existing.ImportedAt
			if acc.ImportedAt.IsZero() {
				acc.ImportedAt = existing.UpdatedAt
			}
			acc.ScheduleDisabled = existing.ScheduleDisabled
			if acc.BoundProxy == "" {
				acc.BoundProxy = existing.BoundProxy
			}
			s.data.Accounts[i] = acc
			found = true
			break
		}
	}
	if !found {
		s.data.Accounts = append(s.data.Accounts, acc)
	}
	return acc, s.persistTokenFileLocked(acc)
}

// Delete removes the account from memory and deletes its file from disk.
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
	base, ok := s.files[id]
	if !ok {
		return nil
	}
	for _, suffix := range []string{".json", settingsSuffix} {
		if err := os.Remove(filepath.Join(s.dir, base+suffix)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove account file %q: %w", base+suffix, err)
		}
	}
	delete(s.files, id)
	delete(s.taken, strings.ToLower(base))
	return nil
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
			return s.persistTokenFileLocked(s.data.Accounts[i])
		}
	}
	return errors.New("account not found")
}

// SetBoundProxy persists the proxy bound to one account (one-account-one-IP).
func (s *Store) SetBoundProxy(id, proxyURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID == id {
			s.data.Accounts[i].BoundProxy = proxyURL
			s.data.Accounts[i].UpdatedAt = time.Now()
			return s.persistTokenFileLocked(s.data.Accounts[i])
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

// MoveToBack moves an account to the end of the scheduling queue. It is used
// when an account is rate-limited so available-first routing continues with the
// next account and only retries the limited account after the queue cycles.
func (s *Store) MoveToBack(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, acc := range s.data.Accounts {
		if acc.ID != id && acc.OID != id && acc.Email != id {
			continue
		}
		if i == len(s.data.Accounts)-1 {
			return true
		}
		moved := acc
		copy(s.data.Accounts[i:], s.data.Accounts[i+1:])
		s.data.Accounts[len(s.data.Accounts)-1] = moved
		if len(s.data.Accounts) > 0 {
			s.nextIdx %= len(s.data.Accounts)
		}
		return true
	}
	return false
}

func (s *Store) EnsureValid(id string) (AccountToken, error) {
	s.mu.Lock()
	var acc AccountToken
	found := false
	for _, a := range s.data.Accounts {
		if a.ID == id || a.OID == id || a.Email == id {
			acc = a
			found = true
			break
		}
	}
	if !found {
		s.mu.Unlock()
		return AccountToken{}, os.ErrNotExist
	}
	if time.Now().Before(acc.ExpiresAt.Add(-30 * time.Second)) {
		s.mu.Unlock()
		return acc, nil
	}
	if acc.RefreshToken == "" {
		for i, a := range s.data.Accounts {
			if a.ID == acc.ID {
				s.data.Accounts[i].Status = "expired"
				_ = s.persistTokenFileLocked(s.data.Accounts[i])
				break
			}
		}
		s.mu.Unlock()
		acc.Status = "expired"
		return acc, fmtExpired()
	}
	s.mu.Unlock()
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

	endpoint := TokenEndpoint()
	if acc.ClientID == DeviceClientID() {
		endpoint = DeviceTokenEndpoint()
	}
	tok, err := Refresh(acc.RefreshToken, acc.ClientID, endpoint)
	if err != nil {
		s.mu.Lock()
		for i, a := range s.data.Accounts {
			if a.ID == acc.ID {
				s.data.Accounts[i].Status = "expired"
				_ = s.persistTokenFileLocked(s.data.Accounts[i])
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

func (s *Store) RefreshAllExpired() []TokenRefreshResult {
	s.mu.Lock()
	candidates := make([]AccountToken, 0, len(s.data.Accounts))
	for _, a := range s.data.Accounts {
		if time.Now().After(a.ExpiresAt.Add(-30*time.Second)) && a.RefreshToken != "" {
			candidates = append(candidates, a)
		}
	}
	s.mu.Unlock()
	var results []TokenRefreshResult
	for _, a := range candidates {
		acc, err := s.EnsureValid(a.ID)
		r := TokenRefreshResult{ID: a.ID, Email: a.Email}
		if err != nil {
			r.Success = false
			r.Error = err.Error()
		} else {
			r.Success = true
			r.ExpiresAt = acc.ExpiresAt
		}
		results = append(results, r)
	}
	return results
}

type TokenRefreshResult struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Success   bool      `json:"success"`
	Error     string    `json:"error,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}
