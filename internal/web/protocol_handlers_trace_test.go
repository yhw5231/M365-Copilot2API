package web

import "testing"

// A /v1/responses streaming request shares one trace record with its inner
// chat request. While the request is in flight, the inner chat handler stores
// the resolved reasoning level and routeUpstreamTrace stores the upstream
// first-token latency. The completion update must preserve both instead of
// blanking the level with the raw (usually empty) client effort and replacing
// the TTFT with the adapter-side measurement, which lands near the total
// duration because the reasoning gate releases answer text only after
// thinking completes.
func TestResponsesStreamTraceUpdatePreservesResolvedLevelAndUpstreamTTFT(t *testing.T) {
	rec := &traceRecord{
		ID:             "t1",
		Status:         "in_progress",
		ReasoningLevel: "high",
		TTFTMs:         2345,
	}
	got := applyResponsesStreamTraceUpdate(rec, "gpt-5.6-luna", "high", 100, 50, 20, 44386, 45000, 200, "")
	if got != 2345 {
		t.Fatalf("effective ttft=%d want upstream 2345 for the usage row", got)
	}
	if rec.ReasoningLevel != "high" {
		t.Fatalf("reasoning level=%q want preserved resolved %q", rec.ReasoningLevel, "high")
	}
	if rec.TTFTMs != 2345 {
		t.Fatalf("ttft=%d want preserved upstream 2345", rec.TTFTMs)
	}
	if rec.Model != "gpt-5.6-luna" || rec.InputTokens != 100 || rec.OutputTokens != 50 || rec.CachedTokens != 20 {
		t.Fatalf("record fields not synced: %+v", rec)
	}
}

// An "auto" effort must not leak into the record either: it resolves to the
// model route's default before the update is applied.
func TestResponsesStreamTraceUpdateResolvesAutoEffort(t *testing.T) {
	rec := &traceRecord{ID: "t2", Status: "in_progress"}
	applyResponsesStreamTraceUpdate(rec, "gpt-5.6-luna", "medium", 10, 5, 0, 0, 1000, 200, "")
	if rec.ReasoningLevel != "medium" {
		t.Fatalf("reasoning level=%q want route default %q", rec.ReasoningLevel, "medium")
	}
}

// Without an upstream first-delta frame (e.g. an upstream failure before any
// text), the adapter-side measurement is the only TTFT signal and fills the gap.
func TestResponsesStreamTraceUpdateFallsBackToAdapterTTFT(t *testing.T) {
	rec := &traceRecord{ID: "t3", Status: "in_progress"}
	got := applyResponsesStreamTraceUpdate(rec, "gpt-5.6-luna", "high", 10, 5, 0, 1200, 2000, 502, "upstream boom")
	if got != 1200 {
		t.Fatalf("effective ttft=%d want adapter fallback 1200", got)
	}
	if rec.TTFTMs != 1200 {
		t.Fatalf("ttft=%d want adapter fallback 1200", rec.TTFTMs)
	}
	if rec.Status != "error" || rec.Error != "upstream boom" || rec.StatusCode != 502 {
		t.Fatalf("error reconciliation=%+v", rec)
	}
}
