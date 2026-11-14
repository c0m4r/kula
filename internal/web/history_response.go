package web

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"kula/internal/storage"
)

// maxConcurrentHistory bounds the /api/history queries answered at once. Each
// holds its whole result in memory while it is read, reduced and sent: up to
// 5000 buckets of full Data, Min and Max samples, around 100 MB for a long
// window on a large host. Further requests wait for a slot.
const maxConcurrentHistory = 2

// historySlotWait is how long a history request waits for a slot before it is
// answered 503. A dashboard has one history request in flight at a time. A
// variable for the tests.
var historySlotWait = 10 * time.Second

// historyStreamChunk is how much encoded JSON writeSamplesJSON gathers before
// sending it. A variable for the tests.
var historyStreamChunk = 1 << 20

// acquireHistorySlot waits for one of the maxConcurrentHistory slots, until
// historySlotWait passes or the client goes away. The caller releases a slot
// it got with releaseHistorySlot. A Server not made by NewServer has no slots
// and no limit.
func (s *Server) acquireHistorySlot(ctx context.Context) bool {
	if s.historySlots == nil {
		return true
	}
	select {
	case s.historySlots <- struct{}{}:
		return true
	default:
	}
	timer := time.NewTimer(historySlotWait)
	defer timer.Stop()
	select {
	case s.historySlots <- struct{}{}:
		return true
	case <-ctx.Done():
	case <-timer.C:
	}
	return false
}

func (s *Server) releaseHistorySlot() {
	if s.historySlots != nil {
		<-s.historySlots
	}
}

// writeHistoryJSON answers with a history payload, either a whole
// storage.HistoryResult or a sectionedHistoryResult, as writeJSON would, but
// without building the whole body in memory first (see writeSamplesJSON).
func writeHistoryJSON(w http.ResponseWriter, payload any) {
	switch p := payload.(type) {
	case *storage.HistoryResult:
		// The store answers an empty window with an empty list; a nil one
		// encodes as writeJSON would encode it, as null.
		if p.Samples == nil {
			writeJSON(w, payload)
			return
		}
		envelope := *p
		envelope.Samples = []*storage.AggregatedSample{}
		writeSamplesJSON(w, &envelope, p.Samples)
	case *sectionedHistoryResult:
		envelope := *p
		envelope.Samples = []sectionedHistorySample{}
		writeSamplesJSON(w, &envelope, p.Samples)
	default:
		writeJSON(w, payload)
	}
}

// writeSamplesJSON writes envelope, whose first JSON field is "samples" and
// holds an empty list, with samples streamed into that list one at a time and
// sent historyStreamChunk bytes at a time: a long window at full width is
// tens of megabytes of JSON, more than its samples take decoded. The bytes are
// the ones writeJSON writes for the whole payload. A value encoding/json
// refuses is answered with a JSON 500 while nothing has been sent yet; after
// that the response can only be cut short, and the failure is logged.
func writeSamplesJSON[S any](w http.ResponseWriter, envelope any, samples []S) {
	const open = `{"samples":[]`
	tail, err := json.Marshal(envelope)
	if err != nil || !bytes.HasPrefix(tail, []byte(open)) {
		if err != nil {
			log.Printf("JSON encode error: %v", err)
		} else {
			log.Printf("JSON encode error: history envelope does not start with its samples")
		}
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	tail = tail[len(open):]

	w.Header().Set("Content-Type", "application/json")
	var buf bytes.Buffer
	sent := false
	flush := func() bool {
		sent = true
		// Writes encoding/json output as application/json (not text/html); the XSS rule doesn't apply.
		// nosemgrep: no-direct-write-to-responsewriter
		_, err := w.Write(buf.Bytes())
		buf.Reset()
		return err == nil
	}
	buf.WriteString(`{"samples":[`)
	for i := range samples {
		encoded, err := json.Marshal(samples[i])
		if err != nil {
			log.Printf("JSON encode error: %v", err)
			if !sent {
				jsonError(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(encoded)
		if buf.Len() >= historyStreamChunk && !flush() {
			return
		}
	}
	buf.WriteByte(']')
	buf.Write(tail)
	buf.WriteByte('\n')
	flush()
}
