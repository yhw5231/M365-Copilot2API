package web

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"m365-copilot2api/internal/auth"
)

// errContentFilterHit is the sentinel returned by streaming callbacks when the
// content filter matched a keyword. It aborts the upstream generation; the
// handler then emits the configured replacement text and closes the stream
// normally. It must never be surfaced to the client as an error, and it must
// never count against the serving account's health.
var errContentFilterHit = errors.New("content filter hit: response replaced by configured text")

// contentFilterRule replaces every upstream response that contains Keyword
// (case-insensitive substring match) with Replacement. An empty Replacement
// deletes the response text entirely.
type contentFilterRule struct {
	Keyword     string `json:"keyword"`
	Replacement string `json:"replacement"`
}

// contentFilterSettingsDefaults mirrors the settings defaults; kept as a
// function so env overrides and the settings store agree on one number.
func contentFilterOpeningBufferDefault() int {
	return envInt("M365_CONTENT_FILTER_OPENING_BUFFER", 256)
}

// activeContentFilterRules returns the enabled rules from the runtime settings
// in configuration order, or nil when the filter is disabled or no usable rule
// is configured.
func activeContentFilterRules() []contentFilterRule {
	s := currentSettings()
	if !s.ContentFilterEnabled || len(s.ContentFilterRules) == 0 {
		return nil
	}
	rules := make([]contentFilterRule, 0, len(s.ContentFilterRules))
	for _, r := range s.ContentFilterRules {
		if strings.TrimSpace(r.Keyword) == "" {
			continue
		}
		rules = append(rules, r)
	}
	if len(rules) == 0 {
		return nil
	}
	return rules
}

// filterContentFull scans a complete text against the enabled rules. The first
// rule (in configuration order) whose keyword occurs anywhere wins; the whole
// text is then replaced by that rule's replacement.
func filterContentFull(text string) (replacement string, hit bool) {
	rules := activeContentFilterRules()
	if len(rules) == 0 || text == "" {
		return "", false
	}
	lower := strings.ToLower(text)
	for _, r := range rules {
		if strings.Contains(lower, strings.ToLower(r.Keyword)) {
			return r.Replacement, true
		}
	}
	return "", false
}

// contentStreamFilter implements the streaming half of the content filter.
//
// Correctness invariant: a keyword can never escape detection by being split
// across push boundaries. Instead of the naive "scan every fixed 100-char
// window" approach, the filter scans the whole pending buffer on every push and
// releases only the prefix that cannot be part of any future match — everything
// after the last (maxKeywordRunes-1) runes stays buffered until more text (or
// Flush) arrives. A keyword of L ≤ maxKeywordRunes runes that starts inside the
// released prefix must therefore end inside the scanned buffer, where the scan
// would have caught it — contradiction. Keywords split across chunks are always
// detected.
//
// On top of the tail holdback an optional opening window (settings
// contentFilterOpeningBuffer, default 256 bytes) buffers the beginning of the
// response. A keyword found inside that window is replaced before ANY byte
// reached the client, so the downstream sees only the replacement text — the
// "previous streamed content also disappears" behavior, which is otherwise
// impossible for already-emitted SSE deltas.
type contentStreamFilter struct {
	keywords      []string // lowercased keywords, configuration order
	replacements  []string
	holdbackRunes int // maxKeywordRunes - 1; 0 when every keyword is 1 rune
	openingBuffer int // bytes to buffer before the first release; 0 disables
	pending       string
	openingDone   bool
	hitIndex      int
	hit           bool
}

// newContentStreamFilter returns nil when the content filter is disabled, so
// call sites can treat a nil filter as pass-through.
func newContentStreamFilter() *contentStreamFilter {
	rules := activeContentFilterRules()
	if rules == nil {
		return nil
	}
	f := &contentStreamFilter{
		keywords:      make([]string, len(rules)),
		replacements:  make([]string, len(rules)),
		openingBuffer: currentSettings().ContentFilterOpeningBuffer,
	}
	maxRunes := 1
	for i, r := range rules {
		f.keywords[i] = strings.ToLower(r.Keyword)
		f.replacements[i] = r.Replacement
		if n := utf8.RuneCountInString(r.Keyword); n > maxRunes {
			maxRunes = n
		}
	}
	if f.openingBuffer < 0 {
		f.openingBuffer = 0
	}
	f.holdbackRunes = maxRunes - 1
	return f
}

func (f *contentStreamFilter) replacement() string {
	if f == nil || !f.hit {
		return ""
	}
	return f.replacements[f.hitIndex]
}

// push feeds one upstream fragment and returns the text that is safe to send
// downstream. After a hit, every further push returns nothing (the caller is
// expected to abort the stream and emit the replacement once).
func (f *contentStreamFilter) push(fragment string) (out string, hit bool, replacement string) {
	if f == nil {
		return fragment, false, ""
	}
	if f.hit {
		return "", true, f.replacement()
	}
	f.pending += fragment
	if hit, repl := f.scan(); hit {
		f.hit = true
		f.hitIndex = repl.ruleIndex
		return "", true, f.replacement()
	}
	if !f.openingDone {
		if f.openingBuffer > 0 && len(f.pending) < f.openingBuffer {
			return "", false, ""
		}
		f.openingDone = true
	}
	out = f.release()
	return out, false, ""
}

