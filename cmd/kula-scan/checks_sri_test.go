package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- the tag scanner -------------------------------------------------------------

func TestScanPageTags(t *testing.T) {
	// Each tag as name, then its src or href, then its other kept attributes.
	describe := func(tags []pageTag) string {
		var out []string
		for _, tag := range tags {
			d := tag.name
			for _, key := range []string{"src", "href", "rel", "integrity", "nonce", "type"} {
				if v, ok := tag.attrs[key]; ok {
					d += " " + key + "=" + v
				}
			}
			out = append(out, d)
		}
		return strings.Join(out, " | ")
	}
	for _, c := range []struct {
		name, page, want string
	}{
		{"quoted, unquoted and mixed case", `<SCRIPT Src=a.js INTEGRITY='sha384-x' nonce="n"></script>`,
			"script src=a.js integrity=sha384-x nonce=n"},
		{"any order", `<link href="b.js" Rel="modulePreload"  integrity = "sha384-y" >`,
			"link href=b.js rel=modulePreload integrity=sha384-y"},
		{"character references", `<script src="a.js" integrity="sha384-ab&#43;cd&#x2B;e&amp;f" nonce=n&#43;1></script>`,
			"script src=a.js integrity=sha384-ab+cd+e&f nonce=n+1"},
		{"the first of an attribute wins", `<script src=a.js src=b.js></script>`, "script src=a.js"},
		{"a slash before an attribute", `<script/src=a.js></script>`, "script src=a.js"},
		{"'>' inside a quoted value", `<a title="x>y" href=nope><script src=c.js></script>`, "script src=c.js"},
		{"<script> in an inline script's string", `<script>var s = "<script src=evil.js>";</script><script src=ok.js></script>`,
			"script | script src=ok.js"},
		{"</script> in a string ends the script, as in a browser", `<script>var s = "</script><script src=x.js>";</script>`,
			"script | script src=x.js"},
		{"an escaped script hides its <script>", `<script><!--<script src=evil.js></script>--></script><script src=ok.js></script>`,
			"script | script src=ok.js"},
		{"an escaped script still ends at </script>", `<script><!-- x </script><script src=ok.js></script>`,
			"script | script src=ok.js"},
		{"<!--> in a script escapes nothing", `<script><!--></script><script src=ok.js></script>`, "script | script src=ok.js"},
		{"a comment", `<!-- <script src=evil.js></script> --><script src=ok.js></script>`, "script src=ok.js"},
		{"a comment ended by --!>", `<!-- x --!><script src=ok.js></script>`, "script src=ok.js"},
		{"an empty comment", `<!--><script src=ok.js></script><!---><script src=ok2.js></script>`, "script src=ok.js | script src=ok2.js"},
		{"an unterminated comment", `<!-- <script src=evil.js></script>`, ""},
		{"doctype and bogus comments", `<!DOCTYPE html><?xml x?><![CDATA[<script src=evil.js>]]><script src=ok.js></script>`,
			"script src=ok.js"},
		{"a bogus end tag", `</ <script src=evil.js>><script src=ok.js></script>`, "script src=ok.js"},
		{"an end tag with a quoted '>'", `</div title=">"><script src=ok.js></script>`, "script src=ok.js"},
		{"textarea and title are text", `<textarea><script src=evil.js></textarea><title><script src=evil2.js></TITLE><script src=ok.js></script>`,
			"script src=ok.js"},
		{"style and noscript are text", `<style>a{} <script src=evil.js></style><noscript><script src=evil2.js></noscript><script src=ok.js></script>`,
			"script src=ok.js"},
		{"plaintext ends the markup", `<plaintext><script src=evil.js></script>`, ""},
		{"an unterminated quoted value", `<script src=ok.js></script><script src="a.js`, "script src=ok.js"},
		{"an unterminated tag", `<script src=ok.js></script><link rel=modulepreload href=b.js`, "script src=ok.js"},
		{"a script running to the end", `<script src=ok.js>var x = 1;`, "script src=ok.js"},
		{"a lone <", `a < b <3 <script src=ok.js></script>`, "script src=ok.js"},
		{"base and other links", `<base href="/kula/"><link rel=stylesheet href=style.css><link rel="preload modulepreload" href=m.js>`,
			"base href=/kula/ | link href=style.css rel=stylesheet | link href=m.js rel=preload modulepreload"},
	} {
		tags, more := scanPageTags(c.page, 64)
		if got := describe(tags); got != c.want || more {
			t.Errorf("%s:\n got %q (more %v)\nwant %q", c.name, got, more, c.want)
		}
	}

	// An inline script keeps its text, bounded.
	tags, _ := scanPageTags(`<script nonce=n>`+strings.Repeat("x", 10000)+`</script>`, 64)
	if len(tags) != 1 || len(tags[0].text) != maxScriptText {
		t.Errorf("inline text kept: %d bytes", len(tags[0].text))
	}
	// More tags than the limit.
	if tags, more := scanPageTags(strings.Repeat("<script src=a.js></script>", 10), 4); len(tags) != 4 || !more {
		t.Errorf("past the limit: %d tags, more %v", len(tags), more)
	}
}

