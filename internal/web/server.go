package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"m365-copilot2api/internal/mcp"
	"m365-copilot2api/internal/outbound"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

type pendingPKCE struct {
	Verifier    string
	Created     time.Time
	Status      string
	Account     any
	Error       string
	RedirectURI string
}

const defaultRateLimitCooldownSeconds = 3600

// rateLimitCooldown returns the configured per-account rate-limit cooldown
// window (web-console setting, M365_RATE_LIMIT_COOLDOWN_SECONDS env, or the
// 1-hour default). During this window the account is not scheduled to avoid
// hammering the upstream service.
func rateLimitCooldown() time.Duration {
	sec := currentSettings().AccountRateLimitCooldownSeconds
	if sec < 1 {
		sec = defaultRateLimitCooldownSeconds
	}
	return time.Duration(sec) * time.Second
}

const maxAccountProbe = 16

const rateLimitProbePrompt = "Reply with exactly: OK"

func (s *Server) markAccountResult(accountID string, err error) {
	if s == nil || s.accountPool == nil || accountID == "" {
		return
	}
	if err != nil {
		s.accountPool.MarkFailure(accountID, err, rateLimitCooldown())
		// A rate-limited account must immediately release its runtime proxy-pool
		// binding so another available account can use that proxy node. This does
		// not alter any manually configured persistent proxy URL.
		if IsRateLimited(err) {
			// Treat available accounts as a queue: once throttled, this account
			// moves behind the others and can re-enter after the queue cycles and
			// its cooldown expires.
			if s.tokens != nil {
				s.tokens.MoveToBack(accountID)
			}
			if pool := outbound.CurrentPool(); pool != nil {
				pool.Unbind(accountID)
			}
		}
		return
	}
	s.accountPool.MarkSuccess(accountID)
}

// confirmRateLimitNotice verifies a text-channel rate-limit notice with a
// separate, fresh ChatHub conversation. A single notice is not enough to cool
// down an account because the upstream can occasionally emit a false positive.
func (s *Server) confirmRateLimitNotice(ctx context.Context, acc auth.AccountToken, noticeErr error) (bool, error) {
	if !errors.Is(noticeErr, chathub.ErrRateLimitNotice) {
		return IsRateLimited(noticeErr), noticeErr
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, probeErr := s.chatWithAccount(probeCtx, acc.ID, chathub.Account{
		AccessToken: acc.AccessToken,
		OID:         acc.OID,
		TID:         acc.TID,
	}, chathub.Request{
		Text:    rateLimitProbePrompt,
		Tone:    "magic",
		Started: true,
		// 鎺㈡祴璧板悓涓€璐﹀彿缁戝畾鐨勮妭鐐癸紝閬垮厤 RateLimit 鎺㈡祴涓插埌鍏跺畠璐﹀彿鐨勮妭鐐广€?
		BindAccount: acc.ID,
	})
	if probeErr == nil {
		return false, nil
	}
	if errors.Is(probeErr, chathub.ErrRateLimitNotice) || IsRateLimited(probeErr) {
		return true, &UpstreamHTTPError{
			Status:     http.StatusTooManyRequests,
			RetryAfter: int(rateLimitCooldown().Seconds()),
		}
	}
	return false, probeErr
}

type Server struct {
	mu                  sync.Mutex
	tokens              *auth.Store
	accountPool         *accountHealth
	accountConcurrency  *accountConcurrency
	sessionConcurrency  *sessionConcurrency
	pkce                map[string]pendingPKCE
	pkceStarts          map[string][]time.Time
	chat                *chathub.Client
	proxyClients        sync.Map
	sessions            *sessionStore
	userSessions        *userSessionStore
	sessionResolver     *sessionResolver
	conversationManager *conversationManager
	adminPassword       string
	adminSessions       map[string]time.Time
	adminSessionTTL     time.Duration
	mustChangePassword  bool
	loginAttempts       map[string]loginAttempt
	apiKeys             *apiKeyStore
	debug               *debugStore
	settings            *settingsStore
	responseMu          sync.Mutex
	responseMessages    map[string]map[string]respHistory
	usage               *usageLog
	trace               *traceStore
	generatedImages     map[string]*generatedImage
	generatedImagesMu   sync.Mutex
	pendingToolsMu      sync.Mutex
	pendingTools        map[string]map[string]pendingToolCall // tenant -> callID -> pendingToolCall
}

const maxResponsesPerTenant = 256

type respHistory struct {
	At       time.Time
	Messages []oaiMsg
}

func (s *Server) clientForProxy(proxyURL string) *chathub.Client {
	if proxyURL == "" {
		return s.chat
	}
	if v, ok := s.proxyClients.Load(proxyURL); ok {
		return v.(*chathub.Client)
	}
	clients, err := outbound.New(proxyURL)
	if err != nil {
		log.Printf("[bound-proxy] invalid proxy %q: %v", proxyURL, err)
		return s.chat
	}
	c := &chathub.Client{
		HTTPHeader: make(http.Header),
		HTTPClient: clients.HTTP,
		Dialer:     clients.WebSocket,
		Pool:       chathub.NewConnPool(clients.WebSocket, make(http.Header)),
		Trace:      s.chat.Trace,
	}
	c.HTTPHeader.Set("Origin", "https://m365.cloud.microsoft")
	c.HTTPHeader.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0")
	actual, _ := s.proxyClients.LoadOrStore(proxyURL, c)
	return actual.(*chathub.Client)
}

func New() (*Server, error) {
	store, err := auth.OpenStore("")
	if err != nil {
		return nil, err
	}
	password, mustChange := loadAdminPassword()
	if password == "" {
		return nil, fmt.Errorf("administrator password is not configured; set M365_ADMIN_PASSWORD, M365_ADMIN_PASSWORD_FILE, or M365_ADMIN_PASSWORD_BOOTSTRAP_FILE")
	}
	adminSessions, err := loadAdminSessions(time.Now())
	if err != nil {
		return nil, fmt.Errorf("load administrator sessions: %w", err)
	}
	adminTTL := adminSessionTTL()
	sessionTTL := 30 * time.Minute
	if v := os.Getenv("M365_USER_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			sessionTTL = d
		}
	}
	srv := &Server{
		tokens:             store,
		accountPool:        newAccountHealth(),
		accountConcurrency: newAccountConcurrency(),
		sessionConcurrency: newSessionConcurrency(),
		pkce:               map[string]pendingPKCE{},
		chat: func() *chathub.Client {
			c := chathub.NewClient()
			c.Trace = func(meta map[string]any) { fmt.Printf("[multimodal-trace] %s\\n", mustJSON(meta)) }
			return c
		}(),
		sessions:            openSessionStore(),
		userSessions:        openUserSessionStore(sessionTTL),
		sessionResolver:     openSessionResolver(),
		conversationManager: openConversationManager(),
		adminPassword:       password,
		adminSessions:       adminSessions,
		adminSessionTTL:     adminTTL,
		mustChangePassword:  mustChange,
		loginAttempts:       map[string]loginAttempt{},
		apiKeys:             openAPIKeys(),
		debug:               openDebugStore(),
		settings:            openSettingsStore(),
		responseMessages:    map[string]map[string]respHistory{},
		pendingTools:        map[string]map[string]pendingToolCall{},
		usage:               openUsageLog(),
		trace:               openTraceStore(),
		generatedImages:     make(map[string]*generatedImage),
	}
	srv.chat.OnUpstream = srv.routeUpstreamTrace
	srv.applyPersistedAccountConcurrency()
	return srv, nil
}

// applyPersistedAccountConcurrency seeds the runtime per-account concurrency
// limiter from settings.json after startup. newAccountConcurrency only reads
// M365_ACCOUNT_DEFAULT_CONCURRENCY / the built-in default, so without this an
// upgrade silently reset the per-account limit to 1 while settings.json still
// held the configured value.
func (s *Server) applyPersistedAccountConcurrency() {
	if s.settings == nil || s.accountConcurrency == nil {
		return
	}
	if saved := s.settings.get().AccountConcurrency; saved >= 1 {
		s.accountConcurrency.SetLimit(saved)
	}
}

// routeUpstreamTrace forwards ChatHub lifecycle frames (request payload, first
// delta, final response, errors) into the matching request-trace record.
func (s *Server) routeUpstreamTrace(traceID, stage string, meta map[string]any) {
	if traceID == "" || !traceEnabled() {
		return
	}
	s.trace.update(traceID, func(rec *traceRecord) {
		switch stage {
		case "upstream_request":
			payload := fmt.Sprint(meta["payload"])
			if len(payload) > maxTraceCaptureBytes {
				payload = payload[:maxTraceCaptureBytes]
			}
			rec.UpstreamReq = redactBody([]byte(payload))
			rec.UpstreamError = ""
		case "upstream_first_delta":
			if ms, ok := meta["first_delta_ms"].(int64); ok {
				rec.TTFTMs = ms
			}
		case "upstream_response":
			if ms, ok := meta["ttft_ms"].(int64); ok {
				rec.TTFTMs = ms
			}
			text := fmt.Sprint(meta["text"])
			if len(text) > maxTraceCaptureBytes {
				text = text[:maxTraceCaptureBytes]
			}
			reasoning := fmt.Sprint(meta["reasoning"])
			if len(reasoning) > maxTraceCaptureBytes {
				reasoning = reasoning[:maxTraceCaptureBytes]
			}
			rec.UpstreamResp = map[string]any{
				"text":         text,
				"reasoning":    reasoning,
				"events":       meta["events"],
				"text_preview": meta["text_preview"],
			}
			// Do NOT mark the record "success" here. This stage fires as soon
			// as the upstream (ChatHub) response is complete, but the handler
			// may still be streaming deltas downstream (SSE) or running
			// post-processing (tool validation / clean-branch repair). The
			// record must stay "in_progress" until the trace middleware's
			// finish callback runs after the handler returns.
		case "upstream_error":
			rec.UpstreamError = fmt.Sprint(meta["error"])
			rec.Error = fmt.Sprint(meta["error"])
			rec.Status = "error"
			rec.StatusCode = http.StatusBadGateway
		}
	})
}

// markTraceError records a terminal error state on the request trace when the
// handler fails after the upstream response completed (for example a streaming
// workspace/tool repair failure). Without it the trace middleware would default
// an HTTP-200 streaming response to "success" even though an SSE error event
// was sent downstream.
func (s *Server) markTraceError(r *http.Request, err error, statusCode int) {
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			if err != nil {
				rec.Error = truncatedError(err)
			}
			rec.Status = "error"
			rec.StatusCode = statusCode
		})
	}
}

func (s *Server) StartConvCacheGC() {
	// Conversation reuse is explicit-session-id driven only; there is no
	// content-keyed conversation cache to garbage collect.
}

func (s *Server) InitM365CloudClient() {
	accounts := s.tokens.List()
	if len(accounts) == 0 {
		return
	}
	acc := accounts[0]
	clientID := os.Getenv("M365_CLIENT_ID")
	if clientID == "" {
		clientID = acc.ClientID
	}
	if clientID == "" {
		clientID = auth.DefaultClientID
	}
	InitM365CloudClient(clientID, acc.TID, acc.RefreshToken)
	log.Printf("[m365-cloud] client initialized for account %s", acc.Email)
}

func (s *Server) RefreshExpiredTokens() {
	results := s.tokens.RefreshAllExpired()
	for _, r := range results {
		if r.Success {
			log.Printf("[token-refresh] account=%s refreshed, expires=%s", r.Email, r.ExpiresAt.Format(time.RFC3339))
		} else {
			log.Printf("[token-refresh] account=%s failed: %s", r.Email, r.Error)
		}
	}
}

func (s *Server) Routes() http.Handler {
	mcp.APIKeyValidator = s.validAPIKey
	registerGoalMCPTools()
	s.registerGoalMCPHandler()
	m := http.NewServeMux()
	m.HandleFunc("/api/admin/login", s.adminLogin)
	m.HandleFunc("/api/admin/logout", s.adminLogout)
	m.HandleFunc("/api/admin/session", s.adminSession)
	m.HandleFunc("/api/admin/change-password", s.adminChangePassword)
	m.HandleFunc("/api/admin/keys", s.adminKeys)
	m.HandleFunc("/api/admin/models", s.adminModels)
	m.HandleFunc("/api/admin/models/test", s.adminModelTest)
	m.HandleFunc("/api/admin/models/sync", s.adminModelSync)
	m.HandleFunc("/api/admin/settings", s.adminSettings)
	m.HandleFunc("/api/admin/proxy-pool", s.proxyPool)
	m.HandleFunc("/api/admin/deployments", s.deployments)
	m.HandleFunc("/api/admin/deployment", s.deploymentAction)
	m.HandleFunc("/api/admin/deployment/check", s.deploymentCheck)
	m.HandleFunc("/api/admin/debug/logs", s.debugList)
	m.HandleFunc("/api/admin/debug/detail", s.debugDetail)
	m.HandleFunc("/api/admin/trace", s.adminTrace)
	m.HandleFunc("/api/admin/trace/status", s.adminTraceStatus)
	m.HandleFunc("/api/admin/trace/clear", s.adminTraceClear)
	m.HandleFunc("/api/health", s.health)
	m.HandleFunc("/api/version", s.version)
	m.HandleFunc("/api/update", s.update)
	m.HandleFunc("/api/accounts", s.accounts)
	m.HandleFunc("/api/accounts/refresh", s.refreshAccount)
	m.HandleFunc("/api/accounts/schedule", s.scheduleAccount)
	m.HandleFunc("/api/accounts/token-health", s.tokenHealth)
	m.HandleFunc("/api/accounts/clear-cooldown", s.clearCooldown)
	m.HandleFunc("/api/accounts/delete", s.deleteAccount)
	m.HandleFunc("/api/accounts/provision", s.provisionAccount)
	m.HandleFunc("/api/accounts/bind-proxy", s.bindProxy)
	m.HandleFunc("/api/auth/start", s.startPKCE)
	m.HandleFunc("/api/auth/status", s.pkceStatus)
	m.HandleFunc("/api/auth/callback", s.callbackPKCE)
	m.HandleFunc("/api/chat", s.chatOnce)
	m.HandleFunc("/api/chat/stream", s.chatStream)
	m.HandleFunc("/api/conversations", s.conversations)
	m.HandleFunc("/api/conversations/delete", s.deleteConversation)
	m.HandleFunc("/api/conversations/cleanup", s.conversationCleanup)
	m.HandleFunc("/api/conversations/whitelist", s.conversationWhitelist)
	m.HandleFunc("/v1/sessions", s.handleSessions)
	m.HandleFunc("/v1/sessions/", s.handleSessionDelete)
	m.HandleFunc("/v1/goal", s.handleGoalState)
	m.HandleFunc("/api/m365/conversations", s.handleM365Conversations)
	m.HandleFunc("/api/m365/conversations/detail", s.handleM365ConversationDetail)
	m.HandleFunc("/api/m365/conversations/delete", s.handleM365Delete)
	m.HandleFunc("/api/m365/conversations/cleanup", s.handleM365Cleanup)
	m.HandleFunc("/api/stats", s.handleCacheStats)
	m.HandleFunc("/api/stats/reset", s.handleCacheStatsReset)
	m.HandleFunc("/api/usage", s.adminUsage)
	m.HandleFunc("/api/usage/logs", s.adminUsageLogs)
	m.HandleFunc("/v1/models", s.openaiModels)
	m.HandleFunc("/v1/chat/completions", s.openaiChat)
	m.HandleFunc("/v1/responses", s.responses)
	m.HandleFunc("/v1/mcp/sse", mcp.HandleSSE)
	m.HandleFunc("/v1/mcp/message", mcp.HandleMessage)
	m.HandleFunc("/v1/mcp/tools", mcp.HandleToolsList)
	m.HandleFunc("/v1/messages", s.anthropicMessages)
	m.HandleFunc("/v1/images/generations", s.imageGenerations)
	m.HandleFunc("/v1/images/edits", s.imageEdits)
	m.HandleFunc("/v1/images/files/", s.generatedImageFile)
	m.HandleFunc("/vendor/", vendorFiles)
	m.HandleFunc("/", s.rootPage)
	return recoverPanics(requestID(httpTrace(securityHeaders(s.adminMiddleware(s.traceCaptureMiddleware(s.debugMiddleware(m)))))))
}

