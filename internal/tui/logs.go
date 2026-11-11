package tui

import (
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
)

// maxHeldLogLines bounds the log lines kept while the TUI runs; a collector
// that fails on every sample would otherwise grow the buffer for as long as
// the session lasts. The newest lines are kept.
const maxHeldLogLines = 200

// heldLog collects the standard logger's output while the TUI owns the
// terminal. Collectors report failures through log, which writes to stderr:
// the same terminal, where a line would draw over the frame and could carry
// an escape sequence taken from host data such as a mount point.
type heldLog struct {
	mu      sync.Mutex
	lines   []string
	dropped int
}

func (h *heldLog) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) == maxHeldLogLines {
		copy(h.lines, h.lines[1:])
		h.lines = h.lines[:len(h.lines)-1]
		h.dropped++
	}
	h.lines = append(h.lines, string(p))
	return len(p), nil
}

// writeTo prints the held lines, each cleaned to one line of plain text.
func (h *heldLog) writeTo(w io.Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dropped > 0 {
		_, _ = fmt.Fprintf(w, "kula tui: %d earlier log lines were dropped\n", h.dropped)
	}
	for _, line := range h.lines {
		_, _ = fmt.Fprintln(w, cleanText(strings.TrimSuffix(line, "\n")))
	}
}

// holdLog points the standard logger at a heldLog. The returned function
// restores the previous output, once the TUI has given the terminal back,
// and prints what was held there.
func holdLog() (release func()) {
	previous := log.Writer()
	held := &heldLog{}
	log.SetOutput(held)
	return func() {
		log.SetOutput(previous)
		held.writeTo(previous)
	}
}
