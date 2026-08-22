package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tiktoken-go/tokenizer"
)

type CacheStats struct {
	mu sync.Mutex

	TotalRequests  int64         `json:"total_requests"`
	CacheHits      int64         `json:"cache_hits"`
	CacheMisses    int64         `json:"cache_misses"`
	TokensSent     int64         `json:"tokens_sent"`
	TokensSaved    int64         `json:"tokens_saved"`
	ActiveSessions int           `json:"active_sessions"`
	MaxSessionAge  time.Duration `json:"max_session_age"`
	HitRate        float64       `json:"hit_rate"`
	SavingsPercent float64       `json:"savings_percent"`

	KeyStats map[string]*KeyStat `json:"key_stats"`

	path    string
	persist *persistStore
}

type statsSnapshot struct {
	TotalRequests int64 `json:"total_requests"`
	CacheHits     int64 `json:"cache_hits"`
	CacheMisses   int64 `json:"cache_misses"`
	TokensSent    int64 `json:"tokens_sent"`
	TokensSaved   int64 `json:"tokens_saved"`
	// Explicit token fields preserve TokensSent/TokensSaved compatibility while
	// making clear that sent means newly submitted input and saved means cached input.
	NewInputTokens    int64               `json:"new_input_tokens"`
	CachedInputTokens int64               `json:"cached_input_tokens"`
	InputTotalTokens  int64               `json:"input_total_tokens"`
	CacheInputPercent float64             `json:"cache_input_percent"`
	ActiveSessions    int                 `json:"active_sessions"`
	MaxSessionAge     time.Duration       `json:"max_session_age"`
	HitRate           float64             `json:"hit_rate"`
	SavingsPercent    float64             `json:"savings_percent"`
	KeyStats          map[string]*KeyStat `json:"key_stats"`
}

type KeyStat struct {
	APIKey        string    `json:"api_key"`
	TotalRequests int64     `json:"total_requests"`
	CacheHits     int64     `json:"cache_hits"`
	CacheMisses   int64     `json:"cache_misses"`
	TokensSent    int64     `json:"tokens_sent"`
	TokensSaved   int64     `json:"tokens_saved"`
	HitRate       float64   `json:"hit_rate"`
	LastUsed      time.Time `json:"last_used"`
}

var cacheStats = openCacheStats()

func openCacheStats() *CacheStats {
	p := statsPath()
	s := &CacheStats{KeyStats: make(map[string]*KeyStat), path: p}
	b, err := os.ReadFile(p)
	if err == nil {
		json.Unmarshal(b, s)
		if s.KeyStats == nil {
			s.KeyStats = make(map[string]*KeyStat)
		}
	}
	s.persist = &persistStore{flush: s.flush}
	return s
}

func statsPath() string {
	return configuredPath("M365_STATS_FILE", "stats.json")
}

func (s *CacheStats) RecordRequest(apiKey string, hit bool, tokensSent, tokensSaved int64, activeSessions int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.TotalRequests++
	s.TokensSent += tokensSent
	s.TokensSaved += tokensSaved
	s.ActiveSessions = activeSessions

	if hit {
		s.CacheHits++
	} else {
		s.CacheMisses++
	}

	if s.TotalRequests > 0 {
		s.HitRate = float64(s.CacheHits) / float64(s.TotalRequests) * 100
	}
	if s.TokensSent+s.TokensSaved > 0 {
		s.SavingsPercent = float64(s.TokensSaved) / float64(s.TokensSent+s.TokensSaved) * 100
	}

	ks, ok := s.KeyStats[apiKey]
	if !ok {
		ks = &KeyStat{APIKey: apiKey}
		s.KeyStats[apiKey] = ks
	}
	ks.TotalRequests++
	ks.TokensSent += tokensSent
	ks.TokensSaved += tokensSaved
	ks.LastUsed = time.Now()
	if hit {
		ks.CacheHits++
	} else {
		ks.CacheMisses++
	}
	if ks.TotalRequests > 0 {
		ks.HitRate = float64(ks.CacheHits) / float64(ks.TotalRequests) * 100
	}
	s.persist.markDirty()
}

func (s *CacheStats) GetStats() statsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make(map[string]*KeyStat, len(s.KeyStats))
	for k, v := range s.KeyStats {
		cp := *v
		keys[k] = &cp
	}
	return statsSnapshot{
		TotalRequests:     s.TotalRequests,
		CacheHits:         s.CacheHits,
		CacheMisses:       s.CacheMisses,
		TokensSent:        s.TokensSent,
		TokensSaved:       s.TokensSaved,
		NewInputTokens:    s.TokensSent,
		CachedInputTokens: s.TokensSaved,
		InputTotalTokens:  s.TokensSent + s.TokensSaved,
		CacheInputPercent: s.SavingsPercent,
		ActiveSessions:    s.ActiveSessions,
		MaxSessionAge:     s.MaxSessionAge,
		HitRate:           s.HitRate,
		SavingsPercent:    s.SavingsPercent,
		KeyStats:          keys,
	}
}

func (s *CacheStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalRequests = 0
	s.CacheHits = 0
	s.CacheMisses = 0
	s.TokensSent = 0
	s.TokensSaved = 0
	s.HitRate = 0
	s.SavingsPercent = 0
	s.KeyStats = make(map[string]*KeyStat)
	s.persist.markDirty()
}

func (s *CacheStats) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0600)
}

var (
	tokenCodecOnce sync.Once
	tokenCodec     tokenizer.Codec
)

func EstimateTokens(text string) int64 {
	if text == "" {
		return 0
	}
	tokenCodecOnce.Do(func() {
		// cl100k_base is a substantially more accurate general-purpose estimate
		// than byte-length division, especially for Chinese and mixed-language text.
		tokenCodec, _ = tokenizer.Get(tokenizer.Cl100kBase)
	})
	if tokenCodec != nil {
		if count, err := tokenCodec.Count(text); err == nil {
			return int64(count)
		}
	}
	// Keep a conservative fallback if tokenizer initialization ever fails.
	runes := []rune(text)
	return int64((len(runes) + 3) / 4)
}