func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/images/files/") {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/vendor/") {
			// Vendored static assets (e.g. the local lucide icon shim) are
			// needed by the login page before any session exists.
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/admin/login" || r.URL.Path == "/api/admin/session" || r.URL.Path == "/api/admin/change-password" || r.URL.Path == "/api/admin/logout" || r.URL.Path == "/api/auth/start" || r.URL.Path == "/api/auth/status" || r.URL.Path == "/api/auth/callback" || r.URL.Path == "/" || r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			if !s.validAPIKey(r) {
				writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "valid API key required")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if s.adminPassword == "" {
			writeOpenAIError(w, http.StatusServiceUnavailable, "configuration_error", "administrator password is not configured")
			return
		}
		if !s.validAdminSession(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
			return
		}
		s.mu.Lock()
		mustChange := s.mustChangePassword
		s.mu.Unlock()
		if mustChange && r.URL.Path != "/api/admin/change-password" && r.URL.Path != "/api/admin/logout" {
			writeOpenAIError(w, http.StatusForbidden, "password_change_required", "administrator password must be changed before using the console")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func secureAdminCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Trust X-Forwarded-Proto from a loopback reverse proxy, or from any
	// peer when TRUST_PROXY_HEADERS is enabled (container/bridge deployment).
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	trusted := trustProxyHeaders() || net.ParseIP(host).IsLoopback()
	return trusted && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) validAdminSession(r *http.Request) bool {
	c, err := r.Cookie("m365_admin_session")
	if err != nil || c.Value == "" {
		return false
	}
	digest := adminSessionDigest(c.Value)
	now := time.Now()
	s.mu.Lock()
	expires, ok := s.adminSessions[digest]
	if !ok {
		s.mu.Unlock()
		return false
	}
	if !now.Before(expires) {
		delete(s.adminSessions, digest)
		if err := saveAdminSessions(s.adminSessions); err != nil {
			log.Printf("persist administrator session expiration: %v", err)
		}
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	return true
}

const maxAdminSessions = 4096

// pruneAdminSessions drops expired entries; callers must hold s.mu.
func pruneAdminSessions(m map[string]time.Time, now time.Time) {
	for k, exp := range m {
		if now.After(exp) {
			delete(m, k)
		}
	}
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	ip, now := clientIP(r), time.Now()
	if ok, wait := s.loginAllowed(ip, now); !ok {
		seconds := int(wait.Seconds()) + 1
		w.Header().Set("Retry-After", fmt.Sprint(seconds))
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many failed login attempts; try again later")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	decodeErr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body)
	s.mu.Lock()
	password := s.adminPassword
	mustChange := s.mustChangePassword
	s.mu.Unlock()
	if decodeErr != nil || body.Password == "" || subtle.ConstantTimeCompare([]byte(body.Password), []byte(password)) != 1 {
		s.recordLoginFailure(ip, now)
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "invalid administrator password")
		return
	}
	s.clearLoginFailures(ip)
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		writeOpenAIError(w, 500, "internal_error", "session failure")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	digest := adminSessionDigest(token)
	expires := now.Add(s.adminSessionTTL)
	s.mu.Lock()
	pruneAdminSessions(s.adminSessions, now)
	if len(s.adminSessions) >= maxAdminSessions {
		// Evict the oldest entry to keep the persisted session set bounded.
		var oldest string
		var oldestExp time.Time
		for k, exp := range s.adminSessions {
			if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = k, exp
			}
		}
		delete(s.adminSessions, oldest)
	}
	s.adminSessions[digest] = expires
	if err := saveAdminSessions(s.adminSessions); err != nil {
		delete(s.adminSessions, digest)
		s.mu.Unlock()
		writeOpenAIError(w, http.StatusInternalServerError, "storage_error", "administrator session could not be saved; check the persistent data directory permissions")
		return
	}
	s.mu.Unlock()
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Value: token, Path: "/", HttpOnly: true, Secure: secureAdminCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: maxAge, Expires: expires})
	jsonOut(w, map[string]any{"status": "authenticated", "must_change_password": mustChange})
}
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("m365_admin_session"); err == nil && c.Value != "" {
		digest := adminSessionDigest(c.Value)
		s.mu.Lock()
		delete(s.adminSessions, digest)
		if err := saveAdminSessions(s.adminSessions); err != nil {
			log.Printf("persist administrator session logout: %v", err)
		}
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "m365_admin_session", Path: "/", HttpOnly: true, Secure: secureAdminCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
	jsonOut(w, map[string]string{"status": "logged_out"})
}
func (s *Server) adminSession(w http.ResponseWriter, r *http.Request) {
	authenticated := s.validAdminSession(r)
	s.mu.Lock()
	mustChange := s.mustChangePassword
	s.mu.Unlock()
	jsonOut(w, map[string]bool{"authenticated": authenticated, "must_change_password": authenticated && mustChange})
}

func (s *Server) adminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonOut(w, map[string]any{"keys": s.apiKeys.list()})
	case http.MethodPost:
		var b struct {
			Name string `json:"name"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&b) != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
			return
		}
		if strings.TrimSpace(b.Name) == "" {
			b.Name = "API key"
		}
		rec, raw, e := s.apiKeys.create(b.Name)
		if e != nil {
			writeOpenAIError(w, 500, "internal_error", e.Error())
			return
		}
		jsonOut(w, map[string]any{"key": raw, "record": rec})
	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		deleted, e := s.apiKeys.delete(id)
		if e != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", e.Error())
			return
		}
		if !deleted {
			writeOpenAIError(w, 404, "not_found", "key not found")
			return
		}
		jsonOut(w, map[string]string{"status": "deleted"})
	case http.MethodPut:
		var b struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Revoked *bool  `json:"revoked"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&b) != nil || b.ID == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
			return
		}
		updated, e := s.apiKeys.update(b.ID, b.Name, b.Revoked)
		if e != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "internal_error", e.Error())
			return
		}
		if !updated {
			writeOpenAIError(w, 404, "not_found", "key not found")
			return
		}
		jsonOut(w, map[string]string{"status": "updated"})
	default:
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
	}
}
func (s *Server) validAPIKey(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if raw == "" {
		v := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			raw = strings.TrimSpace(v[7:])
		}
	}
	return raw != "" && s.apiKeys.valid(raw)
}

func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[jsonOut] encode error: %v", err)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	list := s.tokens.List()
	jsonOut(w, map[string]any{
		"status":             "ok",
		"auth":               []string{"pkce"},
		"chat":               "chathub",
		"clientId":           auth.ClientID(),
		"scope":              auth.Scope(),
		"tokenCache":         s.tokens.Path(),
		"accountCount":       len(list),
		"accountConcurrency": s.accountConcurrency.Snapshot(),
	})
}

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	list := s.tokens.List()
	cfg := currentSettings()
	loc, err := time.LoadLocation(strings.TrimSpace(cfg.TimeZone))
	if err != nil || loc == nil {
		// Minimal container images may not include the IANA time-zone database.
		// Use a non-nil fixed UTC+08:00 fallback so account statistics cannot
		// panic in Time.In when Asia/Shanghai is unavailable.
		loc = time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	usageStats := s.usage.accountStats(time.Now(), loc)
	concurrency := s.accountConcurrency.Snapshot()
	limit, _ := concurrency["limit"].(int)
	inflight, _ := concurrency["inflight"].(map[string]int)

	type view struct {
		ID                 string     `json:"id"`
		Email              string     `json:"email"`
		DisplayName        string     `json:"displayName,omitempty"`
		Status             string     `json:"status"`
		ScheduleEnabled    bool       `json:"scheduleEnabled"`
		CallCount          int64      `json:"callCount"`
		TodayTokens        int64      `json:"todayTokens"`
		LastRequestAt      *time.Time `json:"lastRequestAt,omitempty"`
		CurrentConcurrency int        `json:"currentConcurrency"`
		MaxConcurrency     int        `json:"maxConcurrency"`
		QueuePosition      int        `json:"queuePosition"`
		RateLimited        bool       `json:"rateLimited"`
		CooldownUntil      *time.Time `json:"cooldownUntil,omitempty"`
		OID                string     `json:"oid,omitempty"`
		TID                string     `json:"tid,omitempty"`
		ExpiresAt          time.Time  `json:"expiresAt,omitempty"`
		ImportedAt         time.Time  `json:"importedAt,omitempty"`
		UpdatedAt          time.Time  `json:"updatedAt,omitempty"`
		BoundProxy         string     `json:"boundProxy,omitempty"`
	}
	out := make([]view, 0, len(list))
	for i, a := range list {
		status := a.Status
		var cooldownUntil *time.Time
		var rateLimited bool
		var runtimeCallCount uint64
		if s.accountPool != nil {
			if until, ok := s.accountPool.CooldownUntil(a.ID); ok {
				status = "cooldown"
				cooldownUntil = &until
			}
			runtimeCallCount = s.accountPool.CallCount(a.ID)
			rateLimited = s.accountPool.RateLimited(a.ID)
		}

		stat := usageStats[a.Email]
		callCount := stat.Calls
		if runtimeCalls := int64(runtimeCallCount); runtimeCalls > callCount {
			callCount = runtimeCalls
		}
		var lastRequestAt *time.Time
		if !stat.LastRequestAt.IsZero() {
			value := stat.LastRequestAt
			lastRequestAt = &value
		}
		out = append(out, view{
			ID: a.ID, Email: a.Email, DisplayName: a.DisplayName,
			Status: status, ScheduleEnabled: !a.ScheduleDisabled,
			CallCount: callCount, TodayTokens: stat.TodayTokens, LastRequestAt: lastRequestAt,
			CurrentConcurrency: inflight[a.ID], MaxConcurrency: limit, QueuePosition: i + 1,
			RateLimited: rateLimited, CooldownUntil: cooldownUntil, OID: a.OID, TID: a.TID,
			ExpiresAt: a.ExpiresAt, ImportedAt: a.ImportedAt, UpdatedAt: a.UpdatedAt, BoundProxy: a.BoundProxy,
		})
	}
	jsonOut(w, map[string]any{"accounts": out, "health": s.accountPool.Snapshot(), "accountConcurrency": concurrency, "timeZone": cfg.TimeZone})
}

func (s *Server) refreshAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	acc, err := s.tokens.EnsureValid(strings.TrimSpace(body.ID))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "token_refresh_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "refreshed", "account": map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "expiresAt": acc.ExpiresAt, "updatedAt": acc.UpdatedAt,
	}})
}

func (s *Server) scheduleAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if err := s.tokens.SetScheduleEnabled(strings.TrimSpace(body.ID), body.Enabled); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "updated", "scheduleEnabled": body.Enabled})
}

func (s *Server) tokenHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		results := s.tokens.RefreshAllExpired()
		refreshed, failed := 0, 0
		for _, r := range results {
			if r.Success {
				refreshed++
			} else {
				failed++
			}
		}
		jsonOut(w, map[string]any{"refreshed": refreshed, "failed": failed, "results": results})
		return
	}
	list := s.tokens.List()
	now := time.Now()
	type entry struct {
		ID        string    `json:"id"`
		Email     string    `json:"email"`
		Status    string    `json:"status"`
		ExpiresAt time.Time `json:"expires_at"`
		Expired   bool      `json:"expired"`
		ExpiresIn string    `json:"expires_in"`
	}
	out := make([]entry, 0, len(list))
	for _, a := range list {
		e := entry{ID: a.ID, Email: a.Email, Status: a.Status, ExpiresAt: a.ExpiresAt}
		if now.After(a.ExpiresAt) {
			e.Expired = true
			e.ExpiresIn = "expired"
		} else {
			e.ExpiresIn = a.ExpiresAt.Sub(now).Truncate(time.Second).String()
		}
		out = append(out, e)
	}
	jsonOut(w, map[string]any{"accounts": out, "now": now.Format(time.RFC3339)})
}

func (s *Server) clearCooldown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	s.accountPool.ClearAllCooldowns()
	jsonOut(w, map[string]any{"status": "ok"})
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil || body.ID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	if err := s.tokens.Delete(body.ID); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	jsonOut(w, map[string]string{"status": "deleted"})
}

func (s *Server) provisionAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil || body.Email == "" || body.Password == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "email and password required")
		return
	}
	set, err := auth.ROPC(body.Email, body.Password)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "ropc_error", err.Error())
		return
	}
	acc, err := s.tokens.Upsert(set)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "upsert_error", err.Error())
		return
	}
	jsonOut(w, map[string]any{"status": "provisioned", "account": map[string]any{
		"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName,
		"status": acc.Status, "expiresAt": acc.ExpiresAt,
	}})
}

const maxPKCEStates = 1024
const pkceStateTTL = 10 * time.Minute

// pkceStartWindow / pkceStartLimit bound how often a single client may open a
// new PKCE flow; the pool itself is additionally capped by maxPKCEStates.
const pkceStartWindow = time.Minute
const pkceStartLimit = 10

func (s *Server) prunePKCELocked(now time.Time) {
	for state, p := range s.pkce {
		if now.Sub(p.Created) > pkceStateTTL {
			delete(s.pkce, state)
		}
	}
}

// pkceStartAllowedLocked enforces the per-client start rate limit and records
// the attempt. Callers must hold s.mu.
func (s *Server) pkceStartAllowedLocked(ip string, now time.Time) bool {
	if s.pkceStarts == nil {
		s.pkceStarts = map[string][]time.Time{}
	}
	cutoff := now.Add(-pkceStartWindow)
	recent := s.pkceStarts[ip][:0]
	for _, t := range s.pkceStarts[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= pkceStartLimit {
		s.pkceStarts[ip] = recent
		return false
	}
	s.pkceStarts[ip] = append(recent, now)
	return true
}

func oldestPKCEState(states map[string]pendingPKCE) string {
	var oldest string
	var oldestCreated time.Time
	for state, p := range states {
		if oldest == "" || p.Created.Before(oldestCreated) {
			oldest = state
			oldestCreated = p.Created
		}
	}
	return oldest
}

func (s *Server) bindProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.validAdminSession(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "auth_error", "administrator login required")
		return
	}
	var body struct {
		ID       string `json:"id"`
		ProxyURL string `json:"proxyUrl"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil || body.ID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "id required")
		return
	}
	if body.ProxyURL != "" {
		if err := outbound.ValidateProxyURL(body.ProxyURL); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_proxy", err.Error())
			return
		}
	}
	if err := s.tokens.SetBoundProxy(body.ID, body.ProxyURL); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	acc, _ := s.tokens.Get(body.ID)
	if acc.BoundProxy == "" {
		s.proxyClients.Range(func(key, _ any) bool {
			if keyStr, ok := key.(string); ok && keyStr != "" {
				s.proxyClients.Delete(keyStr)
			}
			return true
		})
	}
	jsonOut(w, map[string]any{"ok": true, "id": body.ID, "boundProxy": acc.BoundProxy})
}

func (s *Server) startPKCE(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()
	s.mu.Lock()
	if !s.pkceStartAllowedLocked(ip, now) {
		s.mu.Unlock()
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "too many PKCE authorization starts; try again later")
		return
	}
	v, err := auth.Verifier()
	if err != nil {
		s.mu.Unlock()
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "pkce failure")
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		s.mu.Unlock()
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "state failure")
		return
	}
	state := hex.EncodeToString(b)
	redirectURI := auth.RedirectURI()
	s.prunePKCELocked(now)
	if len(s.pkce) >= maxPKCEStates {
		delete(s.pkce, oldestPKCEState(s.pkce))
	}
	s.pkce[state] = pendingPKCE{Verifier: v, Created: now, Status: "pending", RedirectURI: redirectURI}
	s.mu.Unlock()
	jsonOut(w, map[string]string{
		"status": "pkce_ready",
		"state":  state,
		"url": auth.AuthorizationURL(
			auth.AuthorizeEndpoint(),
			auth.ClientID(),
			redirectURI,
			state,
			auth.Challenge(v),
			auth.Scope(),
		),
		"redirectUri": redirectURI,
		"note":        "If redirect is nativeclient, paste the final URL/code into /api/auth/callback after login.",
	})
}

func (s *Server) pkceStatus(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing state")
		return
	}
	s.mu.Lock()
	p, ok := s.pkce[state]
	if ok && time.Since(p.Created) > pkceStateTTL {
		delete(s.pkce, state)
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		jsonOut(w, map[string]any{"status": "expired"})
		return
	}
	out := map[string]any{"status": p.Status}
	if p.Account != nil {
		out["account"] = p.Account
	}
	if p.Error != "" {
		out["error"] = p.Error
	}
	jsonOut(w, out)
}
func (s *Server) callbackPKCE(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	oauthError := r.URL.Query().Get("error")
	// also accept pasted full callback URL
	if code == "" && oauthError == "" {
		if u := r.URL.Query().Get("url"); u != "" {
			if parsed, err := http.NewRequest(http.MethodGet, u, nil); err == nil {
				code = parsed.URL.Query().Get("code")
				oauthError = parsed.URL.Query().Get("error")
				if state == "" {
					state = parsed.URL.Query().Get("state")
				}
			}
		}
	}
	if state == "" || (code == "" && oauthError == "") {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing state or authorization result")
		return
	}
	browserNav := strings.Contains(r.Header.Get("Accept"), "text/html")
	s.mu.Lock()
	p, ok := s.pkce[state]
	if ok && time.Since(p.Created) > 10*time.Minute {
		delete(s.pkce, state)
		ok = false
	}
	if ok && (p.Status == "exchanging" || p.Status == "authenticated" || p.Status == "error") {
		// A retry (double click, browser re-POST) must not re-run the exchange:
		// a code can be redeemed only once and a second attempt fails with
		// invalid_grant, clobbering the first success. Terminal results are
		// consumed once; repeating the callback reports conflict instead.
		s.mu.Unlock()
		if browserNav {
			servePKCECompletionPage(w, state)
			return
		}
		if p.Status == "authenticated" || p.Status == "error" {
			http.Error(w, "authorization result already consumed", http.StatusConflict)
			return
		}
		jsonOut(w, map[string]any{"status": p.Status})
		return
	}
	if !ok {
		s.mu.Unlock()
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid or expired state")
		return
	}
	if p.Status != "pending" {
		s.mu.Unlock()
		writeOpenAIError(w, http.StatusConflict, "invalid_request_error", "authorization result already consumed")
		return
	}
	p.Status = "processing"
	s.pkce[state] = p
	if oauthError != "" {
		log.Printf("oauth_error stage=callback error=%q", oauthError)
		p.Status = "error"
		p.Error = oauthError
		s.pkce[state] = p
		s.mu.Unlock()
		if browserNav {
			servePKCECompletionPage(w, state)
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "auth_error", "Microsoft authorization failed: "+oauthError)
		return
	}
	// Capture everything the background exchange needs, then release the lock
	// and let the exchange run without holding the HTTP handler open.
	verifier := p.Verifier
	redirectURI := p.RedirectURI
	p.Status = "exchanging"
	p.Created = time.Now() // fresh expiry window for the exchange itself
	s.pkce[state] = p
	s.mu.Unlock()
	go s.exchangePKCE(state, code, verifier, redirectURI)
	// The manual/pasted-callback flow must never wait on the Microsoft token
	// endpoint inside the request: slow token endpoints (frequent on ARM64 and
	// other long-latency deployments) otherwise surface as a timeout from the
	// browser or a fronting reverse proxy. Poll /api/auth/status instead.
	if browserNav {
		servePKCECompletionPage(w, state)
		return
	}
	jsonOut(w, map[string]any{
		"status":  "exchanging",
		"state":   state,
		"note":    "Token exchange is running in the background. Poll /api/auth/status?state=" + state + " until status is authenticated or error.",
		"timeout": int(auth.TokenExchangeTimeout.Seconds()),
	})
}

// exchangePKCE runs the OAuth code exchange off the HTTP request path. Every
// state mutation happens under s.mu so pkceStatus observes a consistent view.
func (s *Server) exchangePKCE(state, code, verifier, redirectURI string) {
	ctx, cancel := context.WithTimeout(context.Background(), auth.TokenExchangeTimeout)
	defer cancel()

	fail := func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if p, ok := s.pkce[state]; ok {
			p.Status = "error"
			p.Error = err.Error()
			s.pkce[state] = p
		}
	}
	if redirectURI == "" {
		redirectURI = auth.RedirectURI()
	}
	tok, err := auth.ExchangeCode(ctx, code, verifier, redirectURI)
	if err != nil {
		logOAuthError("code_exchange", err)
		log.Printf("pkce exchange failed state=%s err=%v", state, err)
		fail(err)
		return
	}
	acc, err := s.tokens.Upsert(tok)
	if err != nil {
		log.Printf("pkce upsert failed state=%s err=%v", state, err)
		fail(err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.pkce[state]; ok {
		p.Status = "authenticated"
		p.Account = map[string]any{"id": acc.ID, "email": acc.Email, "displayName": acc.DisplayName, "status": acc.Status, "oid": acc.OID, "tid": acc.TID}
		s.pkce[state] = p
	}
}

// servePKCECompletionPage renders a small page for direct browser callbacks
// (custom M365_REDIRECT_URI pointing at this server, loopback or not). It
// polls the background exchange result and reports success/failure instead of
// leaving the user staring at a raw JSON response or a proxy timeout page.
func servePKCECompletionPage(w http.ResponseWriter, state string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>M365 Copilot2API 鎺堟潈纭</title>
<style>body{font:16px system-ui;text-align:center;padding:15vh 20px;color:#242424}main{max-width:520px;margin:auto}h1{font-size:24px;margin-bottom:8px}.muted{color:#666;font-family:ui-monospace,Menlo,Consolas,monospace;font-size:13px}#msg{font-size:15px}</style>
<main><h1>姝ｅ湪纭鎺堟潈</h1><p id="msg" class="muted">姝ｅ湪鍚?Microsoft 鍏戞崲浠ょ墝锛岃绋嶅€欌€?/p></main>
<script>
(async function(){
  const state=%q,box=document.getElementById('msg');
  for(let i=0;i<180;i++){
    let d={};
    try{const r=await fetch('/api/auth/status?state='+encodeURIComponent(state));d=await r.json();}catch(e){}
    if(d.status==='authenticated'){box.textContent='鎺堟潈瀹屾垚锛岃处鍙峰凡鍔犲叆璐﹀彿姹狅紝鍙互鍏抽棴姝ら〉闈€?;box.className='';}
    else if(d.status==='error'){box.textContent='鎺堟潈澶辫触锛?+(d.error||'鏈煡閿欒');box.className='muted';box.style.color='#c0392b';}
    else if(d.status==='expired'){box.textContent='鎺堟潈宸茶繃鏈燂紝璇烽噸鏂板紑濮嬫巿鏉冦€?;box.className='muted';box.style.color='#b9770e';}
    else {await new Promise(x=>setTimeout(x,1000));continue;}
    try{if(window.opener)window.opener.postMessage({type:'m365-auth-complete',state:state},window.location.origin);}catch(e){}
    if(d.status==='authenticated'){setTimeout(()=>window.close(),400);}
    return;
  }
  box.textContent='绛夊緟鎺堟潈瓒呮椂锛岃閲嶆柊寮€濮嬫巿鏉冦€?;box.className='muted';box.style.color='#b9770e';
})();
</script>`, state)
}

