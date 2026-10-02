package auth

import (
	"errors"
	"testing"
	"time"
)

// stubRefresh swaps the token-endpoint refresh for the duration of one test.
func stubRefresh(t *testing.T, fn func(refreshToken, clientID, endpoint string) (TokenSet, error)) {
	t.Helper()
	prev := refreshToken
	refreshToken = fn
	t.Cleanup(func() { refreshToken = prev })
}

func seedExpiredAccount(t *testing.T, store *Store, id string, clientID string) {
	t.Helper()
	if _, err := store.Upsert(TokenSet{
		HomeOID:      id,
		Email:        id + "@example.com",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ClientID:     clientID,
		ExpiresAt:    time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("upsert %s: %v", id, err)
	}
}

// TestTransientRefreshFailureDoesNotMarkExpired is the regression test for the
// "account went offline after a while" report: a network-class refresh failure
// used to be persisted as status=expired, so the admin UI showed a healthy
// account as offline until something happened to retry it.
func TestTransientRefreshFailureDoesNotMarkExpired(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedExpiredAccount(t, store, "oid-1", "")
	stubRefresh(t, func(string, string, string) (TokenSet, error) {
		return TokenSet{}, errors.New("Post \"https://login.microsoftonline.com/token\": dial tcp: i/o timeout")
	})

	if _, err := store.EnsureValid("oid-1"); err == nil {
		t.Fatal("expected the refresh to fail")
	}
	got, ok := store.Get("oid-1")
	if !ok {
		t.Fatal("account disappeared")
	}
	if got.Status != "online" {
		t.Fatalf("transient failure set status=%q, want online", got.Status)
	}
	// The on-disk record must agree: a restart must not resurrect an offline badge.
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	list := reopened.List()
	if len(list) != 1 || list[0].Status != "online" {
		t.Fatalf("persisted status = %+v, want online", list)
	}

	state, ok := store.RefreshState()["oid-1"]
	if !ok {
		t.Fatal("no refresh state recorded for the failed account")
	}
	if state.Permanent {
		t.Fatalf("transport failure marked permanent: %+v", state)
	}
	if state.Failures != 1 || state.NextAttempt.IsZero() || state.LastError == "" {
		t.Fatalf("unexpected refresh state: %+v", state)
	}
	// The timer path must respect the backoff instead of hammering the endpoint.
	if results := store.RefreshDue(time.Minute); len(results) != 0 {
		t.Fatalf("RefreshDue ignored the failure backoff: %+v", results)
	}
}

// TestPermanentRefreshFailureMarksExpired pins the other half of the
// classification: a revoked grant still has to surface as offline so the
// operator knows a fresh authorization is required.
func TestPermanentRefreshFailureMarksExpired(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedExpiredAccount(t, store, "oid-1", "")
	stubRefresh(t, func(string, string, string) (TokenSet, error) {
		return TokenSet{}, &OAuthError{Stage: "Refresh", Code: "invalid_grant", AADSTS: "AADSTS700082", HTTPStatus: 400}
	})

	if _, err := store.EnsureValid("oid-1"); err == nil {
		t.Fatal("expected the refresh to fail")
	}
	got, _ := store.Get("oid-1")
	if got.Status != "expired" {
		t.Fatalf("status=%q want expired", got.Status)
	}
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if list := reopened.List(); list[0].Status != "expired" {
		t.Fatalf("persisted status=%q want expired", list[0].Status)
	}

	state := store.RefreshState()["oid-1"]
	if !state.Permanent {
		t.Fatalf("invalid_grant must be permanent: %+v", state)
	}
	if results := store.RefreshDue(time.Hour); len(results) != 0 {
		t.Fatalf("permanently failed account was retried: %+v", results)
	}
}

// TestRefreshDueRenewsExpiringAccountAndKeepsClientID covers the timer path:
// an account close to expiry is renewed, its client id survives the rotation
// (a device-code account must not be rewritten to the browser PKCE client),
// and the failure state is cleared.
func TestRefreshDueRenewsExpiringAccountAndKeepsClientID(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(TokenSet{
		HomeOID:      "oid-1",
		Email:        "one@example.com",
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		ClientID:     "device-client-1",
		ExpiresAt:    time.Now().Add(30 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	// A first transient failure must not block the account forever: its
	// backoff window is short and the next successful refresh clears it.
	calls := 0
	stubRefresh(t, func(refreshToken, clientID, endpoint string) (TokenSet, error) {
		calls++
		if calls == 1 {
			return TokenSet{}, errors.New("temporary network failure")
		}
		if refreshToken != "old-refresh" {
			t.Fatalf("refresh token=%q want old-refresh", refreshToken)
		}
		if clientID != "device-client-1" {
			t.Fatalf("client id=%q want device-client-1", clientID)
		}
		return TokenSet{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})

	if _, err := store.EnsureValid("oid-1"); err == nil {
		t.Fatal("expected the first refresh to fail")
	}
	// Clear the backoff window to simulate the retry becoming due.
	store.mu.Lock()
	store.refreshFailures["oid-1"].NextAttempt = time.Now().Add(-time.Second)
	store.mu.Unlock()

	results := store.RefreshDue(time.Minute)
	if len(results) != 1 || !results[0].Success {
		t.Fatalf("RefreshDue results=%+v want one success", results)
	}
	got, _ := store.Get("oid-1")
	if got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("token was not rotated: %+v", got)
	}
	if got.Status != "online" {
		t.Fatalf("status=%q want online", got.Status)
	}
	if got.ClientID != "device-client-1" {
		t.Fatalf("client id was rewritten to %q", got.ClientID)
	}
	if _, ok := store.RefreshState()["oid-1"]; ok {
		t.Fatal("successful refresh did not clear the failure state")
	}

	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if list := reopened.List(); list[0].ClientID != "device-client-1" {
		t.Fatalf("persisted client id=%q want device-client-1", list[0].ClientID)
	}
}

// TestRefreshDueRespectsLead verifies the timer only touches accounts that are
// already expired or about to expire.
func TestRefreshDueRespectsLead(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id      string
		expires time.Duration
	}{
		{"far", time.Hour},
		{"near", 2 * time.Minute},
		{"expired", -time.Minute},
	} {
		if _, err := store.Upsert(TokenSet{
			HomeOID:      tc.id,
			Email:        tc.id + "@example.com",
			AccessToken:  "a",
			RefreshToken: "r",
			ExpiresAt:    time.Now().Add(tc.expires),
		}); err != nil {
			t.Fatal(err)
		}
	}
	stubRefresh(t, func(_, _, _ string) (TokenSet, error) {
		return TokenSet{AccessToken: "new", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})

	results := store.RefreshDue(5 * time.Minute)
	if len(results) != 2 {
		t.Fatalf("results=%+v want 2 renewals (near + expired)", results)
	}
	// No account is in a failure state, so nothing may be skipped silently.
	for _, r := range results {
		if !r.Success {
			t.Fatalf("unexpected failure: %+v", r)
		}
	}
}

// TestUpsertPreservesClientIDAcrossRotation guards the device-code regression:
// a rotated token set carries no client id of its own, and the store must not
// fall back to the browser PKCE client.
func TestUpsertPreservesClientIDAcrossRotation(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(TokenSet{
		HomeOID: "oid-1", Email: "one@example.com", AccessToken: "a", RefreshToken: "r",
		ClientID: "device-client-1", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	rotated, err := store.Upsert(TokenSet{
		HomeOID: "oid-1", Email: "one@example.com", AccessToken: "a2", RefreshToken: "r2",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ClientID != "device-client-1" {
		t.Fatalf("client id=%q want device-client-1", rotated.ClientID)
	}
	// An explicit client id (a fresh PKCE authorization) must still win.
	reauth, err := store.Upsert(TokenSet{
		HomeOID: "oid-1", Email: "one@example.com", AccessToken: "a3", RefreshToken: "r3",
		ClientID: "browser-client-2", ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if reauth.ClientID != "browser-client-2" {
		t.Fatalf("client id=%q want browser-client-2", reauth.ClientID)
	}
}
