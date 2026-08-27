package web

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

// withTraceEnabled flips the runtime tracing flag on for the duration of fn
// and restores the prior value afterwards, following the pattern used by the
// other settings-dependent tests in this package.
func withTraceEnabled(t *testing.T, fn func()) {
	t.Helper()
	prior := openSettingsStore().v.TraceEnabled
	openSettingsStore().mu.Lock()
	openSettingsStore().v.TraceEnabled = true
	openSettingsStore().mu.Unlock()
	defer func() {
		openSettingsStore().mu.Lock()
		openSettingsStore().v.TraceEnabled = prior
		openSettingsStore().mu.Unlock()
	}()
	fn()
}

// TestRouteUpstreamTraceResponseStaysInProgress guards the streaming debug
// status: receiving the upstream (ChatHub) completion must NOT flip the trace
// record to "success", because the handler may still be streaming deltas
// downstream (SSE) or running post-processing. The final status is applied by
// the trace middleware's finish callback only after the handler returns.
func TestRouteUpstreamTraceResponseStaysInProgress(t *testing.T) {
	withTraceEnabled(t, func() {
		st := openTraceStore()
		st.byID["t-upstream-response"] = &traceRecord{
			ID:     "t-upstream-response",
			At:     time.Now(),
			Status: "in_progress",
		}
		s := &Server{trace: st}

		s.routeUpstreamTrace("t-upstream-response", "upstream_response", map[string]any{
			"text":         "hello world",
			"reasoning":    "",
			"events":       3,
			"ttft_ms":      int64(120),
			"text_preview": "hello wor",
		})

		got, ok := s.trace.get("t-upstream-response")
		if !ok {
			t.Fatal("trace record missing after upstream_response")
		}
		if got.Status != "in_progress" {
			t.Fatalf("status after upstream_response=%q want in_progress (handler still finishing)", got.Status)
		}
		if got.UpstreamResp == nil {
			t.Fatal("upstream response payload was not captured")
		}
		if got.TTFTMs != 120 {
			t.Fatalf("ttft_ms=%d want 120", got.TTFTMs)
		}
	})
}

// TestRouteUpstreamTraceErrorMarksTerminalError verifies an upstream error is
// still reflected on the trace immediately.
func TestRouteUpstreamTraceErrorMarksTerminalError(t *testing.T) {
	withTraceEnabled(t, func() {
		st := openTraceStore()
		st.byID["t-upstream-error"] = &traceRecord{
			ID:     "t-upstream-error",
			At:     time.Now(),
			Status: "in_progress",
		}
		s := &Server{trace: st}

		s.routeUpstreamTrace("t-upstream-error", "upstream_error", map[string]any{"error": "ws dial refused"})

		got, ok := s.trace.get("t-upstream-error")
		if !ok {
			t.Fatal("trace record missing after upstream_error")
		}
		if got.Status != "error" || got.UpstreamError == "" || got.StatusCode != 502 {
			t.Fatalf("upstream error record=%+v want status=error code=502", got)
		}
	})
}

// TestMarkTraceErrorFlipsStreamingRecordToError guards the streaming error
// paths: when a handler fails after the upstream response (HTTP 200 SSE stream),
// markTraceError must leave a terminal "error" state so the console does not
// show "success".
func TestMarkTraceErrorFlipsStreamingRecordToError(t *testing.T) {
	withTraceEnabled(t, func() {
		st := openTraceStore()
		rec := &traceRecord{ID: "t-mark-error", At: time.Now(), Status: "in_progress"}
		st.byID[rec.ID] = rec
		st.active[rec.ID] = rec
		s := &Server{trace: st}

		// Build a request whose context carries the trace record, as the
		// traceCaptureMiddleware does.
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r = r.WithContext(context.WithValue(r.Context(), traceKey{}, rec))

		s.markTraceError(r, nil, 502)

		got, ok := s.trace.get("t-mark-error")
		if !ok {
			t.Fatal("trace record missing after markTraceError")
		}
		if got.Status != "error" || got.StatusCode != 502 {
			t.Fatalf("markTraceError record=%+v want status=error code=502", got)
		}
	})
}
