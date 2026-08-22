package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAdminSessionTTL = 30 * 24 * time.Hour
	adminSessionFileName   = "admin-sessions.json"
)

type persistedAdminSessions struct {
	Version  int                  `json:"version"`
	Sessions map[string]time.Time `json:"sessions"`
}

func adminSessionPath() string {
	return configuredPath("M365_ADMIN_SESSIONS_FILE", adminSessionFileName)
}

func adminSessionTTL() time.Duration {
	value := strings.TrimSpace(os.Getenv("M365_ADMIN_SESSION_TTL"))
	if value == "" {
		return defaultAdminSessionTTL
	}
	if strings.HasSuffix(value, "d") {
		if days, err := strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 32); err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour
		}
	}
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return d
	}
	if days, err := strconv.ParseInt(value, 10, 32); err == nil && days > 0 {
		return time.Duration(days) * 24 * time.Hour
	}
	return defaultAdminSessionTTL
}

func adminSessionDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func loadAdminSessions(now time.Time) (map[string]time.Time, error) {
	sessions := make(map[string]time.Time)
	data, err := os.ReadFile(adminSessionPath())
	if errors.Is(err, os.ErrNotExist) {
		return sessions, nil
	}
	if err != nil {
		return nil, err
	}
	var persisted persistedAdminSessions
	if err := json.Unmarshal(data, &persisted); err != nil {
		return nil, err
	}
	for digest, expires := range persisted.Sessions {
		if len(digest) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(digest); err != nil {
			continue
		}
		if now.Before(expires) {
			sessions[digest] = expires
		}
	}
	pruneAdminSessions(sessions, now)
	for len(sessions) > maxAdminSessions {
		var oldest string
		var oldestExpiry time.Time
		for digest, expiry := range sessions {
			if oldest == "" || expiry.Before(oldestExpiry) {
				oldest = digest
				oldestExpiry = expiry
			}
		}
		delete(sessions, oldest)
	}
	return sessions, nil
}

func saveAdminSessions(sessions map[string]time.Time) error {
	persisted := persistedAdminSessions{
		Version:  1,
		Sessions: make(map[string]time.Time, len(sessions)),
	}
	for digest, expires := range sessions {
		persisted.Sessions[digest] = expires.UTC()
	}
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(adminSessionPath(), data, 0o600)
}