func (s *Server) resolveAccount(accountID string) (auth.AccountToken, error) {
	explicitAccount := accountID != ""
	if accountID == "" {
		rule := "available-first"
		if s.settings != nil {
			rule = s.settings.get().AccountRoutingRule
		}

		if rule == "round-robin" {
			for i := 0; i < maxAccountProbe; i++ {
				acc, ok := s.tokens.Next()
				if !ok {
					return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
				}
				if s.accountAvailable(acc.ID) {
					accountID = acc.ID
					break
				}
			}
		} else {
			// Available-first preserves configured account order and keeps using
			// the earliest healthy account until its concurrency slots are full.
			for _, acc := range s.tokens.List() {
				if s.accountAvailable(acc.ID) {
					accountID = acc.ID
					break
				}
			}
		}

		if accountID == "" {
			if len(s.tokens.List()) == 0 {
				return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
			}
			until := s.accountPool.EarliestRecovery()
			retry := int(time.Until(until).Seconds())
			if retry < 1 {
				retry = 1
			}
			return auth.AccountToken{}, &UpstreamHTTPError{Status: 429, RetryAfter: retry, LocalCapacity: true, Body: "no account is currently available; all enabled accounts are cooling down or at their concurrency limit"}
		}
	}

	// Explicit account routing remains authoritative, including for accounts
	// excluded from automatic scheduling. Automatic selection applies the
	// scheduling, health, and concurrency gates above.
	if !explicitAccount {
		if !s.tokens.ScheduleEnabled(accountID) {
			return auth.AccountToken{}, fmt.Errorf("account is disabled for scheduling")
		}
		if !s.accountConcurrency.Available(accountID) {
			return auth.AccountToken{}, &UpstreamHTTPError{Status: 429, RetryAfter: 1, LocalCapacity: true, Body: "account is at its concurrency limit; try another account"}
		}
	}
	// A cooling-down or auth-failed account must never be handed out, even when
	// it was explicitly requested (e.g. via a session binding). Fail over to the
	// next healthy account so a client retrying a session-bound request is not
	// stuck on a throttled account.
	if !s.accountPool.Available(accountID) {
		if next, nerr := s.nextProxySafeAccount(accountID); nerr == nil {
			return next, nil
		}
		return auth.AccountToken{}, &UpstreamHTTPError{Status: 429, RetryAfter: 5, LocalCapacity: true, Body: "account is cooling down; try another account"}
	}
	return s.tokens.EnsureValid(accountID)
}

// nextHealthyAccount returns the next round-robin account that is still
// healthy, skipping the given id first, and validates its token. Used by the
// failover path after a rate-limited or auth-failed attempt.
func (s *Server) nextHealthyAccount(avoidID string) (auth.AccountToken, error) {
	for i := 0; i < maxAccountProbe; i++ {
		acc, ok := s.tokens.Next()
		if !ok {
			return auth.AccountToken{}, fmt.Errorf("no accounts; login first")
		}
		if avoidID != "" && acc.ID == avoidID {
			continue
		}
		if !s.accountAvailable(acc.ID) {
			continue
		}
		return s.tokens.EnsureValid(acc.ID)
	}
	return auth.AccountToken{}, fmt.Errorf("no healthy account available for failover")
}

// nextProxySafeAccount returns the next account for failover. When the current
// account lost its proxy node (no unbound healthy node left), it prefers an
// account whose bound node is healthy 鈥?the request moves to that account
// instead of reusing another account's node. Falls back to the regular
// round-robin walk otherwise.
func (s *Server) nextProxySafeAccount(avoidID string) (auth.AccountToken, error) {
	if p := outbound.CurrentPool(); p != nil {
		if target, ok := p.FailoverTarget(avoidID); ok {
			if acc, err := s.tokens.EnsureValid(target); err == nil {
				return acc, nil
			}
		}
	}
	return s.nextHealthyAccount(avoidID)
}

type chatBody struct {
	AccountID      string               `json:"accountId"`
	Message        string               `json:"message"`
	Prompt         string               `json:"prompt"`
	Tone           string               `json:"tone"`
	ConversationID string               `json:"conversationId"`
	SessionID      string               `json:"sessionId"`
	SessionKey     string               `json:"sessionKey"`
	Attachments    []chathub.Attachment `json:"attachments,omitempty"`
	Tools          []chathub.Tool       `json:"tools,omitempty"`
	// Legacy OpenAI-compatible clients still send functions/function_call.
	Functions       []json.RawMessage `json:"functions,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	FunctionCall    any               `json:"function_call,omitempty"`
	Reasoning       *reasoningConfig  `json:"reasoning,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	ResponseFormat  *responseFormat   `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type       string         `json:"type"`
	JSONSchema map[string]any `json:"json_schema,omitempty"`
}

func modelTone(model string) string {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.2":
		return "Gpt_5_2_Chat"
	case "gpt-5.2-reasoning":
		return "Gpt_5_2_Reasoning"
	case "gpt-5.3":
		return "Gpt_5_3_Chat"
	case "gpt-5.4":
		return "Gpt_5_4_Chat"
	case "gpt-5.4-reasoning":
		return "Gpt_5_4_Reasoning"
	case "gpt-5.5":
		return "Gpt_5_5_Chat"
	case "gpt-5.5-reasoning":
		return "Gpt_5_5_Reasoning"
	case "gpt-5.6-reasoning":
		return "Gpt_5_6_Reasoning"
	case "claude", "claude-sonnet":
		return "Claude_Sonnet"
	case "claude-sonnet-reasoning":
		return "Claude_Sonnet_Reasoning"
	case "gpt-5.4-quick":
		return "Gpt_5_4_Chat"
	case "gpt-5.3-think-deeper":
		return "Gpt_5_3_Chat"
	default:
		return "magic"
	}
}

func sseRaw(ctx context.Context, w http.ResponseWriter, f http.Flusher, payload string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

func (s *Server) chatOnce(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var body chatBody
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	text := strings.TrimSpace(firstNonEmpty(body.Message, body.Prompt))
	if text == "" && len(body.Attachments) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "message or attachment required")
		return
	}
	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	requestedAccountID := body.AccountID
	acc, err := s.resolveAccount(requestedAccountID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	// Session-bound failover: resolveAccount returned a different (healthy)
	// account because the requested one is cooling down / auth-failed. The
	// conversation binding belongs to the previous account; clear it so the
	// request starts a fresh conversation on the new account.
	if requestedAccountID != "" && acc.ID != requestedAccountID {
		log.Printf("[account-route] failover requested=%s selected=%s reason=bound_unavailable", requestedAccountID, acc.ID)
		body.AccountID = acc.ID
		body.ConversationID = ""
		body.SessionID = ""
	}
	if acc.OID == "" || acc.TID == "" {
		if claimsOID, claimsTID := extractOIDTID(acc.AccessToken); claimsOID != "" {
			acc.OID = claimsOID
			acc.TID = claimsTID
		}
	}
	if acc.OID == "" || acc.TID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "account_error", "account missing oid/tid 鈥?re-login with PKCE browser client")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{
		AccessToken: acc.AccessToken,
		OID:         acc.OID,
		TID:         acc.TID,
	}, chathub.Request{
		Text:           text,
		Tone:           body.Tone,
		ConversationID: body.ConversationID,
		SessionID:      body.SessionID,
		Attachments:    body.Attachments,
		BindAccount:    acc.ID,
	})
	if err != nil {
		// Tone fallback: an empty completion usually means the requested tone
		// is not available for this tenant. Retry once with the magic tone
		// before surfacing the error (upstream fix 6e44b43).
		if IsEmptyCompletion(err) && body.Tone != "magic" {
			log.Printf("[tone-fallback] tone=%q returned empty, retrying with magic", body.Tone)
			magicReq := chathub.Request{
				Text:           text,
				Tone:           "magic",
				ConversationID: body.ConversationID,
				SessionID:      body.SessionID,
				Attachments:    body.Attachments,
				BindAccount:    acc.ID,
			}
			if res2, err2 := s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, magicReq); err2 == nil && res2.Text != "" {
				res = res2
				err = nil
			} else if err2 != nil {
				err = err2
			}
		}
		if err != nil {
			// Fail over only for an automatically selected, unbound conversation.
			// Proxy-node exhaustion also uses the proxy-aware account selector.
			if body.AccountID == "" && body.ConversationID == "" && (IsRateLimited(err) || IsAuthFailure(err) || errors.Is(err, outbound.ErrNoProxyNode)) {
				next, nerr := s.nextProxySafeAccount(acc.ID)
				if nerr == nil {
					ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
					defer cancel2()
					res2, err2 := s.chatWithAccount(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, chathub.Request{
						Text:           text,
						Tone:           body.Tone,
						ConversationID: body.ConversationID,
						SessionID:      body.SessionID,
						Attachments:    body.Attachments,
						BindAccount:    next.ID,
					})
					if err2 == nil {
						s.markAccountResult(acc.ID, err)
						s.markAccountResult(next.ID, nil)
						acc = next
						res = res2
						err = nil
					} else {
						s.markAccountResult(next.ID, err2)
						err = err2
					}
				}
			}
			if err != nil {
				s.markAccountResult(acc.ID, err)
			}
			writeUpstreamError(w, err)
			return
		}
	}
	s.accountPool.MarkSuccess(acc.ID)
	res.Text = sanitizePublicAssistantText(res.Text)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
	if body.SessionKey != "" {
		s.sessions.upsert(conversation{ID: body.SessionKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: text})
	}
	jsonOut(w, map[string]any{
		"status":         "ok",
		"text":           res.Text,
		"conversationId": res.ConversationID,
		"sessionId":      res.SessionID,
		"requestId":      res.RequestID,
		"throttling":     res.Throttling,
		"result":         res.RawResult,
		"events":         res.Events,
		"images":         res.Images,
		"account":        map[string]any{"id": acc.ID, "email": acc.Email},
	})
}

// dropTransientConversation 寮傛鍒犻櫎 router/repair 杞垱寤虹殑涓€娆℃€т簯绔璇濓紝
// 閬垮厤姣忚姹傞兘寰€ M365 瀵硅瘽鍒楄〃濉炰竴鏉¤褰曘€傚垹闄ゅけ璐ヤ笉闃诲璇锋眰锛岀暀缁?auto_cleanup 鍏滃簳銆?
func (s *Server) dropTransientConversation(conversationID string) {
	if conversationID == "" || m365CloudClient == nil {
		return
	}
	go func(id string) {
		if err := m365CloudClient.DeleteConversation(id); err != nil {
			log.Printf("[transient-conv] delete failed id=%s err=%v", id, err)
		}
	}(conversationID)
}

var (
	errWorkspaceToolCorrectionFailed = errors.New("workspace/tool correction failed")
	errWorkspaceToolMisjudgment      = errors.New("upstream repeatedly misidentified workspace or tool availability")
)

// recoverWorkspaceToolMisjudgment abandons the polluted upstream conversation
// and retries once on a clean branch. Only a distinct, non-empty replacement
// conversation is accepted, so callers can safely persist the returned result.
func (s *Server) recoverWorkspaceToolMisjudgment(
	ctx context.Context,
	acc auth.AccountToken,
	account chathub.Account,
	body *oaiReq,
	bad chathub.Result,
	answerReq chathub.Request,
	toolMaps []map[string]any,
	tone string,
	requestID string,
	userPrompt string,
	ledger agentLedger,
) (chathub.Result, error) {
	badConversationID := strings.TrimSpace(bad.ConversationID)
	if badConversationID != "" {
		s.dropTransientConversation(badConversationID)
	}

	cleanMessages := cleanWorkspaceToolMisjudgments(body.Messages, toolMaps)
	cleanPrompt, cleanAttachments := flattenPromptMessages(cleanMessages, nil)
	cleanPrompt = strings.TrimSpace(cleanPrompt)
	if cleanPrompt == "" {
		cleanPrompt = strings.TrimSpace(answerReq.Text)
	}

	correctionReq := answerReq
	correctionReq.ConversationID = ""
	correctionReq.SessionID = ""
	correctionReq.Started = true
	correctionReq.Tone = tone
	correctionReq.Attachments = append(cleanAttachments, body.Attachments...)
	correctionReq.TraceID = requestID
	correctionReq.BindAccount = acc.ID
	correctionReq.Text = unifiedSandboxCorrection(toolMaps, cleanPrompt)

	corrected, err := s.chatWithAccount(ctx, acc.ID, account, correctionReq)
	if err != nil {
		return chathub.Result{}, fmt.Errorf("%w: %v", errWorkspaceToolCorrectionFailed, err)
	}
	correctedConversationID := strings.TrimSpace(corrected.ConversationID)
	if correctedConversationID == "" || correctedConversationID == badConversationID {
		if correctedConversationID != "" && correctedConversationID != badConversationID {
			s.dropTransientConversation(correctedConversationID)
		}
		return chathub.Result{}, fmt.Errorf("%w: correction did not create a distinct conversation", errWorkspaceToolCorrectionFailed)
	}
	if strings.TrimSpace(corrected.Text) == "" {
		s.dropTransientConversation(correctedConversationID)
		return chathub.Result{}, fmt.Errorf("%w: correction returned an empty response", errWorkspaceToolCorrectionFailed)
	}
	if needsWorkspaceToolMisjudgmentCorrection(corrected.Text, toolMaps, userPrompt, ledger) {
		s.dropTransientConversation(correctedConversationID)
		return chathub.Result{}, errWorkspaceToolMisjudgment
	}

	body.Messages = cleanMessages
	body.ConversationID = corrected.ConversationID
	body.SessionID = corrected.SessionID
	body.Attachments = cleanAttachments
	return corrected, nil
}

