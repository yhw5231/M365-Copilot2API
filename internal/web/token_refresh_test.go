package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"m365-copilot2api/internal/auth"
)

// refreshTestStore seeds one expired account per id, all pointing at the same
// (test) token endpoint.
func refreshTestStore(t *testing.T, ids ...string) *auth.Store {
	t.Helper()
	store, err := auth.OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, id := range ids {
		if _, err := store.Upsert(auth.TokenSet{
			HomeOID:      id,
			Email:        id + "@example.com",
			AccessToken:  "expired-access",
			RefreshToken: id + "-refresh",
			ExpiresAt:    time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}
	return store
}

// tokenEndpointFor returns a token endpoint that revokes "revoked-refresh" and
// fails every other grant with a retryable 503.
func tokenEndpointFor(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PostFormValue("refresh_token") == "revoked-refresh" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "invalid_grant",
				"error_description": "AADSTS700082: The refresh token has expired due to inactivity.",
			})
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestRefreshTokensOnceClassifiesFailures is the loop-level counterpart of the
// auth-package tests: a revoked grant is reported as needing re-authorization,
// a 503 is reported as a transient retry, and only the former turns the account
// offline.
func TestRefreshTokensOnceClassifiesFailures(t *testing.T) {
	server := tokenEndpointFor(t)
	t.Setenv("M365_TOKEN_ENDPOINT", server.URL)
	store := refreshTestStore(t, "revoked", "flaky")
	s := &Server{tokens: store}

	results := s.refreshTokensOnce(time.Minute)
	if len(results) != 2 {
		t.Fatalf("results=%+v want 2 attempts", results)
	}
	byID := map[string]auth.TokenRefreshResult{}
	for _, r := range results {
		byID[r.ID] = r
		if r.Success {
			t.Fatalf("unexpected success: %+v", r)
		}
	}
	if !byID["revoked"].Permanent {
		t.Fatalf("revoked grant not reported as permanent: %+v", byID["revoked"])
	}
	if byID["flaky"].Permanent {
		t.Fatalf("503 reported as permanent: %+v", byID["flaky"])
	}

	revoked, _ := store.Get("revoked")
	if revoked.Status != "expired" {
		t.Fatalf("revoked account status=%q want expired", revoked.Status)
	}
	flaky, _ := store.Get("flaky")
	if flaky.Status != "online" {
		t.Fatalf("transient failure set status=%q, want online", flaky.Status)
	}

	// Both accounts are now inside a backoff window, so the next tick must not
	// hit the token endpoint again.
	if again := s.refreshTokensOnce(time.Minute); len(again) != 0 {
		t.Fatalf("backoff ignored on the second sweep: %+v", again)
	}
}

// TestTokenRefreshLoopRenewsIdleAccount drives the real background loop: an
// account that nothing ever uses must still be renewed by the timer.
func TestTokenRefreshLoopRenewsIdleAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "renewed-access",
			"refresh_token": r.PostFormValue("refresh_token"),
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv("M365_TOKEN_ENDPOINT", server.URL)
	t.Setenv("M365_TOKEN_REFRESH_INTERVAL_SECONDS", "1")
	t.Setenv("M365_TOKEN_REFRESH_LEAD_SECONDS", "600")

	store := refreshTestStore(t, "idle")
	s := &Server{tokens: store}
	s.StartTokenRefreshLoop()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if acc, ok := store.Get("idle"); ok && acc.AccessToken == "renewed-access" {
			if acc.Status != "online" {
				t.Fatalf("renewed account status=%q want online", acc.Status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("background loop never renewed the idle account")
}

// TestStartTokenRefreshLoopDisabled verifies the off switch actually keeps the
// loop from running.
func TestStartTokenRefreshLoopDisabled(t *testing.T) {
	t.Setenv("M365_TOKEN_REFRESH", "0")
	t.Setenv("M365_TOKEN_REFRESH_INTERVAL_SECONDS", "1")
	server := tokenEndpointFor(t)
	t.Setenv("M365_TOKEN_ENDPOINT", server.URL)
	store := refreshTestStore(t, "idle")
	s := &Server{tokens: store}
	s.StartTokenRefreshLoop()

	time.Sleep(1500 * time.Millisecond)
	if acc, _ := store.Get("idle"); acc.AccessToken != "expired-access" {
		t.Fatalf("disabled loop still refreshed the account: %+v", acc)
	}
}
