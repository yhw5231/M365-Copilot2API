package chathub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// ErrRateLimitNotice identifies the human-readable rate-limit response that
// ChatHub sometimes sends through the text channel instead of HTTP 429.
// Callers must independently probe the account before marking it unhealthy.
var ErrRateLimitNotice = errors.New("upstream rate-limit notice")

// ErrEmptyCompletion indicates upstream returned an empty completion because
// the requested tone is not available for this tenant. The web layer can
// fall back to "magic" and retry.
var ErrEmptyCompletion = errors.New("upstream returned empty completion; tone may be unavailable for this tenant")

// DialError carries the HTTP status and optional Retry-After from a failed
// WebSocket dial so the web layer can route it into the correct cooldown.
type DialError struct {
	Status     int
	RetryAfter int
}

func (e *DialError) Error() string {
	return fmt.Sprintf("ws dial: upstream %d", e.Status)
}

var chTrace = os.Getenv("M365_TRACE") == "1"

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const (
	rs          = "\x1e"
	defaultTone = "magic"
	wsBase      = "wss://substrate.office.com/m365Copilot/Chathub"
	// maxAttachments bounds per-request remote downloads: each image is
	// base64-encoded and held in memory alongside the multipart body.
	maxAttachments   = 10
	maxAttachmentMiB = 10
)

// Variants mirrored from the verified browser / Python probe.
const variants = "EnableMcpServerWidgets,feature.EnableMcpServerWidgets,feature.EnableLuForChatCIQ,feature.enableChatCIQPlugin,EnableRequestPlugins,feature.EnableSensitivityLabels,EnableUnsupportedUrlDetector,feature.IsCustomEngineCopilotEnabled,feature.bizchatfluxv3,feature.enablechatpages,feature.enableCodeCanvas,feature.turnOnWorkTabRecommendation,turnOffWorkTabUpsellFromClient,feature.turnOnDARecommendation,feature.IsStreamingModeInChatRequestEnabled,IncludeSourceAttributionsConcise,SkipPublishEmptyMessage,feature.EnableDeduplicatingSourceAttributions,Enable3PActionProgressMessages,feature.enableClientWebRtc,feature.EnableMeetingRecapOfSeriesMeetingWithCiq,feature.EnableReferencesListCompleteSignal,feature.StorageMessageSplitDisabled,feature.EnableCuaTakeControlApi,feature.cwcallowedos,feature.disabledisallowedmsgs,feature.enableCitationsForSynthesisData,feature.enableGenerateGraphicArtOptionsSet,cdximagen,feature.EnableUpdatedUXForConfirmationDialog,feature.EnableClientFileURLSupportForOfficeWebPaidCopilot,feature.EnableDesignEditorImageGrounding,feature.EnableDesignerEditor,feature.OfficeWebToHelix,feature.OfficeDesktopToHelix,feature.M365TeamsHubToHelix,feature.OwaHubToHelix,feature.MonarchHubToHelix,feature.Win32OutlookHubToHelix,feature.MacOutlookHubToHelix,Agt_bizchat_enableGpt5ForHelix"

type Account struct {
	AccessToken string
	OID         string
	TID         string
}

type Request struct {
	Text           string
	Tone           string
	ConversationID string
	SessionID      string
	Attachments    []Attachment
	Tools          []Tool
	ToolChoice     any
	MCPServerURL   string // URL of the MCP HTTP SSE server for tool discovery
	// Started is true only for the first turn of a ChatHub conversation.
	Started bool
	// TraceID correlates OnUpstream lifecycle frames with a request-level trace
	// record so the web layer can attach them to the correct debug entry.
	TraceID string
	// RequestID optionally pins the upstream request id that is returned in
	// Result.RequestID and used for the ChatHub session URL. When empty, a
	// random one is generated per request. Callers that need to reference the
	// id before the request completes (e.g. streaming pass-through frames) can
	// set it explicitly.
	RequestID string
	// BindAccount pins this request's ChatHub traffic to the proxy node bound
	// to the account ID (account-node binding in the outbound pool). Empty
	// keeps the pool's default round-robin behavior.
	BindAccount string
}

// StreamEvent is the protocol-neutral event exposed while ChatHub is still
// producing a response. Text events are safe to show immediately; progress and
// tool events are normally buffered by protocol adapters.
type StreamEvent struct {
	Kind        string
	Text        string
	MessageType string
	ContentType string
	ToolName    string
	Arguments   json.RawMessage
	Raw         json.RawMessage
}

type StreamHandler func(StreamEvent) error

