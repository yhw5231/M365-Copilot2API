package web

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestCaptureLimitShortInput verifies strings under the cap pass through
// unchanged: no marker, no rewriting.
func TestCaptureLimitShortInput(t *testing.T) {
	s := "hello 世界"
	if got := captureLimit(s); got != s {
		t.Fatalf("short input rewritten: got %q", got)
	}
}

// TestCaptureLimitTruncatesWithMarker verifies an oversized capture is bounded
// to maxTraceCaptureBytes, ends with the truncation marker, and stays valid
// UTF-8 (no rune split at the cut).
func TestCaptureLimitTruncatesWithMarker(t *testing.T) {
	// Chinese payload: every rune is 3 bytes, so a byte-naive cut at
	// maxTraceCaptureBytes lands mid-rune unless the cut backs up.
	s := strings.Repeat("中", maxTraceCaptureBytes)
	got := captureLimit(s)
	if len(got) > maxTraceCaptureBytes {
		t.Fatalf("captured %d bytes, cap is %d", len(got), maxTraceCaptureBytes)
	}
	if !strings.HasSuffix(got, "\n…[trace capture truncated]") {
		t.Fatalf("missing truncation marker, tail=%q", got[max(0, len(got)-40):])
	}
	if !utf8.ValidString(got) {
		t.Fatal("captured string is not valid UTF-8")
	}
	if !strings.HasPrefix(got, "中中中") {
		t.Fatal("prefix content lost")
	}
}

// TestCaptureLimitMixedScriptBoundary verifies a payload whose cap boundary
// falls between ASCII and multi-byte runes keeps both sides valid.
func TestCaptureLimitMixedScriptBoundary(t *testing.T) {
	ascii := strings.Repeat("a", maxTraceCaptureBytes-2)
	s := ascii + "中文中文中文"
	got := captureLimit(s)
	if !utf8.ValidString(got) {
		t.Fatal("captured string is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "\n…[trace capture truncated]") {
		t.Fatal("missing truncation marker")
	}
}
