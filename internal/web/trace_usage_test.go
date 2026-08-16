package web

import (
	"encoding/json"
	"net/http/httptest"
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