// TestScanPageTagsOnLargePages: the page is hostile input; the scanner reads
// a large one in linear time and holds no more than it keeps.
func TestScanPageTagsOnLargePages(t *testing.T) {
	const size = 5 << 20
	for name, page := range map[string]string{
		"many tags":                           strings.Repeat(`<div class="x" id=y>`, size/20) + `<script src=end.js></script>`,
		"one long attribute":                  `<div title="` + strings.Repeat("a", size) + `"></div><script src=end.js></script>`,
		"one long unquoted name":              `<div ` + strings.Repeat("a", size) + `></div><script src=end.js></script>`,
		"many comments":                       strings.Repeat(`<!-- c -->`, size/10) + `<script src=end.js></script>`,
		"many end-tag lookalikes in a script": `<script>` + strings.Repeat(`</scrip`, size/7) + `</script><script src=end.js></script>`,
		"an escaped script":                   `<script><!--` + strings.Repeat(`<script>-</script>`, size/18) + `--></script><script src=end.js></script>`,
		"many < in a textarea":                `<textarea>` + strings.Repeat(`</textare`, size/9) + `</textarea><script src=end.js></script>`,
	} {
		start := time.Now()
		tags, _ := scanPageTags(page, maxPageTags)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("%s: %s for a %d-byte page", name, elapsed, len(page))
		}
		if len(tags) == 0 || tags[len(tags)-1].attrs["src"] != "end.js" {
			t.Errorf("%s: the script after it was not found", name)
		}
	}
	// Unterminated: no tag, and no panic.
	for _, page := range []string{"<!--" + strings.Repeat("-", size), `<script src="` + strings.Repeat("x", size),
		"<script>" + strings.Repeat("<!--<script>", size/12), strings.Repeat("<", size), strings.Repeat("</", size/2)} {
		_, _ = scanPageTags(page, maxPageTags)
	}
}

func TestStrongestHashes(t *testing.T) {
	sum := sha512.Sum384([]byte("x"))
	h384 := base64.StdEncoding.EncodeToString(sum[:])
	for _, c := range []struct {
		value, alg string
		n          int
	}{
		{"sha384-" + h384, "sha384", 1},
		{"sha256-abc= sha384-" + h384 + " sha384-other=", "sha384", 2},
		{"sha512-x sha256-y", "sha512", 1},
		{"SHA384-" + h384 + "?ct=application/javascript", "sha384", 1},
		{"md5-x sha1-y", "", 0},
		{"", "", 0},
		{"sha384", "", 0},
	} {
		alg, digests := strongestHashes(c.value)
		if alg != c.alg || len(digests) != c.n {
			t.Errorf("%q: %q with %d digests, want %q with %d", c.value, alg, len(digests), c.alg, c.n)
		}
	}
	if _, digests := strongestHashes("sha384-" + h384); string(digests[0]) != string(sum[:]) {
		t.Error("the digest does not decode")
	}
	if _, digests := strongestHashes("sha384-" + strings.TrimRight(base64.URLEncoding.EncodeToString(sum[:]), "=")); string(digests[0]) != string(sum[:]) {
		t.Error("an unpadded URL-safe digest does not decode")
	}
}