func workspaceToolCorrectionErrorCode(err error) string {
	if errors.Is(err, errWorkspaceToolMisjudgment) {
		return "workspace_tool_misjudgment"
	}
	return "workspace_tool_correction_failed"
}

func workspaceToolCorrectionPublicMessage(err error) string {
	if errors.Is(err, errWorkspaceToolMisjudgment) {
		return "upstream repeatedly misidentified workspace or tool availability"
	}
	return "upstream workspace/tool correction failed"
}

func (s *Server) adminModelSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	syncUpstreamTones()
	tones := liveUpstreamTones()
	jsonOut(w, map[string]any{"synced": true, "upstream_tones": tones, "count": len(tones)})
}

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	jsonOut(w, map[string]any{"object": "list", "data": modelCatalog()})
}

// adminModelTest 鐢辨帶鍒跺彴妯″瀷娴嬭瘯璋冪敤锛岄€氳繃绠＄悊鍛樹細璇濋壌鏉冿紝涓嶄緷璧栨槑鏂?API Key
// 锛堟ā鍨嬫祴璇曡蛋鏈嶅姟绔处鍙锋睜锛屽瘑閽ュ垪琛ㄨ櫧鍙噸澶嶆樉绀哄畬鏁?key锛屼篃涓嶅洖浼犲瘑閽ユ槑鏂囷級銆?
func (s *Server) adminModelTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var b struct {
		Model string `json:"model"`
	}
	if json.NewDecoder(r.Body).Decode(&b) != nil || strings.TrimSpace(b.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json: model required")
		return
	}
	if err := checkModelAvailable(b.Model, currentSettings().ModelMappings); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	start := time.Now()
	status := http.StatusOK
	errMessage := ""
	accountEmail := ""
	defer func() {
		if s.usage == nil {
			return
		}
		s.usage.record(UsageRecord{
			Time:         time.Now(),
			APIKeyPrefix: "admin:model-test",
			AccountEmail: accountEmail,
			Model:        b.Model,
			Endpoint:     "/api/admin/models/test",
			Stream:       false,
			DurationMs:   time.Since(start).Milliseconds(),
			Status:       status,
			Error:        errMessage,
		})
	}()

	acc, err := s.resolveAccount("")
	if err != nil {
		status = upstreamStatus(err)
		errMessage = truncatedError(err)
		writeUpstreamError(w, err)
		return
	}
	accountEmail = acc.Email
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		status = http.StatusBadRequest
		errMessage = "account missing oid/tid"
		writeOpenAIError(w, status, "account_error", errMessage)
		return
	}
	tone, _ := reasoningTone(b.Model, "")
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	res, err := s.chatWithAccount(ctx, acc.ID, chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}, chathub.Request{
		Text: `Say "OK" in one word.`,
		Tone: tone,
	})
	ms := time.Since(start).Milliseconds()
	if err != nil {
		status = http.StatusBadGateway
		errMessage = truncatedError(err)
		writeOpenAIError(w, status, "m365_error", upstreamError(err))
		return
	}
	jsonOut(w, map[string]any{"ok": true, "model": b.Model, "reply": sanitizePublicAssistantTextForModel(res.Text, b.Model), "latency_ms": ms})
}

func (s *Server) openaiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	data := modelCatalog()
	created := time.Now().Unix()
	for _, model := range data {
		model["created"] = created
	}
	// Codex v0.144.5 requires `models`, while OpenAI-compatible clients use
	// `data`. Keep both aliases backed by the same catalog.
	jsonOut(w, map[string]any{"object": "list", "data": data, "models": data})
}

type oaiMsg struct {
	Role             string           `json:"role"`
	Content          any              `json:"content"`
	Name             string           `json:"name,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ToolCalls        []map[string]any `json:"tool_calls,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
}

type oaiReq struct {
	Model          string          `json:"model"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Messages       []oaiMsg        `json:"messages"`
	Stream         bool            `json:"stream"`
	StreamOptions  *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	MaxTokens           *int     `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	FrequencyPenalty    *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64 `json:"presence_penalty,omitempty"`
	Stop                any      `json:"stop,omitempty"`
	N                   *int     `json:"n,omitempty"`
	Seed                *int64   `json:"seed,omitempty"`
	Logprobs            *bool    `json:"logprobs,omitempty"`
	TopLogprobs         *int     `json:"top_logprobs,omitempty"`
	User                string   `json:"user"`
	AccountID           string   `json:"accountId"`
	ConversationID      string   `json:"conversation_id"`
	SessionID           string   `json:"session_id"`
	SessionKey          string   `json:"session_key"`
	ConversationIDC     string   `json:"conversationId,omitempty"`
	SessionIDC          string   `json:"sessionId,omitempty"`
	// NewConversation forces a fresh upstream conversation: the session
	// resolver, user-session and conversation-cache reuse paths are all skipped
	// so the full message history is submitted to a brand-new conversation.
	NewConversation   bool                 `json:"new_conversation,omitempty"`
	Attachments       []chathub.Attachment `json:"attachments,omitempty"`
	Tools             []chathub.Tool       `json:"tools,omitempty"`
	Functions         []json.RawMessage    `json:"functions,omitempty"`
	ToolChoice        any                  `json:"tool_choice,omitempty"`
	FunctionCall      any                  `json:"function_call,omitempty"`
	ParallelToolCalls *bool                `json:"parallel_tool_calls,omitempty"`
	Reasoning         *reasoningConfig     `json:"reasoning,omitempty"`
	ReasoningEffort   string               `json:"reasoning_effort,omitempty"`
	// Metadata carries OpenAI-style client metadata through to the response so
	// it is echoed back (chat completions) instead of being silently dropped.
	Metadata map[string]string `json:"metadata,omitempty"`
}

func (r *oaiReq) shouldSendStreamUsage() bool {
	if r.StreamOptions == nil {
		return true
	}
	return r.StreamOptions.IncludeUsage
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func contentToString(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			t, _ := m["type"].(string)
			switch t {
			case "text", "input_text", "output_text":
				if s, _ := m["text"].(string); s != "" {
					b.WriteString(s)
				}
			case "image_url":
				url := extractMediaURL(m, "image_url")
				b.WriteString("[image:" + shortHash(url) + "]")
			case "input_image", "image":
				url := extractMediaURL(m, "image_url", "url", "source")
				if raw, ok2 := m["image_url"].(map[string]any); ok2 {
					if u := stringValue(raw, "url", "data", "image_url"); u != "" {
						url = u
					}
				}
				if raw, ok2 := m["source"].(map[string]any); ok2 && url == "" {
					url = stringValue(raw, "url", "data", "source")
				}
				b.WriteString("[image:" + shortHash(url) + "]")
			case "input_file", "file":
				url := stringValue(m, "file_data", "file_url", "url", "source", "file_id")
				b.WriteString("[file:" + shortHash(url) + "]")
			case "input_audio", "audio":
				url := stringValue(m, "data", "audio_url", "url", "source")
				b.WriteString("[audio:" + shortHash(url) + "]")
			}
		}
		return b.String()
	default:
		return fmt.Sprint(v)
	}
}

func extractMediaURL(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case map[string]any:
			if u, ok := v["url"].(string); ok && u != "" {
				return u
			}
		}
	}
	return ""
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func normalizeLegacyTools(body *oaiReq) {
	if len(body.Tools) == 0 && len(body.Functions) > 0 {
		body.Tools = make([]chathub.Tool, 0, len(body.Functions))
		for _, f := range body.Functions {
			body.Tools = append(body.Tools, chathub.Tool{Type: "function", Function: f})
		}
	}
	if body.ToolChoice == nil && body.FunctionCall != nil {
		body.ToolChoice = body.FunctionCall
	}
}

func toolMapsFromChatHubTools(tools []chathub.Tool) []map[string]any {
	toolMaps := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		var function map[string]any
		if err := json.Unmarshal(tool.Function, &function); err != nil {
			continue
		}
		toolMaps = append(toolMaps, map[string]any{
			"type":     tool.Type,
			"function": function,
		})
	}
	return toolMaps
}

func buildAnswerRequest(answerPrompt, tone string, body oaiReq, ledger agentLedger, planningMode string, mcpServerURL string) chathub.Request {
	if len(ledger.Completed) > 0 || len(ledger.Pending) > 0 {
		answerPrompt += "\n" + ledger.RouterContext()
	}
	if len(ledger.Completed) > 0 {
		answerPrompt += "\nFINAL ANSWER RULE: Report only actions supported by completed tool results. If the goal is not fully verified, state exactly what remains unconfirmed."
	}
	if inst := unifiedWorkspaceInstruction(toolMapsFromChatHubTools(body.Tools)); inst != "" {
		answerPrompt = inst + "\n\n" + answerPrompt
	}
	if conciseOutputEnabled() {
		answerPrompt = conciseOutputPolicy + "\n\n" + answerPrompt
	}
	req := chathub.Request{Text: answerPrompt, Tone: tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments}
	if planningMode == "native" {
		req.Tools = body.Tools
		req.ToolChoice = body.ToolChoice
	}
	if mcpServerURL != "" {
		req.Tools = body.Tools
		if req.ToolChoice == nil {
			req.ToolChoice = body.ToolChoice
		}
	}
	if len(body.Tools) > 0 {
		mcpTools := make([]mcp.Tool, 0, len(body.Tools))
		for _, t := range body.Tools {
			var f struct {
				Name, Description string
				Parameters        json.RawMessage `json:"parameters"`
			}
			if json.Unmarshal(t.Function, &f) != nil || f.Name == "" {
				continue
			}
			var schema map[string]any
			if json.Unmarshal(f.Parameters, &schema) != nil {
				schema = map[string]any{"type": "object"}
			}
			mcpTools = append(mcpTools, mcp.Tool{Name: f.Name, Description: f.Description, InputSchema: schema})
		}
		if len(mcpTools) > 0 {
			mcp.GlobalToolRegistry.MergeTools(mcpTools)
		}
	}
	req.MCPServerURL = mcpServerURL
	return req
}

// failoverRequest rebuilds the retry request for a different account after a
// throttled or auth-failed upstream attempt. Fresh unbound requests move to the
// new account as-is; requests that reused a cloud conversation (resolver-
// injected session binding) clear the old conversation and resubmit the
// complete message context, because that conversation belongs to the previous
// account and cannot be continued on the new one.
func failoverRequest(base chathub.Request, body oaiReq, resolvedConversationID, tone string, ledger agentLedger, planningMode, mcpServerURL string, task *taskLedger, nextID string) chathub.Request {
	req := base
	if body.ConversationID != "" && body.ConversationID == resolvedConversationID {
		full, atts := flattenPromptMessages(body.Messages, nil)
		full = withTaskLedger(full, task)
		req = buildAnswerRequest(full, tone, body, ledger, planningMode, mcpServerURL)
		req.Attachments = atts
		req.ConversationID = ""
		req.SessionID = ""
		req.TraceID = base.TraceID
		req.RequestID = base.RequestID
	}
	req.BindAccount = nextID
	return req
}

