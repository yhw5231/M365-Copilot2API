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
// upstream, where the model keeps echoing ever-broken citations.
//
// The same channel also marks file references with literal <File>…</File>
// tags inside the answer text. They are equally protocol markup: the upstream
// UI renders them as file chips, OpenAI-format clients show the raw tag text.
// The tag pair is removed and the path text inside is kept, so the answer
// reads "<File>src/main.py</File> 中可见" as "src/main.py 中可见".
//
// This module strips complete markers, the dangling fragments a truncated
// upstream stream can leave at the end of an answer, and unclosed marker
// fragments that a mid-answer truncation leaves behind, and is applied to the
// answer text on every delivery path. Set M365_KEEP_CITE_MARKERS=1 only for a
// client that actually understands and renders the tokens (it also keeps the
// <File> tags).

const (
	citationMarkerOpen   = "\ue200" // U+E200 — marker open (new family)
	citationMarkerSep    = "\ue202" // U+E202 — id separator
	citationMarkerClose  = "\ue201" // U+E201 — marker close
	citationLegacyOpen   = "\uE008" // U+E008 — marker open (legacy family)
	citationLegacyClose  = "\uE009" // U+E009 — separator/close (legacy family)
	fileMarkerOpenTag    = "<File>"
	fileMarkerCloseTag   = "</File>"
)

var citationMarkerCompletePattern = regexp.MustCompile(citationMarkerOpen + `cite(?:` + citationMarkerSep + `[^` + citationMarkerOpen + citationMarkerClose + `]+)+` + citationMarkerClose)
var citationMarkerDanglingPattern = regexp.MustCompile(citationMarkerOpen + `cite(?:` + citationMarkerSep + `(?:fc_[0-9a-fA-F-]*|turn\d*(?:search|news|image)?\d*))*$`)
var citationMarkerUnclosedPattern = regexp.MustCompile(citationMarkerOpen + `cite(?:` + citationMarkerSep + `(?:fc_[0-9a-fA-F-]+|turn\d+(?:search|news|image)\d+))+`)
var citationLegacyCompletePattern = regexp.MustCompile(citationLegacyOpen + `cite(?:` + citationLegacyClose + `[^` + citationLegacyOpen + citationLegacyClose + `]+)+` + citationLegacyClose)
var citationLegacyDanglingPattern = regexp.MustCompile(citationLegacyOpen + `cite(?:` + citationLegacyClose + `(?:fc_[0-9a-fA-F-]*|turn\d*(?:search|news|image)?\d*))*$`)
var citationLegacyUnclosedPattern = regexp.MustCompile(citationLegacyOpen + `cite(?:` + citationLegacyClose + `(?:fc_[0-9a-fA-F-]+|turn\d+(?:search|news|image)\d+))+`)
var citationXMLCompletePattern = regexp.MustCompile(`(?i)<cite>[^<]*</cite>`)
var fileMarkerPairPattern = regexp.MustCompile(`(?is)<File>(.*?)</File>`)
var fileMarkerOpenPattern = regexp.MustCompile(`(?i)<File>`)
var fileMarkerClosePattern = regexp.MustCompile(`(?i)</File>`)
var fileMarkerLooseOpenPattern = regexp.MustCompile(`(?i)<File`)
var fileMarkerLooseClosePattern = regexp.MustCompile(`(?i)</File`)
var fileMarkerTruncatedOpenPattern = regexp.MustCompile(`(?i)<f(?:i(?:l(?:e)?)?)?$`)
var fileMarkerTruncatedClosePattern = regexp.MustCompile(`(?i)</f(?:i(?:l(?:e)?)?)?$`)

// keepCitationMarkers reports whether the raw upstream citation tokens are
// passed through instead of being stripped. Default is to strip; set
// M365_KEEP_CITE_MARKERS=1 only for clients that render the tokens (the same
// flag also keeps the <File>…</File> reference tags).
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
	// A truncated upstream stream can also drop the closing token mid-answer,
	// leaving an unclosed marker embedded in otherwise valid text. The ids are
	// protocol-shaped (fc_<uuid> or turn ids), so stripping the token without
	// a close is safe.
	if next := citationMarkerUnclosedPattern.ReplaceAllString(text, ""); next != text {
		text = next
	}
	if next := citationLegacyUnclosedPattern.ReplaceAllString(text, ""); next != text {
		text = next
	}
	text = citationMarkerDanglingPattern.ReplaceAllString(text, "")
	text = citationLegacyDanglingPattern.ReplaceAllString(text, "")
	text = citationXMLCompletePattern.ReplaceAllString(text, "")
	return text
}