// ---- a UI page like Kula's -----------------------------------------------------------

// sriAssets are the scripts of the mock UI page, by path below its mount.
var sriAssets = map[string]string{
	"js/chartjs/chartjs-bundle.min.js": "/*! chart */var Chart={};\n",
	"js/app/api.js":                    "export const api = 1;\n",
	"js/app/main.js":                   "import {api} from './api.js';\nconsole.log(api);\n",
}

func sriHash(body string) string {
	sum := sha512.Sum384([]byte(body))
	return "sha384-" + base64.StdEncoding.EncodeToString(sum[:])
}

// templateAttr escapes s as html/template escapes an attribute value: '+'
// becomes &#43;, which Kula's integrity values and nonces carry.
func templateAttr(s string) string {
	return strings.ReplaceAll(s, "+", "&#43;")
}

// sriOptions shape the mock UI page.
type sriOptions struct {
	mount string // the base path it is served under, e.g. "/kula": the page then carries <base href="/kula/">
	head  string // markup added to the page's head; NONCE stands for the response's nonce
	gzip  bool   // scripts are gzip-encoded for a client that accepts it
	uiOff bool   // / is not served
}

// sriMock serves a Kula with auth off whose UI page loads sriAssets the way
// index.html does: a deferred classic script, module preloads and a module,
// each with its sha384 integrity, and inline scripts with the CSP nonce.
// Scripts carry Kula's ETag.
func sriMock(o sriOptions) http.Handler {
	sri := func(path string) string { return templateAttr(sriHash(sriAssets[path])) }
	inner := http.NewServeMux()
	inner.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("kula is healthy"))
	})
	inner.HandleFunc("/api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"auth_required": false})
	})
	inner.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if body, ok := sriAssets[path]; ok {
			sum := sha256.Sum256([]byte(body))
			w.Header().Set("ETag", `W/"`+hex.EncodeToString(sum[:16])+`"`)
			w.Header().Set("Content-Type", "application/javascript")
			if o.gzip && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				var b bytes.Buffer
				zw := gzip.NewWriter(&b)
				_, _ = zw.Write([]byte(body))
				_ = zw.Close()
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = w.Write(b.Bytes())
				return
			}
			_, _ = w.Write([]byte(body))
			return
		}
		if path != "" || o.uiOff {
			http.NotFound(w, r)
			return
		}
		nonce := "n+" + randToken(20) + "=="
		base := ""
		if o.mount != "" {
			base = `<base href="` + o.mount + `/">`
		}
		n := templateAttr(nonce)
		page := `<!DOCTYPE html>
<html lang="en">
<head>
    ` + base + `
    <script nonce="` + n + `">window.KULA_BASE_PATH = "` + o.mount + `";</script>
    <!-- defer keeps Chart.js off the render path; <script src="commented.js"> is not loaded -->
    <script defer src="js/chartjs/chartjs-bundle.min.js" integrity="` + sri("js/chartjs/chartjs-bundle.min.js") + `"
        nonce="` + n + `" crossorigin="anonymous"></script>
    <link rel="stylesheet" href="style.css">
    <link rel="modulepreload" href="js/app/api.js" integrity="` + sri("js/app/api.js") + `" nonce="` + n + `" crossorigin="anonymous">
    <link rel="modulepreload" href="js/app/main.js" integrity="` + sri("js/app/main.js") + `" nonce="` + n + `" crossorigin="anonymous">
    ` + strings.ReplaceAll(o.head, "NONCE", n) + `
</head>
<body>
    <script nonce="` + n + `">(function () { var s = "<script src=evil.js>"; })();</script>
    <script type="module" nonce="` + n + `" src="js/app/main.js" integrity="` + sri("js/app/main.js") + `"
        crossorigin="anonymous"></script>
</body>
</html>`
		w.Header().Set("Content-Security-Policy", kulaCSP(nonce))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	if o.mount == "" {
		return inner
	}
	outer := http.NewServeMux()
	outer.Handle(o.mount+"/", http.StripPrefix(o.mount, inner))
	return outer
}