type Result struct {
	Text           string
	Reasoning      string
	ConversationID string
	SessionID      string
	RequestID      string
	Throttling     any
	RawResult      string
	Events         []json.RawMessage
	Normalized     []Event
	Images         []string
	// TTFTMs is the time from upstream payload submission to the first visible
	// text delta, in milliseconds.
	TTFTMs int64
}

type Client struct {
	HTTPHeader http.Header
	HTTPClient *http.Client
	Dialer     *websocket.Dialer
	Pool       *ConnPool
	// Trace receives attachment-only metadata; URL contents are never exposed.
	Trace func(map[string]any)
	// OnUpstream receives request-lifecycle frames for debug tracing. Every
	// meta map is freshly allocated per call and must not be retained.
	// traceID correlates the frame with the originating trace record.
	OnUpstream func(traceID, stage string, meta map[string]any)
}

func NewClient() *Client {
	h := make(http.Header)
	h.Set("Origin", "https://m365.cloud.microsoft")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0")
	d := outbound.WebSocketDialer()
	return &Client{
		HTTPHeader: h,
		HTTPClient: outbound.HTTPClient(),
		Dialer:     d,
		Pool:       NewConnPool(d, h),
	}
}

// takeWithHeaders dials a pooled connection with per-request headers. It
// mirrors ConnPool.Take but accepts the caller's headers because the pool's
// fixed base header cannot carry the account token: the local security rule
// sends credentials in the "Authorization" header, never in the WS query
// string, so the token cannot be baked into the shared pool header up front.
// Dial failures with an HTTP status that should route into the web layer's
// cooldown (429/401/403) surface as *DialError, matching the direct-dial path.
func (p *ConnPool) takeWithHeaders(ctx context.Context, oid, tid, sessionID, wsURL string, header http.Header) (*websocket.Conn, bool, error) {
	key := p.key(oid, tid, sessionID)
	p.mu.Lock()
	pc := p.conns[key]
	if pc != nil {
		delete(p.conns, key)
	}
	p.mu.Unlock()
	if pc != nil {
		_ = pc.conn.SetReadDeadline(time.Time{})
		_ = pc.conn.SetWriteDeadline(time.Time{})
		return pc.conn, true, nil
	}
	conn, resp, err := p.dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil {
			retryAfter := 0
			if v, _ := strconv.Atoi(resp.Header.Get("Retry-After")); v > 0 {
				retryAfter = v
			}
			log.Printf("[connpool] dial failed oid=%s status=%d Retry-After=%d", oid, resp.StatusCode, retryAfter)
			if resp.StatusCode == 429 || resp.StatusCode == 401 || resp.StatusCode == 403 {
				return nil, false, &DialError{Status: resp.StatusCode, RetryAfter: retryAfter}
			}
		}
		return nil, false, err
	}
	return conn, false, nil
}

func (c *Client) Chat(ctx context.Context, acc Account, req Request) (Result, error) {
	return c.ChatWithDelta(ctx, acc, req, nil)
}

// ChatWithEvents is the compatibility entry point for the full event stream.
// The initial implementation exposes every upstream text delta immediately;
// the existing ChatWithDelta path remains the source of truth until the
// SignalR frame parser is migrated to emit progress/tool events as well.
func (c *Client) ChatWithEvents(ctx context.Context, acc Account, req Request, handler StreamHandler) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, func(text string) error {
		if handler == nil {
			return nil
		}
		return handler(StreamEvent{Kind: "text", Text: text})
	}, handler, nil)
}

// ChatWithDelta preserves Chat semantics while exposing upstream text deltas as
// soon as SignalR delivers them. onDelta must return quickly; returning an error
// cancels the request. Full snapshot messages are retained for final-result
// reconstruction but are not emitted as deltas, preventing duplicate text.
func (c *Client) ChatWithDelta(ctx context.Context, acc Account, req Request, onDelta func(string) error) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, onDelta, nil, nil)
}

// ChatWithRawEvents streams every raw SignalR frame to onRaw as soon as it is
// read from the socket, before any normalization. The web layer uses it for
// pass-through endpoints that must relay the native frame stream downstream
// without waiting for the full completion. onRaw must return quickly; an error
// cancels the request. The returned Result is identical to Chat's.
func (c *Client) ChatWithRawEvents(ctx context.Context, acc Account, req Request, onRaw func(json.RawMessage) error) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, nil, nil, onRaw)
}

