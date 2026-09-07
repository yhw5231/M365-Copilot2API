package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultErrorMaxRecords is how many error records the gateway keeps unless the
// operator raises the bound in the console. Errors are recorded independently
// of the debug mode so a failed request stays inspectable even when the full
// request capture is off.
const defaultErrorMaxRecords = 50

// errorStore keeps a bounded ring of failed /v1/ requests. Records reuse the
// traceRecord shape so the error console renders the same lifecycle payload
// (downstream request / upstream exchange / downstream response) as the debug
// console.
type errorStore struct {
	mu   sync.RWMutex
	path string
	byID map[string]*traceRecord
}

func errorStorePath() string {
	if p := envPath("M365_ERROR_FILE"); p != "" {
		return p
	}
	return defaultDataPath("error-records.json")
}

func openErrorStore() *errorStore {
	e := &errorStore{path: errorStorePath(), byID: map[string]*traceRecord{}}
	if b, err := os.ReadFile(e.path); err == nil {
		var records []traceRecord
		if json.Unmarshal(b, &records) == nil {
			for i := range records {
				rec := records[i]
				if rec.ID == "" {
					continue
				}
				if rec.Status == "" {
					rec.Status = "error"
				}
				e.byID[rec.ID] = &rec
			}
		}
	}
	e.trimToLocked(errorMaxRecords())
	_ = e.persistLocked()
	return e
}

// persistLocked atomically saves retained error records oldest-first, the same
// on-disk convention as the trace store.
func (e *errorStore) persistLocked() error {
	records := make([]traceRecord, 0, len(e.byID))
	for _, rec := range e.byID {
		records = append(records, *rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].At.Before(records[j].At) })
	b, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(e.path, b, 0600)
}

// record inserts a finalized error record, dropping the oldest entries beyond
// the configured bound. A nil store (tests constructing a bare Server) is a
// no-op so the capture middleware never has to guard.
func (e *errorStore) record(rec *traceRecord) {
	if e == nil || rec == nil {
		return
	}
	if rec.ID == "" {
		rec.ID = "err_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	rec.Status = "error"
	e.mu.Lock()
	e.byID[rec.ID] = rec
	e.trimToLocked(errorMaxRecords())
	_ = e.persistLocked()
	e.mu.Unlock()
}

func (e *errorStore) trimToLocked(max int) {
	if len(e.byID) <= max {
		return
	}
	all := make([]*traceRecord, 0, len(e.byID))
	for _, rec := range e.byID {
		all = append(all, rec)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	for _, rec := range all[max:] {
		delete(e.byID, rec.ID)
	}
}

// trimTo reclaims records immediately when the operator lowers the bound.
func (e *errorStore) trimTo(max int) {
	e.mu.Lock()
	e.trimToLocked(max)
	_ = e.persistLocked()
	e.mu.Unlock()
}

func (e *errorStore) get(id string) (traceRecord, bool) {
	e.mu.RLock()
	rec, ok := e.byID[id]
	if !ok {
		e.mu.RUnlock()
		return traceRecord{}, false
	}
	out := *rec
	e.mu.RUnlock()
	return out, true
}

// page returns retained error records newest-first as lightweight summaries.
func (e *errorStore) page(limit, offset int) ([]traceRecord, int) {
	e.mu.RLock()
	all := make([]*traceRecord, 0, len(e.byID))
	for _, rec := range e.byID {
		all = append(all, rec)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].At.After(all[j].At) })
	total := len(all)
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
	e.mu.RUnlock()
	return out, total
}

func (e *errorStore) clear() {
	e.mu.Lock()
	e.byID = map[string]*traceRecord{}
	_ = e.persistLocked()
	e.mu.Unlock()
}

// errorMaxNorm clamps the configured bound; 0 or negative falls back to the
// 50-record default so a stale settings file can never disable error capture.
func errorMaxNorm(max int) int {
	if max <= 0 {
		return defaultErrorMaxRecords
	}
	if max > 2000 {
		return 2000
	}
	return max
}

func errorMaxRecords() int {
	return errorMaxNorm(currentSettings().ErrorMaxRecords)
}

// extractErrorText pulls a human-readable message out of a gateway error
// response: the OpenAI error shape when present, otherwise a bounded excerpt of
// the raw body so the console never shows a blank error line.
func extractErrorText(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var shaped struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &shaped) == nil && shaped.Error.Message != "" {
		msg := shaped.Error.Message
		if shaped.Error.Code != nil {
			msg = fmt.Sprintf("%v: %s", shaped.Error.Code, shaped.Error.Message)
		}
		return msg
	}
	const maxLen = 300
	if len(trimmed) > maxLen {
		return trimmed[:maxLen] + "…"
	}
	return trimmed
}

func (s *Server) adminErrorRecordsStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if id := strings.TrimSpace(q.Get("id")); id != "" {
		if rec, ok := s.errors.get(id); ok {
			jsonOut(w, map[string]any{"max": errorMaxRecords(), "total": 1, "records": []traceRecord{rec}})
			return
		}
		jsonOut(w, map[string]any{"max": errorMaxRecords(), "total": 0, "records": []traceRecord{}})
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
	records, total := s.errors.page(limit, offset)
	jsonOut(w, map[string]any{"max": errorMaxRecords(), "total": total, "records": records})
}

// adminErrorRecords handles the error-record bound: GET returns the live
// config, POST saves it through the settings store.
func (s *Server) adminErrorRecords(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"max": errorMaxRecords()})
	case http.MethodPost:
		var patch struct {
			Max *int `json:"max"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&patch) != nil || patch.Max == nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		cur := s.settings.get()
		cur.ErrorMaxRecords = *patch.Max
		if e := s.settings.save(cur); e != nil {
			writeOpenAIError(w, 400, "invalid_request_error", e.Error())
			return
		}
		// Reclaim records immediately when the bound was reduced.
		s.errors.trimTo(errorMaxNorm(cur.ErrorMaxRecords))
		jsonOut(w, map[string]any{"ok": true, "max": errorMaxNorm(cur.ErrorMaxRecords)})
	default:
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
	}
}

// adminErrorRecordsClear empties all retained error records.
func (s *Server) adminErrorRecordsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	s.errors.clear()
	jsonOut(w, map[string]any{"ok": true})
}