func (s *Server) openaiChat(w http.ResponseWriter, r *http.Request) {
	requestID := requestIDFrom(r)
	if requestID == "" {
		requestID = uuid.NewString()
	}
	startedAt := requestStartedAtFrom(r)
	log.Printf("[req-trace] id=%s stage=http_start stream=%t", requestID, r.URL.Query().Get("stream") == "true")
	defer func() {
		log.Printf("[req-trace] id=%s stage=http_return total_ms=%d", requestID, time.Since(startedAt).Milliseconds())
	}()
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	const maxChatRequestBody = 10 << 20
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatRequestBody))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "read body")
		return
	}
	var body oaiReq
	if err := json.Unmarshal(raw, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	// Whether the client explicitly pinned an account in the request body.
	// Session-bound accounts (session_key / session_id / previous_response_id)
	// are injected by the gateway below; they must still be eligible for
	// failover when they go into cooldown, whereas a client-pinned account is
	// an explicit routing choice that the gateway keeps honoring.
	clientPinnedAccount := body.AccountID != ""
	// Debug-mode tracing: the trace middleware owns the lifecycle; enrich the
	// record with chat-specific metadata as the request progresses.
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			rec.Model = firstNonEmpty(body.Model, "m365-copilot")
			rec.Stream = body.Stream
			rec.APIKeyPrefix = apiKeyPrefix(r)
		})
	}
	responseFormat := body.ResponseFormat
	mappings := currentSettings().ModelMappings
	if err := checkModelAvailable(body.Model, mappings); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}
	effort := body.ReasoningEffort
	if body.Reasoning != nil && strings.TrimSpace(body.Reasoning.Effort) != "" {
		effort = body.Reasoning.Effort
	}
	// "auto" (and an omitted effort) resolves to the model's configured default
	// reasoning level from model route settings.
	effort = resolveReasoningEffort(effort, body.Model, mappings)
	body.ReasoningEffort = effort
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			rec.ReasoningLevel = effort
		})
	}
	tone, toneErr := reasoningTone(body.Model, effort)
	if toneErr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", toneErr.Error())
		return
	}
	normalizeLegacyTools(&body)
	body.ConversationID = firstNonEmpty(body.ConversationID, body.ConversationIDC)
	body.SessionID = firstNonEmpty(body.SessionID, body.SessionIDC)

	// Serialize requests that explicitly target the same logical session. Hold
	// the lock through the complete request so a later turn cannot resolve or
	// update the session while the preceding turn is still in flight.
	sessionLockID := firstNonEmpty(sessionIDFromRequest(r), body.SessionKey, body.SessionID, body.ConversationID)
	releaseSession, err := s.sessionConcurrency.Acquire(r.Context(), sessionLockID)
	if err != nil {
		writeOpenAIError(w, http.StatusRequestTimeout, "request_cancelled", err.Error())
		return
	}
	defer releaseSession()

	log.Printf("[req-trace] id=%s stage=body_parsed messages=%d tools=%d choice=%s raw_bytes=%d", requestID, len(body.Messages), len(body.Tools), normalizedToolChoiceMode(body.ToolChoice), len(raw))
	if err := validateToolConversation(body.Messages); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "tool_protocol_error", err.Error())
		return
	}
	// Rebuild a protocol-neutral evidence ledger from actual tool calls/results.
	// Round limits apply only to the current user turn; full history still informs evidence.
	ledger := buildAgentLedger(body.Messages)
	activeLedger := buildAgentLedger(activeMessages(body.Messages))
	if err := activeLedger.CanContinue(maxToolRounds()); err != nil {
		// Graceful termination for round-limit: the model has exhausted its
		// tool round budget. Inject a summary-request instead of a hard 409
		// so the model can finalize its answer gracefully.
		if strings.Contains(err.Error(), "round limit") {
			body.Messages = append(body.Messages,
				oaiMsg{Role: "system", Content: fmt.Sprintf("Tool round limit reached (%d). You must now summarize your work and provide a final answer. Do NOT call any more tools.", maxToolRounds())},
			)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "tool_round_limit", "message": err.Error(), "completed_calls": len(activeLedger.Completed)}})
			return
		}
	}
	// Preserve role boundaries when adapting OpenAI messages to ChatHub's
	// single message.text field. This keeps system/developer instructions,
	// history, and the current user turn distinguishable.
	// Apply the three-tier context budget before flattening: tier 1
	// (instructions) and tier 2 (tool evidence) survive, tier 3 (ordinary
	// history) is trimmed oldest-first once the request exceeds the effective
	// input budget the M365 backend can actually consume.
	body.Messages = budgetMessages(body.Messages, modelRouteMaxInputTokens(body.Model, currentSettings().ModelMappings))
	var prompt string
	prompt, body.Attachments = flattenPromptMessages(body.Messages, body.Attachments)
	log.Printf("[req-trace] id=%s stage=prompt_flattened prompt_len=%d attachments=%d", requestID, len(prompt), len(body.Attachments))
	fmt.Printf("[multimodal-entry] messages=%d attachments=%d prompt_len=%d\n", len(body.Messages), len(body.Attachments), len(prompt))
	prompt = strings.TrimSpace(prompt)
	if responseFormat != nil {
		switch responseFormat.Type {
		case "json_object":
			prompt += "\nYou must respond with valid JSON."
		case "json_schema":
			if responseFormat.JSONSchema != nil {
				if schema, ok := responseFormat.JSONSchema["schema"]; ok {
					prompt += "\nYou must respond with valid JSON that conforms to this schema:\n" + mustJSON(schema)
				} else {
					prompt += "\nYou must respond with valid JSON."
				}
			} else {
				prompt += "\nYou must respond with valid JSON."
			}
		}
	}
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "messages required")
		return
	}
	if answer, ok := publicIdentityAnswer(body.Messages, body.Model); ok && responseFormat == nil {
		s.writePublicIdentityChatResponse(w, r, &body, prompt, answer, startedAt)
		return
	}

	if body.SessionKey != "" && !body.NewConversation {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	// The `user` field is an end-user identifier, not a conversation identifier:
	// it must never auto-continue a previous conversation. A fresh request from
	// the same user without an explicit session id starts a new conversation.
	// Account selection: for a new session (no binding), resolveAccount applies
	// the routing rule ("available-first" or "round-robin"). Sessions bound via
	// an explicit session key, header, or previous_response_id stay sticky to
	// their account and conversation, so incremental context reuse (cache) is
	// preserved; resolveAccount returns 429 when the bound account is at its
	// concurrency limit or cooling down.
	answerPrompt := prompt
	// cachedTokens reports how much of the input was served from the reused
	// conversation. cacheOnAccountSwitch overrides this with the value that
	// applied before an account switch, so downstream still sees the cache
	// savings even though the new account started a fresh conversation.
	// sentPrompt captures the true incremental prompt before it is overwritten
	// by answerReq.Text (which includes workspace instruction + ledger context
	// that are not in the "prompt" baseline). Without this, the cached-tokens
	// calculation would compare the full prompt against an inflated sent text
	// and return 0 when the overhead exceeds the history.
	cachedOverride := int64(-1)
	var sentPrompt string
	// storedContextPrompt carries the upstream conversation's persisted history
	// when an explicit session is reused with only the current turn submitted
	// (explicit_incremental). Those tokens are the cached portion of the logical
	// prompt and must be reflected in prompt_tokens / cached_tokens even though
	// the request body itself does not echo them.
	var storedContextPrompt string
	cachedTokens := func() int64 {
		if cachedOverride >= 0 {
			return cachedOverride
		}
		if storedContextPrompt != "" {
			// explicit_incremental reuse: the upstream conversation already held
			// storedContextPrompt; only the current turn (delta) was re-submitted.
			// The persisted history is the cached portion of the logical prompt.
			delta := sentPrompt
			if delta == "" {
				delta = answerPrompt
			}
			return EstimateTokens(storedContextPrompt+"\n"+delta) - EstimateTokens(delta)
		}
		if sentPrompt != "" {
			return cachedInputTokens(prompt, sentPrompt)
		}
		return cachedInputTokens(prompt, answerPrompt)
	}
	resolvedConversationID := ""
	if body.ConversationID == "" && len(body.Messages) > 0 && !body.NewConversation {
		resolved := s.sessionResolver.Resolve(r, &body)
		if !resolved.IsNew {
			body.AccountID = firstNonEmpty(body.AccountID, resolved.AccountID)
			log.Printf("[session-resolver] matched=%s conversation=%s history=%d total=%d reset=%t", resolved.MatchedBy, resolved.ConversationID, resolved.HistoryLen, len(body.Messages), resolved.ResetUpstream)
			if resolved.ResetUpstream {
				// Keep the stable downstream session identity, but detach this request
				// from the old upstream conversation and submit the complete compacted
				// context when creating its replacement.
				body.ConversationID = ""
				body.SessionID = ""
				resolvedConversationID = ""
				answerPrompt, body.Attachments = flattenPromptMessages(body.Messages, nil)
			} else {
				resolvedConversationID = resolved.ConversationID
				body.ConversationID = resolved.ConversationID
				body.SessionID = resolved.SessionID
				if resolved.HistoryLen > 0 && resolved.HistoryLen < len(body.Messages) {
					incPrompt, incAtt := flattenPromptMessages(body.Messages[resolved.HistoryLen:], nil)
					incPrompt = strings.TrimSpace(incPrompt)
					if incPrompt != "" {
						answerPrompt = incPrompt
						body.Attachments = incAtt
					}
				} else if len(resolved.StoredContext) > 0 {
					// explicit_incremental: the client submitted only its current turn,
					// but the upstream conversation already holds StoredContext. Its
					// tokens are the cached portion of the logical prompt.
					if sp, _ := flattenPromptMessages(resolved.StoredContext, nil); strings.TrimSpace(sp) != "" {
						storedContextPrompt = strings.TrimSpace(sp)
					}
				}
			}
		}
	}

	accountID := body.AccountID
	acc, err := s.resolveAccount(accountID)
	if err != nil {
		log.Printf("[account-route] resolve failed requested=%q err=%v", accountID, err)
		s.usage.record(UsageRecord{
			Time:           time.Now(),
			APIKeyPrefix:   apiKeyPrefix(r),
			Model:          firstNonEmpty(body.Model, "m365-copilot"),
			ReasoningLevel: body.ReasoningEffort,
			Endpoint:       "/v1/chat/completions",
			Stream:         body.Stream,
			DurationMs:     time.Since(startedAt).Milliseconds(),
			Status:         upstreamStatus(err),
			Error:          truncatedError(err),
		})
		if tr := traceFromRequest(r); tr != nil {
			s.trace.update(tr.ID, func(rec *traceRecord) {
				rec.Status = "error"
				rec.StatusCode = upstreamStatus(err)
				rec.Error = truncatedError(err)
			})
		}
		writeUpstreamError(w, err)
		return
	}
	log.Printf("[account-route] selected id=%q email=%q token_present=%t oid_present=%t tid_present=%t", acc.ID, acc.Email, acc.AccessToken != "", acc.OID != "", acc.TID != "")
	// Session-bound failover: when the requested account (e.g. one bound by a
	// session resolver or session_key) is cooling down or auth-failed,
	// resolveAccount already failed over to another healthy account. The old
	// conversation/session binding belongs to the previous account and must be
	// cleared so the request continues on the new account with the full context.
	if accountID != "" && acc.ID != accountID {
		if s.settings.get().CacheOnAccountSwitch {
			cachedOverride = cachedInputTokens(prompt, answerPrompt)
		}
		log.Printf("[account-route] failover requested=%s selected=%s reason=bound_unavailable", accountID, acc.ID)
		body.AccountID = acc.ID
		body.ConversationID = ""
		body.SessionID = ""
		resolvedConversationID = ""
		answerPrompt, body.Attachments = flattenPromptMessages(body.Messages, nil)
	}
	// Old-session restart ("閲嶆柊鍙戣捣瀵硅瘽"): a returning session whose warm
	// window has lapsed (no reserved slot) and whose bound account has no idle
	// unreserved capacity must not block in the queue. Re-initiate the
	// conversation on another account that has idle concurrency; when no
	// alternative account has capacity, fall through to the normal FIFO queue
	// on the bound account.
	if body.SessionID != "" && body.ConversationID != "" && !s.accountConcurrency.HasReservation(acc.ID, body.SessionID) && !s.accountConcurrency.HasUnreservedSlot(acc.ID) {
		if next, nerr := s.nextHealthyAccount(acc.ID); nerr == nil {
			if s.settings.get().CacheOnAccountSwitch {
				cachedOverride = cachedInputTokens(prompt, answerPrompt)
			}
			log.Printf("[account-restart] session=%s old_account=%s new_account=%s reason=no_idle_concurrency cached=%d", body.SessionID, acc.ID, next.ID, cachedOverride)
			acc = next
			body.AccountID = next.ID
			body.ConversationID = ""
			body.SessionID = ""
			resolvedConversationID = ""
			answerPrompt, body.Attachments = flattenPromptMessages(body.Messages, nil)
		}
	}
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			rec.AccountEmail = acc.Email
		})
	}
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "account_error", "account missing oid/tid")
		return
	}

	// Server-side task ledger: restore the persistent task for this downstream
	// session (it survives context compaction and account switches), or seed a
	// fresh ledger from this request when the session is new. The ledger is
	// injected into every answer/router prompt so the model always knows the
	// original goal, the constraints and how far the task has progressed.
	task := s.sessionTaskLedger(r, &body)
	if task == nil {
		task = buildTaskLedger(&body)
	}
	answerPrompt = withTaskLedger(answerPrompt, task)
	prompt = withTaskLedger(prompt, task)
	// When the goal is already complete and the client sends a continuation
	// round, inject the server-side completion context so the model reports
	// the recorded outcome instead of claiming it cannot close the goal.
	if task != nil && task.IsComplete() && goalRoundRequest(body.Messages, task, body.Tools) {
		suffix := task.goalRoundInjectedContext()
		answerPrompt += suffix
		prompt += suffix
	}
	// Round-budget state: when the round counter has reached its ceiling, the
	// goal can never make further progress. Inject the structured fact so the
	// model wraps up with the honest status instead of re-attempting the work
	// every round. This is a precise signal (Round: N/M from the protocol),
	// not a keyword match.
	if task != nil {
		cur, max, ok := goalRoundCounter(body.Messages)
		if ok && cur >= max && !task.IsComplete() && (task.GoalID != "" || goalRoundRequest(body.Messages, task, body.Tools)) {
			note := fmt.Sprintf("\n\n[TASK_LEDGER] ROUND_BUDGET_EXHAUSTED: %d/%d rounds used. "+
				"Stop continuing the goal, report the current status and any unverified steps, and close the goal with update_goal(action=complete) if the work is done.", cur, max)
			answerPrompt += note
			prompt += note
		}
	}

	// Conversation continuation is driven ONLY by explicit session identity
	// (session_id / x-session-id header, body session_key/session_id/
	// conversation_id, or /v1/responses previous_response_id), never by content
	// similarity.
	// A request without an explicit identifier starts a fresh upstream
	// conversation even when its messages match a previous request, so distinct
	// conversations (and distinct users) can never silently merge.

	// Normalize tools once. Selection is always made by the upstream model;
	// the gateway only validates its structured decision and converts protocols.
	toolMaps := make([]map[string]any, 0, len(body.Tools))
	for _, tool := range body.Tools {
		var f map[string]any
		_ = json.Unmarshal(tool.Function, &f)
		toolMaps = append(toolMaps, map[string]any{"type": tool.Type, "function": f})
	}
	if body.ToolChoice == nil && len(toolMaps) > 0 {
		body.ToolChoice = "auto"
	}
	var mcpServerURL string
	if len(toolMaps) > 0 {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		mcpServerURL = fmt.Sprintf("%s://%s/v1/mcp/sse", scheme, r.Host)
		log.Printf("[mcp] tools=%d mcp_gateway=%s", len(toolMaps), mcpServerURL)
	}
	validateCalls := func(stage string, calls []detectedToolCall) ([]detectedToolCall, int) {
		valid, rejected := validateDetectedToolCalls(calls, toolMaps, body.ToolChoice)
		for _, call := range rejected {
			log.Printf("[tool-validation] id=%s stage=%s rejected_name=%q reason=%q", requestID, stage, call.Name, call.Reason)
		}
		return valid, len(rejected)
	}
	planningMode := s.settings.get().ToolPlanningMode

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}
	// The stream is opened by the actual response path below. Do not emit a
	// tool preamble here: a request may contain tools in its schema while still
	// being an ordinary text question.
	// Streaming requests must not wait for the synchronous tool router. This
	// path forwards ordinary upstream text deltas immediately; tool routing for
	// non-streaming requests remains below until the event-level tool protocol
	// is available end-to-end.
	if planningMode == "router" && body.Stream && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		// Preserve the existing validated tool router for streaming tool turns.
		// Only fall through to text streaming when the router explicitly selects
		// no tool; this prevents a natural-language preamble from becoming a
		// completed assistant turn with the actual call lost.
		routePrompt := modelToolRouterPrompt(answerPrompt+"\n"+ledger.RouterContext(), toolMaps, body.ToolChoice)
		log.Printf("[req-trace] id=%s stage=router_start prompt_len=%d", requestID, len(routePrompt))
		routeRes, routeErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments, TraceID: requestID, BindAccount: acc.ID})
		log.Printf("[req-trace] id=%s stage=router_return elapsed_ms=%d err=%t", requestID, time.Since(startedAt).Milliseconds(), routeErr != nil)
		// Router turns run in a throwaway cloud conversation that is never
		// reused by the answer turn; delete it so the conversation list does
		// not accumulate one entry per routed request.
		if routeErr == nil && routeRes.ConversationID != "" {
			s.dropTransientConversation(routeRes.ConversationID)
		}
		if routeErr != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "tool router: "+routeErr.Error())
			return
		}
		calls, parsed := parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
		calls = filterCompletedCalls(calls, ledger)
		calls, _ = validateCalls("router", calls)
		if !parsed {
			repairRes, repairErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:\n` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments, TraceID: requestID, BindAccount: acc.ID})
			if repairErr == nil && repairRes.ConversationID != "" {
				s.dropTransientConversation(repairRes.ConversationID)
			}
			if repairErr == nil {
				calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				calls = filterCompletedCalls(calls, ledger)
				calls, _ = validateCalls("router", calls)
			}
		}
		if parsed && len(calls) > 0 {
			scope := fmt.Sprintf("%d:%v:stream", len(body.Messages), completedCallIDs(ledger))
			for i := range calls {
				calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
			}
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			if body.ParallelToolCalls != nil && !*body.ParallelToolCalls && len(calls) > 1 {
				calls = calls[:1]
			}
			_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), true, body.shouldSendStreamUsage(), cachedTokens(), calls, routeRes)
			s.recordToolUsage(r, acc, &body, routeRes, startedAt)
			return
		}
	}
	if body.Stream && !isReasoningTone(tone) {
		answerReq := buildAnswerRequest(answerPrompt, tone, body, ledger, planningMode, mcpServerURL)
		answerReq.TraceID = requestID
		answerReq.BindAccount = acc.ID
		// Preserve the true incremental prompt before it is replaced by the full
		// answer request text, so cached-token accounting measures only what the
		// upstream conversation actually re-sent (not the workspace instruction
		// or ledger context added by buildAnswerRequest).
		if sentPrompt == "" {
			sentPrompt = answerPrompt
		}
		answerPrompt = answerReq.Text
		log.Printf("[req-trace] id=%s stage=answer_start prompt_len=%d native_tools=%d mcp=%s", requestID, len(answerPrompt), len(answerReq.Tools), mcpServerURL)
		id := "chatcmpl-" + uuid.NewString()
		model := firstNonEmpty(body.Model, "m365-copilot")
		// Bound the per-account queue wait BEFORE the stream preamble: if the
		// request would only sit in the FIFO queue, fail with HTTP 429 (鎺掗槦瓒呮椂)
		// instead of sending ": connected" and then silence.
		if body.SessionID != "" {
			if err := s.accountConcurrency.WaitForSlot(r.Context(), acc.ID, body.SessionID); err != nil {
				log.Printf("[req-trace] id=%s stage=queue_timeout account=%s err=%v", requestID, acc.ID, err)
				writeUpstreamError(w, err)
				return
			}
		}
		setSSEHeaders(w)
		// Expose the stable downstream session id so a client that echoes it
		// back (session_id / x-session-id header / body.session_id) keeps its
		// task ledger and goal state across rounds.
		if resolved := s.sessionResolver.Resolve(r, &body); !resolved.IsNew {
			w.Header().Set(sessionHeaderName, resolved.SessionID)
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "stream unsupported")
			return
		}
		keepalive := startSSEKeepalive(w, flusher, 0)
		defer keepalive.stop()
		if err := keepalive.lockedWrite(": connected\n\n"); err != nil {
			return
		}
		var text strings.Builder
		var streamedTools []detectedToolCall
		first := true
		noTools := len(toolMaps) == 0
		identityFilter := newPublicIdentityStreamFilter(model)
		emitText := func(part string) error {
			if part == "" {
				return nil
			}
			// Push-only: the identity filter holds partial identity boundaries
			// until a safe boundary is reached. The final Flush is called in
			// flushText at the end of the stream.
			part = identityFilter.Push(part)
			if part == "" {
				return nil
			}
			if err := r.Context().Err(); err != nil {
				return err
			}
			delta := map[string]any{"content": part}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
			return keepalive.lockedWrite("data: " + mustJSON(chunk) + "\n\n")
		}
		flushText := func() error {
			if part := identityFilter.Flush(); part != "" {
				delta := map[string]any{"content": part}
				if first {
					delta["role"] = "assistant"
					first = false
				}
				chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}}
				return keepalive.lockedWrite("data: " + mustJSON(chunk) + "\n\n")
			}
			return nil
		}
		res, err := s.chatWithAccountEvents(ctx, acc.ID, account, answerReq, func(ev chathub.StreamEvent) error {
			if ev.Kind == "tool" && ev.ToolName != "" && len(ev.Arguments) > 0 {
				streamedTools = append(streamedTools, detectedToolCall{ID: "call_" + uuid.NewString(), Name: ev.ToolName, Arguments: ev.Arguments})
				return nil
			}
			if ev.Kind != "text" || ev.Text == "" {
				return nil
			}
			if noTools {
				// No tool validation to wait for: forward the delta immediately
				// so the downstream client sees the first token as soon as the
				// upstream produces it.
				if err := emitText(ev.Text); err != nil {
					return err
				}
			}
			text.WriteString(ev.Text)
			return nil
		})
		if err != nil && !clientPinnedAccount && (body.ConversationID == "" || body.ConversationID == resolvedConversationID) && (IsRateLimited(err) || IsAuthFailure(err) || errors.Is(err, outbound.ErrNoProxyNode)) {
			// A throttled stream may retry on the next healthy account: only the
			// ": connected" preamble reached the client, so the retried stream is
			// indistinguishable from a fresh request.
			next, nerr := s.nextProxySafeAccount(acc.ID)
			if nerr != nil {
				// no healthy alternative
			} else {
				if s.settings.get().CacheOnAccountSwitch {
					cachedOverride = cachedTokens()
				}
				failoverReq := failoverRequest(answerReq, body, resolvedConversationID, tone, ledger, planningMode, mcpServerURL, task, next.ID)
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				res2, err2 := s.chatWithAccountEvents(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq, func(ev chathub.StreamEvent) error {
					if ev.Kind == "tool" && ev.ToolName != "" && len(ev.Arguments) > 0 {
						streamedTools = append(streamedTools, detectedToolCall{ID: "call_" + uuid.NewString(), Name: ev.ToolName, Arguments: ev.Arguments})
						return nil
					}
					if ev.Kind != "text" || ev.Text == "" {
						return nil
					}
					if noTools {
						if err := emitText(ev.Text); err != nil {
							return err
						}
					}
					text.WriteString(ev.Text)
					return nil
				})
				if err2 == nil {
					task.recordSwitch(acc.ID, next.ID)
					res = res2
					acc = next
					err = nil
				} else {
					err = err2
					s.accountPool.MarkFailure(next.ID, err2, rateLimitCooldown())
				}
			}
		}
		if err != nil {
			log.Printf("[req-trace] id=%s stage=stream_error err=%v", requestID, err)
			s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown())
			// Record the failed stream so the request log does not show a
			// zero-token row: the input is known from the prompt, and the
			// output reflects whatever the upstream produced before failing.
			pt := EstimateTokens(answerPrompt)
			ct := EstimateTokens(res.Text)
			s.usage.record(UsageRecord{
				Time:           time.Now(),
				APIKeyPrefix:   apiKeyPrefix(r),
				AccountEmail:   acc.Email,
				Model:          firstNonEmpty(body.Model, "m365-copilot"),
				ReasoningLevel: body.ReasoningEffort,
				Endpoint:       "/v1/chat/completions",
				Stream:         true,
				InputTokens:    int64(pt),
				OutputTokens:   int64(ct),
				DurationMs:     time.Since(startedAt).Milliseconds(),
				Status:         upstreamStatus(err),
				Error:          truncatedError(err),
			})
			if tr := traceFromRequest(r); tr != nil {
				s.trace.update(tr.ID, func(rec *traceRecord) {
					rec.Status = "error"
					rec.StatusCode = http.StatusBadGateway
					rec.InputTokens = int64(pt)
					rec.OutputTokens = int64(ct)
					rec.Error = truncatedError(err)
				})
			}
			msg := upstreamError(err)
			code := "upstream_error"
			if IsRateLimited(err) {
				msg = "upstream is rate limiting; try again shortly"
				code = "rate_limit"
			} else if IsQueueTimeout(err) {
				msg = "account is at capacity; the request queued too long, please retry shortly"
				code = "queue_timeout"
			} else if errors.Is(err, context.DeadlineExceeded) {
				msg = "upstream timed out while generating the response"
			} else if errors.Is(err, context.Canceled) {
				msg = "upstream connection was interrupted"
			}
			msg = sanitizePublicInternalText(msg)
			// Preserve whatever the upstream produced before failing: a long
			// answer takes minutes to generate, and discarding the buffered text
			// turns a retryable timeout into a user-visible total loss. The
			// error event still follows so clients know the response is partial.
			// When the request has no tools, deltas were already forwarded
			// inline as they arrived, so only drain the identity filter tail.
			if noTools {
				_ = flushText()
			} else if text.Len() > 0 {
				_ = emitText(text.String())
			}
			_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(map[string]any{"error": map[string]any{"message": msg, "code": code}})+"\n\n")
			_ = keepalive.lockedWriteCtx(r.Context(), "data: [DONE]\n\n")
			return
		}
		s.accountPool.MarkSuccess(acc.ID)
		res.Text = sanitizePublicAssistantTextForModel(res.Text, body.Model)
		res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
		// Empty completion detection: if upstream returned no text and no tool
		// calls, surface a clear error instead of an empty successful response.
		if text.Len() == 0 && strings.TrimSpace(res.Text) == "" && len(streamedTools) == 0 {
			msg := "upstream returned empty completion; the requested model may be unavailable for this tenant"
			_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(map[string]any{"error": map[string]any{"message": msg, "code": "upstream_error"}})+"\n\n")
			_ = keepalive.lockedWriteCtx(r.Context(), "data: [DONE]\n\n")
			return
		}
		// Some ChatHub updates contain no text event and place the completed
		// answer only in the final Result, and long answers can lose text when
		// ChatHub rewrites its answer buffer mid-stream (non-prefix snapshots
		// are skipped to avoid duplicates). The result text is the
		// authoritative complete answer, so whenever it is longer than the
		// delta-assembled text, replace the buffer with it. This both recovers
		// the empty case and prevents a truncated prefix from being emitted as
		// a complete answer.
		// When streaming inline (no tools), deltas were already forwarded as
		// they arrived; only emit the unseen suffix if the result is longer.
		if noTools {
			// Deltas were already forwarded inline, so we cannot un-send text.
			// If the authoritative result extends what the client already saw
			// (a strict prefix superset), append only the unseen suffix.
			if len(res.Text) > text.Len() && strings.HasPrefix(res.Text, text.String()) {
				suffix := res.Text[text.Len():]
				if err := emitText(suffix); err != nil {
					log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
					return
				}
				text.WriteString(suffix)
			}
		} else if len(res.Text) > text.Len() {
			text.Reset()
			text.WriteString(res.Text)
		}
		rawCalls := streamedTools
		if len(rawCalls) == 0 {
			rawCalls = fencedToolCalls(text.String(), toolMaps, body.ToolChoice)
		}
		calls, rejected := validateCalls("stream", rawCalls)
		toolResult := chathub.Result{Text: text.String()}
		if len(calls) == 0 && rejected > 0 {
			// A native ChatHub event can contain a fabricated or empty tool name.
			// Do not leak it to the local runner: ask the model to remap the intent
			// to exactly one of the tools the client actually declared.
			repairPrompt := modelToolRouterPrompt(prompt+"\n"+ledger.RouterContext(), toolMaps, "required") +
				"\nREPAIR RULE: The previous upstream event selected an undeclared tool. Select one declared tool that performs the intended operation. Never return unknown_tool."
			repairRes, repairErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: repairPrompt, Tone: tone, Attachments: body.Attachments})
			if repairErr == nil {
				repaired, parsed := parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				if parsed {
					repaired = filterCompletedCalls(repaired, ledger)
					calls, _ = validateCalls("stream-repair", repaired)
					if len(calls) > 0 {
						toolResult = repairRes
					}
				}
			}
			if len(calls) == 0 {
				log.Printf("[tool-validation] id=%s stage=stream-repair failed", requestID)
				_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "upstream selected an undeclared tool and repair failed", "code": "invalid_tool_call"}})+"\n\n")
				_ = keepalive.lockedWriteCtx(r.Context(), "data: [DONE]\n\n")
				return
			}
		}
		calls, _ = validateCalls("stream", calls)
		calls = filterCompletedCalls(calls, ledger)
		if len(calls) > 0 {
			log.Printf("[req-trace] id=%s stage=tool_calls_detected count=%d names=%v", requestID, len(calls), func() []string {
				var n []string
				for _, c := range calls {
					n = append(n, c.Name)
				}
				return n
			}())
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			if body.ParallelToolCalls != nil && !*body.ParallelToolCalls && len(calls) > 1 {
				calls = calls[:1]
			}
			_ = writeToolResponse(w, id, model, true, body.shouldSendStreamUsage(), cachedTokens(), calls, toolResult)
			s.bindConversation(acc, &body, r, res, answerPrompt, startedAt, task, false)
			return
		}
		if noTools {
			// Text was already forwarded inline; drain the identity filter tail
			// so the final fragment reaches the client.
			if err := flushText(); err != nil {
				log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
				return
			}
		} else if err := emitText(text.String()); err != nil {
			log.Printf("[req-trace] id=%s stage=stream_write err=%v", requestID, err)
			return
		}
		if body.shouldSendStreamUsage() {
			pt := EstimateTokens(prompt) + EstimateTokens(storedContextPrompt)
			ct := EstimateTokens(res.Text)
			usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}}, "usage": usageWithCache(pt, ct, cachedTokens())}
			_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(usageChunk)+"\n\n")
		}
		finishChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}
		_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(finishChunk)+"\n\n")
		_ = keepalive.lockedWriteCtx(r.Context(), "data: [DONE]\n\n")
		s.bindConversation(acc, &body, r, res, answerPrompt, startedAt, task, true)
		return
	}
	// Ask the upstream model to select and validate the next tool. The gateway
	// remains tool-agnostic; it only validates and serializes the decision.
	// Stream requests (content or reasoning) are routed by the streaming tool
	// router above, which returns before this point; guard with !body.Stream so
	// a reasoning stream that fell through the content-stream block does not
	// run this non-stream router (it would emit tool SSE before the stream
	// preamble is written).
	if !body.Stream && planningMode == "router" && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		routePrompt := modelToolRouterPrompt(answerPrompt+"\n"+ledger.RouterContext(), toolMaps, body.ToolChoice)
		routeRes, routeErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments, TraceID: requestID, BindAccount: acc.ID})
		if routeErr != nil {
			s.accountPool.MarkFailure(acc.ID, routeErr, rateLimitCooldown())
			if IsRateLimited(routeErr) || IsAuthFailure(routeErr) {
				next, nerr := s.nextHealthyAccount(acc.ID)
				if nerr == nil {
					ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
					defer cancel2()
					if res2, err2 := s.chatWithAccount(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments}); err2 == nil {
						task.recordSwitch(acc.ID, next.ID)
						routeRes, routeErr = res2, nil
						acc = next
						account = chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}
					} else {
						s.accountPool.MarkFailure(next.ID, err2, rateLimitCooldown())
					}
				}
			}
			if routeErr != nil {
				msg := upstreamError(routeErr)
				if IsRateLimited(routeErr) {
					msg = "upstream is rate limiting; try again shortly"
				}
				writeOpenAIError(w, http.StatusBadGateway, "tool_router_error", msg)
				return
			}
			s.accountPool.MarkSuccess(acc.ID)
		}
		calls, parsed := parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
		if !parsed {
			repairRes, repairErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Do not invent calls; use {"calls":[]} if unrecoverable. OUTPUT:
` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments, TraceID: requestID, BindAccount: acc.ID})
			if repairErr == nil {
				calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
			}
			if !parsed {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "model returned an invalid tool routing decision")
				return
			}
		}
		calls = filterCompletedCalls(calls, ledger)
		calls, _ = validateCalls("router", calls)
		if len(calls) > 0 {
			scope := fmt.Sprintf("%d:%v", len(body.Messages), completedCallIDs(ledger))
			for i := range calls {
				calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
			}
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			if body.ParallelToolCalls != nil && !*body.ParallelToolCalls && len(calls) > 1 {
				calls = calls[:1]
			}
			_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, body.shouldSendStreamUsage(), cachedTokens(), calls, routeRes)
			s.recordToolUsage(r, acc, &body, routeRes, startedAt)
			return
		}
		if fmt.Sprint(body.ToolChoice) == "required" {
			defs, _ := json.Marshal(toolMaps)
			retryText := `Select at least one required next tool call from FUNCTION_DEFINITIONS. Validate every argument against its schema. Return JSON only as {"calls":[{"name":"function_name","arguments":{}}]}.
APPLICATION_REQUEST_AND_EVIDENCE:
` + prompt + "\n" + ledger.RouterContext() + "\nFUNCTION_DEFINITIONS:\n" + string(defs)
			retryRes, retryErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: retryText, Tone: tone, Attachments: body.Attachments, TraceID: requestID, BindAccount: acc.ID})
			if retryErr == nil {
				calls, parsed = parseModelToolDecision(retryRes.Text, toolMaps, body.ToolChoice)
				calls = filterCompletedCalls(calls, ledger)
				calls, _ = validateCalls("router", calls)
				if parsed && len(calls) > 0 {
					scope := fmt.Sprintf("%d:%v:required-retry", len(body.Messages), completedCallIDs(ledger))
					for i := range calls {
						calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
					}
					calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
					if body.ParallelToolCalls != nil && !*body.ParallelToolCalls && len(calls) > 1 {
						calls = calls[:1]
					}
					_ = writeToolResponse(w, "chatcmpl-"+uuid.NewString(), firstNonEmpty(body.Model, "m365-copilot"), body.Stream, body.shouldSendStreamUsage(), cachedTokens(), calls, retryRes)
					s.recordToolUsage(r, acc, &body, retryRes, startedAt)
					return
				}
			}
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "model did not select a required tool after constrained retry")
			return
		}
	}
	answerReq := buildAnswerRequest(answerPrompt, tone, body, ledger, planningMode, mcpServerURL)
	answerReq.TraceID = requestID
	answerReq.BindAccount = acc.ID
	// Reasoning gate and output for the ChainOfThought transcript.
	// The user has chosen to drop reasoning (no think block) and the
	// second content, and to stream the first content directly as the
	// answer. When gate is disabled (window=0) and reasoning is discarded,
	// the model behaves like a non-reasoning stream: content flows inline
	// with no buffering and no duplicate output.
	if isReasoningTone(tone) && body.Stream {
		// Reasoning gate is intentionally disabled (ReasoningGateWindow = 0)
		// so the first content streams immediately without buffering.
		// Reasoning content is discarded — the client sees only the answer.
	}
	// Preserve the true incremental prompt before the full answer request text
	// overwrites it (see the streaming path above).
	if sentPrompt == "" {
		sentPrompt = answerPrompt
	}
	answerPrompt = answerReq.Text
	var res chathub.Result
	// Reasoning-tone streams (tone ends in "_Reasoning") reach here via
	// ChatWithReasoning, which surfaces the ChainOfThought transcript as
	// streaming reasoning_content. Non-reasoning streams return inside the
	// content-stream block above, so this branch is reached only by models
	// whose tone drives reasoning generation.
	if body.Stream {
		// Bound the per-account queue wait BEFORE the stream preamble (see the
		// content-stream path above): fail with HTTP 429 instead of "silence
		// after :connected".
		if body.SessionID != "" {
			if err := s.accountConcurrency.WaitForSlot(r.Context(), acc.ID, body.SessionID); err != nil {
				log.Printf("[req-trace] id=%s stage=queue_timeout account=%s err=%v", requestID, acc.ID, err)
				writeUpstreamError(w, err)
				return
			}
		}
		setSSEHeaders(w)
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "stream unsupported")
			return
		}
		keepalive := startSSEKeepalive(w, flusher, 0)
		defer keepalive.stop()
		id := "chatcmpl-" + uuid.NewString()
		model := firstNonEmpty(body.Model, "m365-copilot")
		firstDelta := true
		writeChunk := func(delta map[string]any) error {
			if err := r.Context().Err(); err != nil {
				return err
			}
			// The first SSE chunk must carry the assistant role; subsequent
			// chunks carry content or reasoning deltas.
			if firstDelta {
				firstDelta = false
				withRole := map[string]any{"role": "assistant", "content": nil}
				for k, v := range delta {
					withRole[k] = v
				}
				delta = withRole
			}
			chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": delta}}}
			return keepalive.lockedWrite("data: " + mustJSON(chunk) + "\n\n")
		}
		contentFilter := newPublicIdentityStreamFilter(firstNonEmpty(body.Model, defaultPublicModelName))
		reasoningFilter := newPublicReasoningStreamFilter()
		var bufferedContent strings.Builder
		var bufferedReasoning strings.Builder

		// Text opening-window gate for workspace/tool misjudgment detection.
		// When the protocol gates pass (correction is possible), the first
		// textGateWindow bytes of text are buffered. If a misjudgment is found
		// the tainted text is discarded and a correction request produces fresh
		// content. If the window proves clean, the buffer and all subsequent
		// text stream inline. When no correction is possible, text flows inline
		// immediately with zero buffering.
		const textGateWindow = 300
		textGateActive := workspaceToolMisjudgmentPossible(toolMaps, prompt, ledger)
		var textGateBuf strings.Builder
		textGateReleased := !textGateActive
		textGateMisjudged := false
		corrected := false

		onDelta := func(content string) error {
			bufferedContent.WriteString(content)
			if textGateMisjudged {
				return nil
			}
			if textGateActive && !textGateReleased {
				textGateBuf.WriteString(content)
				if textGateBuf.Len() >= textGateWindow {
					if needsWorkspaceToolMisjudgmentCorrection(textGateBuf.String(), toolMaps, prompt, ledger) {
						textGateMisjudged = true
						textGateBuf.Reset()
						return nil
					}
					textGateReleased = true
					if c := contentFilter.Push(textGateBuf.String()); c != "" {
						return writeChunk(map[string]any{"content": c})
					}
					textGateBuf.Reset()
				}
				return nil
			}
			if c := contentFilter.Push(content); c != "" {
				return writeChunk(map[string]any{"content": c})
			}
			return nil
		}
		onReasoning := func(reasoning string) error {
			// Reasoning (think block) is intentionally dropped: the client sees
			// only the answer content stream. Still accumulate for res.Reasoning
			// so the request record / trace retains the transcript for debugging.
			bufferedReasoning.WriteString(reasoning)
			return nil
		}
		if err := keepalive.lockedWrite(": connected\n\n"); err != nil {
			return
		}
		res, err = s.chatWithAccountReasoning(ctx, acc.ID, account, answerReq, onDelta, onReasoning)
		if err != nil && !clientPinnedAccount && (body.ConversationID == "" || body.ConversationID == resolvedConversationID) && (IsRateLimited(err) || IsAuthFailure(err) || errors.Is(err, outbound.ErrNoProxyNode)) {
			// Retry a throttled stream on the next healthy account; the client
			// has only seen the ": connected" preamble so far, so the retry is
			// indistinguishable from a fresh request.
			next, nerr := s.nextProxySafeAccount(acc.ID)
			if nerr == nil {
				if s.settings.get().CacheOnAccountSwitch {
					cachedOverride = cachedTokens()
				}
				failoverReq := failoverRequest(answerReq, body, resolvedConversationID, tone, ledger, planningMode, mcpServerURL, task, next.ID)
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				if res2, err2 := s.chatWithAccountReasoning(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq, onDelta, onReasoning); err2 == nil {
					task.recordSwitch(acc.ID, next.ID)
					res = res2
					acc = next
					err = nil
				} else {
					err = err2
					s.accountPool.MarkFailure(next.ID, err2, rateLimitCooldown())
				}
			}
		}
		if err == nil {
			// The upstream stream has been fully buffered. Validate the complete
			// assistant response before emitting any client-visible body content or
			// persisting conversation state.
			// res.Text from chatWithAccountReasoning is the authoritative completion:
			// ChatHub signals text as full snapshots OR cursor rewrites, and
			// non-prefix rewrites are skipped by emitSnapshot to avoid duplicates, so
			// the delta-assembled bufferedContent can end up a truncated prefix of the
			// real answer. The completion frame's result.message (res.Text) restores
			// the authoritative complete text. Never let the partial streamed deltas
			// overwrite it; fall back to them only when the completion carried no text.
			if res.Text == "" && bufferedContent.Len() > 0 {
				res.Text = bufferedContent.String()
			}
			if res.Reasoning == "" && bufferedReasoning.Len() > 0 {
				res.Reasoning = bufferedReasoning.String()
			}
			// The correction fires either when the opening-window gate already
			// detected a misjudgment (textGateMisjudged — the tainted text was
			// discarded before reaching the client), or when the whole response
			// was shorter than the window and the completion-time check catches
			// it on the still-buffered text. In both cases nothing client-visible
			// has been sent, so a fresh correction request can replace the answer
			// cleanly. The correction runs as a reasoning stream so its
			// ChainOfThought continues the already-streamed reasoning seamlessly.
			if textGateMisjudged || (textGateActive && !textGateReleased && needsWorkspaceToolMisjudgmentCorrection(res.Text, toolMaps, prompt, ledger)) {
				bufferedContent.Reset()
				bufferedReasoning.Reset()
				correctionReq := answerReq
				correctionReq.Text = unifiedSandboxCorrection(toolMaps, prompt)
				correctionOnDelta := func(content string) error {
					bufferedContent.WriteString(content)
					if c := contentFilter.Push(content); c != "" {
						return writeChunk(map[string]any{"content": c})
					}
					return nil
				}
				correctionOnReasoning := func(reasoning string) error {
					bufferedReasoning.WriteString(reasoning)
					if rc := reasoningFilter.Push(reasoning); rc != "" {
						return writeChunk(map[string]any{"reasoning_content": rc})
					}
					return nil
				}
				res2, correctionErr := s.chatWithAccountReasoning(ctx, acc.ID, account, correctionReq, correctionOnDelta, correctionOnReasoning)
				if correctionErr != nil {
					_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "upstream workspace/tool correction failed", "code": "workspace_tool_correction_failed"}})+"\n\n")
					_ = keepalive.lockedWriteCtx(r.Context(), "data: [DONE]\n\n")
					s.markTraceError(r, correctionErr, http.StatusBadGateway)
					return
				}
				if needsWorkspaceToolMisjudgmentCorrection(res2.Text, toolMaps, prompt, ledger) {
					_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "upstream repeatedly misidentified workspace or tool availability", "code": "workspace_tool_misjudgment"}})+"\n\n")
					_ = keepalive.lockedWriteCtx(r.Context(), "data: [DONE]\n\n")
					s.markTraceError(r, errors.New("upstream repeatedly misidentified workspace or tool availability"), http.StatusBadGateway)
					return
				}
				res = res2
				corrected = true
			}
			res.Text = sanitizePublicAssistantTextForModel(res.Text, body.Model)
			res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
			// Tool-call detection for the reasoning stream. ChatWithReasoning's
			// onEvent handler drops non-reasoning events, so native ChatHub tool
			// events never reach this path; the upstream instead encodes the tool
			// decision as fenced JSON or a CALL_TOOL line inside res.Text. Detect it
			// here and answer with tool_calls so the downstream agent can execute the
			// tool and continue its loop, instead of treating the call text as a
			// final answer (which closes the session with openStep=null). Tool-call
			// responses are short (fenced blocks keep outside text ≤240 chars, a
			// CALL_TOOL line is a single line), so they always sit inside the
			// buffered text window — the gate has not released anything to the
			// client yet, so nothing needs to be un-sent.
			if len(toolMaps) > 0 && res.Text != "" && !textGateMisjudged && !corrected {
				calls := fencedToolCalls(res.Text, toolMaps, body.ToolChoice)
				if len(calls) == 0 {
					if parsedCalls, parsed := parseModelToolDecision(res.Text, toolMaps, body.ToolChoice); parsed && len(parsedCalls) > 0 {
						calls = parsedCalls
					}
				}
				calls = filterCompletedCalls(calls, ledger)
				calls, _ = validateCalls("reasoning-stream", calls)
				if len(calls) > 0 {
					calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
					if body.ParallelToolCalls != nil && !*body.ParallelToolCalls && len(calls) > 1 {
						calls = calls[:1]
					}
					scope := fmt.Sprintf("%d:%v:reasoning-stream", len(body.Messages), completedCallIDs(ledger))
					for i := range calls {
						calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
					}
					writeToolChunk := func(delta map[string]any, finish any) error {
						chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
						return keepalive.lockedWrite("data: " + mustJSON(chunk) + "\n\n")
					}
					// Reasoning was already streamed inline via onReasoning, so the
					// first tool-call chunk only needs the assistant role marker.
					_ = writeToolChunk(map[string]any{"role": "assistant", "content": nil}, nil)
					for i, tc := range calls {
						typ := tc.Type
						if typ == "" {
							typ = "function"
						}
						isLast := i == len(calls)-1
						_ = writeToolChunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": tc.ID, "type": typ, "function": map[string]any{"name": tc.Name, "arguments": ""}}}}, nil)
						args := string(tc.Arguments)
						const argChunkSize = 512
						for off := 0; off < len(args); off += argChunkSize {
							end := off + argChunkSize
							if end > len(args) {
								end = len(args)
							}
							for end < len(args) && !utf8.RuneStart(args[end]) {
								end++
							}
							argChunk := args[off:end]
							isLastArgChunk := off+argChunkSize >= len(args)
							var finish any
							if isLast && isLastArgChunk {
								finish = "tool_calls"
							}
							_ = writeToolChunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": argChunk}}}}, finish)
						}
						if len(args) == 0 && isLast {
							_ = writeToolChunk(map[string]any{}, "tool_calls")
						}
					}
					if body.shouldSendStreamUsage() {
						pt := EstimateTokens(prompt) + EstimateTokens(storedContextPrompt)
						ct := EstimateTokens(res.Text)
						usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": nil}}, "usage": usageWithCache(pt, ct, cachedTokens())}
						_ = keepalive.lockedWrite("data: " + mustJSON(usageChunk) + "\n\n")
					}
					_ = keepalive.lockedWrite("data: [DONE]\n\n")
					s.recordToolUsage(r, acc, &body, res, startedAt)
					s.bindConversation(acc, &body, r, res, answerPrompt, startedAt, task, false)
					return
				}
			}
			// Reasoning is always streamed inline now; drain the filter tail so
			// the last fragment reaches the client.
			if reasoning := reasoningFilter.Flush(); reasoning != "" {
				if writeErr := writeChunk(map[string]any{"reasoning_content": reasoning}); writeErr != nil {
					return
				}
			}
			// Content depends on the text gate state:
			//   - misjudged: the tainted text was discarded and the correction
			//     request already wrote its own content inline, so nothing
			//     remains to write here.
			//   - active & not released: the whole response fit inside the
			//     buffered window, so send the authoritative completion text now.
			//   - otherwise: text was already forwarded inline; drain the filter
			//     tail for any remaining fragment.
			if textGateMisjudged || corrected {
				// correction content already written inline above
			} else if textGateActive && !textGateReleased {
				if content := contentFilter.Push(res.Text) + contentFilter.Flush(); content != "" {
					if writeErr := writeChunk(map[string]any{"content": content}); writeErr != nil {
						return
					}
				}
			} else {
				if content := contentFilter.Flush(); content != "" {
					if writeErr := writeChunk(map[string]any{"content": content}); writeErr != nil {
						return
					}
				}
			}
			s.accountPool.MarkSuccess(acc.ID)
		} else {
			log.Printf("[req-trace] id=%s stage=stream_error err=%v", requestID, err)
			s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown())
			code := "upstream_error"
			msg := upstreamError(err)
			if IsEmptyCompletion(err) {
				code = "upstream_empty_completion"
				msg = "upstream completed without assistant content; retry the request or verify that the requested model is available for this tenant"
			} else if IsRateLimited(err) {
				code = "rate_limit_error"
				msg = "upstream is rate limiting; try again shortly"
			} else if IsQueueTimeout(err) {
				code = "queue_timeout"
				msg = "account is at capacity; the request queued too long, please retry shortly"
			}
			msg = sanitizePublicInternalText(msg)
			// Reasoning is always streamed inline; drain filter tails so the
			// partial content reaches the client before the error event.
			if reasoning := reasoningFilter.Flush(); reasoning != "" {
				_ = writeChunk(map[string]any{"reasoning_content": reasoning})
			}
			if content := contentFilter.Flush(); content != "" {
				_ = writeChunk(map[string]any{"content": content})
			}
			_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(map[string]any{"error": map[string]any{"message": msg, "code": code}})+"\n\n")
			_ = keepalive.lockedWriteCtx(r.Context(), "data: [DONE]\n\n")
			return
		}
		pt := EstimateTokens(prompt) + EstimateTokens(storedContextPrompt)
		ct := EstimateTokens(res.Text)
		cached := cachedTokens()
		if tr := traceFromRequest(r); tr != nil {
			s.trace.update(tr.ID, func(rec *traceRecord) {
				rec.InputTokens = int64(pt)
				rec.OutputTokens = int64(ct)
				rec.CachedTokens = cached
			})
		}
		log.Printf("[usage] stream id=%s pt=%d ct=%d cached=%d res.Text=%d", id, pt, ct, cached, len(res.Text))
		if err == nil && ct == 0 {
			_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(map[string]any{"error": map[string]any{"message": "upstream returned empty completion; the requested model may be unavailable for this tenant", "code": "upstream_error"}})+"\n\n")
		}
		finish := "stop"
		if err != nil {
			finish = "stop"
		}
		usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usageWithCache(pt, ct, cached)}
		_ = keepalive.lockedWriteCtx(r.Context(), "data: "+mustJSON(usageChunk)+"\n\n")
		_ = keepalive.lockedWriteCtx(r.Context(), "data: [DONE]\n\n")
		s.bindConversation(acc, &body, r, res, answerPrompt, startedAt, task, true)
		return
	} else {
		res, err = s.chatWithAccount(ctx, acc.ID, account, answerReq)
		if IsEmptyCompletion(err) && tone != "magic" {
			log.Printf("[tone-fallback] tone=%q returned empty, retrying with magic", tone)
			magicReq := answerReq
			magicReq.Tone = "magic"
			if res2, err2 := s.chatWithAccount(ctx, acc.ID, account, magicReq); err2 == nil && res2.Text != "" {
				res = res2
				err = nil
			}
		}
		if err != nil && !clientPinnedAccount && (body.ConversationID == "" || body.ConversationID == resolvedConversationID) && (IsRateLimited(err) || IsAuthFailure(err) || errors.Is(err, outbound.ErrNoProxyNode)) {
			// Failover only when nothing pins the request to a conversation or
			// account; a fresh chat can safely retry on the next healthy account.
			next, nerr := s.nextProxySafeAccount(acc.ID)
			if nerr == nil {
				if s.settings.get().CacheOnAccountSwitch {
					cachedOverride = cachedTokens()
				}
				failoverReq := failoverRequest(answerReq, body, resolvedConversationID, tone, ledger, planningMode, mcpServerURL, task, next.ID)
				ctx2, cancel2 := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
				defer cancel2()
				res2, err2 := s.chatWithAccount(ctx2, next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, failoverReq)
				if err2 == nil {
					task.recordSwitch(acc.ID, next.ID)
					res = res2
					acc = next
					err = nil
					s.accountPool.MarkSuccess(next.ID)
				} else {
					err = err2
				}
			}
		}
	}
	if err != nil {
		s.accountPool.MarkFailure(acc.ID, err, rateLimitCooldown())
		if task != nil {
			task.recordFailure(err)
		}
		s.usage.record(UsageRecord{
			Time:           time.Now(),
			APIKeyPrefix:   apiKeyPrefix(r),
			AccountEmail:   acc.Email,
			Model:          firstNonEmpty(body.Model, "m365-copilot"),
			ReasoningLevel: body.ReasoningEffort,
			Endpoint:       "/v1/chat/completions",
			Stream:         body.Stream,
			DurationMs:     time.Since(startedAt).Milliseconds(),
			Status:         upstreamStatus(err),
			Error:          truncatedError(err),
		})
		if tr := traceFromRequest(r); tr != nil {
			s.trace.update(tr.ID, func(rec *traceRecord) {
				rec.Status = "error"
				rec.StatusCode = http.StatusBadGateway
				rec.Error = truncatedError(err)
			})
		}
		writeUpstreamError(w, err)
		return
	}
	s.accountPool.MarkSuccess(acc.ID)

	// Do not update SessionKey or userSessions until the complete response has
	// passed workspace/tool-misjudgment validation and any clean-branch repair.
	// This prevents the invalid upstream conversation A from becoming reusable.
	model := body.Model
	if model == "" {
		model = "m365-copilot"
	}
	id := "chatcmpl-" + uuid.NewString()
	// Validate and, when necessary, repair the complete assistant answer before
	// any conversation binding, cache update, or client output. The shared helper
	// always creates a clean upstream branch and accepts only a distinct, valid
	// replacement conversation.
	if len(toolMaps) > 0 && needsWorkspaceToolMisjudgmentCorrection(res.Text, toolMaps, prompt, ledger) {
		res, err = s.recoverWorkspaceToolMisjudgment(ctx, acc, account, &body, res, answerReq, toolMaps, tone, requestID, prompt, ledger)
		if err != nil {
			log.Printf("[workspace-tool-eject] id=%s clean-branch correction failed: %v", requestID, err)
			writeOpenAIError(w, http.StatusBadGateway, workspaceToolCorrectionErrorCode(err), workspaceToolCorrectionPublicMessage(err))
			return
		}
	}
	invalidDetectedTool := false
	if rawCalls := fencedToolCalls(res.Text, toolMaps, body.ToolChoice); len(rawCalls) > 0 {
		calls, rejected := validateCalls("fenced", rawCalls)
		invalidDetectedTool = rejected > 0
		if len(calls) > 0 {
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			if body.ParallelToolCalls != nil && !*body.ParallelToolCalls && len(calls) > 1 {
				calls = calls[:1]
			}
			_ = writeToolResponse(w, id, model, body.Stream, body.shouldSendStreamUsage(), cachedTokens(), calls, res)
			return
		}
	}
	if rawCalls := nativeToolCalls(res.Events, body.Tools); len(rawCalls) > 0 {
		calls, rejected := validateCalls("native", rawCalls)
		invalidDetectedTool = invalidDetectedTool || rejected > 0
		if len(calls) > 0 {
			calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
			if body.ParallelToolCalls != nil && !*body.ParallelToolCalls && len(calls) > 1 {
				calls = calls[:1]
			}
			_ = writeToolResponse(w, id, model, body.Stream, body.shouldSendStreamUsage(), cachedTokens(), calls, res)
			return
		}
	}
	// Recover natural-language tool intent in native mode, and repair any
	// structured event that failed the declared-name/schema boundary.
	if (planningMode == "native" || invalidDetectedTool) && len(toolMaps) > 0 && fmt.Sprint(body.ToolChoice) != "none" {
		routePrompt := modelToolRouterPrompt(prompt+"\n"+ledger.RouterContext(), toolMaps, body.ToolChoice)
		routeRes, routeErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: routePrompt, Tone: tone, Attachments: body.Attachments, TraceID: requestID, BindAccount: acc.ID})
		if routeErr == nil {
			calls, parsed := parseModelToolDecision(routeRes.Text, toolMaps, body.ToolChoice)
			if !parsed {
				repairRes, repairErr := s.chatWithAccount(ctx, acc.ID, account, chathub.Request{Text: `Repair this tool routing output into JSON only with shape {"calls":[{"name":"function_name","arguments":{}}]}. Use {"calls":[]} if no tool is needed. OUTPUT:\n` + compactToolResult(routeRes.Text, 6000), Tone: tone, Attachments: body.Attachments, TraceID: requestID, BindAccount: acc.ID})
				if repairErr == nil {
					calls, parsed = parseModelToolDecision(repairRes.Text, toolMaps, body.ToolChoice)
				}
			}
			calls, _ = validateCalls("native-recovery", calls)
			calls = filterCompletedCalls(calls, ledger)
			if parsed && len(calls) > 0 {
				scope := fmt.Sprintf("%d:%v:native-recovery", len(body.Messages), completedCallIDs(ledger))
				for i := range calls {
					calls[i].ID = scopedCallID(calls[i].Name, string(calls[i].Arguments), i, scope)
				}
				calls = limitToolCalls(calls, adaptiveToolCallLimit(calls, configuredToolCallLimit(s.settings)))
				_ = writeToolResponse(w, id, model, body.Stream, body.shouldSendStreamUsage(), cachedTokens(), calls, routeRes)
				return
			}
		}
	}
	if isContentPolicyBlock(res.Text) {
		log.Printf("[content-policy] M365 blocked the request, returning 503")
		writeOpenAIError(w, http.StatusServiceUnavailable, "upstream_content_blocked", "M365 content policy blocked this request; try again or switch account")
		return
	}
	if len(toolMaps) > 0 && !completionEvidenceAllowsUpgraded(res.Text, ledger) {
		res.Text = "I cannot confirm completion because no matching tool results were returned. No external action has been verified."
	}
	// Honor an explicit output cap on the final answer. Tool-call responses
	// returned above are never truncated (capping JSON would corrupt the call).
	res.Text = capOutputTokens(res.Text, effectiveMaxOutput(&body))
	res.Text = sanitizePublicAssistantTextForModel(res.Text, body.Model)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
	log.Printf("[debug] res.Text bytes=%d content=%q", len(res.Text), res.Text)

	// Never serialize an empty assistant message as a successful completion.
	// Some clients interpret finish_reason=stop with blank content as
	// "completed response with no content". Image-only responses remain valid.
	if strings.TrimSpace(res.Text) == "" && len(res.Images) == 0 {
		log.Printf("[req-trace] id=%s stage=empty_completion model=%s reasoning_bytes=%d events=%d", requestID, model, len(res.Reasoning), len(res.Events))
		writeOpenAIError(w, http.StatusBadGateway, "upstream_empty_completion", "upstream completed without assistant content; retry the request or verify that the requested model is available for this tenant")
		return
	}

	// Persist only the final validated response. This point is after workspace/tool
	// misjudgment detection, the bounded corrective retry, tool-call recovery,
	// content-policy handling, and completion-evidence normalization, but before
	// either streaming or non-streaming client output.
	if res.ConversationID != "" {
		s.bindConversation(acc, &body, r, res, prompt, startedAt, task, true)
		resolved := s.sessionResolver.Resolve(r, &body)
		if !resolved.IsNew {
			w.Header().Set(sessionHeaderName, resolved.SessionID)
		}
	}
	created := time.Now().Unix()

	if body.Stream {
		setSSEHeaders(w)
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "stream unsupported")
			return
		}
		// one-shot "stream" 鈥?emit full content then done
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": res.Text},
			}},
		}
		b, _ := json.Marshal(chunk)
		_ = sseRaw(r.Context(), w, flusher, "data: "+string(b)+"\n\n")
		pt := EstimateTokens(prompt) + EstimateTokens(storedContextPrompt)
		ct := EstimateTokens(res.Text)
		usageChunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usageWithCache(pt, ct, cachedTokens())}
		_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(usageChunk)+"\n\n")
		_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
		return
	}

	if responseFormat != nil && (responseFormat.Type == "json_object" || responseFormat.Type == "json_schema") {
		res.Text = normalizeJSONText(res.Text)
	}
	content := any(res.Text)
	if len(res.Images) > 0 {
		parts := []any{map[string]any{"type": "text", "text": res.Text}}
		for _, u := range res.Images {
			du, _ := downloadImageAsDataURIWithToken(u, acc.AccessToken)
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": du}})
		}
		content = parts
	}
	assistant := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if res.Reasoning != "" {
		assistant["reasoning_content"] = res.Reasoning
	}
	// 涓婃父 ChatHub 涓嶈繑鍥?token 璁℃暟锛屾寜璇锋眰/鍥炲鏂囨湰鏈湴浼扮畻濉厖
	// OpenAI 瑕佹眰鐨?usage 瀛楁銆?
	pt := EstimateTokens(prompt) + EstimateTokens(storedContextPrompt)
	ct := EstimateTokens(res.Text)
	cached := cachedTokens()
	meta := compatM365Metadata(res)
	// 璋冭瘯鍙鎬э細m365.cache_hit / m365.cached_tokens 鐩存帴鏍囧嚭鏈璇锋眰鏄惁
	// 澶嶇敤浜嗕笂娓镐細璇濄€佸鐢ㄤ簡澶氬皯杈撳叆 token锛? = 鏈懡涓紝鏂板缓浼氳瘽鍏ㄩ噺鍙戦€侊級銆?
	meta["cache_hit"] = cached > 0
	meta["cached_tokens"] = cached
	if params, ok := ignoredSamplingParams(&body); ok {
		meta["ignored_parameters"] = params
		meta["sampling_note"] = samplingNote
	}
	completion := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       assistant,
			"finish_reason": "stop",
		}},
		"m365": meta,
		// 鎶娿€屽閲忚姹備腑琚笂娓镐細璇濆鐢ㄧ殑杈撳叆銆嶄互鏍囧噯瀛楁
		// usage.prompt_tokens_details.cached_tokens 杩斿洖缁欎笅娓革紙sub2api 绛?
		// 杞彂灞備笌瀹㈡埛绔嵁姝よ瘑鍒紦瀛樺懡涓級銆?
		"usage": usageWithCache(pt, ct, cached),
	}
	if len(body.Metadata) > 0 {
		completion["metadata"] = body.Metadata
	}
	jsonOut(w, completion)
}

