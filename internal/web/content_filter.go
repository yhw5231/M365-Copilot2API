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

// activeContentFilterInputRules mirrors activeContentFilterRules for the
// input-side rule set. Input review is a gate on what may enter the gateway at
// all, so there is no replacement concept: every hit rejects the request.
func activeContentFilterInputRules() []contentFilterRule {
	s := currentSettings()
	if !s.ContentFilterInputEnabled || len(s.ContentFilterInputRules) == 0 {
		return nil
	}
	rules := make([]contentFilterRule, 0, len(s.ContentFilterInputRules))
	for _, r := range s.ContentFilterInputRules {
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

// filterInputContent scans client-supplied text against the enabled input
// rules. The first rule (in configuration order) whose keyword occurs anywhere
// (case-insensitive) wins; the matched keyword is returned for logging.
func filterInputContent(text string) (keyword string, hit bool) {
	rules := activeContentFilterInputRules()
	if len(rules) == 0 || text == "" {
		return "", false
	}
	lower := strings.ToLower(text)
	for _, r := range rules {
		kw := strings.ToLower(strings.TrimSpace(r.Keyword))
		if kw != "" && strings.Contains(lower, kw) {
			return r.Keyword, true
		}
	}
	return "", false
}

// inputReviewSample is the per-message cap on the text fed to the input
// review. A user message longer than this cannot hide a keyword that the
// streaming response filter would still see mirrored back, and capping keeps
// the scan O(1) per message on agent payloads with megabyte histories.
const inputReviewSampleLimit = 256 * 1024

// reviewUserInput scans the user-supplied messages of a request against the
// input rule set. Only client-authored turns participate: system/developer and
// service-injected messages (tool reminders, corrections, the identity policy)
// are gateway fixtures, not user input, and would otherwise trip rules meant
// for the user (e.g. the model names the policy itself talks around).
func reviewUserInput(messages []oaiMsg) (keyword string, hit bool) {
	if len(activeContentFilterInputRules()) == 0 {
		return "", false
	}
	for _, m := range messages {
		if m.ServiceInjected {
			continue
		}
		// Only genuine user turns are gated. System/developer messages are
		// gateway or client fixtures and tool results originate from the
		// model's own tool calls — none of it is user input.
		if !strings.EqualFold(strings.TrimSpace(m.Role), "user") {
			continue
		}
		text := contentToString(m.Content)
		if len(text) > inputReviewSampleLimit {
			text = text[:inputReviewSampleLimit]
		}
		if kw, ok := filterInputContent(text); ok {
			return kw, true
		}
	}
	return "", false
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

// blockFilteredSession blocks the downstream session after a filter hit. A
// client whose history triggered the keyword once will trigger it on every
// replay (it cannot remove the turn that produced the hit), so dropping only
// the upstream binding turns every retry into a fresh repeat of the same
// violation. Blocking makes every further request on the session fail fast
// locally; the block survives restarts and expires with the configured TTL.
func (s *Server) blockFilteredSession(r *http.Request, body *oaiReq) {
	if s == nil || s.blockedSessions == nil {
		return
	}
	explicitID := explicitSessionID(r, body)
	if explicitID == "" {
		log.Printf("[content-filter] hit on a request without an explicit session id; nothing to block")
		return
	}
	if rec := s.blockedSessions.BlockedRecord(explicitID); rec.BlockedAt.IsZero() {
		s.blockedSessions.Block(explicitID, "content filter hit")
		log.Printf("[content-filter] session %s blocked after keyword hit (ttl %s)", explicitID, s.blockedSessions.ttl)
	}
}

// contentFilterRejectMessage is the client-facing text for a rejected request,
// on the wire as HTTP 403 (headers not yet sent) or as an in-stream error
// frame (a stream already in progress).
const contentFilterRejectMessage = "content policy violated: the response was blocked by the gateway's content filter and this session is now blocked; start a new session to continue"

// recordContentFilterRejection writes the usage row and debug-trace update for
// a request the content filter rejected. The row carries the 403 status so the
// usage/error pages show the policy block instead of a silent success.
func (s *Server) recordContentFilterRejection(r *http.Request, body *oaiReq, acc auth.AccountToken, prompt string, startedAt time.Time) {
	if s == nil {
		return
	}
	pt := EstimateTokens(prompt)
	model := firstNonEmpty(body.Model, defaultPublicModelName)
	log.Printf("[content-filter] hit: request rejected with 403 and the session blocked")
	if s.usage != nil {
		s.usage.record(UsageRecord{
			Time:           time.Now(),
			APIKeyPrefix:   extractAPIKey(r),
			AccountEmail:   acc.Email,
			Model:          model,
			ReasoningLevel: body.ReasoningEffort,
			Endpoint:       "/v1/chat/completions",
			InputTokens:    int64(pt),
			DurationMs:     time.Since(startedAt).Milliseconds(),
			Status:         http.StatusForbidden,
		})
	}
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			rec.Status = "error"
			rec.StatusCode = http.StatusForbidden
			rec.InputTokens = int64(pt)
			rec.Error = "content filter hit: session blocked"
		})
	}
}

// rejectContentFilterHit is the shared bookkeeping for every rejection path:
// block the session (persistent), drop the upstream binding, and record the
// rejection. The wire response (403 JSON or in-stream error frame) is emitted
// by the caller, which knows whether response headers were already sent.
func (s *Server) rejectContentFilterHit(r *http.Request, body *oaiReq, acc auth.AccountToken, prompt string, startedAt time.Time) {
	s.blockFilteredSession(r, body)
	s.dropFilteredSession(r, body)
	s.recordContentFilterRejection(r, body, acc, prompt, startedAt)
}

// rejectUserInputHit is the bookkeeping for an input-review hit: identical to
// the output path (block the session so replaying the same input cannot
// retry-spam, drop the binding, record the 403) with its own log line so
// operators can tell which rule set fired.
func (s *Server) rejectUserInputHit(r *http.Request, body *oaiReq, acc auth.AccountToken, scannedText string, startedAt time.Time) {
	log.Printf("[content-filter] input review hit: request rejected with 403 and the session blocked")
	s.rejectContentFilterHit(r, body, acc, scannedText, startedAt)
}

// lastUserContent returns the text of the most recent user message, or "" when
// the request carries none. Used for the usage/trace record of an input
// rejection so the operator sees what was scanned.
func lastUserContent(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			return contentToString(messages[i].Content)
		}
	}
	return ""
}
