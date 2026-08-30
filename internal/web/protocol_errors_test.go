package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicErrorEnvelope(t *testing.T) {
	rr := httptest.NewRecorder()
	writeAnthropicError(rr, 400, "invalid_request_error", "bad json")
	var v map[string]any
	if json.Unmarshal(rr.Body.Bytes(), &v) != nil || v["type"] != "error" {
		t.Fatalf("body=%s", rr.Body.String())
	}
	e, _ := v["error"].(map[string]any)
	if e["type"] != "invalid_request_error" || e["message"] != "bad json" {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatal("missing json content type")
	}
}
func TestOpenAIErrorEnvelopeAndExtraction(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesError(rr, 409, "tool_round_limit", "limit reached")
	if got := errorMessage(rr.Body.Bytes(), "fallback"); got != "limit reached" {
		t.Fatalf("message=%q", got)
	}
}
func TestMaxToolRoundsConfig(t *testing.T) {
	t.Setenv("M365_MAX_TOOL_ROUNDS", "7")
	if maxToolRounds() != 7 {
		t.Fatal("configured limit ignored")
	}
	t.Setenv("M365_MAX_TOOL_ROUNDS", "9999")
	if maxToolRounds() != 0 {
		t.Fatal("invalid limit should fall back to 0 (unlimited)")
	}
	t.Setenv("M365_MAX_TOOL_ROUNDS", "0")
	if maxToolRounds() != 0 {
		t.Fatal("0 must be accepted as unlimited")
	}
}

func TestPipeResponseWriterCapturesBodyWithoutTruncatingStream(t *testing.T) {
	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	done := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(pr)
		done <- data
	}()
	// A single write larger than the capture budget must reach the pipe intact
	// while only the bounded prefix is retained for diagnostics.
	big := bytes.Repeat([]byte("x"), maxInnerErrorCapture+64)
	if n, _ := irw.Write(big); n != len(big) {
		t.Fatalf("pipe write truncated: n=%d want=%d", n, len(big))
	}
	if irw.status != http.StatusOK {
		t.Fatalf("status=%d", irw.status)
	}
	_ = pw.Close()
	if got := <-done; len(got) != len(big) {
		t.Fatalf("pipe content truncated: len=%d want=%d", len(got), len(big))
	}
	if irw.body.Len() != maxInnerErrorCapture {
		t.Fatalf("capture len=%d want=%d", irw.body.Len(), maxInnerErrorCapture)
	}
}