func (s *Server) writePublicIdentityChatResponse(w http.ResponseWriter, r *http.Request, body *oaiReq, prompt, answer string, startedAt time.Time) {
	model := firstNonEmpty(body.Model, defaultPublicModelName)
	id := "chatcmpl-" + uuid.NewString()
	created := time.Now().Unix()
	inputTokens := EstimateTokens(prompt)
	outputTokens := EstimateTokens(answer)
	usage := map[string]any{"prompt_tokens": inputTokens, "completion_tokens": outputTokens, "total_tokens": inputTokens + outputTokens}
	if s.usage != nil {
		s.usage.record(UsageRecord{
			Time:         time.Now(),
			APIKeyPrefix: extractAPIKey(r),
			Model:        model,
			Endpoint:     "/v1/chat/completions",
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			DurationMs:   time.Since(startedAt).Milliseconds(),
			Status:       http.StatusOK,
		})
	}
	if !body.Stream {
		jsonOut(w, map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": answer},
				"finish_reason": "stop",
			}},
			"usage": usage,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "stream unsupported")
		return
	}
	chunk := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": answer},
			"finish_reason": nil,
		}},
	}
	_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(chunk)+"\n\n")
	finish := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage}
	_ = sseRaw(r.Context(), w, flusher, "data: "+mustJSON(finish)+"\n\n")
	_ = sseRaw(r.Context(), w, flusher, "data: [DONE]\n\n")
}

