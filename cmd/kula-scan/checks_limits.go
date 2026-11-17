package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Limits Kula puts on the AI assistant and on history queries, probed past
// them. All are aggressive: the chat probes spend the scanner's address's
// share of the chat limiter, and the history burst loads the store.
//
// No chat probe starts a chat. handleOllamaChat checks the method, that the
// assistant is enabled and the rate limit, then decodes the body, and only a
// body that decodes takes a chat slot and reaches the backend. Every body
// sent here is JSON that never decodes: an object left open ({"kula_scan":),
// or one closed by the wrong bracket after an over-long string. A body such
// as null or {} decodes, and would start a real model call.

func limitChecks() []check {
	return []check{
		{ids: []string{"INPUT-CHAT"}, run: runChatBodyCapCheck},
		{ids: []string{"DOS-HISTORY"}, run: runHistoryBurstCheck},
	}
}

// chatRateChecks is the chat rate-limit probe. It spends what is left of the
// minute's chat budget, so it runs after every other probe that sends a chat
// request (INPUT-CHAT, FUZZ-BODY), and before FUZZ-LIVENESS.
func chatRateChecks() []check {
	return []check{{ids: []string{"RATE-CHAT"}, run: runChatRateCheck}}
}

const (
	chatPath      = "/api/ollama/chat"
	chatRateLimit = 10       // Kula's ollamaChatRateLimit: chat requests a minute per address
	chatMaxBody   = 32 << 10 // Kula's ollamaMaxBody
	// chatBadBody is a chat body that never decodes: the object is left open.
	chatBadBody = `{"kula_scan":`
	// chatBigBody is how far INPUT-CHAT's body runs past the cap: further
	// than the 256 KiB Go's server reads of a body its handler left unread
	// before it answers. Kula's middleware wraps the ResponseWriter, so
	// MaxBytesReader cannot tell the server to close the connection instead;
	// with less left unread, the server would wait for the whole body before
	// it answered, cap or no cap.
	chatBigBody = chatMaxBody + 288<<10
)

// chatAccess returns the headers a chat request needs to reach the handler
// (a same-origin Origin, and with auth the session cookie and its CSRF
// token), or the status and detail of the finding that says why it cannot.
func (s *Scanner) chatAccess() (map[string]string, Status, string) {
	hdr := map[string]string{"Content-Type": "application/json", "Origin": s.base.Scheme + "://" + s.base.Host}
	if !s.authEnabled {
		if !s.ollama {
			return nil, StatusSkip, "the AI assistant is not enabled (/api/config says ollama_enabled is false)."
		}
		return hdr, StatusPass, ""
	}
	if !s.ensureSession() {
		return nil, StatusSkip, "auth is enabled and no session is available to reach the chat: " + s.noSessionReason() + "."
	}
	// Without a session discovery could not read whether the assistant is on.
	r := s.do(http.MethodGet, "/api/config", map[string]string{"Cookie": "kula_session=" + s.session}, "")
	var config struct {
		OllamaEnabled bool `json:"ollama_enabled"`
	}
	switch {
	case r.err != nil:
		return nil, StatusError, "could not read /api/config to learn whether the AI assistant is enabled: " + r.err.Error()
	case r.status != http.StatusOK || json.Unmarshal([]byte(r.body), &config) != nil:
		return nil, StatusError, fmt.Sprintf("/api/config answered HTTP %d to the session, not the configuration, so whether the AI assistant is enabled is unknown.", r.status)
	case !config.OllamaEnabled:
		return nil, StatusSkip, "the AI assistant is not enabled (/api/config says ollama_enabled is false)."
	}
	hdr["Cookie"] = "kula_session=" + s.session
	if s.csrf != "" {
		hdr["X-CSRF-Token"] = s.csrf
	}
	return hdr, StatusPass, ""
}

// statusCounts renders how many answers had each status, for evidence.
func statusCounts(counts map[int]int) string {
	var parts []string
	for _, status := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("HTTP %d ×%d", status, counts[status]))
	}
	return strings.Join(parts, ", ")
}

