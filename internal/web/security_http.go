package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:web
var webFS embed.FS

var webContent http.FileSystem

func init() {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	webContent = http.FS(sub)
}

func vendorFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/vendor/")
	if name == "" || name != path.Base(name) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	f, err := webContent.Open("vendor/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; form-action 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self'; script-src 'self' 'unsafe-inline'")
		if r.URL.Path == "/" || r.URL.Path == "/login" || r.URL.Path == "/api/admin/login" || r.URL.Path == "/api/admin/session" || r.URL.Path == "/api/admin/change-password" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rootPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/login" && r.URL.Path != "/conversation" {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			// Unmatched /v1/* routes answer in the API error dialect: a
			// plain-text 404 makes SDKs raise unparseable-body errors.
			writeEndpointError(w, r, http.StatusNotFound, "invalid_request_error", "unknown API endpoint: "+r.URL.Path)
			return
		}
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	// Do not rely on the dashboard's JavaScript session check to reveal the
	// login form. If scripts fail, are delayed, or the large dashboard document
	// is still loading, an unauthenticated browser would otherwise appear stuck.
	// Route unauthenticated root requests directly to the dedicated login page.
	if r.URL.Path == "/" && !s.validAdminSession(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	// A valid session that still owes the default-password change (or a
	// revoked/expired one) is sent to the dedicated login page too: the
	// dashboard API calls would otherwise fail with 403 password_change_required
	// and the embedded login form is no longer part of the dashboard shell.
	if r.URL.Path == "/" {
		s.mu.Lock()
		mustChange := s.mustChangePassword
		s.mu.Unlock()
		if mustChange {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
	}

	name := "index.html"
	if r.URL.Path == "/login" {
		name = "login.html"
	} else if r.URL.Path == "/conversation" {
		name = "conversation.html"
	}
	f, err := webContent.Open(name)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "web interface unavailable")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "web interface unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, name, st.ModTime(), f)
}
