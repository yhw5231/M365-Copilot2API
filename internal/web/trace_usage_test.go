package web

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageLogsFilterByKeyAccountAndStatus(t *testing.T) {
	s := &usageLog{}
	base := time.Now()
	ok := UsageRecord{
		Time:           base,
		APIKeyPrefix:   "sk-abc",
		AccountEmail:   "alice@example.com",
		Model:          "gpt-5.6-sol",
		ReasoningLevel: "medium",
		Endpoint:       "/v1/chat/completions",
		Stream:         true,
		InputTokens:    10,
		OutputTokens:   20,
		DurationMs:     100,
		Status:         200,
	}
	s.records = append(s.records, ok, ok)
	s.records = append(s.records, UsageRecord{
		Time:         base.Add(-time.Hour),
		APIKeyPrefix: "sk-xyz",
		AccountEmail: "bob@example.com",
		Model:        "gpt-5.4",
		Endpoint:     "/v1/messages",
		InputTokens:  5,
		OutputTokens: 10,
		DurationMs:   50,
		Status:       502,
		Error:        "upstream boom",
	})

	got := s.logs(50, 0, usageLogFilter{Key: "abc"})
	if got["total"].(int) != 2 {
		t.Fatalf("key filter total=%d want 2", got["total"].(int))
	}
	got = s.logs(50, 0, usageLogFilter{Account: "bob"})
	if got["total"].(int) != 1 {
		t.Fatalf("account filter total=%d want 1", got["total"].(int))
	}
	got = s.logs(50, 0, usageLogFilter{Status: "error"})
	logs := got["logs"].([]UsageRecord)
	if got["total"].(int) != 1 || len(logs) != 1 || logs[0].Status != 502 {
		t.Fatalf("error filter logs=%d total=%d", len(logs), got["total"].(int))
	}
	got = s.logs(50, 0, usageLogFilter{Error: "boom"})
	if got["total"].(int) != 1 {
		t.Fatalf("error text filter total=%d want 1", got["total"].(int))
	}
	got = s.logs(50, 0, usageLogFilter{Q: "gpt-5.6"})
	if got["total"].(int) != 2 {
		t.Fatalf("free text filter total=%d want 2", got["total"].(int))
	}
	// Time-range filter
	got = s.logs(50, 0, usageLogFilter{From: base.Add(-30 * time.Minute), HasFrom: true})
	if got["total"].(int) != 2 {
		t.Fatalf("from filter total=%d want 2", got["total"].(int))
	}
	// Pagination: newest first, one page of 1.
	got = s.logs(1, 0, usageLogFilter{})
	if got["total"].(int) != 3 || len(got["logs"].([]UsageRecord)) != 1 {
		t.Fatalf("pagination total=%d page=%d want 3/1", got["total"].(int), len(got["logs"].([]UsageRecord)))
	}
	if latest := got["logs"].([]UsageRecord)[0]; latest.Model == "" {
		t.Fatal("unexpected record")
	}
}

func TestTraceStoreRingTrimsOldestToBound(t *testing.T) {
	st := openTraceStore()
	for i := 0; i < 120; i++ {
		id := "r" + string(rune('a'+i%26)) + string(rune('0'+i%10))
		st.byID[id] = &traceRecord{ID: id, At: time.Now().Add(time.Duration(i) * time.Minute)}
	}
	st.trimToLocked(50)
	if len(st.byID) != 50 {
		t.Fatalf("trim to bound kept %d records, want 50", len(st.byID))
	}
	// newest timestamps survive
	newest := st.list()
	if len(newest) != 50 || !newest[0].At.After(newest[len(newest)-1].At) {
		t.Fatalf("list not newest-first: n=%d", len(newest))
	}
}

func TestTraceFinishFlipsInProgressToTerminal(t *testing.T) {
	st := openTraceStore()
	rec := &traceRecord{ID: "x", At: time.Now(), Status: "in_progress"}
	st.byID[rec.ID] = rec
	st.finish("x", nil)
	if rec.Status != "success" {
		t.Fatalf("default finish status=%q want success", rec.Status)
	}

	rec2 := &traceRecord{ID: "y", At: time.Now(), Status: "in_progress"}
	st.byID[rec2.ID] = rec2
	st.finish("y", func(r *traceRecord) {
		r.Status = "error"
		r.StatusCode = 502
		r.Error = "boom"
	})
	if rec2.Status != "error" || rec2.Error != "boom" {
		t.Fatalf("error finish record=%+v", rec2)
	}
}

