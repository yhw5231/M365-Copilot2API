package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// maxTraceCaptureBytes bounds how much of each request/response body is kept.
const maxTraceCaptureBytes = 256 << 10

// captureLimit bounds a captured body to maxTraceCaptureBytes, cutting on a
// rune boundary and appending a visible truncation marker so console readers
// can tell a captured prefix from the complete body. Note: the marker makes an
// originally-JSON body invalid JSON, so redactBody then keeps it as a plain
// string — acceptable for upstream chat payloads, which carry no credential
// keys.
func captureLimit(s string) string {
	if len(s) <= maxTraceCaptureBytes {
		return s
	}
	const marker = "\n…[trace capture truncated]"
	cut := maxTraceCaptureBytes - len(marker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

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
	CachedTokens   int64     `json:"cachedTokens,omitempty"`
	SpeedTPs       float64   `json:"speedTps,omitempty"`
	// SessionMatched is the session-resolver hit kind (e.g. explicit_prefix_3,
	// explicit_incremental). Empty means the request started a fresh upstream
	// conversation — the debug console uses it to answer "did the session
	// match?" without digging through logs.
	SessionMatched string `json:"sessionMatched,omitempty"`
	// ConversationID is the upstream conversation that served (or was matched
	// for) this request.
	ConversationID string `json:"conversationId,omitempty"`
	AccountEmail   string `json:"accountEmail,omitempty"`
	APIKeyPrefix   string `json:"apiKeyPrefix,omitempty"`
	Error          string `json:"error,omitempty"`
	DownstreamReq  any    `json:"downstreamReq,omitempty"`
	DownstreamResp any    `json:"downstreamResp,omitempty"`
	UpstreamReq    any    `json:"upstreamReq,omitempty"`
	UpstreamResp   any    `json:"upstreamResp,omitempty"`
	UpstreamError  string `json:"upstreamError,omitempty"`
}

// traceStore keeps a bounded ring of traceRecord entries. The bound is read
// from the runtime settings on every mutation so the operator can change it
// without a restart. Old entries are dropped automatically once the bound is
// exceeded.
type traceStore struct {
	mu     sync.RWMutex
	path   string
	byID   map[string]*traceRecord
	active map[string]*traceRecord
}

func traceStorePath() string {
	if p := envPath("M365_TRACE_FILE"); p != "" {
		return p
	}
	return defaultDataPath("trace-records.json")
}

func openTraceStore() *traceStore {
	t := &traceStore{
		path:   traceStorePath(),
		byID:   map[string]*traceRecord{},
		active: map[string]*traceRecord{},
	}
	if b, err := os.ReadFile(t.path); err == nil {
		var records []traceRecord
		if json.Unmarshal(b, &records) == nil {
			for i := range records {
				rec := records[i]
				if rec.ID == "" {
					continue
				}
				if rec.Status == "in_progress" {
					rec.Status = "error"
					if rec.Error == "" {
						rec.Error = "server restarted before request completed"
					}
				}
				t.byID[rec.ID] = &rec
			}
		}
	}
	t.trimLocked()
	_ = t.persistLocked()
	return t
}

// persistLocked atomically saves retained trace records to the server data
// directory. The caller must hold t.mu when concurrent access is possible.
func (t *traceStore) persistLocked() error {
	records := make([]traceRecord, 0, len(t.byID))
	for _, rec := range t.byID {
		records = append(records, *rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].At.Before(records[j].At) })
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(t.path, b, 0600)
}

func traceEnabled() bool {
	enabled, _ := traceConfig()
	return enabled
}

// begin registers a new in-progress trace record. It returns nil when tracing
// is disabled so callers can skip work cheaply.
func (t *traceStore) begin(rec *traceRecord) *traceRecord {
	return t.beginFor(rec, traceEnabled())
}

