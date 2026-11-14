package web

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kula/internal/collector"
	"kula/internal/config"
	"kula/internal/storage"
)

// writeHistoryJSON streams the samples but must send the bytes writeJSON
// sends for the whole payload, in one chunk or many.
func TestWriteHistoryJSONMatchesWriteJSON(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	result := &storage.HistoryResult{
		Samples: []*storage.AggregatedSample{
			{Timestamp: ts, Data: &collector.Sample{Timestamp: ts, System: collector.SystemStats{Hostname: `<b>"x"&</b>`}}},
			nil,
			{Timestamp: ts.Add(time.Second), Data: &collector.Sample{Timestamp: ts.Add(time.Second)}},
		},
		Tier: 1, Resolution: "1s", RequestedFrom: ts, RequestedTo: ts.Add(time.Minute),
		ValidAggregations: []string{"data"},
	}
	sections, ordered, err := parseHistorySections("sys,cpu")
	if err != nil {
		t.Fatal(err)
	}
	payloads := map[string]any{
		"whole":     result,
		"sectioned": selectHistorySections(result, sections, ordered),
		"empty":     &storage.HistoryResult{ValidAggregations: []string{"data"}},
	}
	defer func(chunk int) { historyStreamChunk = chunk }(historyStreamChunk)
	for name, payload := range payloads {
		want := httptest.NewRecorder()
		writeJSON(want, payload)
		for _, chunk := range []int{1, 1 << 20} {
			historyStreamChunk = chunk
			got := httptest.NewRecorder()
			writeHistoryJSON(got, payload)
			if got.Code != http.StatusOK || got.Body.String() != want.Body.String() {
				t.Errorf("%s, chunk %d: got %d %q\nwant %q", name, chunk, got.Code, got.Body.String(), want.Body.String())
			}
			if ct := got.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("%s: Content-Type %q", name, ct)
			}
		}
	}

	// A refused value is still a JSON 500 while nothing has been sent.
	historyStreamChunk = 1 << 20
	bad := &storage.HistoryResult{Samples: []*storage.AggregatedSample{{Coverage: math.NaN()}}}
	rec := httptest.NewRecorder()
	writeHistoryJSON(rec, bad)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("NaN sample: status %d, want 500", rec.Code)
	}
}

// A window the store holds nothing of answers "samples":[], whole or by
// section, not null.
func TestHandleHistoryAnswersAnEmptyWindowWithAnEmptyList(t *testing.T) {
	store := newTestStoreWithSample(t)
	defer func() { _ = store.Close() }()
	s := newTestServer(t, store)
	for _, query := range []string{
		"from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z",
		"from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z&sections=cpu",
	} {
		rec := httptest.NewRecorder()
		s.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history?"+query, nil))
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), `{"samples":[],`) {
			t.Errorf("%s: %d %.120q, want 200 starting {\"samples\":[],", query, rec.Code, rec.Body.String())
		}
	}
}

// Only maxConcurrentHistory queries run at once; a request that finds every
// slot taken waits, then is answered 503, and a released slot serves again.
func TestHistorySlotsBoundConcurrentQueries(t *testing.T) {
	s := NewServer(config.WebConfig{}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})
	defer func(wait time.Duration) { historySlotWait = wait }(historySlotWait)
	historySlotWait = 50 * time.Millisecond
	for range maxConcurrentHistory {
		if !s.acquireHistorySlot(context.Background()) {
			t.Fatal("a free slot was refused")
		}
	}
	start := time.Now()
	if s.acquireHistorySlot(context.Background()) {
		t.Fatal("a slot was granted beyond maxConcurrentHistory")
	}
	if waited := time.Since(start); waited < historySlotWait {
		t.Errorf("refused after %v, before waiting %v", waited, historySlotWait)
	}
	rec := httptest.NewRecorder()
	s.handleHistory(rec, httptest.NewRequest(http.MethodGet, "/api/history", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("busy history: status %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	historySlotWait = time.Hour
	if s.acquireHistorySlot(ctx) {
		t.Error("a slot was granted to a request whose client went away")
	}
	s.releaseHistorySlot()
	if !s.acquireHistorySlot(context.Background()) {
		t.Error("a released slot was not granted again")
	}
}
