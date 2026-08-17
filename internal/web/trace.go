package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxTraceCaptureBytes bounds how much of each request/response body is kept.
const maxTraceCaptureBytes = 256 << 10

// defaultTraceMaxRecords is how many full request captures the debug mode keeps
// unless the operator changes the value in settings.
const defaultTraceMaxRecords = 50

// traceRecord is one debug-mode entry that follows a request through its whole
// lifecycle: downstream request -> upstream (ChatHub) request -> upstream
// response -> downstream response, with a live status that flips from
// "in_progress" to "success" or "error".
type traceRecord struct {
	ID             string    `json:"id"`
	At             time.Time `json:"at"`
	Endpoint       string    `json:"endpoint"`
	Method         string    `json:"method"`
	Model          string    `json:"model,omitempty"`
	ReasoningLevel string    `json:"reasoningLevel,omitempty"`
	Stream         bool      `json:"stream"`
	Status         string    `json:"status"` // in_progress | success | error
	StatusCode     int       `json:"statusCode,omitempty"`
	DurationMs     int64     `json:"durationMs,omitempty"`
	TTFTMs         int64     `json:"ttftMs,omitempty"`
	InputTokens    int64     `json:"inputTokens,omitempty"`
	OutputTokens   int64     `json:"outputTokens,omitempty"`
	SpeedTPs       float64   `json:"speedTps,omitempty"`
	AccountEmail   string    `json:"accountEmail,omitempty"`
	APIKeyPrefix   string    `json:"apiKeyPrefix,omitempty"`
	Error          string    `json:"error,omitempty"`
	DownstreamReq  any       `json:"downstreamReq,omitempty"`
	DownstreamResp any       `json:"downstreamResp,omitempty"`
	UpstreamReq    any       `json:"upstreamReq,omitempty"`
	UpstreamResp   any       `json:"upstreamResp,omitempty"`
	UpstreamError  string    `json:"upstreamError,omitempty"`
}

// traceStore keeps a bounded ring of traceRecord entries. The bound is read
// from the runtime settings on every mutation so the operator can change it
// without a restart. Old entries are dropped automatically once the bound is
// exceeded.
type traceStore struct {
	mu     sync.RWMutex
	byID   map[string]*traceRecord
	active map[string]*traceRecord
}

func openTraceStore() *traceStore {
	return &traceStore{byID: map[string]*traceRecord{}, active: map[string]*traceRecord{}}
}

func traceEnabled() bool {
	enabled, _ := traceConfig()
	return enabled
}

// begin registers a new in-progress trace record. It returns nil when tracing
// is disabled so callers can skip work cheaply.
func (t *traceStore) begin(rec *traceRecord) *traceRecord {
	if !traceEnabled() {
		return nil
	}
	if rec.ID == "" {
		rec.ID = "trace_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	if rec.Status == "" {
		rec.Status = "in_progress"
	}
	t.mu.Lock()
	t.byID[rec.ID] = rec
	t.active[rec.ID] = rec
	t.trimLocked()
	t.mu.Unlock()
	return rec
}

// update mutates an existing trace record under the store lock. It is a no-op
// when the request was never traced (disabled or unknown id).
func (t *traceStore) update(id string, fn func(*traceRecord)) {
	if id == "" {
		return
	}
	t.mu.Lock()
	if rec, ok := t.byID[id]; ok && fn != nil {
		fn(rec)
	}
	t.mu.Unlock()
}

// finish marks a record as terminal, removing it from the active set. Records
// that receive no explicit status from the handler default to "success".
func (t *traceStore) finish(id string, fn func(*traceRecord)) {
	t.mu.Lock()
	if rec, ok := t.byID[id]; ok {
		if fn != nil {
			fn(rec)
		}
		delete(t.active, id)
		if rec.Status == "" || rec.Status == "in_progress" {
			rec.Status = "success"
		}
		if rec.StatusCode == 0 && rec.Status == "error" {
			rec.StatusCode = http.StatusBadGateway
		}
	}
	t.trimLocked()
	t.mu.Unlock()
}

func (t *traceStore) activeCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.active)
}

// trimLocked drops the oldest records when the ring exceeds the configured
// maximum. In-progress records are never evicted by this pass to avoid deleting
// a record the handler is still writing to; the overflow is bounded by the
// explicit active-set cap below.
func (t *traceStore) trimLocked() {
	_, max := traceConfig()
	t.trimToLocked(max)
}