// ChatWithReasoning is the streaming entry point used by the OpenAI-compatible
// layer. onDelta receives answer text tokens, onReasoning receives the
// multi-step ChainOfThought transcript that ChatHub marks with
// contentOrigin=ChainOfThoughtSummary / addToChainOfThought=true.
func (c *Client) ChatWithReasoning(ctx context.Context, acc Account, req Request, onDelta func(string) error, onReasoning func(string) error) (Result, error) {
	return c.chatWithHandlers(ctx, acc, req, onDelta, func(ev StreamEvent) error {
		if ev.Kind == "reasoning" && ev.Text != "" && onReasoning != nil {
			return onReasoning(ev.Text)
		}
		return nil
	}, nil)
}

func (c *Client) chatWithHandlers(ctx context.Context, acc Account, req Request, onDelta func(string) error, onEvent StreamHandler, onRaw func(json.RawMessage) error) (Result, error) {
	startedAt := time.Now()
	log.Printf("chathub timing start prompt_len=%d", len(req.Text))
	if acc.AccessToken == "" || acc.OID == "" || acc.TID == "" {
		return Result{}, fmt.Errorf("missing access token / oid / tid")
	}
	if strings.TrimSpace(req.Text) == "" && len(req.Attachments) == 0 {
		return Result{}, fmt.Errorf("empty prompt and no attachments")
	}
	if req.Tone == "" {
		req.Tone = defaultTone
	}
	firstTurn := req.Started
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
		firstTurn = true
	}
	if req.ConversationID == "" {
		req.ConversationID = uuid.NewString()
		firstTurn = true
	}
	requestID := req.RequestID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	// Account-node binding: when the request names a bound account, its
	// ChatHub traffic (attachment upload/download and the WebSocket stream)
	// goes through the proxy node pinned to that account.
	httpClient := c.HTTPClient
	dialer := c.Dialer
	if req.BindAccount != "" {
		httpClient = outbound.HTTPClientFor(req.BindAccount)
		dialer = outbound.WebSocketDialerFor(req.BindAccount)
	}
	// Attachment upload and the WebSocket dial are independent network round
	// trips. Run them concurrently so total latency is max(upload, dial) instead
	// of upload+dial (upstream perf fix: saves ~200ms when images are present).
	attachCh := make(chan error, 1)
	if len(req.Attachments) > 0 {
		go func() { attachCh <- c.uploadAttachments(ctx, acc, req.ConversationID, req.Attachments, httpClient) }()
	}

	wsURL, err := buildWSURL(acc, req.SessionID, req.ConversationID, requestID)
	if err != nil {
		return Result{}, err
	}

	dialStarted := time.Now()
	// The access token travels in the Authorization header, never in the WS
	// query string: query tokens leak into proxy logs, HTTP traces and error
	// output. If an upstream ever requires query auth, re-introduce it with a
	// documented warning that proxy logs must not capture query strings.
	dialHeaders := c.HTTPHeader.Clone()
	if dialHeaders == nil {
		dialHeaders = make(http.Header)
	}
	dialHeaders.Set("Authorization", "Bearer "+acc.AccessToken)

	var conn *websocket.Conn
	var reused bool
	if c.Pool != nil && req.BindAccount == "" {
		var poolErr error
		conn, reused, poolErr = c.Pool.takeWithHeaders(ctx, acc.OID, acc.TID, req.SessionID, wsURL, dialHeaders)
		if poolErr != nil {
			if c.OnUpstream != nil {
				c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": "ws dial: " + poolErr.Error()})
			}
			return Result{}, fmt.Errorf("ws dial: %w", poolErr)
		}
	}
	if conn == nil {
		var resp *http.Response
		conn, resp, err = dialer.DialContext(ctx, wsURL, dialHeaders)
		if err != nil {
			if resp != nil && (resp.StatusCode == 429 || resp.StatusCode == 401 || resp.StatusCode == 403) {
				retryAfter := 0
				if v, _ := strconv.Atoi(resp.Header.Get("Retry-After")); v > 0 {
					retryAfter = v
				}
				log.Printf("chathub ws_dial %d Retry-After=%d", resp.StatusCode, retryAfter)
				if c.OnUpstream != nil {
					c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": fmt.Sprintf("ws dial: upstream %d", resp.StatusCode)})
				}
				return Result{}, &DialError{Status: resp.StatusCode, RetryAfter: retryAfter}
			}
			if c.OnUpstream != nil {
				c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": "ws dial: " + err.Error()})
			}
			return Result{}, fmt.Errorf("ws dial: %w", err)
		}
	}
	log.Printf("chathub timing ws_dial_ms=%d total_ms=%d reused=%t", time.Since(dialStarted).Milliseconds(), time.Since(startedAt).Milliseconds(), reused)

	// Bound every WebSocket message before the handshake or streaming loop reads it.
	// This prevents a malformed or compromised upstream from causing unbounded allocation.
	conn.SetReadLimit(8 << 20)

	// Gorilla WebSocket permits one concurrent writer. Serialize all writes so
	// keepalive and request writes cannot corrupt the connection.
	var writeMu sync.Mutex
	wsWrite := func(msgType int, data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(msgType, data)
	}

	returnConn := true
	defer func() {
		if returnConn && conn != nil && c.Pool != nil {
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			c.Pool.Return(acc.OID, acc.TID, req.SessionID, conn)
		} else if conn != nil {
			conn.Close()
		}
	}()

	if len(req.Attachments) > 0 {
		if attachErr := <-attachCh; attachErr != nil {
			returnConn = false
			if c.OnUpstream != nil {
				c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": attachErr.Error()})
			}
			return Result{}, fmt.Errorf("upload attachment: %w", attachErr)
		}
	}

	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))

	if !reused {
		if err := wsWrite(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+rs)); err != nil {
			returnConn = false
			if c.OnUpstream != nil {
				c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": "handshake send: " + err.Error()})
			}
			return Result{}, fmt.Errorf("handshake send: %w", err)
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			returnConn = false
			if c.OnUpstream != nil {
				c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": "handshake recv: " + err.Error()})
			}
			return Result{}, fmt.Errorf("handshake recv: %w", err)
		}
	}

	payload := chatPayload(req.Text, req.SessionID, req.ConversationID, requestID, req.Tone, firstTurn, req.Attachments, req.Tools, req.ToolChoice, req.MCPServerURL)
	log.Printf("chathub prompt-trace text=%d tools=%d payload=%d", len(req.Text), len(req.Tools), len(payload))
	if c.OnUpstream != nil {
		c.OnUpstream(req.TraceID, "upstream_request", map[string]any{
			"endpoint":        "ws://substrate.office.com/m365Copilot/Chathub",
			"conversation_id": req.ConversationID,
			"session_id":      req.SessionID,
			"prompt_len":      len(req.Text),
			"tone":            req.Tone,
			"tools":           len(req.Tools),
			"attachments":     len(req.Attachments),
			"payload":         payload,
		})
	}
	if c.Trace != nil {
		meta := map[string]any{"stage": "chathub_payload", "attachment_count": len(req.Attachments), "payload_has_attachments": strings.Contains(payload, `"attachments"`), "attachments": []map[string]any{}}
		for _, a := range req.Attachments {
			meta["attachments"] = append(meta["attachments"].([]map[string]any), map[string]any{"type": a.Type, "mime_type": a.MimeType, "url_length": len(a.URL), "data_url": strings.HasPrefix(a.URL, "data:"), "name": a.Name})
		}
		c.Trace(meta)
	}
	log.Printf("chathub timing handshake_ms=%d", time.Since(dialStarted).Milliseconds())
	payloadSentAt := time.Now()
	if err := wsWrite(websocket.TextMessage, []byte(payload)); err != nil {
		returnConn = false
		if c.OnUpstream != nil {
			c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": "chat send: " + err.Error()})
		}
		return Result{}, fmt.Errorf("chat send: %w", err)
	}

	var deltas []string
	var streamed strings.Builder
	var firstDeltaAt time.Time
	emitDelta := func(d string) error {
		if d == "" {
			return nil
		}
		if chTrace {
			log.Printf("[trace:emitDelta] len=%d streamed=%d preview=%q", len(d), streamed.Len()+len(d), truncate(d, 80))
		}
		if streamed.Len() == 0 {
			firstDeltaAt = time.Now()
			log.Printf("chathub timing first_delta_ms=%d len=%d", firstDeltaAt.Sub(payloadSentAt).Milliseconds(), len(d))
			if c.OnUpstream != nil {
				c.OnUpstream(req.TraceID, "upstream_first_delta", map[string]any{
					"first_delta_ms": firstDeltaAt.Sub(payloadSentAt).Milliseconds(),
					"len":            len(d),
				})
			}
		}
		streamed.WriteString(d)
		deltas = append(deltas, d)
		if onDelta != nil {
			return onDelta(d)
		}
		return nil
	}
	// ChatHub signals text either as a full snapshot or as cursor rewrites.
	// Only the portion not already streamed may be emitted; naive prefix
	// checks misfire when upstream rewrites the whole buffer, which duplicated
	// answers (AAA…). Match any overlap and emit the tail.
	// Upstream rate limiting surfaces as a human-readable notice on the text
	// channel instead of an HTTP 429. Detect it before any real content has
	// streamed so the web layer can fail over rather than answer with it.
	// The "throttling" frame itself is per-conversation quota metadata and is
	// NOT a rate-limit signal.
	rateLimited := func(text string) bool {
		if streamed.Len() != 0 {
			return false
		}
		t := strings.ToLower(text)
		return strings.Contains(t, "temporarily unable to respond to this many requests") ||
			strings.Contains(t, "太多请求") ||
			strings.Contains(t, "无法响应这么多请求") ||
			strings.Contains(t, "too many requests") ||
			strings.Contains(t, "please retry") && strings.Contains(t, "later")
	}
	emitSnapshot := func(snapshot string) error {
		if snapshot == "" {
			return nil
		}
		if chTrace {
			log.Printf("[trace:emitSnapshot] cur=%d snapshot=%d", streamed.Len(), len(snapshot))
		}
		if rateLimited(snapshot) {
			return ErrRateLimitNotice
		}
		cur := streamed.String()
		if cur == "" {
			return emitDelta(snapshot)
		}
		if strings.HasPrefix(snapshot, cur) {
			return emitDelta(snapshot[len(cur):])
		}
		if len(snapshot) <= len(cur) {
			return nil
		}
		log.Printf("[emitSnapshot] skip: cur=%d snapshot=%d (non-prefix rewrite)", len(cur), len(snapshot))
		return nil
	}
	var final string
	var throttling any
	var rawResult string
	var events []json.RawMessage
	seenStreamTools := map[string]bool{}
	var reasoningBuf strings.Builder

	deadline := time.Now().Add(5 * time.Minute)
	type wsRead struct {
		msg []byte
		err error
	}
	readCh := make(chan wsRead, 1)
	go func() {
		for {
			_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			_, msg, err := conn.ReadMessage()
			readCh <- wsRead{msg: msg, err: err}
			if err != nil {
				return
			}
		}
	}()
	for time.Now().Before(deadline) {
		var read wsRead
		select {
		case <-ctx.Done():
			returnConn = false
			if c.OnUpstream != nil {
				c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": ctx.Err().Error()})
			}
			return Result{}, ctx.Err()
		case read = <-readCh:
		}
		if read.err != nil {
			returnConn = false
			// Never convert a timeout or dropped WebSocket into a successful
			// partial response. A response is complete only after SignalR type 3.
			if c.OnUpstream != nil {
				c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": "ws read before completion: " + read.err.Error()})
			}
			return Result{}, fmt.Errorf("ws read before completion: %w", read.err)
		}
		for _, part := range strings.Split(string(read.msg), rs) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if chTrace {
				log.Printf("[trace:ws] frame_len=%d preview=%q", len(part), truncate(part, 120))
			}
			b := []byte(part)
			events = append(events, json.RawMessage(b))
			if onRaw != nil {
				if err := onRaw(json.RawMessage(b)); err != nil {
					returnConn = false
					return Result{}, err
				}
			}
			var obj map[string]any
			if err := json.Unmarshal(b, &obj); err != nil {
				continue
			}
			t, _ := obj["type"].(float64)
			target, _ := obj["target"].(string)

			// SignalR ping
			if int(t) == 6 {
				_ = wsWrite(websocket.TextMessage, []byte(`{"type":6}`+rs))
				continue
			}

			if int(t) == 1 && target == "update" {
				args, _ := obj["arguments"].([]any)
				for _, raw := range args {
					arg, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					msgs, _ := arg["messages"].([]any)
					if onEvent != nil {
						for _, ev := range extractToolEvents(arg, seenStreamTools) {
							if err := onEvent(ev); err != nil {
								returnConn = false
								return Result{}, err
							}
						}
					}

					for _, ev := range classifyUpdateMessages(msgs) {
						if ev.Kind == "reasoning" {
							reasoningBuf.WriteString(ev.Text)
						}
						ev.Raw = eventRaw(arg)
						if ev.Kind != "text" && onEvent != nil {
							if err := onEvent(ev); err != nil {
								returnConn = false
								return Result{}, err
							}
						}
					}
					if thr, ok := arg["throttling"]; ok {
						throttling = thr
					}
					// writeAtCursor is assistant output belonging to the update argument
					// itself. Do not discard it merely because messages[] also contains a
					// progress, search, code, or tool event. ChatHub can coalesce task
					// progress and the visible task-completion summary into one update.
					if w := updateCursorSnapshot(arg); w != "" {
						if err := emitSnapshot(w); err != nil {
							returnConn = false
							return Result{}, err
						}
					}
					if msgs, ok := arg["messages"].([]any); ok {
						for _, mraw := range msgs {
							m, ok := mraw.(map[string]any)
							if !ok {
								continue
							}
							author, _ := m["author"].(string)
							text, _ := m["text"].(string)
							mt, _ := m["messageType"].(string)
							if author == "bot" && mt == "" && text != "" {
								// ChatHub often sends the first visible text as a full snapshot,
								// followed by cursor deltas. Emit only the unseen suffix.
								if err := emitSnapshot(text); err != nil {
									returnConn = false
									return Result{}, err
								}
							}
						}
					}
				}
				continue
			}

			if int(t) == 2 {
				item, _ := obj["item"].(map[string]any)
				if item != nil {
					if thr, ok := item["throttling"]; ok {
						throttling = thr
					}
					if res, ok := item["result"].(map[string]any); ok {
						rawResult, _ = res["value"].(string)
						if msg, ok := res["message"].(string); ok {
							final = msg
							if rateLimited(final) {
								returnConn = false
								return Result{}, ErrRateLimitNotice
							}
						}
					}
				}
				// completion frame often follows; keep reading a bit but we already have content
				continue
			}

			if int(t) == 3 {
				if errObj, ok := obj["error"].(map[string]any); ok {
					returnConn = false
					if c.OnUpstream != nil {
						c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": fmt.Sprintf("chathub completion error: %v", errObj)})
					}
					return Result{}, fmt.Errorf("chathub completion error: %v", errObj)
				}
				log.Printf("chathub timing completion_frame_ms=%d streamed_text=%d events=%d", time.Since(payloadSentAt).Milliseconds(), streamed.Len(), len(events))
				text := streamed.String()
				// The visible text can be truncated when ChatHub rewrites the
				// answer buffer: writeAtCursor snapshots that do not extend the
				// already-streamed prefix are skipped to avoid duplicate output,
				// so a long answer that gets restructured mid-stream can end up
				// with only its opening streamed. The result frame's message is
				// the authoritative complete answer, so prefer it whenever it is
				// at least as complete as the delta-assembled text.
				if final != "" && len(final) >= len(text) {
					text = final
				}
				if text == "" {
					text = strings.Join(deltas, "")
				}
				if rateLimited(text) {
					returnConn = false
					if c.OnUpstream != nil {
						c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": ErrRateLimitNotice.Error()})
					}
					return Result{}, ErrRateLimitNotice
				}
				if text == "" {
					returnConn = false
					return Result{}, ErrEmptyCompletion
				}
				ttft := int64(0)
				if !firstDeltaAt.IsZero() {
					ttft = firstDeltaAt.Sub(payloadSentAt).Milliseconds()
				}
				if c.OnUpstream != nil {
					preview := text
					if len(preview) > 500 {
						preview = preview[:500]
					}
					reasoning := reasoningBuf.String()
					if len(reasoning) > 4096 {
						reasoning = reasoning[:4096]
					}
					c.OnUpstream(req.TraceID, "upstream_response", map[string]any{
						"text":          text,
						"reasoning":     reasoning,
						"text_len":      len(text),
						"reasoning_len": reasoningBuf.Len(),
						"events":        len(events),
						"ttft_ms":       ttft,
						"text_preview":  preview,
					})
				}
				return Result{
					Text:           text,
					Reasoning:      reasoningBuf.String(),
					ConversationID: req.ConversationID,
					SessionID:      req.SessionID,
					RequestID:      requestID,
					Throttling:     throttling,
					RawResult:      rawResult,
					Events:         events,
					Normalized:     NormalizeEvents(events),
					Images:         imageURLs(events),
					TTFTMs:         ttft,
				}, nil
			}
		}
	}

	// Reaching the overall deadline without a SignalR completion frame is
	// an incomplete upstream response. Do not return accumulated deltas as if
	// they were a successful, finished answer.
	returnConn = false
	if c.OnUpstream != nil {
		c.OnUpstream(req.TraceID, "upstream_error", map[string]any{"error": "chathub response deadline exceeded before completion"})
	}
	return Result{}, fmt.Errorf("chathub response deadline exceeded before completion")
}