const defaultPublicModelName = "m365-copilot"

// sessionHeaderName 鏄壙杞芥樉寮忎細璇?ID 鐨勯閫夎姹傚ご銆傛爣鍑?OpenAI 鍏煎瀹㈡埛绔?
// 锛堝 DSH / pi-ai锛夐粯璁ゅ彂閫?session_id锛岀綉鍏充互瀹冧綔涓洪粯璁や細璇濇爣璇嗐€?
const sessionHeaderName = "session_id"

// sessionHeaderAlt 鏄悓涓€浼氳瘽鏍囪瘑鐨勫彟涓€鍛藉悕锛氶儴鍒嗗鎴风锛坧i-ai 鐨?openrouter
// 鏍煎紡銆丱penAI-internal 椋庢牸锛変娇鐢?x-session-id 鍙戦€佺浉鍚岀殑浼氳瘽 ID锛岀綉鍏冲吋瀹硅鍙栥€?
const sessionHeaderAlt = "x-session-id"

// sessionHeaderClientRequestID 鏄悓涓€浼氳瘽鏍囪瘑鐨勭涓夊懡鍚嶏細pi-ai 鍦?openai
// 浼氳瘽鍜屽舰寮忎笅浼氶櫎浜?session_id 澶栭『甯﹀彂閫?x-client-request-id
// 锛堝€间笌 sessionId 鐩稿悓锛夈€傚皢瀹冧綔涓哄洖閫€鍙栧彇锛屼繚璇佷笉鍙戝彂
// session_id 鐨勫鎴风涔熻兘鎸佺画浼氳瘽鍙嶇敤銆?
const sessionHeaderClientRequestID = "x-client-request-id"