// flush drains the buffer at the end of the stream. A keyword found only now
// (the whole response fit in the buffering windows) still triggers the
// replacement; otherwise the held-back tail is released verbatim.
func (f *contentStreamFilter) flush() (out string, hit bool, replacement string) {
	if f == nil {
		return "", false, ""
	}
	if f.hit {
		return "", true, f.replacement()
	}
	if hit, repl := f.scan(); hit {
		f.hit = true
		f.hitIndex = repl.ruleIndex
		return "", true, f.replacement()
	}
	out = f.pending
	f.pending = ""
	return out, false, ""
}

type contentFilterScanHit struct {
	ruleIndex int
}

// scan looks for the earliest keyword occurrence in the pending buffer.
func (f *contentStreamFilter) scan() (bool, contentFilterScanHit) {
	if len(f.pending) == 0 {
		return false, contentFilterScanHit{}
	}
	lower := strings.ToLower(f.pending)
	bestIdx := -1
	bestRule := -1
	bestLen := 0
	for i, kw := range f.keywords {
		idx := strings.Index(lower, kw)
		if idx < 0 {
			continue
		}
		if bestIdx < 0 || idx < bestIdx || (idx == bestIdx && len(kw) > bestLen) {
			bestIdx, bestRule, bestLen = idx, i, len(kw)
		}
	}
	if bestIdx < 0 {
		return false, contentFilterScanHit{}
	}
	return true, contentFilterScanHit{ruleIndex: bestRule}
}

// release emits everything except the last holdbackRunes runes of the pending
// buffer, cutting on a rune boundary. A trailing incomplete rune sequence (a
// push split a multi-byte character) never counts as a held-back rune and is
// always kept in the pending buffer, so the holdback always covers
// holdbackRunes COMPLETE runes and no keyword byte can leak through a cut.
func (f *contentStreamFilter) release() string {
	if f.holdbackRunes <= 0 {
		out := f.pending
		f.pending = ""
		return out
	}
	complete, _ := splitTrailingPartialRune(f.pending)
	cut := byteOffsetKeepingTailRunes(complete, f.holdbackRunes)
	out := f.pending[:cut]
	f.pending = f.pending[cut:]
	return out
}

// splitTrailingPartialRune splits off a trailing incomplete UTF-8 sequence
// (0-3 bytes). Upstream fragments are valid UTF-8, so at most the last rune of
// the pending buffer can be incomplete.
func splitTrailingPartialRune(value string) (complete, partial string) {
	if value == "" || utf8.ValidString(value) {
		return value, ""
	}
	i := len(value) - 1
	for i >= 0 && !utf8.RuneStart(value[i]) {
		i--
	}
	if i < 0 {
		return "", value
	}
	return value[:i], value[i:]
}

// byteOffsetKeepingTailRunes returns the byte offset that keeps exactly count
// trailing runes of value (or the whole value when it has fewer runes).
func byteOffsetKeepingTailRunes(value string, count int) int {
	total := utf8.RuneCountInString(value)
	if total <= count {
		return 0
	}
	keep := count
	offset := len(value)
	for offset > 0 {
		r, size := utf8.DecodeLastRuneInString(value[:offset])
		_ = r
		offset -= size
		keep--
		if keep <= 0 {
			break
		}
	}
	return offset
}

// dropFilteredSession terminates the downstream session after a filter hit: the
// upstream conversation now contains the tainted assistant answer, so the
// binding is removed and the next request on the same session starts a fresh
// upstream conversation built from the client's own (replaced) history.
func (s *Server) dropFilteredSession(r *http.Request, body *oaiReq) {
	if s == nil || s.sessionResolver == nil {
		return
	}
	explicitID := explicitSessionID(r, body)
	if explicitID == "" {
		return
	}
	if s.sessionResolver.DeleteSession(explicitID) {
		log.Printf("[content-filter] session %s terminated after keyword hit", explicitID)
	}
}

// recordContentReviewStop writes the usage row and debug-trace update for a
// request that the content filter stopped. The row records the replacement as
// the completion so the request table does not show a zero-token row.
func (s *Server) recordContentReviewStop(r *http.Request, body *oaiReq, acc auth.AccountToken, prompt string, startedAt time.Time, replacement string, stream bool) {
	if s == nil {
		return
	}
	pt := EstimateTokens(prompt)
	ct := EstimateTokens(replacement)
	model := firstNonEmpty(body.Model, defaultPublicModelName)
	log.Printf("[content-filter] hit: response replaced with %d-byte configured text, session terminated", len(replacement))
	if s.usage != nil {
		s.usage.record(UsageRecord{
			Time:           time.Now(),
			APIKeyPrefix:   extractAPIKey(r),
			AccountEmail:   acc.Email,
			Model:          model,
			ReasoningLevel: body.ReasoningEffort,
			Endpoint:       "/v1/chat/completions",
			Stream:         stream,
			InputTokens:    int64(pt),
			OutputTokens:   int64(ct),
			DurationMs:     time.Since(startedAt).Milliseconds(),
			Status:         http.StatusOK,
		})
	}
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			rec.Status = "success"
			rec.StatusCode = http.StatusOK
			rec.InputTokens = int64(pt)
			rec.OutputTokens = int64(ct)
			rec.Error = "content filter hit: response replaced"
		})
	}
}