// integrityFindings runs the integrity check against handler, mounted at
// mount, with options.
func integrityFindings(t *testing.T, handler http.Handler, mount string, opts Options) []Finding {
	t.Helper()
	ts := httptest.NewServer(handler)
	defer ts.Close()
	opts.Target, opts.Timeout = ts.URL+mount, 5*time.Second
	s, err := NewScanner(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.discover()
	return runIntegrityChecks(s)
}

func TestIntegrityOfKulasPage(t *testing.T) {
	findings := integrityFindings(t, sriMock(sriOptions{}), "", Options{})
	for _, id := range []string{"SRI-001", "SRI-002"} {
		wantStatus(t, findings, id, StatusPass)
	}
	if f, _ := byID(findings, "SRI-001"); !strings.Contains(f.Detail, "4 scripts and module preloads are same-origin with sha384 integrity, and its 2 inline scripts carry the CSP nonce") {
		t.Errorf("SRI-001: %q", f.Detail)
	}
	if f, _ := byID(findings, "SRI-002"); !strings.Contains(f.Detail, "the 3 scripts") {
		t.Errorf("SRI-002 does not count the scripts once each: %q", f.Detail)
	}
}

func TestIntegrityUnderABasePathAndEncodings(t *testing.T) {
	for name, c := range map[string]struct {
		mount string
		opts  sriOptions
		scan  Options
	}{
		"a base path":                         {"/kula", sriOptions{mount: "/kula"}, Options{}},
		"gzip-encoded scripts":                {"", sriOptions{gzip: true}, Options{}},
		"gzip the client leaves to the check": {"", sriOptions{gzip: true}, Options{Headers: []string{"Accept-Encoding: gzip"}}},
		"a base path, gzip-encoded":           {"/kula", sriOptions{mount: "/kula", gzip: true}, Options{}},
	} {
		rec := &requestRecorder{}
		findings := integrityFindings(t, rec.wrap(sriMock(c.opts)), c.mount, c.scan)
		for _, id := range []string{"SRI-001", "SRI-002"} {
			if f, _ := byID(findings, id); f.Status != StatusPass {
				t.Errorf("%s: %s = %s %q | %s", name, id, f.Status, f.Detail, f.Evidence)
			}
		}
		for _, r := range rec.requests() {
			if strings.HasSuffix(r.path, ".js") && !strings.HasPrefix(r.path, c.mount+"/js/") {
				t.Errorf("%s: fetched %s, outside %s/js/", name, r.path, c.mount)
			}
		}
	}

	// A <base href> that moves the scripts elsewhere on the origin.
	moved := sriMock(sriOptions{mount: "/kula/static"})
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/kula/" {
			r.URL.Path = "/kula/static/"
		}
		moved.ServeHTTP(w, r)
	})
	findings := integrityFindings(t, page, "/kula", Options{})
	wantStatus(t, findings, "SRI-002", StatusPass)
}

func TestIntegrityFindsUncheckedScripts(t *testing.T) {
	for _, c := range []struct {
		name, head, detail string
		sev                Severity
	}{
		{"no integrity", `<script src="js/app/extra.js"></script>`, "script /js/app/extra.js (no integrity)", SevMedium},
		{"an unsupported algorithm", `<link rel=modulepreload href=js/app/x.js integrity="md5-abc">`, "has no sha256, sha384 or sha512 hash", SevMedium},
		{"another origin", `<script src="https://cdn.example/x.js" integrity="sha384-abc" nonce="NONCE"></script>`, "script https://cdn.example/x.js", SevHigh},
		{"a protocol-relative URL", `<script src="//cdn.example/y.js"></script>`, "script http://cdn.example/y.js", SevHigh},
		{"no nonce", `<script>alert(1)</script>`, `inline script #2 ("alert(1)") carries no nonce`, SevMedium},
		{"a wrong nonce", `<script nonce="stale">x()</script>`, "carries a nonce the CSP does not announce", SevMedium},
	} {
		findings := integrityFindings(t, sriMock(sriOptions{head: c.head}), "", Options{})
		f, _ := byID(findings, "SRI-001")
		if f.Status != StatusFail || f.Severity != c.sev || !strings.Contains(f.Detail, c.detail) {
			t.Errorf("%s: SRI-001 = %s %s %q, want a %s FAIL naming %q", c.name, f.Status, f.Severity, f.Detail, c.sev, c.detail)
		}
		// The scripts that carry integrity still match.
		wantStatus(t, findings, "SRI-002", StatusPass)
	}
}

