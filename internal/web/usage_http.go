package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 365 {
			days = n
		}
	}
	jsonOut(w, map[string]any{
		"days":  days,
		"stats": s.usage.snapshot(days),
	})
}

func (s *Server) adminUsageLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := 50
	offset := 0
	q := r.URL.Query()
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	var f usageLogFilter
	f.Key = strings.TrimSpace(q.Get("key"))
	f.Account = strings.TrimSpace(q.Get("account"))
	f.Model = strings.TrimSpace(q.Get("model"))
	f.Endpoint = strings.TrimSpace(q.Get("endpoint"))
	f.Error = strings.TrimSpace(q.Get("error"))
	f.Q = strings.TrimSpace(q.Get("q"))
	f.Status = strings.TrimSpace(q.Get("status"))
	if v := strings.TrimSpace(q.Get("stream")); v != "" {
		b := v == "true" || v == "1"
		f.Stream = &b
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.From, f.HasFrom = t, true
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.To, f.HasTo = t, true
		}
	}
	if v := q.Get("min_tokens"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			f.MinTokens, f.HasMinTok = n, true
		}
	}
	if v := q.Get("max_tokens"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			f.MaxTokens, f.HasMaxTok = n, true
		}
	}
	jsonOut(w, s.usage.logs(limit, offset, f))
}