func buildWSURL(acc Account, sessionID, conversationID, requestID string) (string, error) {
	q := url.Values{}
	q.Set("chatsessionid", requestID)
	q.Set("clientrequestid", requestID)
	q.Set("X-SessionId", sessionID)
	q.Set("ConversationId", conversationID)
	// Note: the access token is deliberately NOT part of the query string.
	// It is sent as "Authorization: Bearer" on the dial request so it never
	// lands in proxy logs / traces. See Client.Chat.
	q.Set("variants", variants)
	// source must keep quotes like the browser probe
	q.Set("source", `"officeweb"`)
	q.Set("product", "Office")
	q.Set("agentHost", "Bizchat.FullScreen")
	q.Set("licenseType", "Starter")
	q.Set("agent", "web")
	q.Set("scenario", "OfficeWebIncludedCopilot")

	// url.Values encodes quotes; probe used safe='",' so keep quotes unescaped-ish.
	// Gorilla/url will encode " to %22 which MS accepts.
	u := fmt.Sprintf("%s/%s@%s?%s", wsBase, acc.OID, acc.TID, q.Encode())
	return u, nil
}

func (c *Client) uploadAttachments(ctx context.Context, acc Account, conversationID string, attachments []Attachment, httpClient *http.Client) error {
	imageCount := 0
	for i := range attachments {
		a := &attachments[i]
		if a.Type != "image" {
			continue
		}
		imageCount++
		if imageCount > maxAttachments {
			return fmt.Errorf("too many image attachments: limit is %d", maxAttachments)
		}
		// For non-data URLs, download the image first
		imageData := a.URL
		if !strings.HasPrefix(a.URL, "data:") {
			if err := ValidateRemoteDownloadURL(a.URL); err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
			if err != nil {
				return fmt.Errorf("attachment %d: create request: %w", i, err)
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("attachment %d: download: %w", i, err)
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttachmentMiB<<20))
			resp.Body.Close()
			if err != nil {
				return fmt.Errorf("attachment %d: read body: %w", i, err)
			}
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("attachment %d: HTTP %d", i, resp.StatusCode)
			}
			mimeType := resp.Header.Get("Content-Type")
			if mimeType == "" {
				mimeType = "image/png"
			}
			imageData = "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(body)
		}
		comma := strings.IndexByte(imageData, ',')
		if comma < 0 {
			return fmt.Errorf("invalid image data URL")
		}
		encoded := imageData[comma+1:]
		if !strings.Contains(strings.ToLower(imageData[:comma]), ";base64") {
			return fmt.Errorf("image URL is not base64")
		}
		if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
			return fmt.Errorf("decode image: %w", err)
		}
		form := url.Values{}
		form.Set("scenario", "UploadImage")
		form.Set("conversationId", conversationID)
		// The browser sends the complete data URL in FileBase64, including the
		// media-type prefix. UploadFile accepts this form and returns docId.
		// Live-verified 2026-08-08: UploadFile rejects multipart bodies
		// (HTTP 400 InvalidRequest); it requires x-www-form-urlencoded like
		// PyRIT's httpx client sends.
		form.Set("FileBase64", imageData)
		if c.Trace != nil {
			c.Trace(map[string]any{"stage": "upload_start", "index": i, "conversation_id": conversationID, "mime_type": a.MimeType, "base64_length": len(encoded), "token_present": acc.AccessToken != ""})
		}
		form.Add("optionsSets", "cwcgptvsan")
		form.Add("optionsSets", "flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://substrate.office.com/m365Copilot/UploadFile", strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if acc.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
		}
		req.Header.Set("Accept", "application/json")
		// Required by the enterprise Copilot UploadFile image-input path.
		// This feature gate is documented in the prior reverse-proxy research
		// and mirrors the PyRIT request flow.
		req.Header.Set("X-Variants", "feature.EnableImageSupportInUploadFile")
		req.Header.Set("X-Scenario", "OfficeWebIncludedCopilot")
		req.Header.Set("Referer", "https://m365.cloud.microsoft/")
		for k, vv := range c.HTTPHeader {
			for _, v := range vv {
				if k != "Origin" || v != "" {
					req.Header.Add(k, v)
				}
			}
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("upload image: %w", err)
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("read upload response: %w", readErr)
		}
		if len(data) > 2<<20 {
			return fmt.Errorf("upload response exceeds %d bytes", 2<<20)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("upload returned %s: %s", resp.Status, strings.TrimSpace(string(data[:minInt(len(data), 500)])))
		}
		var out struct {
			DocID    string `json:"docId"`
			FileName string `json:"fileName"`
			FileType string `json:"fileType"`
			Result   struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return fmt.Errorf("decode upload response: %w", err)
		}
		if out.Result.Value != "Success" || out.DocID == "" {
			return fmt.Errorf("upload failed: %s", strings.TrimSpace(string(data[:minInt(len(data), 500)])))
		}
		a.DocID = out.DocID
		a.FileType = strings.TrimPrefix(strings.ToLower(out.FileType), ".")
		// ChatHub's ImageFile annotation uses jpg for JPEG uploads.
		if a.FileType == "jpeg" {
			a.FileType = "jpg"
		}
		if a.Name == "" {
			a.Name = out.FileName
		}
		if c.Trace != nil {
			c.Trace(map[string]any{"stage": "upload_success", "doc_id": a.DocID, "file_name": a.Name, "file_type": a.FileType})
		}
	}
	return nil
}

