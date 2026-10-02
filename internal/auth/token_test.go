package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsPermanentRefreshError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transport", fmt.Errorf("Post %q: dial tcp 127.0.0.1:443: connect: connection refused", "https://login.microsoftonline.com/token"), false},
		{"invalid_grant", &OAuthError{Stage: "Refresh", Code: "invalid_grant", AADSTS: "AADSTS700082", HTTPStatus: 400}, true},
		{"invalid_client", &OAuthError{Code: "invalid_client", HTTPStatus: 401}, true},
		{"unauthorized_client", &OAuthError{Code: "unauthorized_client", HTTPStatus: 400}, true},
		{"interaction_required", &OAuthError{Code: "interaction_required", HTTPStatus: 400}, true},
		{"login_required", &OAuthError{Code: "login_required", HTTPStatus: 400}, true},
		{"consent_required", &OAuthError{Code: "consent_required", HTTPStatus: 400}, true},
		{"temporarily_unavailable", &OAuthError{Code: "temporarily_unavailable", HTTPStatus: 400}, false},
		{"slow_down", &OAuthError{Code: "slow_down", HTTPStatus: 400}, false},
		{"server_error_500", &OAuthError{Code: "server_error", HTTPStatus: 500}, false},
		{"unknown_5xx", &OAuthError{Code: "weird", HTTPStatus: 503}, false},
		{"throttled", &OAuthError{Code: "weird", HTTPStatus: http.StatusTooManyRequests}, false},
		{"unknown_4xx_code", &OAuthError{Code: "some_new_code", HTTPStatus: 400}, true},
		{"http_400_without_code", &OAuthError{HTTPStatus: 400}, false},
		{"wrapped_permanent", fmt.Errorf("refresh: %w", &OAuthError{Code: "invalid_grant", HTTPStatus: 400}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPermanentRefreshError(tc.err); got != tc.want {
				t.Fatalf("IsPermanentRefreshError(%v) = %v want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRefreshReturnsTypedOAuthError pins the classification contract to the
// actual token endpoint path: the refresh grant must yield an *OAuthError so a
// revoked grant can be told apart from a network failure. Before this, the
// refresh path returned a plain formatted error and every failure — including
// a proxy reset — looked permanent to the caller.
func TestRefreshReturnsTypedOAuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_grant",
			"error_description": "AADSTS700082: The refresh token has expired due to inactivity.",
		})
	}))
	defer server.Close()

	_, err := Refresh("stale-refresh-token", "client-1", server.URL)
	if err == nil {
		t.Fatal("expected refresh to fail")
	}
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		t.Fatalf("refresh error is not an *OAuthError: %T %v", err, err)
	}
	if oauthErr.Code != "invalid_grant" || oauthErr.AADSTS != "AADSTS700082" {
		t.Fatalf("unexpected oauth error: %+v", oauthErr)
	}
	if oauthErr.Stage != "Refresh" {
		t.Fatalf("stage=%q want Refresh", oauthErr.Stage)
	}
	if !IsPermanentRefreshError(err) {
		t.Fatal("invalid_grant must classify as permanent")
	}
}

// TestRefreshServerErrorIsTransient verifies that a 5xx from the token
// endpoint never marks the account as needing re-authorization.
func TestRefreshServerErrorIsTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"server_error"}`))
	}))
	defer server.Close()

	_, err := Refresh("refresh-token", "client-1", server.URL)
	if err == nil {
		t.Fatal("expected refresh to fail")
	}
	if IsPermanentRefreshError(err) {
		t.Fatalf("5xx must be transient, got %v", err)
	}
}