// beginFor registers a record under an explicitly resolved switch. The capture
// middleware resolves the switch from the server's own settings store (which in
// production is the same store the traceConfig singleton reads) so tests can
// drive the debug mode deterministically.
func (t *traceStore) beginFor(rec *traceRecord, enabled bool) *traceRecord {
	if !enabled {
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

// update mutates the same in-memory trace record throughout the request. It is
// deliberately not persisted here: streaming deltas and multiple upstream WS
// stages belong to one downstream request and must not create partial records
// on disk. The completed record is persisted atomically by finish.
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
	_ = t.persistLocked()
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

// cloneTraceRecord returns a detached copy so readers never race with handler
// updates to the retained record.
func cloneTraceRecord(rec *traceRecord) traceRecord {
	if rec == nil {
		return traceRecord{}
	}
	return *rec
}

// cloneTraceSummary returns only the lightweight fields needed by the trace
// list. Full request and response payloads are fetched separately by id when
// the operator opens a record.
func cloneTraceSummary(rec *traceRecord) traceRecord {
	if rec == nil {
		return traceRecord{}
	}
	return traceRecord{
		ID:             rec.ID,
		At:             rec.At,
		Endpoint:       rec.Endpoint,
		Method:         rec.Method,
		Model:          rec.Model,
		ReasoningLevel: rec.ReasoningLevel,
		Stream:         rec.Stream,
		Status:         rec.Status,
		StatusCode:     rec.StatusCode,
		DurationMs:     rec.DurationMs,
		TTFTMs:         rec.TTFTMs,
		InputTokens:    rec.InputTokens,
		OutputTokens:   rec.OutputTokens,
		CachedTokens:   rec.CachedTokens,
		SpeedTPs:       rec.SpeedTPs,
		SessionMatched: rec.SessionMatched,
		ConversationID: rec.ConversationID,
		AccountEmail:   rec.AccountEmail,
		APIKeyPrefix:   rec.APIKeyPrefix,
		Error:          rec.Error,
		UpstreamError:  rec.UpstreamError,
	}
}

// get returns one detached record without copying or sorting the full store.
func (t *traceStore) get(id string) (traceRecord, bool) {
	t.mu.RLock()
	rec, ok := t.byID[id]
	if !ok {
		t.mu.RUnlock()
		return traceRecord{}, false
	}
	out := cloneTraceRecord(rec)
	t.mu.RUnlock()
	return out, true
}

// page returns retained records newest-first. It sorts lightweight pointers and
// copies only the requested page instead of JSON-round-tripping the full store.
func (t *traceStore) page(limit, offset int) ([]traceRecord, int, int) {
	t.mu.RLock()
	all := make([]*traceRecord, 0, len(t.byID))
	for _, rec := range t.byID {
		all = append(all, rec)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	total := len(all)
	active := len(t.active)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]traceRecord, 0, end-offset)
	for _, rec := range all[offset:end] {
		out = append(out, cloneTraceSummary(rec))
	}
	t.mu.RUnlock()
	return out, total, active
}

// list remains available for existing internal callers and tests.
func (t *traceStore) list() []traceRecord {
	out, _, _ := t.page(len(t.byID), 0)
	return out
}

// clear drops every retained trace record (used by the console "clear" action).
func (t *traceStore) clear() {
	t.mu.Lock()
	t.byID = map[string]*traceRecord{}
	t.active = map[string]*traceRecord{}
	_ = t.persistLocked()
	t.mu.Unlock()
}

func (s *Server) adminTraceStatus(w http.ResponseWriter, r *http.Request) {
	enabled, max := traceConfig()
	q := r.URL.Query()
	// Optional id lookup avoids copying and sorting the complete trace store.
	if id := strings.TrimSpace(q.Get("id")); id != "" {
		if rec, ok := s.trace.get(id); ok {
			jsonOut(w, map[string]any{"enabled": enabled, "max": max, "active": s.trace.activeCount(), "total": 1, "records": []traceRecord{rec}})
			return
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
	records, total, active := s.trace.page(limit, offset)
	jsonOut(w, map[string]any{"enabled": enabled, "max": max, "active": active, "total": total, "records": records})
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

// traceEnabledCurrent resolves the debug-trace switch from the server's own
// settings store when available; in production that store IS the settings
// singleton, so behavior is unchanged, while tests can inject a standalone
// store to drive the switch deterministically.
func (s *Server) traceEnabledCurrent() bool {
	if s != nil && s.settings != nil {
		return s.settings.get().TraceEnabled
	}
	return traceEnabled()
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
//
// Independent of the debug switch, every /v1/ request is observed so a failed
// request (non-2xx status, handler panic, or a handler-reported error) lands in
// the error-record ring — the error console must stay useful when the full
// debug capture is off.
func (s *Server) traceCaptureMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enabled := s.traceEnabledCurrent()
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestBodyLimit(r)))
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		startedAt := requestStartedAtFrom(r)
		rec := &traceRecord{
			ID:       requestIDFrom(r),
			At:       startedAt,
			Endpoint: r.URL.Path,
			Method:   r.Method,
			Status:   "in_progress",
		}
		if rec.ID == "" {
			rec.ID = "trace_" + strconv.FormatInt(time.Now().UnixNano(), 10)
		}
		if enabled {
			// The live console shows the request payload while the handler runs,
			// so the redacted body lands on the record before it begins.
			rec.DownstreamReq = redactBody(body)
			if s.trace.beginFor(rec, enabled) == nil {
				next.ServeHTTP(w, r)
				return
			}
		}
		rc := &traceResponseWriter{ResponseWriter: w}
		start := time.Now()
		// Recover any handler panic so the trace record is always finalized
		// (DownstreamResp, StatusCode, DurationMs, Status) instead of staying
		// stuck in_progress with downstream — and status 0. The panic is
		// re-raised after the update so recoverPanics (outermost middleware)
		// still writes the 500 error to the client.
		var panicVal any
		if enabled {
			var ctx context.Context
			ctx = context.WithValue(r.Context(), traceKey{}, rec)
			func() {
				defer func() {
					if v := recover(); v != nil {
						panicVal = v
					}
				}()
				next.ServeHTTP(rc, r.WithContext(ctx))
			}()
			s.trace.update(rec.ID, func(x *traceRecord) {
				x.DownstreamResp = redactBody([]byte(captureLimit(rc.body.String())))
				x.StatusCode = rc.status
				x.DurationMs = time.Since(start).Milliseconds()
			})
			s.trace.finish(rec.ID, func(x *traceRecord) {
				if panicVal != nil {
					x.Error = fmt.Sprintf("handler panic: %v", panicVal)
					x.Status = "error"
					return
				}
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
			// Mirror the finalized record into the error ring when the request
			// failed; the copy carries the full debug payloads. Handlers that
			// reject a request before reporting a trace error (early 4xx
			// validation paths) leave Error empty — recover the message from
			// the captured response body so the error console never shows a
			// blank reason.
			if finished, ok := s.trace.get(rec.ID); ok && finished.Status == "error" {
				if finished.Error == "" {
					finished.Error = extractErrorText([]byte(captureLimit(rc.body.String())))
				}
				s.errors.record(&finished)
			}
		} else {
			func() {
				defer func() {
					if v := recover(); v != nil {
						panicVal = v
					}
				}()
				next.ServeHTTP(rc, r)
			}()
			// Debug capture is off: still keep a self-contained error record
			// (redacted request/response bodies, status, error message) so the
			// error console works without the trace overhead.
				if panicVal != nil || rc.status >= 400 {
					errRec := &traceRecord{
						ID:             "err_" + strconv.FormatInt(time.Now().UnixNano(), 10),
						At:             startedAt,
						Endpoint:       r.URL.Path,
						Method:         r.Method,
						Status:         "error",
						StatusCode:     rc.status,
						DurationMs:     time.Since(start).Milliseconds(),
						APIKeyPrefix:   apiKeyPrefix(r),
						DownstreamReq:  redactBody(body),
						DownstreamResp: redactBody([]byte(captureLimit(rc.body.String()))),
					}
				if panicVal != nil {
					errRec.Error = fmt.Sprintf("handler panic: %v", panicVal)
				} else {
					errRec.Error = extractErrorText([]byte(captureLimit(rc.body.String())))
				}
				if reqModel := peekRequestModel(body); reqModel != "" {
					errRec.Model = reqModel
				}
				s.errors.record(errRec)
			}
		}
		if panicVal != nil {
			panic(panicVal)
		}
	})
}

// peekRequestModel extracts the "model" field from a JSON request body without
// fully validating it — a best-effort label for error records captured while
// the debug capture is off.
func peekRequestModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.Model
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

// Unwrap lets http.NewResponseController reach the real underlying writer so
// SetWriteDeadline (used by writeSSE / sseDataRaw / emitText) is applied to
// the actual socket instead of silently failing. Without it a write to a dead
// or stalled client can block the handler goroutine indefinitely, leaving the
// trace record stuck in_progress with no downstream response.
func (t *traceResponseWriter) Unwrap() http.ResponseWriter {
	return t.ResponseWriter
}