func chatPayload(text, sessionID, conversationID, requestID, tone string, firstTurn bool, attachments []Attachment, tools []Tool, toolChoice any, mcpServerURL string) string {
	text = toolProtocolPrompt(text, tools, toolChoice, len(clientPlugins(tools, mcpServerURL)) > 0)
	message := map[string]any{
		"author":                "user",
		"attachments":           attachments,
		"inputMethod":           "Keyboard",
		"text":                  text,
		"entityAnnotationTypes": []string{"People", "File", "Event", "Email", "TeamsMessage"},
		"requestId":             requestID,
		"locationInfo": map[string]any{
			"timeZoneOffset": 8,
			"timeZone":       "Asia/Shanghai",
		},
		"locale":            "zh-cn",
		"messageType":       "Chat",
		"experienceType":    "Default",
		"adaptiveCards":     []any{},
		"clientPreferences": map[string]any{},
	}
	// The browser does not send an OpenAI attachments array to ChatHub. It
	// sends a file annotation after the file has been uploaded by Office.
	annotations := make([]any, 0, len(attachments))
	for _, a := range attachments {
		if a.Type != "image" || a.DocID == "" {
			continue
		}
		if a.Name == "" {
			a.Name = "image." + a.FileType
		}
		fileType := a.FileType
		if fileType == "" {
			fileType = strings.TrimPrefix(strings.ToLower(a.MimeType), "image/")
		}
		if fileType == "" || fileType == "image" || fileType == "*" {
			fileType = "jpg"
		}
		annotations = append(annotations, map[string]any{
			"id": a.DocID,
			"messageAnnotationMetadata": map[string]any{
				"@type": "File", "annotationType": "File",
				"fileType": fileType, "fileName": a.Name,
			},
			"messageAnnotationType": "ImageFile",
		})
	}
	if len(annotations) > 0 {
		message["messageAnnotations"] = annotations
		message["connectedFederatedConnections"] = []string{"dummyId"}
	}
	// Restore the old gateway's multimodal injection path. The historical
	// implementation merged imageUrl/imageBase64 directly into message rather
	// than relying solely on the newer attachments array.
	for _, a := range attachments {
		if a.Type != "image" || a.URL == "" {
			continue
		}
		if strings.HasPrefix(a.URL, "data:") {
			if comma := strings.IndexByte(a.URL, ','); comma >= 0 && comma+1 < len(a.URL) {
				message["imageBase64"] = a.URL[comma+1:]
			}
		} else {
			message["imageUrl"] = a.URL
		}
		break
	}
	optionsSets := []any{
		"search_result_progress_messages_with_search_queries",
		"update_textdoc_response_after_streaming",
		"deepleo_networking_timeout_10minutes_canmore",
		"cwc_flux_image",
		"cwcfluxgptv",
		"flux_v3_gptv_enable_upload_multi_image_in_turn_wo_ch",
		"gptvnorm2048",
		"cwc_fileupload_odb",
		"update_memory_plugin",
		"add_custom_instructions",
		"cwc_flux_v3",
		"flux_v3_progress_messages",
		"enable_batch_token_processing",
		"enable_gg_gpt",
	}
	chat := map[string]any{
		"arguments": []any{
			map[string]any{
				"source":              "officeweb",
				"clientCorrelationId": uuid.NewString(),
				"sessionId":           sessionID,
				"optionsSets":         optionsSets,
				"options":             map[string]any{},
				"allowedMessageTypes": []string{
					"Chat", "Suggestion", "Disengaged", "Progress", "EndOfRequest", "InternalLoaderMessage",
				},
				"sliceIds":          []any{},
				"threadLevelGptId":  map[string]any{},
				"conversationId":    conversationID,
				"traceId":           uuid.NewString(),
				"isStartOfSession":  firstTurn,
				"productThreadType": "Office",
				"clientInfo": map[string]any{
					"clientPlatform": "mcmcopilot-web",
					"clientAppName":  "Office",
				},
				"tone":          tone,
				"streamingMode": "ConciseWithPadding",
				"message":       message,

				"plugins":    clientPlugins(tools, mcpServerURL),
				"toolChoice": toolChoice,
			},
		},
		"invocationId": "0",
		"target":       "chat",
		"type":         4,
	}
	metrics := map[string]any{
		"arguments": []any{
			map[string]any{
				"Timestamps": map[string]string{
					"ConnectionStart":       "",
					"UserInputStart":        "",
					"ConnectionEstablished": "",
					"UserInputSubmit":       "",
				},
			},
		},
		"target": "Metrics",
		"type":   1,
	}
	b1, _ := json.Marshal(chat)
	b2, _ := json.Marshal(metrics)
	return string(b1) + rs + string(b2) + rs
}
