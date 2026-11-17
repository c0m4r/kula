package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
)

// TestUsageNamesEveryFlag: the hand-written usage lists every registered
// flag and no other, every category and -expect key, and every exit status.
func TestUsageNamesEveryFlag(t *testing.T) {
	var usage bytes.Buffer
	printUsage(&usage)
	text := usage.String()

	flags, _ := newFlagSet(io.Discard)
	registered := map[string]bool{}
	flags.VisitAll(func(f *flag.Flag) {
		registered[f.Name] = true
		if !regexp.MustCompile(`(?m)^  -` + regexp.QuoteMeta(f.Name) + `( |$)`).MatchString(text) {
			t.Errorf("the usage does not list -%s", f.Name)
		}
	})
	_, flagsSection, _ := strings.Cut(text, "\nFlags:\n")
	flagsSection, _, _ = strings.Cut(flagsSection, "\n\n")
	for _, m := range regexp.MustCompile(`(?m)^  -([a-zA-Z-]+)`).FindAllStringSubmatch(flagsSection, -1) {
		if !registered[m[1]] {
			t.Errorf("the usage lists -%s, which is not a flag", m[1])
		}
	}

	for _, words := range [][]string{categories(), expectKeyNames()} {
		if !strings.Contains(text, strings.Join(words, ",")) {
			t.Errorf("the usage does not list %s", strings.Join(words, ","))
		}
	}
	_, exits, _ := strings.Cut(text, "\nExit status:\n")
	for _, code := range []int{exitClean, exitFailing, exitUsage, exitInconclusive, exitInterrupted} {
		if !regexp.MustCompile(fmt.Sprintf(`(?m)^  %d +\S`, code)).MatchString(exits) {
			t.Errorf("the usage does not explain exit status %d", code)
		}
	}

	// -h prints it, and is no error.
	code, stdout, stderr := runCaptured(t, "-h")
	if code != exitClean || stdout != "" || stderr != text {
		t.Errorf("-h: exit %d, stdout %q, stderr is the usage: %v", code, stdout, stderr == text)
	}
}
