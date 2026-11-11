package tui

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"
)

// captureStandardLog points the standard logger at a buffer for one test.
func captureStandardLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previousOutput, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	})
	return &output
}

func TestLogLinesWaitForTheTerminalAndArriveCleaned(t *testing.T) {
	output := captureStandardLog(t)

	release := holdLog()
	log.Printf("Error reading mount stats for %s: busy", "/media/u/x\x1b]52;c;cm0gLXJmIH4K\x07")
	log.Printf("plain line")
	if output.Len() != 0 {
		t.Fatalf("a log line reached the terminal while the TUI held it: %q", output.String())
	}

	release()
	got := output.String()
	want := "Error reading mount stats for /media/u/x�]52;c;cm0gLXJmIH4K�: busy\nplain line\n"
	if got != want {
		t.Fatalf("held lines printed as %q, want %q", got, want)
	}
	if bad, found := terminalControl(got); found {
		t.Fatalf("held lines carry %q", bad)
	}

	log.Printf("after")
	if !strings.HasSuffix(output.String(), "after\n") {
		t.Fatal("the logger's output was not restored")
	}
}

func TestHeldLogKeepsTheNewestLines(t *testing.T) {
	output := captureStandardLog(t)

	release := holdLog()
	for index := range maxHeldLogLines + 50 {
		log.Printf("line %d", index)
	}
	release()

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != maxHeldLogLines+1 {
		t.Fatalf("printed %d lines, want %d held lines and a note", len(lines), maxHeldLogLines+1)
	}
	if lines[0] != "kula tui: 50 earlier log lines were dropped" {
		t.Fatalf("first line %q, want the dropped-lines note", lines[0])
	}
	if lines[1] != "line 50" || lines[len(lines)-1] != fmt.Sprintf("line %d", maxHeldLogLines+49) {
		t.Fatalf("kept %q … %q, want the newest lines", lines[1], lines[len(lines)-1])
	}
}