// ---- RATE-CHAT ---------------------------------------------------------------------

// runChatRateCheck sends more chat requests than Kula allows an address in a
// minute and expects a 429. Each body fails the decoder, which Kula reaches
// only after the limiter has counted the request.
func runChatRateCheck(s *Scanner) []Finding {
	hdr, st, why := s.chatAccess()
	if why != "" {
		return []Finding{finding("RATE-CHAT", SevInfo, st, why)}
	}
	const burst = chatRateLimit + 4
	counts := map[int]int{}
	sent, failed, limitedAt := 0, 0, 0
	for sent < burst && limitedAt == 0 {
		sent++
		r := s.do(http.MethodPost, chatPath, hdr, chatBadBody)
		switch {
		case r.err != nil:
			failed++
		case r.status == http.StatusTooManyRequests:
			limitedAt = sent
			counts[r.status]++
		default:
			counts[r.status]++
		}
	}
	evidence := statusCounts(counts)
	switch {
	case limitedAt == 1:
		return []Finding{finding("RATE-CHAT", SevMedium, StatusPass,
			"the first chat request was already answered 429: earlier requests from this address (INPUT-CHAT, FUZZ-BODY, another client) spent this minute's budget, and the limiter holds it.").
			withEvidence("%s", evidence)}
	case limitedAt > 0:
		return []Finding{finding("RATE-CHAT", SevMedium, StatusPass,
			fmt.Sprintf("the chat answered 429 at request %d, after letting %d through (Kula allows %d a minute per address).", limitedAt, limitedAt-1, chatRateLimit)).
			withEvidence("%s", evidence)}
	case failed == sent:
		return []Finding{finding("RATE-CHAT", SevInfo, StatusError,
			fmt.Sprintf("could not send any of %d chat requests.", sent))}
	case counts[http.StatusServiceUnavailable] > 0 && counts[http.StatusBadRequest] == 0:
		return []Finding{finding("RATE-CHAT", SevInfo, StatusSkip,
			"the chat answered 503: the AI assistant is not enabled.").withEvidence("%s", evidence)}
	case counts[http.StatusBadRequest] == 0 && counts[http.StatusUnauthorized]+counts[http.StatusForbidden] > 0:
		return []Finding{finding("RATE-CHAT", SevInfo, StatusError,
			"the chat refused the scanner's session, CSRF token or origin, so its rate limiter was not reached.").withEvidence("%s", evidence)}
	}
	return []Finding{finding("RATE-CHAT", SevMedium, StatusWarn,
		fmt.Sprintf("%d chat requests within a minute were never answered 429 (Kula allows %d a minute per address)%s.", sent-failed, chatRateLimit, failedNote(failed, sent))).
		withEvidence("%s", evidence).
		withRemediation("Upgrade Kula, whose chat allows ten requests a minute per address; behind a proxy, enable web.trust_proxy only when the proxy sets X-Forwarded-For, or every client shares one address.")}
}

// failedNote says how many of sent requests failed, when any did.
func failedNote(failed, sent int) string {
	if failed == 0 {
		return ""
	}
	return fmt.Sprintf("; %d of %d requests failed", failed, sent)
}

// ---- INPUT-CHAT ----------------------------------------------------------------------