// rewriting serves next, but appends suffix to the body of path, keeping
// the other headers, as a proxy that rewrites scripts does.
func rewriting(next http.Handler, path, suffix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del("Accept-Encoding")
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.Code)
		_, _ = w.Write(append(rec.Body.Bytes(), suffix...))
	})
}

func TestIntegrityFindsRewrittenScripts(t *testing.T) {
	findings := integrityFindings(t, rewriting(sriMock(sriOptions{}), "/js/app/main.js", "/* injected */"), "", Options{})
	f, _ := byID(findings, "SRI-002")
	if f.Status != StatusFail || f.Severity != SevHigh || !strings.Contains(f.Detail, "1 of the 3 scripts") || !strings.Contains(f.Detail, "/js/app/main.js") {
		t.Errorf("SRI-002 = %s %s %q, want a HIGH FAIL naming main.js", f.Status, f.Severity, f.Detail)
	}
	size := len(sriAssets["js/app/main.js"]) + len("/* injected */")
	if !strings.Contains(f.Evidence, fmt.Sprintf("received %d bytes", size)) || !strings.Contains(f.Evidence, "no longer match the ETag Kula sent") {
		t.Errorf("SRI-002 evidence: %q", f.Evidence)
	}
	wantStatus(t, findings, "SRI-001", StatusPass)
}

func TestIntegritySkipsAndErrors(t *testing.T) {
	// The UI is off.
	for _, id := range []string{"SRI-001", "SRI-002"} {
		f, _ := byID(integrityFindings(t, sriMock(sriOptions{uiOff: true}), "", Options{}), id)
		if f.Status != StatusSkip || !strings.Contains(f.Detail, "HTTP 404") {
			t.Errorf("UI off: %s = %s %q", id, f.Status, f.Detail)
		}
	}
	// A page with no scripts.
	if f, _ := byID(integrityFindings(t, secureMock(), "", Options{}), "SRI-001"); f.Status != StatusSkip || !strings.Contains(f.Detail, "no scripts") {
		t.Errorf("no scripts: SRI-001 = %s %q", f.Status, f.Detail)
	}
	// Kula refuses the scanner's Host name.
	refused := withHostCheck(sriMock(sriOptions{}), authOffGuard)
	ts := httptest.NewServer(refused)
	defer ts.Close()
	s, err := NewScanner(Options{Target: "http://kula.example.com", Connect: ts.Listener.Addr().String(), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := byID(runIntegrityChecks(s), "SRI-001"); f.Status != StatusSkip || !strings.Contains(f.Detail, "DISC-003") {
		t.Errorf("a refused Host name: SRI-001 = %s %q", f.Status, f.Detail)
	}
	// A script too large to read whole is not a mismatch.
	huge := sriMock(sriOptions{})
	large := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/js/app/api.js" {
			_, _ = w.Write(bytes.Repeat([]byte("a"), maxAssetBytes+1))
			return
		}
		huge.ServeHTTP(w, r)
	})
	if f, _ := byID(integrityFindings(t, large, "", Options{}), "SRI-002"); f.Status != StatusError || !strings.Contains(f.Detail, "/js/app/api.js: larger than 16 MiB") {
		t.Errorf("a truncated script: SRI-002 = %s %q", f.Status, f.Detail)
	}
	// The page cannot be fetched.
	s, err = NewScanner(Options{Target: "http://" + closedAddress(t), Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, runIntegrityChecks(s), "SRI-002", StatusError)
}
