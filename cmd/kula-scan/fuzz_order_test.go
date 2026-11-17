package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestLoginBudgetOrder: every probe that spends login attempts runs before
// RATE-LOGIN and BYPASS-XFF, which exhaust the limiter on purpose; nothing
// but those two and FUZZ-LIVENESS runs after the first of them (a check
// there that opened the scan's session, under -only, would find login
// throttled); and FUZZ-LIVENESS is the last check.
func TestLoginBudgetOrder(t *testing.T) {
	checks := allChecks()
	at := map[string]int{}
	for i, c := range checks {
		for _, id := range c.ids {
			at[id] = i
		}
	}
	lockout := min(at["RATE-LOGIN"], at["BYPASS-XFF"])
	// AUTH-005 opens the scan's session. CSRF-001 and CSRF-002 log in with
	// origin validation off, and so do FUZZ-SMUGGLE's requests.
	for _, id := range []string{"AUTH-005", "AUTH-006", "CSRF-001", "CSRF-002", "INPUT-AGG", "FUZZ-BODY", "FUZZ-SMUGGLE"} {
		if at[id] >= lockout {
			t.Errorf("%s runs at %d, not before the lockout probes at %d", id, at[id], lockout)
		}
	}
	for _, c := range checks[lockout:] {
		switch c.ids[0] {
		case "RATE-LOGIN", "BYPASS-XFF", "FUZZ-LIVENESS":
		default:
			t.Errorf("%v runs after the lockout probes", c.ids)
		}
	}
	if last := checks[len(checks)-1]; len(last.ids) != 1 || last.ids[0] != "FUZZ-LIVENESS" {
		t.Errorf("the last check is %v, want FUZZ-LIVENESS", last.ids)
	}
}

// TestAggressiveFuzzRunKeepsTheLoginBudget: a run with -aggressive and
// -fuzz against a Kula with its login limiters spends the attempts in
// budget order: FUZZ-BODY's six login bodies reach the parser, and
// RATE-LOGIN and BYPASS-XFF still pass. With origin validation off, CSRF-001,
// CSRF-002 and one of FUZZ-SMUGGLE's requests spend three more, and every
// judgement holds.
func TestAggressiveFuzzRunKeepsTheLoginBudget(t *testing.T) {
	sel, err := newSelection("auth,csrf,INPUT-AGG,fuzz,RATE-LOGIN,BYPASS-XFF", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		originCheck bool
		admitted    int // login attempts the address limiter admitted before RATE-LOGIN
	}{
		{true, 10},
		{false, 13},
	} {
		m := newKulaMock(true, false)
		m.noOriginCheck = !c.originCheck
		s, _ := m.serve(t, Options{Username: kmUser, Password: kmPass, Aggressive: true})
		findings := s.Run(context.Background(), sel)

		for _, id := range []string{"AUTH-005", "AUTH-006", "INPUT-AGG", "FUZZ-BODY", "RATE-LOGIN", "BYPASS-XFF", "FUZZ-LIVENESS"} {
			wantStatus(t, findings, id, StatusPass)
		}
		if f, _ := byID(findings, "FUZZ-BODY"); !strings.Contains(f.Detail, "login: 6 planned, 6 sent: 6 reached its body parser") {
			t.Errorf("origin check %v: FUZZ-BODY's login bodies did not all reach the parser: %s", c.originCheck, f.Detail)
		}
		// RATE-LOGIN's 8 and BYPASS-XFF's 12 come last; the limiter admits
		// 15 in all.
		m.mu.Lock()
		statuses := append([]int(nil), m.loginStatuses...)
		m.mu.Unlock()
		if len(statuses) != c.admitted+8+12 {
			t.Fatalf("origin check %v: %d login attempts, want %d before RATE-LOGIN and its 8 and BYPASS-XFF's 12: %v",
				c.originCheck, len(statuses), c.admitted, statuses)
		}
		admitted := 0
		for _, st := range statuses {
			if st != http.StatusTooManyRequests {
				admitted++
			}
		}
		if before := statuses[:c.admitted]; admitted != 15 || slices.Contains(before, http.StatusTooManyRequests) {
			t.Errorf("origin check %v: the limiter admitted %d attempts; the ones before RATE-LOGIN were answered %v", c.originCheck, admitted, before)
		}
	}
}

// TestFuzzBannerStatesTheLoginCost: the -fuzz warning says how many login
// attempts fuzzing spends.
func TestFuzzBannerStatesTheLoginCost(t *testing.T) {
	m := newKulaMock(false, false)
	_, ts := m.serve(t, Options{})
	_, _, stderr := runCaptured(t, "-fuzz", "-seed", "7", "-only", "discovery", ts.URL)
	want := "FUZZ MODE: sends malformed/extreme input across the surface (seed 7). " +
		"With auth on it spends up to 6 of this address's 15 login attempts in five minutes (7 with origin validation off)"
	if !strings.Contains(stderr, want) {
		t.Errorf("the -fuzz banner does not state the login cost:\n%s", stderr)
	}
}