// runChatBodyCapCheck sends a chat body far past the cap (chatBigBody), over
// a raw connection, and holds back its last bytes. Kula, which stops reading
// at the cap, answers while the body is still incomplete; a server that
// reads it all answers only once the rest is sent. The body never decodes,
// cut short or whole: an over-long string closed by the wrong bracket.
func runChatBodyCapCheck(s *Scanner) []Finding {
	hdr, st, why := s.chatAccess()
	if why != "" {
		return []Finding{finding("INPUT-CHAT", SevInfo, st, why)}
	}
	prefix := `{"kula_scan":"` + strings.Repeat("A", chatBigBody)
	const tail = `"]`
	size := len(prefix) + len(tail)
	own := http.Header{}
	for name, value := range hdr {
		own.Set(name, value)
	}
	own.Set("Content-Length", strconv.Itoa(size))
	head := fmt.Sprintf("POST %s%s HTTP/1.1\r\nHost: %s\r\n%s\r\n", s.base.Path, chatPath, s.base.Host, s.rawHeaderLines(own))

	conn, err := s.dialRaw()
	if err != nil {
		return []Finding{finding("INPUT-CHAT", SevInfo, StatusError, "could not connect to send an oversized chat body: "+err.Error())}
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(s.timeout))
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(conn, head+prefix)
		written <- err
	}()

	// An answer before the rest of the body is sent means the server stopped
	// reading at its cap.
	first := make([]byte, 1)
	n, readErr := conn.Read(first)
	early := n > 0
	if !early {
		var timeout net.Error
		if !errors.As(readErr, &timeout) || !timeout.Timeout() {
			return []Finding{finding("INPUT-CHAT", SevInfo, StatusError,
				fmt.Sprintf("the target closed the connection without answering a %d KiB chat body: %v", size>>10, readErr))}
		}
		if err := <-written; err != nil {
			return []Finding{finding("INPUT-CHAT", SevInfo, StatusError, "could not send the oversized chat body: "+err.Error())}
		}
		_ = conn.SetDeadline(time.Now().Add(s.timeout))
		if _, err := io.WriteString(conn, tail); err != nil {
			return []Finding{finding("INPUT-CHAT", SevInfo, StatusError, "could not send the end of the oversized chat body: "+err.Error())}
		}
		if n, readErr = conn.Read(first); n == 0 {
			return []Finding{finding("INPUT-CHAT", SevInfo, StatusError,
				fmt.Sprintf("no answer to a %d KiB chat body, sent whole: %v", size>>10, readErr))}
		}
	}
	resp, err := http.ReadResponse(bufio.NewReader(io.MultiReader(bytes.NewReader(first), &io.LimitedReader{R: conn, N: maxResponseHeaderBytes})), nil)
	if err != nil {
		return []Finding{finding("INPUT-CHAT", SevInfo, StatusError, "the answer to an oversized chat body could not be read: "+err.Error())}
	}
	_ = resp.Body.Close()
	if s.verbose {
		_, _ = fmt.Fprintf(s.log, "    POST %s (%d bytes, raw) -> %d\n", chatPath, size, resp.StatusCode)
	}

	when := "only after the scanner had sent all of it"
	if early {
		when = fmt.Sprintf("before the scanner had sent its last %d bytes", len(tail))
	}
	evidence := fmt.Sprintf("HTTP %d %s", resp.StatusCode, when)
	if resp.Close {
		evidence += ", with Connection: close"
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return []Finding{finding("INPUT-CHAT", SevInfo, StatusSkip,
			"the chat answered 429 without reading the body: this minute's chat budget for this address is spent; rerun in a minute.")}
	case resp.StatusCode == http.StatusServiceUnavailable:
		return []Finding{finding("INPUT-CHAT", SevInfo, StatusSkip, "the chat answered 503: the AI assistant is not enabled.")}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return []Finding{finding("INPUT-CHAT", SevInfo, StatusError,
			fmt.Sprintf("the chat refused the scanner's session, CSRF token or origin (HTTP %d), so its body cap was not reached.", resp.StatusCode))}
	case resp.StatusCode != http.StatusBadRequest && resp.StatusCode != http.StatusRequestEntityTooLarge:
		return []Finding{finding("INPUT-CHAT", SevMedium, StatusWarn,
			fmt.Sprintf("a %d KiB chat body was answered HTTP %d, neither 400 nor 413.", size>>10, resp.StatusCode)).
			withEvidence("%s", evidence).
			withRemediation("The chat body must be capped (Kula reads at most 32 KiB of it) and refused when it is not a chat request.")}
	case early:
		return []Finding{finding("INPUT-CHAT", SevLow, StatusPass,
			fmt.Sprintf("a %d KiB chat body was refused with HTTP %d before it was complete: the 32 KiB cap stops Kula reading it.", size>>10, resp.StatusCode)).
			withEvidence("%s", evidence)}
	default:
		return []Finding{finding("INPUT-CHAT", SevMedium, StatusWarn,
			fmt.Sprintf("a %d KiB chat body was answered HTTP %d only once the scanner had sent all of it: no cap stopped the server reading it, or a proxy in front buffers request bodies (scan Kula directly, with -connect, to tell).", size>>10, resp.StatusCode)).
			withEvidence("%s", evidence).
			withRemediation("Upgrade Kula, which reads at most 32 KiB of a chat body, or bound request bodies at the proxy in front.")}
	}
}

