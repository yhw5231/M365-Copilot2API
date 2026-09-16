package web

import (
	"os"
	"regexp"
	"strings"
)

// Upstream ChatHub delivers answer citations as inline protocol tokens embedded
// in the message text:
//
//	new family:   U+E200 "cite" U+E202 <id> [U+E202 <id> ...] U+E201
//	legacy form:  U+E008 "cite" U+E009 <id> [U+E009 <id> ...] U+E009
//	             <cite><id>[, <id>]</cite>
//
// The ids reference prior assistant tool calls (fc_<uuid>) or upstream turn
// sources (turn\d+(search|news|image)\d+). These tokens are protocol metadata
// of the upstream chat channel, not part of the answer: OpenAI-format clients
// have no way to render them, they surface as raw "cite …" text in the
// conversation, and once they land in stored history they are replayed back
// upstream, where the model keeps echoing ever-broken citations. The public
// identity sanitizer historically stripped only the turn-id form (and only
// under M365_PUBLIC_IDENTITY_POLICY), so tool-result citations (fc_<uuid>)
// went through untouched.
//
// This module strips complete markers and the dangling fragments a truncated
// upstream stream can leave at the end of an answer, and is applied to the
// answer text on every delivery path. Set M365_KEEP_CITE_MARKERS=1 only for a
// client that actually understands and renders the tokens.

const (
	citationMarkerOpen   = "\ue200" // U+E200 — marker open (new family)
	citationMarkerSep    = "\ue202" // U+E202 — id separator
	citationMarkerClose  = "\ue201" // U+E201 — marker close
	citationLegacyOpen   = "\uE008" // U+E008 — marker open (legacy family)
	citationLegacyClose  = "\uE009" // U+E009 — separator/close (legacy family)
)

var citationMarkerCompletePattern = regexp.MustCompile(citationMarkerOpen + `cite(?:` + citationMarkerSep + `[^` + citationMarkerOpen + citationMarkerClose + `]+)+` + citationMarkerClose)
var citationMarkerDanglingPattern = regexp.MustCompile(citationMarkerOpen + `cite(?:` + citationMarkerSep + `[^` + citationMarkerOpen + citationMarkerClose + `]*)*$`)
var citationLegacyCompletePattern = regexp.MustCompile(citationLegacyOpen + `cite(?:` + citationLegacyClose + `[^` + citationLegacyOpen + citationLegacyClose + `]+)+` + citationLegacyClose)
var citationLegacyDanglingPattern = regexp.MustCompile(citationLegacyOpen + `cite(?:` + citationLegacyClose + `[^` + citationLegacyOpen + citationLegacyClose + `]*)+$`)
var citationXMLCompletePattern = regexp.MustCompile(`(?i)<cite>[^<]*</cite>`)

// keepCitationMarkers reports whether the raw upstream citation tokens are
// passed through instead of being stripped. Default is to strip; set
// M365_KEEP_CITE_MARKERS=1 only for clients that render the tokens.
func keepCitationMarkers() bool {
	raw, ok := os.LookupEnv("M365_KEEP_CITE_MARKERS")
	if !ok || strings.TrimSpace(raw) == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}

// sanitizeCitationMarkers removes complete and dangling upstream citation
// tokens from a fully assembled text. Non-marker occurrences of the PUA
// characters are preserved.
func sanitizeCitationMarkers(text string) string {
	if text == "" || keepCitationMarkers() {
		return text
	}
	if next := citationMarkerCompletePattern.ReplaceAllString(text, ""); next != text {
		text = next
	}
	if next := citationLegacyCompletePattern.ReplaceAllString(text, ""); next != text {
		text = next
	}
	text = citationMarkerDanglingPattern.ReplaceAllString(text, "")
	text = citationLegacyDanglingPattern.ReplaceAllString(text, "")
	text = citationXMLCompletePattern.ReplaceAllString(text, "")
	return text
}

// citationStreamFilter strips the tokens from an incremental delta stream.
// Markers arrive split across deltas, so the tail after the last marker open
// is held back until a close arrives; Flush drops a dangling open fragment
// left by a truncated stream.
type citationStreamFilter struct {
	pending string
}

func newCitationStreamFilter() *citationStreamFilter {
	return &citationStreamFilter{}
}

func (f *citationStreamFilter) Push(fragment string) string {
	if f == nil {
		return sanitizeCitationMarkers(fragment)
	}
	if keepCitationMarkers() {
		f.pending = ""
		return fragment
	}
	f.pending += fragment
	lastOpen := strings.LastIndex(f.pending, citationMarkerOpen)
	if lastOpen < 0 {
		out := f.pending
		f.pending = ""
		return sanitizeCitationMarkers(out)
	}
	tail := f.pending[lastOpen:]
	if strings.Index(tail, citationMarkerClose) >= 0 {
		// The trailing token is already closed: everything is safe to emit.
		out := f.pending
		f.pending = ""
		return sanitizeCitationMarkers(out)
	}
	// Keep the undertermined tail; only the settled head is released. The cut
	// sits exactly at the marker open, so a marker is never split in half.
	head := f.pending[:lastOpen]
	f.pending = tail
	return sanitizeCitationMarkers(head)
}

func (f *citationStreamFilter) Flush() string {
	if f == nil {
		return ""
	}
	out := sanitizeCitationMarkers(f.pending)
	f.pending = ""
	return out
}