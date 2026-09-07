package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestErrorStore(t *testing.T) *errorStore {
	t.Helper()
	t.Setenv("M365_ERROR_FILE", filepath.Join(t.TempDir(), "error-records.json"))
	return openErrorStore()
}

func TestErrorStoreDefaultKeepsFifty(t *testing.T) {
	e := newTestErrorStore(t)
	for i := 0; i < 80; i++ {
		e.record(&traceRecord{ID: "err-" + string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('a'+i/26%26)), At: time.Now().Add(time.Duration(i) * time.Millisecond)})
	}
	if got := len(e.byID); got != defaultErrorMaxRecords {
		t.Fatalf("records retained=%d want default %d", got, defaultErrorMaxRecords)
	}
	// The oldest records must have been dropped, newest kept.
	records, total := e.page(200, 0)
	if total != defaultErrorMaxRecords {
		t.Fatalf("page total=%d want %d", total, defaultErrorMaxRecords)
	}
	if len(records) == 0 || records[0].At.Before(records[len(records)-1].At) {
		t.Fatalf("page must be newest-first, got %+v", records)
	}
}

func TestErrorStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "error-records.json")
	t.Setenv("M365_ERROR_FILE", path)
	e := openErrorStore()
	e.record(&traceRecord{ID: "err-1", Endpoint: "/v1/chat/completions", Error: "upstream boom"})
	reopened := openErrorStore()
	rec, ok := reopened.get("err-1")
	if !ok || rec.Error != "upstream boom" {
		t.Fatalf("record not persisted: %+v ok=%v", rec, ok)
	}
}

func TestErrorMaxNormDefaultsTo50(t *testing.T) {
	if got := errorMaxNorm(0); got != defaultErrorMaxRecords {
		t.Fatalf("errorMaxNorm(0)=%d want %d", got, defaultErrorMaxRecords)
	}
	if got := errorMaxNorm(9999); got != 2000 {
		t.Fatalf("errorMaxNorm(9999)=%d want 2000", got)
	}
}

func TestExtractErrorTextPrefersOpenAIShape(t *testing.T) {
	body := []byte(`{"error":{"message":"upstream exploded","code":"upstream_error"}}`)
	if got := extractErrorText(body); got != "upstream_error: upstream exploded" {
		t.Fatalf("extractErrorText=%q", got)
	}
	if got := extractErrorText([]byte("plain gateway failure")); got != "plain gateway failure" {
		t.Fatalf("extractErrorText=%q", got)
	}
	if got := extractErrorText(nil); got != "" {
		t.Fatalf("extractErrorText(nil)=%q", got)
	}
}

// errorTestSettings builds a store with an explicit trace switch so the capture
// tests never depend on the package-wide settings singleton.
func errorTestSettings(t *testing.T, traceEnabled bool) *settingsStore {
	t.Helper()
	st := newSettingsStore(filepath.Join(t.TempDir(), "settings.json"), filepath.Join(t.TempDir(), "account-settings.json"))
	v := defaultRuntimeSettings()
	v.TraceEnabled = traceEnabled
	if err := st.save(v); err != nil {
		t.Fatal(err)
	}
	return st
}

// TestErrorCaptureWithoutTraceMode drives the capture middleware with the debug
// trace disabled: a failed /v1/ request must land in the error ring with the
// downstream payloads, while successful requests never do.
func TestErrorCaptureWithoutTraceMode(t *testing.T) {
	e := newTestErrorStore(t)
	s := &Server{errors: e, settings: errorTestSettings(t, false)}
	if traceEnabled() {
		t.Fatal("debug trace must be disabled for this test")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"inner chat request failed","code":"upstream_error"}}`))
	})
	srv := httptest.NewServer(s.traceCaptureMiddleware(handler))
	defer srv.Close()

	// Failure: recorded.
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"gpt-5.6","stream":false}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	records, total := e.page(10, 0)
	if total != 1 || records[0].StatusCode != http.StatusBadGateway {
		t.Fatalf("error record missing: total=%d records=%+v", total, records)
	}
	if records[0].Error != "upstream_error: inner chat request failed" {
		t.Fatalf("error message=%q", records[0].Error)
	}
	full, ok := e.get(records[0].ID)
	if !ok {
		t.Fatal("record not found by id")
	}
	if full.Model != "gpt-5.6" {
		t.Fatalf("model label=%q want gpt-5.6", full.Model)
	}
	reqJSON, err := json.Marshal(full.DownstreamReq)
	if err != nil || !strings.Contains(string(reqJSON), "gpt-5.6") {
		t.Fatalf("downstream request not captured: %s (%v)", string(reqJSON), err)
	}
	respJSON, _ := json.Marshal(full.DownstreamResp)
	if !strings.Contains(string(respJSON), "inner chat request failed") {
		t.Fatalf("downstream response not captured: %s", string(respJSON))
	}

	// Success: not recorded.
	ok2, err := http.Post(srv.URL+"/v1/models", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	ok2.Body.Close()
	if _, total := e.page(10, 0); total != 1 {
		t.Fatalf("successful request must not be recorded, total=%d", total)
	}
}

// TestErrorCaptureMirrorsTraceRecord verifies the debug-mode path: a handler
// that reports an error through the live trace record is mirrored into the
// error ring with its full payloads.
func TestErrorCaptureMirrorsTraceRecord(t *testing.T) {
	e := newTestErrorStore(t)
	s := &Server{errors: e, trace: openTraceStore(), settings: errorTestSettings(t, true)}
	if !s.traceEnabledCurrent() {
		t.Fatal("debug trace must be enabled for this test")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tr := traceFromRequest(r); tr != nil {
			s.trace.update(tr.ID, func(x *traceRecord) {
				x.Model = "gpt-5.6-luna"
				x.Error = "ChatHub returned no text or tool call"
			})
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: response.failed"))
	})
	srv := httptest.NewServer(s.traceCaptureMiddleware(handler))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5.6-luna"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	records, total := e.page(10, 0)
	if total != 1 {
		t.Fatalf("handler-reported error must be mirrored, total=%d", total)
	}
	if records[0].Error != "ChatHub returned no text or tool call" || records[0].Model != "gpt-5.6-luna" {
		t.Fatalf("mirrored record=%+v", records[0])
	}
}