// sanitizeFileMarkers removes the <File>…</File> reference tags while keeping
// the file path text between them, and drops stray or damaged tag fragments a
// truncated stream can leave behind.
func sanitizeFileMarkers(text string) string {
	if text == "" || keepCitationMarkers() {
		return text
	}
	if next := fileMarkerPairPattern.ReplaceAllString(text, "$1"); next != text {
		text = next
	}
	// Complete tags with no matching close (or vice versa) and tags damaged by
	// truncation ("<File" without ">") are protocol noise: drop the tag tokens
	// themselves, keep the surrounding text. The loose literals run after the
	// complete-tag patterns, so any remaining "<File"/"</File" is necessarily
	// damaged. Truncated-tag partials at the end of the text ("<Fi", "</Fil")
	// are dropped too.
	text = fileMarkerOpenPattern.ReplaceAllString(text, "")
	text = fileMarkerClosePattern.ReplaceAllString(text, "")
	text = fileMarkerLooseOpenPattern.ReplaceAllString(text, "")
	text = fileMarkerLooseClosePattern.ReplaceAllString(text, "")
	text = fileMarkerTruncatedOpenPattern.ReplaceAllString(text, "")
	text = fileMarkerTruncatedClosePattern.ReplaceAllString(text, "")
	return text
}

// sanitizeUpstreamMarkers removes every upstream channel marker (cite tokens
// and <File> reference tags) from assembled text. This is deliberately not
// policy-gated: the markers are channel protocol, never deliverable content.
func sanitizeUpstreamMarkers(text string) string {
	return sanitizeFileMarkers(sanitizeCitationMarkers(text))
}

// partialFileTagIndex returns the start index of a trailing partial <File> or
// </File> tag in value (a prefix of either tag, up to one byte short of the
// full tag), or -1 when the value does not end inside a tag.
func partialFileTagIndex(value string) int {
	tail := value
	if len(tail) > 16 {
		tail = tail[len(tail)-16:]
	}
	lower := strings.ToLower(tail)
	for i := 1; i < len(fileMarkerOpenTag); i++ {
		if strings.HasSuffix(lower, strings.ToLower(fileMarkerOpenTag[:i])) {
			return len(value) - i
		}
	}
	for i := 1; i < len(fileMarkerCloseTag); i++ {
		if strings.HasSuffix(lower, strings.ToLower(fileMarkerCloseTag[:i])) {
			return len(value) - i
		}
	}
	return -1
}

// citationStreamFilter strips the tokens from an incremental delta stream.
// Markers arrive split across deltas, so the tail after the last marker open
// (or a partial <File> tag) is held back until a close arrives; Flush drops a
// dangling open fragment left by a truncated stream.
type citationStreamFilter struct {
	pending string
}

func newCitationStreamFilter() *citationStreamFilter {
	return &citationStreamFilter{}
}

func (f *citationStreamFilter) Push(fragment string) string {
	if f == nil {
		return sanitizeUpstreamMarkers(fragment)
	}
	if keepCitationMarkers() {
		f.pending = ""
		return fragment
	}
	f.pending += fragment
	hold := f.holdIndex()
	if hold < 0 {
		out := f.pending
		f.pending = ""
		return sanitizeUpstreamMarkers(out)
	}
	// Only the settled head (before the held tail) is released. The cut sits
	// exactly at the marker open or partial tag, so a marker is never split in
	// half and a half-open tag never reaches the client.
	head := f.pending[:hold]
	f.pending = f.pending[hold:]
	return sanitizeUpstreamMarkers(head)
}

// holdIndex returns the index at which the pending buffer must start being
// held back: the last citation marker open that has not been closed, the last
// <File> open that has not been closed (its path content is ambiguous until
// the close arrives), or the start of a trailing partial tag, whichever is
// later. -1 when nothing is held and the whole buffer is settled.
func (f *citationStreamFilter) holdIndex() int {
	hold := -1
	if lastOpen := strings.LastIndex(f.pending, citationMarkerOpen); lastOpen >= 0 {
		if strings.Index(f.pending[lastOpen:], citationMarkerClose) < 0 {
			hold = lastOpen
		}
	}
	if fileHold := f.holdFileOpenIndex(); fileHold > hold {
		hold = fileHold
	}
	if fileHold := partialFileTagIndex(f.pending); fileHold > hold {
		hold = fileHold
	}
	return hold
}

// holdFileOpenIndex returns the index of the last <File> open tag that has no
// matching </File> close after it, or -1. When the last open is closed every
// earlier open is closed too (the closes are position-ordered), so a single
// check on the last open is sufficient.
func (f *citationStreamFilter) holdFileOpenIndex() int {
	lower := strings.ToLower(f.pending)
	open := strings.LastIndex(lower, "<file>")
	if open < 0 {
		return -1
	}
	if strings.Index(lower[open:], "</file>") >= 0 {
		return -1
	}
	return open
}

func (f *citationStreamFilter) Flush() string {
	if f == nil {
		return ""
	}
	out := sanitizeUpstreamMarkers(f.pending)
	f.pending = ""
	return out
}