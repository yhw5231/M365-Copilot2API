package web

import (
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"net/http/httptest"
	"testing"
	"time"
)

// fullInputOf sums the diff-method full logical input for a request the way
// openaiChat computes it: the flattened prompt token estimate.
func fullInputOf(body *oaiReq) int64 {
	total := int64(0)
	for _, msg := range body.Messages {
		total += EstimateTokens(contentToString(msg.Content))
	}
	return total
}

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
	// Diff method: the reused upstream conversation held everything except the
	// current turn, so the call site passes that share as cached and the full
	// logical input alongside it. InputTokens = full - cached.
	full := fullInputOf(body)
	var wantCache int64
	for _, msg := range body.Messages[:len(body.Messages)-1] {
		wantCache += EstimateTokens(contentToString(msg.Content))
	}

	s.recordToolUsage(req, auth.AccountToken{Email: "test@example.com"}, body, res, time.Now().Add(-time.Second), wantCache, full)

	if len(s.usage.records) != 1 {
		t.Fatalf("usage record count=%d want 1", len(s.usage.records))
	}
	rec := s.usage.records[0]
	wantInput := full - wantCache
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
	// Diff-method invariant: input + cache always equals the full logical input.
	if rec.InputTokens+rec.CacheTokens != full {
		t.Fatalf("input+cache=%d want full=%d", rec.InputTokens+rec.CacheTokens, full)
	}
}

func TestRecordToolUsageSingleMessageHasNoCachedTokens(t *testing.T) {
	s := &Server{usage: &usageLog{persist: &persistStore{flush: func() error { return nil }}}}
	body := &oaiReq{Messages: []oaiMsg{{Role: "user", Content: "current request only"}}}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	s.recordToolUsage(req, auth.AccountToken{}, body, chathub.Result{Text: "result"}, time.Now(), 0, fullInputOf(body))

	rec := s.usage.records[0]
	if rec.InputTokens == 0 {
		t.Fatal("single current message should produce input tokens")
	}
	if rec.CacheTokens != 0 {
		t.Fatalf("single current message cache tokens=%d want 0", rec.CacheTokens)
	}
}

// TestRecordToolUsageWithoutSessionIDHasNoCachedTokens guards the accounting contract:
// recordToolUsage records exactly the cached count the call site computed for
// the client, never re-estimates it from headers. A caller with no reuse passes
// 0 and the full logical input lands in InputTokens.
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

	s.recordToolUsage(req, auth.AccountToken{Email: "test@example.com"}, body, res, time.Now().Add(-time.Second), 0, fullInputOf(body))

	rec := s.usage.records[0]
	if rec.CacheTokens != 0 {
		t.Fatalf("no reuse: cache tokens=%d want 0 (all messages are new input)", rec.CacheTokens)
	}
	if rec.InputTokens == 0 {
		t.Fatalf("no reuse: input tokens must include all messages, got 0")
	}
	if rec.InputTokens != fullInputOf(body) {
		t.Fatalf("no reuse: input tokens=%d want full=%d", rec.InputTokens, fullInputOf(body))
	}
}
