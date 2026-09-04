package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type debugRecord struct {
	ID           string    `json:"id"`
	At           time.Time `json:"at"`
	Path         string    `json:"path"`
	Method       string    `json:"method"`
	Status       int       `json:"status"`
	Level        string    `json:"level"`
	DurationMS   int64     `json:"durationMs"`
	TTFTMS       *int64    `json:"ttftMs,omitempty"`
	InputTokens  *int      `json:"inputTokens"`
	CachedTokens *int      `json:"cachedTokens,omitempty"`
	OutputTokens *int      `json:"outputTokens"`
	TokenSource  string    `json:"tokenSource"`
	CacheHit     *bool     `json:"cacheHit"`
	CacheSource  string    `json:"cacheSource"`
	// SessionMatched / ConversationID 透传 trace 的会话命中信息：
	// SessionMatched 为 resolver 命中方式（空=未命中，新会话），
	// ConversationID 为服务本请求的上游对话 ID。
	SessionMatched string `json:"sessionMatched,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	AccountEmail   string `json:"accountEmail,omitempty"`
	Client         any    `json:"client"`
	Upstream       any    `json:"upstream"`
	Gateway        any    `json:"gateway"`
}
type debugStore struct {
	mu      sync.RWMutex
	records []debugRecord
	path    string
}

func openDebugStore() *debugStore {
	p := envPath("M365_DEBUG_LOG")
	if p == "" {
		p = defaultDataPath("debug-logs.jsonl")
	}
	return &debugStore{path: p}
}

var sensitiveKeys = map[string]bool{
	"api_key": true, "apikey": true, "accesstoken": true, "authorization": true,
	"access_token": true, "refreshtoken": true, "refresh_token": true,
	"clientsecret": true, "client_secret": true, "password": true, "current_password": true,
	"new_password": true, "token": true, "bearer": true, "session_key": true,
	"secret": true, "next_token": true, "pkce_verifier": true, "code_verifier": true,
	"sessionkey": true, "nexttoken": true, "pkceverifier": true, "codeverifier": true,
	"currentpassword": true, "newpassword": true,
}

func redactBody(b []byte) any {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return string(b)
	}
	redactValue(v)
	return v
}

func redactValue(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if sensitiveKeys[strings.ToLower(k)] {
				if _, isNested := val.(map[string]any); isNested {
					redactValue(val)
				} else {
					x[k] = "[redacted]"
				}
			} else {
				redactValue(val)
			}
		}
	case []any:
		for _, e := range x {
			redactValue(e)
		}
	}
}
func debugLevel(status int) string {
	if status >= 500 {
		return "error"
	}
	if status >= 400 {
		return "warn"
	}
	return "info"
}
func debugLevelRank(level string) int {
	switch level {
	case "debug":
		return 0
	case "info":
		return 1
	case "warn":
		return 2
	case "error":
		return 3
	case "silent":
		return 4
	}
	return 1
}
func (d *debugStore) add(r debugRecord) {
	configured := currentSettings().LogLevel
	if configured == "silent" || debugLevelRank(r.Level) < debugLevelRank(configured) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.records = append(d.records, r)
	if len(d.records) > 500 {
		d.records = d.records[len(d.records)-500:]
	}
	if e := os.MkdirAll(filepath.Dir(d.path), 0700); e != nil {
		return
	}
	d.rotateIfNeededLocked()
	if f, e := os.OpenFile(d.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); e == nil {
		b, _ := json.Marshal(r)
		_, _ = f.Write(append(b, '\n'))
		_ = f.Close()
	}
}

// maxDebugLogBytes bounds a single debug-logs.jsonl file; the file is rotated
// (renamed with a timestamp) before appending when it exceeds the limit so a
// long-running server cannot fill the data directory.
const maxDebugLogBytes = 10 << 20

func (d *debugStore) rotateIfNeededLocked() {
	st, err := os.Stat(d.path)
	if err != nil || st.Size() < maxDebugLogBytes {
		return
	}
	_ = os.Rename(d.path, d.path+"."+time.Now().Format("20060102-150405"))
}
func (d *debugStore) list() []debugRecord {
	records, _ := d.listPage(1, 0)
	return records
}

func (d *debugStore) listPage(page, pageSize int) ([]debugRecord, int) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	total := len(d.records)
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = total
	}
	if pageSize > 200 {
		pageSize = 200
	}

	start := (page - 1) * pageSize
	if start >= total {
		return []debugRecord{}, total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	records := make([]debugRecord, 0, end-start)
	for i := total - 1 - start; i >= total-end; i-- {
		records = append(records, d.records[i])
	}
	return records, total
}
func (d *debugStore) get(id string) (debugRecord, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, r := range d.records {
		if r.ID == id {
			return r, true
		}
	}
	return debugRecord{}, false
}

const (
	maxDebugCaptureBytes = 256 << 10
	// Unified request-body ceilings for the open API. JSON chat endpoints
	// (chat/completions, responses, messages) are capped at 10 MiB; the
	// image endpoint may carry base64 attachments and gets a wider, still
	// bounded, 32 MiB ceiling. Multipart /images/edits keeps its own tighter
	// per-file cap (maxImageEditRequestBytes).
	maxChatRequestBody  = 10 << 20
	maxImageRequestBody = 32 << 20
)

// requestBodyLimit picks the matching body ceiling for a request path.
func requestBodyLimit(r *http.Request) int64 {
	if strings.HasPrefix(r.URL.Path, "/v1/images/") {
		return maxImageRequestBody
	}
	return maxChatRequestBody
}

type limitedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len() >= maxDebugCaptureBytes {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > maxDebugCaptureBytes-b.Len() {
		_, _ = b.Buffer.Write(p[:maxDebugCaptureBytes-b.Len()])
		b.truncated = true
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

type captureWriter struct {
	http.ResponseWriter
	status int
	body   limitedBuffer
}

func (c *captureWriter) WriteHeader(s int) { c.status = s; c.ResponseWriter.WriteHeader(s) }
func (c *captureWriter) Flush() {
	if c.status == 0 {
		c.status = 200
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (c *captureWriter) Header() http.Header { return c.ResponseWriter.Header() }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = 200
	}
	c.body.Write(b)
	return c.ResponseWriter.Write(b)
}
func (s *Server) debugMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		logLevel := currentSettings().LogLevel
		if logLevel == "silent" || debugLevelRank(logLevel) > debugLevelRank("debug") {
			next.ServeHTTP(w, r)
			return
		}
		in, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestBodyLimit(r)))
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Forward the complete body; redactBody applies the smaller capture
		// limit only when writing the debug record.
		r.Body = io.NopCloser(bytes.NewReader(in))
		cw := &captureWriter{ResponseWriter: w}
		startedAt := requestStartedAtFrom(r)
		next.ServeHTTP(cw, r)
		out := cw.body.Bytes()
		rec := debugRecord{
			ID:          "dbg_" + uuid.NewString(),
			At:          startedAt,
			Level:       debugLevel(cw.status),
			Path:        r.URL.Path,
			Method:      r.Method,
			Status:      cw.status,
			DurationMS:  time.Since(startedAt).Milliseconds(),
			TokenSource: "unavailable_from_chathub",
			CacheSource: "not_reported_by_upstream",
			Client:      redactBody(in),
			Gateway:     redactBody(out),
			Upstream:    map[string]any{"captured": false, "reason": "ChatHub transport tracing not yet attached to request context"},
		}

		// The trace middleware may pass a derived request to downstream handlers,
		// so use the retained record keyed by this request ID after the handler
		// completes. Fall back to the context pointer for compatible middleware
		// orderings.
		var tr traceRecord
		var hasTrace bool
		if requestID := requestIDFrom(r); requestID != "" {
			tr, hasTrace = s.trace.get(requestID)
		}
		if !hasTrace {
			if active := traceFromRequest(r); active != nil {
				tr = cloneTraceRecord(active)
				hasTrace = true
			}
		}
		if hasTrace {
			rec.AccountEmail = tr.AccountEmail
			rec.SessionMatched = tr.SessionMatched
			rec.ConversationID = tr.ConversationID
			if tr.TTFTMs > 0 {
				ttft := tr.TTFTMs
				rec.TTFTMS = &ttft
			}
			if tr.InputTokens > 0 || tr.OutputTokens > 0 || tr.CachedTokens > 0 {
				inputTokens := int(tr.InputTokens)
				cachedTokens := int(tr.CachedTokens)
				outputTokens := int(tr.OutputTokens)
				rec.InputTokens = &inputTokens
				rec.CachedTokens = &cachedTokens
				rec.OutputTokens = &outputTokens
				rec.TokenSource = "trace"
				rec.CacheSource = "trace"
				cacheHit := tr.CachedTokens > 0
				rec.CacheHit = &cacheHit
			}
			if tr.UpstreamReq != nil || tr.UpstreamResp != nil || tr.UpstreamError != "" {
				rec.Upstream = map[string]any{
					"request":  tr.UpstreamReq,
					"response": tr.UpstreamResp,
					"error":    tr.UpstreamError,
				}
			}
		}
		s.debug.add(rec)
	})
}
func (s *Server) debugList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pageValue := strings.TrimSpace(q.Get("page"))
	pageSizeValue := strings.TrimSpace(q.Get("pageSize"))

	// Preserve the legacy response behavior when no pagination parameters are
	// supplied, while allowing the debug page to request only its current page.
	if pageValue == "" && pageSizeValue == "" {
		jsonOut(w, map[string]any{"records": s.debug.list()})
		return
	}

	page := 1
	pageSize := 50
	if parsed, err := strconv.Atoi(pageValue); err == nil && parsed > 0 {
		page = parsed
	}
	if parsed, err := strconv.Atoi(pageSizeValue); err == nil && parsed > 0 {
		pageSize = parsed
	}
	if pageSize > 200 {
		pageSize = 200
	}

	records, total := s.debug.listPage(page, pageSize)
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	jsonOut(w, map[string]any{
		"records":    records,
		"page":       page,
		"pageSize":   pageSize,
		"total":      total,
		"totalPages": totalPages,
	})
}
func (s *Server) debugDetail(w http.ResponseWriter, r *http.Request) {
	if x, ok := s.debug.get(r.URL.Query().Get("id")); ok {
		jsonOut(w, x)
		return
	}
	writeOpenAIError(w, http.StatusNotFound, "not_found", "not found")
}