// trimToLocked drops the oldest records when the ring exceeds max. In-progress
// records are never evicted by this pass to avoid deleting a record the handler
// is still writing to; if even the active set would exceed the bound (a burst
// of concurrent requests), the oldest active records are dropped too.
func (t *traceStore) trimToLocked(max int) {
	if max <= 0 {
		max = defaultTraceMaxRecords
	}
	all := make([]*traceRecord, 0, len(t.byID))
	for _, rec := range t.byID {
		all = append(all, rec)
	}
	if len(all) <= max {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	drop := all[max:]
	for _, rec := range drop {
		if _, inFlight := t.active[rec.ID]; inFlight {
			continue
		}
		delete(t.byID, rec.ID)
	}
	// Hard cap: if the active set itself would exceed the bound after the pass
	// (a burst of concurrent requests), evict the oldest active records too.
	if len(t.byID) > max {
		again := make([]*traceRecord, 0, len(t.byID))
		for _, rec := range t.byID {
			again = append(again, rec)
		}
		sort.Slice(again, func(i, j int) bool { return again[i].At.After(again[j].At) })
		for _, rec := range again[max:] {
			delete(t.byID, rec.ID)
			delete(t.active, rec.ID)
		}
	}
}

// list returns all retained records newest-first as deep copies so concurrent
// handler updates never race with the admin API reader.
func (t *traceStore) list() []traceRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()
	raw := make([]traceRecord, 0, len(t.byID))
	for _, rec := range t.byID {
		raw = append(raw, *rec)
	}
	var out []traceRecord
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// clear drops every retained trace record (used by the console "clear" action).
func (t *traceStore) clear() {
	t.mu.Lock()
	t.byID = map[string]*traceRecord{}
	t.active = map[string]*traceRecord{}
	t.mu.Unlock()
}

func (s *Server) adminTraceStatus(w http.ResponseWriter, r *http.Request) {
	enabled, max := traceConfig()
	all := s.trace.list()
	q := r.URL.Query()
	// Optional id filter returns at most the matching record (used by the
	// console detail view, which may be paged out of the current listing).
	if id := strings.TrimSpace(q.Get("id")); id != "" {
		for _, rec := range all {
			if rec.ID == id {
				jsonOut(w, map[string]any{"enabled": enabled, "max": max, "active": s.trace.activeCount(), "total": 1, "records": []traceRecord{rec}})
				return
			}
		}
		jsonOut(w, map[string]any{"enabled": enabled, "max": max, "active": s.trace.activeCount(), "total": 0, "records": []traceRecord{}})
		return
	}
	limit := 10
	offset := 0
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
	total := len(all)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	jsonOut(w, map[string]any{"enabled": enabled, "max": max, "active": s.trace.activeCount(), "total": total, "records": all[offset:end]})
}

// adminTrace handles debug-mode configuration: GET returns the live config,
// POST sets enabled/max atomically through the settings store.
func (s *Server) adminTrace(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		enabled, max := traceConfig()
		jsonOut(w, map[string]any{"enabled": enabled, "max": max})
	case http.MethodPost:
		var patch struct {
			Enabled *bool `json:"enabled"`
			Max     *int  `json:"max"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&patch) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		cur := s.settings.get()
		if patch.Enabled != nil {
			cur.TraceEnabled = *patch.Enabled
		}
		if patch.Max != nil {
			cur.TraceMaxRecords = *patch.Max
		}
		if e := s.settings.save(cur); e != nil {
			writeOpenAIError(w, 400, "invalid_request_error", e.Error())
			return
		}
		if !cur.TraceEnabled {
			s.trace.clear()
		} else {
			// Reclaim records immediately when the bound was reduced.
			s.trace.trimToLocked(traceMaxNorm(cur.TraceMaxRecords))
		}
		jsonOut(w, map[string]any{"ok": true, "enabled": cur.TraceEnabled, "max": cur.TraceMaxRecords})
	default:
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
	}
}

// adminTraceClear empties all retained trace records.
func (s *Server) adminTraceClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	s.trace.clear()
	jsonOut(w, map[string]any{"ok": true})
}

type traceKey struct{}

// traceMaxNorm normalizes a configured record bound into the effective range.
func traceMaxNorm(max int) int {
	if max <= 0 {
		return defaultTraceMaxRecords
	}
	if max > 2000 {
		return 2000
	}
	return max
}

// traceFromRequest finds the active trace record attached to a request by the
// traceCaptureMiddleware. It is a no-op (returns nil) when tracing is disabled.
func traceFromRequest(r *http.Request) *traceRecord {
	if v, ok := r.Context().Value(traceKey{}).(*traceRecord); ok {
		return v
	}
	return nil
}

// traceCaptureMiddleware is the debug-mode capture point. When tracing is
// enabled it snapshots the downstream request body and the exact bytes written
// back to the client (the downstream response), and keeps the record live while
// the handler runs so in-flight status is visible in the console.
func (s *Server) traceCaptureMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enabled, _ := traceConfig()
		if !enabled || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestBodyLimit(r)))
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &traceRecord{
			ID:            requestIDFrom(r),
			At:            time.Now(),
			Endpoint:      r.URL.Path,
			Method:        r.Method,
			Status:        "in_progress",
			DownstreamReq: redactBody(body),
		}
		if rec.ID == "" {
			rec.ID = "trace_" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		if s.trace.begin(rec) == nil {
			next.ServeHTTP(w, r)
			return
		}
		rc := &traceResponseWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rc, r.WithContext(context.WithValue(r.Context(), traceKey{}, rec)))
		s.trace.update(rec.ID, func(x *traceRecord) {
			x.DownstreamResp = redactBody(rc.body.Bytes())
			x.StatusCode = rc.status
			x.DurationMs = time.Since(start).Milliseconds()
		})
		s.trace.finish(rec.ID, func(x *traceRecord) {
			if x.StatusCode >= 400 || x.Error != "" {
				x.Status = "error"
			}
			if x.Status == "in_progress" {
				if x.Error != "" {
					x.Status = "error"
				} else {
					x.Status = "success"
				}
			}
		})
	})
}

// traceResponseWriter captures the downstream response while streaming it
// through to the client. Body capture is capped like the debug middleware so
// SSE and image responses never blow up the ring buffer.
type traceResponseWriter struct {
	http.ResponseWriter
	status int
	body   limitedBuffer
}

func (t *traceResponseWriter) WriteHeader(s int) {
	if t.status == 0 {
		t.status = s
	}
	t.ResponseWriter.WriteHeader(s)
}
func (t *traceResponseWriter) Flush() {
	if t.status == 0 {
		t.status = 200
	}
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (t *traceResponseWriter) Header() http.Header { return t.ResponseWriter.Header() }
func (t *traceResponseWriter) Write(b []byte) (int, error) {
	if t.status == 0 {
		t.status = 200
	}
	if t.body.Len() < maxTraceCaptureBytes {
		t.body.Write(b)
	}
	return t.ResponseWriter.Write(b)
}