func TestTraceStoreClearDropsAll(t *testing.T) {
	st := openTraceStore()
	st.byID["a"] = &traceRecord{ID: "a", At: time.Now()}
	st.active["a"] = st.byID["a"]
	st.clear()
	if len(st.byID) != 0 || len(st.active) != 0 {
		t.Fatal("clear left records behind")
	}
}

func TestAdminTraceStatusPaginationAndIDFilter(t *testing.T) {
	st := openTraceStore()
	s := &Server{trace: st}
	for i := 0; i < 25; i++ {
		rec := &traceRecord{ID: "trace_" + string(rune('a'+i)), At: time.Now().Add(time.Duration(i) * time.Minute), Model: "m"}
		st.byID[rec.ID] = rec
	}

	// Default page: 10 records newest first, total 25.
	r := httptest.NewRequest("GET", "/api/admin/trace/status", nil)
	w := httptest.NewRecorder()
	s.adminTraceStatus(w, r)
	var page struct {
		Total   int           `json:"total"`
		Records []traceRecord `json:"records"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 25 || len(page.Records) != 10 {
		t.Fatalf("default page total=%d len=%d want 25/10", page.Total, len(page.Records))
	}

	// Second page via offset.
	r = httptest.NewRequest("GET", "/api/admin/trace/status?limit=10&offset=10", nil)
	w = httptest.NewRecorder()
	s.adminTraceStatus(w, r)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 25 || len(page.Records) != 10 {
		t.Fatalf("page2 total=%d len=%d want 25/10", page.Total, len(page.Records))
	}

	// ID filter returns exactly the matching record.
	target := st.list()[3]
	r = httptest.NewRequest("GET", "/api/admin/trace/status?id="+target.ID, nil)
	w = httptest.NewRecorder()
	s.adminTraceStatus(w, r)
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || len(page.Records) != 1 || page.Records[0].ID != target.ID {
		t.Fatalf("id filter total=%d len=%d rec=%s want 1/1/%s", page.Total, len(page.Records), page.Records[0].ID, target.ID)
	}
}

func TestUsageLogPersistsAndReloadsFromDataDir(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("M365_DATA_DIR", dataDir)
	t.Setenv("M365_USAGE_LOG", "")

	first := openUsageLog()
	wantPath := filepath.Join(dataDir, "usage.jsonl")
	if first.Path != wantPath {
		t.Fatalf("usage path=%q want %q", first.Path, wantPath)
	}

	first.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: "test-key",
		AccountEmail: "persist@example.com",
		Model:        "test-model",
		Endpoint:     "/v1/chat/completions",
		InputTokens:  11,
		OutputTokens: 7,
		Status:       200,
	})
	if err := first.persist.flushNowBlocking(); err != nil {
		t.Fatalf("flush usage log: %v", err)
	}

	reloaded := openUsageLog()
	if len(reloaded.records) != 1 {
		t.Fatalf("reloaded records=%d want 1", len(reloaded.records))
	}
	got := reloaded.records[0]
	if got.AccountEmail != "persist@example.com" || got.InputTokens != 11 || got.OutputTokens != 7 || got.Status != 200 {
		t.Fatalf("reloaded record=%+v", got)
	}
}

func TestUsageTokenSemanticsNoCachePartialAndFullCache(t *testing.T) {
	tests := []struct {
		name             string
		newInput         int64
		cachedInput      int64
		output           int64
		wantInputTotal   int64
		wantTotal        int64
		wantCachePercent float64
	}{
		{name: "no cache", newInput: 100, cachedInput: 0, output: 20, wantInputTotal: 100, wantTotal: 120, wantCachePercent: 0},
		{name: "partial cache", newInput: 40, cachedInput: 60, output: 20, wantInputTotal: 100, wantTotal: 120, wantCachePercent: 60},
		{name: "full cache", newInput: 0, cachedInput: 100, output: 20, wantInputTotal: 100, wantTotal: 120, wantCachePercent: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := UsageRecord{InputTokens: tt.newInput, CacheTokens: tt.cachedInput, OutputTokens: tt.output}
			rec.normalizeTokens()
			if rec.InputTotalTokens != tt.wantInputTotal || rec.TotalTokens != tt.wantTotal || rec.CacheInputPercent != tt.wantCachePercent {
				t.Fatalf("normalized tokens=%+v, want input_total=%d total=%d cache_percent=%v", rec, tt.wantInputTotal, tt.wantTotal, tt.wantCachePercent)
			}
		})
	}
}

func TestUsageSnapshotFieldsAndBreakdownsStayConsistent(t *testing.T) {
	now := time.Now()
	s := &usageLog{records: []UsageRecord{
		{Time: now.Add(-time.Hour), APIKeyPrefix: "key-a", Model: "model-a", Endpoint: "/v1/chat/completions", InputTokens: 40, CacheTokens: 60, OutputTokens: 20},
		{Time: now.Add(-2 * time.Hour), APIKeyPrefix: "key-a", Model: "model-a", Endpoint: "/v1/chat/completions", InputTokens: 10, OutputTokens: 5},
	}}
	stats := s.snapshot(7)
	summary := stats["summary"].(map[string]any)
	if summary["new_input_tokens"].(int64) != 50 || summary["input_total_tokens"].(int64) != 110 || summary["cache_tokens"].(int64) != 60 || summary["output_tokens"].(int64) != 25 || summary["total_tokens"].(int64) != 135 {
		t.Fatalf("unexpected summary token fields: %#v", summary)
	}
	if summary["tokens"].(int64) != summary["total_tokens"].(int64) {
		t.Fatalf("legacy tokens=%v differs from total_tokens=%v", summary["tokens"], summary["total_tokens"])
	}
	models := stats["models"].([]map[string]any)
	endpoints := stats["endpoints"].([]map[string]any)
	keys := stats["keys"].([]map[string]any)
	trend := stats["trend"].([]map[string]any)
	if len(models) != 1 || models[0]["tokens"].(int64) != 135 || len(endpoints) != 1 || endpoints[0]["tokens"].(int64) != 135 || len(keys) != 1 || keys[0]["tokens"].(int64) != 135 {
		t.Fatalf("breakdown totals inconsistent: models=%#v endpoints=%#v keys=%#v", models, endpoints, keys)
	}
	var trendTokens int64
	for _, point := range trend {
		trendTokens += point["tokens"].(int64)
	}
	if trendTokens != 135 {
		t.Fatalf("trend tokens=%d want 135", trendTokens)
	}
}

func TestUsageLogsFilteredPaginationNewestFirst(t *testing.T) {
	base := time.Now()
	s := &usageLog{}
	for i := 0; i < 8; i++ {
		key := "skip"
		if i%2 == 0 {
			key = "match"
		}
		s.records = append(s.records, UsageRecord{Time: base.Add(time.Duration(i) * time.Minute), APIKeyPrefix: key, InputTokens: int64(i + 1)})
	}
	got := s.logs(2, 1, usageLogFilter{Key: "match"})
	if got["total"].(int) != 4 {
		t.Fatalf("filtered total=%d want 4", got["total"].(int))
	}
	logs := got["logs"].([]UsageRecord)
	if len(logs) != 2 || logs[0].InputTokens != 5 || logs[1].InputTokens != 3 {
		t.Fatalf("page=%+v, want matching records 5 then 3", logs)
	}
	for _, rec := range logs {
		if rec.InputTotalTokens != rec.InputTokens+rec.CacheTokens || rec.TotalTokens != rec.InputTotalTokens+rec.OutputTokens {
			t.Fatalf("page returned inconsistent token fields: %+v", rec)
		}
	}
}

func TestCacheStatsExplicitTokenFields(t *testing.T) {
	s := &CacheStats{KeyStats: make(map[string]*KeyStat), persist: &persistStore{flush: func() error { return nil }}}
	s.RecordRequest("key", true, 40, 60, 1)
	got := s.GetStats()
	if got.TotalRequests != 1 || got.CacheHits != 1 || got.CacheMisses != 0 {
		t.Fatalf("request statistics=%+v", got)
	}
	if got.NewInputTokens != 40 || got.CachedInputTokens != 60 || got.InputTotalTokens != 100 || got.CacheInputPercent != 60 {
		t.Fatalf("explicit cache token statistics=%+v", got)
	}
	if got.TokensSent != got.NewInputTokens || got.TokensSaved != got.CachedInputTokens || got.SavingsPercent != got.CacheInputPercent {
		t.Fatalf("legacy and explicit cache fields differ: %+v", got)
	}
}