// sessionIDFromRequest 鎻愬彇璇锋眰澶翠腑鐨勬樉寮忎細璇?ID銆俿ession_id锛堥粯璁わ紝DSH/pi-ai
// 鍙戦€侊級浼樺厛锛泋-session-id 浣滀负鍏煎鍒悕鏍￠獙锛宨-client-request-id 浣滀负
// pi-ai 鐨勫叾浣欏洖閫€銆備笁鑰呴兘涓嶅瓨鍦ㄦ椂杩斿洖绌轰覆锛岃皟鐢ㄦ柟鎸夈€屾棤鏄惧紡浼氳瘽
// ID銆嶅鐞嗭紙鏂板紑瀵硅瘽锛岀粷涓嶅鐢級銆?
func sessionIDFromRequest(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get(sessionHeaderName)); id != "" {
		return id
	}
	if id := strings.TrimSpace(r.Header.Get(sessionHeaderAlt)); id != "" {
		return id
	}
	return strings.TrimSpace(r.Header.Get(sessionHeaderClientRequestID))
}

// bindConversation 鍦ㄨ姹傚畬鎴愬悗鐧昏浼氳瘽瑙ｆ瀽鍣ㄧ储寮曚笌缂撳瓨缁熻锛屾祦寮忎笌闈炴祦寮?
// 璺緞鍏辩敤銆俧inalRound 涓?false 鏃惰烦杩囨湇鍔＄鐘舵€佷慨姝ｏ紙宸ュ叿璋冪敤杞棤娉曞畨鍏?
// 鍒ゆ柇瀹屾垚鎺緸鐨勭湡瀹炴剰鍥撅級銆?
func (s *Server) bindConversation(acc auth.AccountToken, body *oaiReq, r *http.Request, res chathub.Result, prompt string, startedAt time.Time, task *taskLedger, finalRound bool) {
	if res.ConversationID == "" {
		return
	}
	if task != nil {
		// Fold this turn's tool evidence into the persistent ledger and pin it
		// to the account/conversation that actually served the task.
		task.mergeEvidence(buildAgentLedger(body.Messages))
		task.bind(acc.ID, res.ConversationID, res.SessionID)
		// State correction: a goal-protocol turn whose final answer states
		// completion (with tool evidence) closes the goal server-side. The
		// merged evidence already handles explicit update_goal(action=complete);
		// this catches the "work is done, but I could not call update_goal"
		// case the agent reports as the last remaining step. Only final-round
		// answers are eligible 鈥?a tool-call round must never close the goal
		// (the model may still be mid-implementation).
		if finalRound && !task.IsComplete() && goalRoundRequest(body.Messages, task, body.Tools) && goalCompletionSignal(res.Text, buildAgentLedger(body.Messages)) {
			task.markComplete("server-side correction: final answer states completion with tool evidence")
		}
	}
	historyBody := *body
	historyBody.Messages = append(cloneMessages(body.Messages), oaiMsg{
		Role:             "assistant",
		Content:          res.Text,
		ReasoningContent: res.Reasoning,
	})
	// Persist the task ledger only for goal-protocol requests (the client
	// declares create_goal/get_goal/update_goal) or when a goal id already
	// exists. A non-goal request — such as the DSH session-title generation
	// pass that carries no tools — must not pin its transient "goal" (e.g.
	// "Generate the session title") onto the session; otherwise the next
	// request in the same session re-injects that goal into the upstream
	// prompt and the model answers with a title instead of the real task.
	bindTask := task
	if task != nil && !toolsDeclareGoal(body.Tools) && task.GoalID == "" {
		bindTask = nil
	}
	s.sessionResolver.BindWithTask(res.SessionID, res.ConversationID, acc.ID, &historyBody, "", r, bindTask)
	s.conversationManager.Record(res.ConversationID, acc.ID, prompt)
	if s.conversationManager.ShouldCleanup() {
		if cleaned := s.conversationManager.Cleanup(); len(cleaned) > 0 {
			log.Printf("[conversation-manager] auto-cleaned %d conversations", len(cleaned))
		}
	}

	apiKey := extractAPIKey(r)
	// Only count history tokens when the request carries an explicit session ID
	// header AND the session was actually reused upstream. Without a session_id
	// header the resolver always returns IsNew, so there is no upstream reuse
	// and counting history tokens as "cached" would be misleading (DSH/pi-ai
	// sends full history every turn but does not send session_id by default).
	historyTokens := int64(0)
	explicitID := sessionIDFromRequest(r)
	if explicitID != "" {
		upper := len(body.Messages) - 1
		if upper < 0 {
			upper = 0
		}
		for _, msg := range body.Messages[:upper] {
			historyTokens += EstimateTokens(contentToString(msg.Content))
		}
		// Single-turn incremental reuse (explicit_incremental): the request carried
		// only the current turn, but the upstream conversation already held the
		// persisted history. Account those pre-existing tokens as cached history so
		// the server-side usage record matches the cached_tokens reported to the
		// client. A first turn has no session binding yet, so this stays zero.
		if upper == 0 && body.SessionID != "" {
			if sess, ok := s.sessionResolver.GetSession(body.SessionID); ok && len(sess.ContextHistory) > 0 {
				for _, msg := range sess.ContextHistory {
					historyTokens += EstimateTokens(contentToString(msg.Content))
				}
			}
		}
	}
	newTokens := EstimateTokens(prompt)
	outTokens := EstimateTokens(res.Text)
	durMs := time.Since(startedAt).Milliseconds()
	ttft := res.TTFTMs
	speed := 0.0
	if durMs > 0 && outTokens > 0 {
		speed = float64(outTokens) / (float64(durMs) / 1000.0)
	}
	sessions := s.sessionResolver.ListSessions()
	cacheStats.RecordRequest(apiKey, historyTokens > 0, newTokens, historyTokens, len(sessions))
	keyLabel := apiKeyPrefix(r)
	s.usage.record(UsageRecord{
		Time:           time.Now(),
		APIKeyPrefix:   keyLabel,
		AccountEmail:   acc.Email,
		Model:          firstNonEmpty(body.Model, "m365-copilot"),
		ReasoningLevel: body.ReasoningEffort,
		Endpoint:       "/v1/chat/completions",
		Stream:         body.Stream,
		InputTokens:    newTokens,
		OutputTokens:   outTokens,
		CacheTokens:    historyTokens,
		TTFTMs:         ttft,
		SpeedTPs:       speed,
		DurationMs:     durMs,
		Status:         200,
	})
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			rec.Model = firstNonEmpty(body.Model, "m365-copilot")
			rec.Stream = body.Stream
			rec.AccountEmail = acc.Email
			rec.ReasoningLevel = body.ReasoningEffort
			rec.InputTokens = newTokens
			rec.OutputTokens = outTokens
			rec.CachedTokens = historyTokens
			rec.TTFTMs = ttft
			rec.SpeedTPs = speed
			rec.DurationMs = durMs
		})
	}
}

// recordToolUsage records a successful usage entry for requests that return a
// tool-call response short-circuit (routed tool turns) without a final answer.
func (s *Server) recordToolUsage(r *http.Request, acc auth.AccountToken, body *oaiReq, res chathub.Result, startedAt time.Time) {
	// Tool-call short-circuits do not pass through bindConversation, so calculate
	// their request and cached-history tokens here instead of leaving them zero.
	// Only count history as cached when the request carries an explicit session
	// ID (the resolver never reuses without it), matching bindConversation.
	var inputTokens, cacheTokens int64
	last := len(body.Messages) - 1
	for i, msg := range body.Messages {
		tokens := EstimateTokens(contentToString(msg.Content))
		if i < last && sessionIDFromRequest(r) != "" {
			cacheTokens += tokens
		} else {
			inputTokens += tokens
		}
	}
	outputTokens := EstimateTokens(res.Text)
	durationMs := time.Since(startedAt).Milliseconds()

	s.usage.record(UsageRecord{
		Time:           time.Now(),
		APIKeyPrefix:   apiKeyPrefix(r),
		AccountEmail:   acc.Email,
		Model:          firstNonEmpty(body.Model, "m365-copilot"),
		ReasoningLevel: body.ReasoningEffort,
		Endpoint:       "/v1/chat/completions",
		Stream:         body.Stream,
		InputTokens:    inputTokens,
		OutputTokens:   outputTokens,
		CacheTokens:    cacheTokens,
		TTFTMs:         res.TTFTMs,
		DurationMs:     durationMs,
		Status:         200,
	})
	if tr := traceFromRequest(r); tr != nil {
		s.trace.update(tr.ID, func(rec *traceRecord) {
			rec.Model = firstNonEmpty(body.Model, "m365-copilot")
			rec.Stream = body.Stream
			rec.AccountEmail = acc.Email
			rec.ReasoningLevel = body.ReasoningEffort
			rec.InputTokens = inputTokens
			rec.OutputTokens = outputTokens
			rec.CachedTokens = cacheTokens
			rec.TTFTMs = res.TTFTMs
			rec.DurationMs = durationMs
		})
	}
}

func extractAPIKey(r *http.Request) string {
	key := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if key == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			key = strings.TrimSpace(auth[7:])
		}
	}
	return key
}

// apiKeyPrefix returns a short, privacy-safe key label for usage/trace records.
func apiKeyPrefix(r *http.Request) string {
	key := extractAPIKey(r)
	if len(key) > 8 {
		return key[:8] + "..."
	}
	return key
}

// truncatedError keeps error messages stored in usage/trace records bounded.
func truncatedError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func extractOIDTID(accessToken string) (oid, tid string) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	if v, ok := m["oid"].(string); ok {
		oid = v
	}
	if v, ok := m["tid"].(string); ok {
		tid = v
	}
	return oid, tid
}
