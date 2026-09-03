package web

import (
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRecordToolUsageRecordsInputAndCachedTokens(t *testing.T) {
	s := &Server{usage: &usageLog{persist: &persistStore{flush: func() error { return nil }}}}
	body := &oaiReq{
		Model: "gpt-5.6-sol",
		Messages: []oaiMsg{
			{Role: "system", Content: "persistent system instructions"},
			{Role: "user", Content: "previous user message"},
			{Role: "assistant", Content: "previous assistant response"},
			{Role: "user", Content: "current request"},
		},
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-tool-usage-test")
	res := chathub.Result{Text: "tool routing result"}
	// The call site passes the same cached count it reported to the client
	// (history restored from a reused upstream conversation).
	var wantCache int64
	for _, msg := range body.Messages[:len(body.Messages)-1] {
		wantCache += EstimateTokens(contentToString(msg.Content))
	}

	s.recordToolUsage(req, auth.AccountToken{Email: "test@example.com"}, body, res, time.Now().Add(-time.Second), wantCache)

	if len(s.usage.records) != 1 {
		t.Fatalf("usage record count=%d want 1", len(s.usage.records))
	}
	rec := s.usage.records[0]
	wantInput := EstimateTokens(contentToString(body.Messages[len(body.Messages)-1].Content))
	if rec.InputTokens != wantInput {
		t.Fatalf("input tokens=%d want %d", rec.InputTokens, wantInput)
	}
	if rec.CacheTokens != wantCache {
		t.Fatalf("cache tokens=%d want %d", rec.CacheTokens, wantCache)
	}
	if rec.InputTokens == 0 || rec.CacheTokens == 0 {
		t.Fatalf("tool usage tokens must be non-zero: input=%d cache=%d", rec.InputTokens, rec.CacheTokens)
	}
	if rec.OutputTokens != EstimateTokens(res.Text) {
		t.Fatalf("output tokens=%d want %d", rec.OutputTokens, EstimateTokens(res.Text))
	}
}

func TestRecordToolUsageSingleMessageHasNoCachedTokens(t *testing.T) {
	s := &Server{usage: &usageLog{persist: &persistStore{flush: func() error { return nil }}}}
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "current request only"}}}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	s.recordToolUsage(req, auth.AccountToken{}, body, chathub.Result{Text: "result"}, time.Now(), 0)

	rec := s.usage.records[0]
	if rec.InputTokens == 0 {
		t.Fatal("single current message should produce input tokens")
	}
	if rec.CacheTokens != 0 {
		t.Fatalf("single current message cache tokens=%d want 0", rec.CacheTokens)
	}
}

// TestRecordToolUsageReportsPassedCachedValue guards the accounting contract:
// recordToolUsage records exactly the cached count the call site computed for
// the client, never re-estimates it from headers. A caller that knows the
// upstream conversation was reused passes its cached total and input tokens are
// the residual; a caller with no reuse passes 0 and all messages count as input.
func TestRecordToolUsageWithoutSessionIDHasNoCachedTokens(t *testing.T) {
	s := &Server{usage: &usageLog{persist: &persistStore{flush: func() error { return nil }}}}
	body := &oaiReq{
		Model: "gpt-5.6-sol",
		Messages: []oaiMsg{
			{Role: "system", Content: "persistent system instructions"},
			{Role: "user", Content: "previous user message"},
			{Role: "assistant", Content: "previous assistant response"},
			{Role: "user", Content: "current request"},
		},
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-tool-usage-test")
	res := chathub.Result{Text: "tool routing result"}

	s.recordToolUsage(req, auth.AccountToken{Email: "test@example.com"}, body, res, time.Now().Add(-time.Second), 0)

	rec := s.usage.records[0]
	if rec.CacheTokens != 0 {
		t.Fatalf("no reuse: cache tokens=%d want 0 (all messages are new input)", rec.CacheTokens)
	}
	if rec.InputTokens == 0 {
		t.Fatalf("no reuse: input tokens must include all messages, got 0")
	}
}