// ---- DOS-HISTORY -------------------------------------------------------------------

// historyBurstWait is the least time a burst request is given before it
// counts as hung: Kula keeps a history request waiting up to 10 seconds
// for one of its two query slots before it answers 503 (historySlotWait),
// longer than the default -timeout. A variable for the tests.
var historyBurstWait = 15 * time.Second

// historyBurst is how many history requests the burst sends at once, four
// times Kula's limit of two queries at a time.
const historyBurst = 8

// burstAnswer is how one burst request ended.
type burstAnswer struct {
	status     int
	retryAfter string
	err        error // the request failed, or its body was cut short
	took       time.Duration
}

// runHistoryBurstCheck sends a burst of concurrent history requests of the
// widest window Kula allows (31 days, 5000 points), narrowed to one section
// so the answers stay small, and polls /health while they are in flight.
// Each must be answered with data, or deferred with Kula's 503 and
// Retry-After, before it counts as hung; /health must keep answering.
func runHistoryBurstCheck(s *Scanner) []Finding {
	hdr, ok := s.apiHeaders()
	if !ok {
		return []Finding{finding("DOS-HISTORY", SevInfo, StatusSkip,
			"auth is enabled and no session is available to reach /api/history: "+s.noSessionReason()+".")}
	}
	to := time.Now().UTC().Truncate(time.Second)
	from := to.Add(-31 * 24 * time.Hour)
	path := fmt.Sprintf("/api/history?from=%s&to=%s&points=5000&sections=lavg", from.Format(time.RFC3339), to.Format(time.RFC3339))
	wait := max(s.timeout, historyBurstWait)

	answers := make([]burstAnswer, historyBurst)
	var wg sync.WaitGroup
	for i := range answers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i] = s.burstRequest(path, hdr, wait)
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	// /health, at once and then every quarter second until the burst is over.
	healthy, probes := 0, 0
	var unhealthy []string
	poll := func() {
		probes++
		r := s.do(http.MethodGet, "/health", nil, "")
		switch {
		case r.err != nil:
			unhealthy = append(unhealthy, r.err.Error())
		case r.status >= 500:
			unhealthy = append(unhealthy, fmt.Sprintf("HTTP %d", r.status))
		default:
			healthy++
		}
	}
	poll()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for running := true; running; {
		select {
		case <-done:
			running = false
		case <-ticker.C:
			poll()
		}
	}

	var hung, broken, odd, undelivered, refused int
	answered, deferred := 0, 0
	var slowest time.Duration
	counts := map[int]int{}
	var errs []string
	for _, a := range answers {
		slowest = max(slowest, a.took)
		if a.status != 0 {
			counts[a.status]++
		}
		switch {
		case a.err != nil && isTimeout(a.err):
			hung++
		case a.err != nil && a.status == 0 && isDialError(a.err):
			undelivered++
			errs = append(errs, a.err.Error())
		case a.err != nil:
			broken++
			errs = append(errs, a.err.Error())
		case a.status == http.StatusOK:
			answered++
		case a.status == http.StatusServiceUnavailable && a.retryAfter != "":
			deferred++
		case a.status == http.StatusUnauthorized || a.status == http.StatusForbidden:
			refused++
		case a.status >= 500 && a.status != http.StatusServiceUnavailable:
			broken++
		default:
			odd++
		}
	}
	evidence := strings.TrimPrefix(fmt.Sprintf("%s; slowest %s; /health %d of %d", statusCounts(counts), slowest.Round(time.Millisecond), healthy, probes), "; ")
	if len(errs) > 0 {
		evidence += "; errors: " + strings.Join(slices.Compact(slices.Sorted(slices.Values(errs))), " | ")
	}
	burst := fmt.Sprintf("%d concurrent 31-day, 5000-point history requests", historyBurst)
	switch {
	case undelivered == historyBurst:
		return []Finding{finding("DOS-HISTORY", SevInfo, StatusError,
			"could not send any of "+burst+".").withEvidence("%s", evidence)}
	case refused == historyBurst:
		return []Finding{finding("DOS-HISTORY", SevInfo, StatusError,
			"the history requests were refused (HTTP 401/403) with the scanner's session, so the burst reached no query.").withEvidence("%s", evidence)}
	case hung > 0 || len(unhealthy) > 0:
		var what []string
		if hung > 0 {
			what = append(what, fmt.Sprintf("%d of %d requests got no answer within %s", hung, historyBurst, wait))
		}
		if len(unhealthy) > 0 {
			what = append(what, fmt.Sprintf("/health failed %d of %d times during the burst (%s)", len(unhealthy), probes, unhealthy[0]))
		}
		return []Finding{finding("DOS-HISTORY", SevHigh, StatusFail,
			"under a burst of "+burst+", "+strings.Join(what, ", and ")+": history queries can starve the server.").
			withEvidence("%s", evidence).
			withRemediation("Upgrade Kula (0.22.0 and later run two history queries at a time and answer the rest 503 with Retry-After), and keep a proxy in front from queuing requests without a timeout.")}
	case broken > 0:
		return []Finding{finding("DOS-HISTORY", SevMedium, StatusFail,
			fmt.Sprintf("under a burst of %s, %d answered with a server error or a dropped connection, not data or Kula's 503 with Retry-After.", burst, broken)).
			withEvidence("%s", evidence).
			withRemediation("Upgrade Kula (0.22.0 and later defer history requests past two at a time with 503 and Retry-After).")}
	case odd > 0 || refused > 0:
		return []Finding{finding("DOS-HISTORY", SevLow, StatusWarn,
			fmt.Sprintf("under a burst of %s, %d answered neither with data nor with Kula's 503 and Retry-After.", burst, odd+refused)).
			withEvidence("%s", evidence)}
	}
	note := ""
	if undelivered > 0 {
		note = fmt.Sprintf("; %d could not be sent", undelivered)
	}
	probed := "the probe"
	if probes > 1 {
		probed = fmt.Sprintf("each of %d probes", probes)
	}
	return []Finding{finding("DOS-HISTORY", SevMedium, StatusPass,
		fmt.Sprintf("a burst of %s was answered: %d with data and %d deferred with 503 and Retry-After, the slowest after %s%s; /health answered %s sent during the burst. A store that answers each query quickly may never show Kula's cap of two queries at a time.",
			burst, answered, deferred, slowest.Round(time.Millisecond), note, probed)).
		withEvidence("%s", evidence)}
}

// burstRequest GETs path with headers, given wait in all, and reads its
// body to the end so the server releases its query slot.
func (s *Scanner) burstRequest(path string, headers map[string]string, wait time.Duration) burstAnswer {
	ctx, cancel := context.WithTimeout(s.ctx, wait)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.urlFor(path), nil)
	if err != nil {
		return burstAnswer{err: err}
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	s.addExtraHeaders(req.Header)
	// The client's own timeout is -timeout; this request may take longer.
	client := *s.client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return burstAnswer{err: err, took: time.Since(start)}
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHistoryBody))
	a := burstAnswer{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), err: err, took: time.Since(start)}
	if s.verbose {
		_, _ = fmt.Fprintf(s.log, "    GET %s -> %d (%s)\n", printable(path), resp.StatusCode, a.took.Round(time.Millisecond))
	}
	return a
}

// isTimeout reports whether err is a request that ran out of time.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

// isDialError reports whether err is a connection that could not be made,
// so the request never reached the target.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
