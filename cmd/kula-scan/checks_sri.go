package main

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"html"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Subresource integrity. Kula names every script of its UI page with an
// integrity hash of the bytes it serves (calculateSRIs hashes them after
// minification), and admits its inline scripts by the CSP nonce. A proxy or
// CDN in front that rewrites assets (minifies them again, injects a script,
// a "rocket loader") breaks that in the field: the browser refuses a script
// whose bytes changed, and an injected one runs unchecked. SRI-001 reads the
// page for scripts that carry no integrity, come from another origin, or
// lack the nonce; SRI-002 fetches each script the page names and compares
// its bytes, as a browser receives them, with the hash. Scripts a module
// imports are not followed: the page preloads every one of them.

func integrityChecks() []check {
	return []check{{ids: []string{"SRI-001", "SRI-002"}, run: runIntegrityChecks}}
}

// Bounds on what the integrity check reads of the target.
const (
	maxPageBytes    = 4 << 20  // the UI page; Kula's is about 50 KB
	maxAssetBytes   = 16 << 20 // one script, decoded; Kula's largest is about 200 KB
	maxPageTags     = 512      // script, link and base tags read from the page; Kula's has about 50
	maxAssetFetches = 256      // distinct same-origin scripts fetched
	maxScriptText   = 256      // bytes of an inline script kept for evidence
)

// ---- fetching ------------------------------------------------------------------

// fetched is the answer to a bounded GET: its status and headers, and its
// body as a browser reads it, after content decoding.
type fetched struct {
	status    int
	header    http.Header
	body      []byte
	encoding  string // the content coding the body was decoded from, "" for none
	truncated bool   // the body is longer than the limit; body holds its start
	err       error  // the request failed: no answer
	readErr   error  // the body could not be read or decoded
}

// fetchBounded GETs target, a URL on the target's origin, and reads at most
// limit bytes of its decoded body. Unlike do, it says when the body was
// longer, and it decodes a content coding the HTTP client left encoded (an
// Accept-Encoding sent with -H).
func (s *Scanner) fetchBounded(target string, limit int64) fetched {
	ctx, cancel := context.WithTimeout(s.ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fetched{err: err}
	}
	s.addExtraHeaders(req.Header)
	resp, err := s.client.Do(req)
	if err != nil {
		if s.verbose {
			_, _ = fmt.Fprintf(s.log, "    GET %s -> %s\n", printable(req.URL.RequestURI()), printable(err.Error()))
		}
		return fetched{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	f := fetched{status: resp.StatusCode, header: resp.Header}
	body := io.Reader(resp.Body)
	if resp.Uncompressed {
		f.encoding = "gzip" // the client asked for it and decoded it
	} else {
		f.encoding = strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
		if body, err = decodeContent(resp.Body, f.encoding); err != nil {
			f.readErr = err
		}
	}
	if f.readErr == nil {
		f.body, f.readErr = io.ReadAll(io.LimitReader(body, limit+1))
		if int64(len(f.body)) > limit {
			f.body, f.truncated = f.body[:limit], true
		}
	}
	if f.encoding == "identity" {
		f.encoding = ""
	}
	if s.verbose {
		_, _ = fmt.Fprintf(s.log, "    GET %s -> %d (%d bytes)\n", printable(req.URL.RequestURI()), resp.StatusCode, len(f.body))
	}
	return f
}

// decodeContent returns r decoded from the content coding encoding.
func decodeContent(r io.Reader, encoding string) (io.Reader, error) {
	switch encoding {
	case "", "identity":
		return r, nil
	case "gzip", "x-gzip":
		return gzip.NewReader(r)
	case "deflate":
		// HTTP's deflate is a zlib stream, which some servers send raw.
		br := bufio.NewReader(r)
		if head, err := br.Peek(2); err == nil && head[0]&0x0f == 8 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
			return zlib.NewReader(br)
		}
		return flate.NewReader(br), nil
	default:
		return nil, fmt.Errorf("the body is sent with Content-Encoding %q, which the scanner cannot decode", encoding)
	}
}

// ---- reading the page ------------------------------------------------------------

// pageTag is a start tag the page scanner keeps: a script, link or base.
type pageTag struct {
	name  string            // lower case
	attrs map[string]string // the attributes the check reads, by lower-case name, character references decoded; the first of a name wins
	text  string            // a script's inline text, up to maxScriptText bytes
}

// keptTags are the elements scanPageTags returns.
func keptTag(name string) bool {
	return name == "script" || name == "link" || name == "base"
}

// keptAttribute reports whether scanPageTags keeps an attribute of a kept
// tag: only these, so a hostile page cannot make it hold more.
func keptAttribute(name string) bool {
	switch name {
	case "src", "href", "integrity", "nonce", "type", "rel":
		return true
	}
	return false
}

// scanPageTags returns the script, link and base start tags of an HTML
// page, in order, as a browser's tokenizer finds them. It is written by hand
// (the module has no HTML parser, and regular expressions cannot follow
// HTML's states): it reads comments, doctypes and bogus comments, end tags
// with their attributes, the text of script (with its escaped and double-
// escaped states, so "<script>" inside an inline script or its comment is
// text), the raw text of style, xmp, iframe, noembed, noframes and noscript
// (as a browser running scripts reads it), the text of title and textarea,
// and plaintext, which ends the markup. Attribute values may be quoted
// either way or not at all, and names are matched in any case. It builds no
// tree, so the contents of svg and math are read as HTML. Its work is linear
// in the page's length, and it keeps at most limit tags: more is true when
// the page has more, and the tags after them are not read.
func scanPageTags(page string, limit int) (tags []pageTag, more bool) {
	n := len(page)
	for i := 0; i < n; {
		lt := strings.IndexByte(page[i:], '<')
		if lt < 0 {
			break
		}
		i += lt
		rest := page[i:]
		switch {
		case strings.HasPrefix(rest, "<!--"):
			i = commentEnd(page, i+4)
		case strings.HasPrefix(rest, "<!"), strings.HasPrefix(rest, "<?"):
			i = bogusCommentEnd(page, i+2)
		case strings.HasPrefix(rest, "</"):
			switch {
			case len(rest) > 2 && isASCIIAlpha(rest[2]):
				_, _, i, _ = readTag(page, i+2, false)
			case len(rest) > 2 && rest[2] == '>':
				i += 3
			default:
				i = bogusCommentEnd(page, i+2)
			}
		case len(rest) > 1 && isASCIIAlpha(rest[1]):
			name, attrs, next, complete := readTag(page, i+1, true)
			if !complete {
				return tags, false // the page ends inside the tag, which a browser drops
			}
			tag := pageTag{name: name, attrs: attrs}
			i = next
			switch name {
			case "script":
				textEnd, after := scriptEnd(page, i)
				tag.text = page[i:min(textEnd, i+maxScriptText)]
				i = after
			case "style", "xmp", "iframe", "noembed", "noframes", "noscript", "title", "textarea":
				i = rawTextEnd(page, i, name)
			case "plaintext":
				i = n
			}
			if keptTag(name) {
				if len(tags) == limit {
					return tags, true
				}
				tags = append(tags, tag)
			}
		default:
			i++ // a '<' that starts no tag is text
		}
	}
	return tags, false
}

// readTag reads a tag from i, the first character of its name, to just after
// its '>'. It returns the lower-case name, and the kept attributes when keep
// is set and the tag is a kept one. complete is false, and next is the end
// of the page, when the page ends inside the tag.
func readTag(page string, i int, keep bool) (name string, attrs map[string]string, next int, complete bool) {
	n := len(page)
	start := i
	for i < n && !isTagSpace(page[i]) && page[i] != '/' && page[i] != '>' {
		i++
	}
	name = asciiLower(page[start:i])
	if keep && keptTag(name) {
		attrs = map[string]string{}
	}
	for {
		for i < n && (isTagSpace(page[i]) || page[i] == '/') {
			i++
		}
		if i >= n {
			return name, attrs, n, false
		}
		if page[i] == '>' {
			return name, attrs, i + 1, true
		}
		// An attribute name: its first character belongs to it, even '='.
		nameStart := i
		i++
		for i < n && !isTagSpace(page[i]) && page[i] != '/' && page[i] != '>' && page[i] != '=' {
			i++
		}
		attrName := page[nameStart:i]
		for i < n && isTagSpace(page[i]) {
			i++
		}
		value := ""
		if i < n && page[i] == '=' {
			i++
			for i < n && isTagSpace(page[i]) {
				i++
			}
			if i >= n {
				return name, attrs, n, false
			}
			switch quote := page[i]; quote {
			case '"', '\'':
				end := strings.IndexByte(page[i+1:], quote)
				if end < 0 {
					return name, attrs, n, false
				}
				value = page[i+1 : i+1+end]
				i += end + 2
			case '>':
				// A missing value: the tag ends at this '>'.
			default:
				valueStart := i
				for i < n && !isTagSpace(page[i]) && page[i] != '>' {
					i++
				}
				value = page[valueStart:i]
			}
		}
		if attrs != nil {
			key := asciiLower(attrName)
			if _, seen := attrs[key]; !seen && keptAttribute(key) {
				attrs[key] = html.UnescapeString(value)
			}
		}
	}
}

// commentEnd returns where a comment whose text starts at i ends: after
// "-->" or "--!>", at once for "<!-->" and "<!--->", or at the end of the
// page.
func commentEnd(page string, i int) int {
	rest := page[i:]
	switch {
	case strings.HasPrefix(rest, ">"):
		return i + 1
	case strings.HasPrefix(rest, "->"):
		return i + 2
	}
	window, end := rest, len(page)
	if j := strings.Index(rest, "-->"); j >= 0 {
		window, end = rest[:j], i+j+3
	}
	if j := strings.Index(window, "--!>"); j >= 0 {
		end = i + j + 4
	}
	return end
}

// bogusCommentEnd returns where a doctype, a processing instruction or
// another bogus comment whose text starts at i ends: after the next '>'.
func bogusCommentEnd(page string, i int) int {
	if j := strings.IndexByte(page[i:], '>'); j >= 0 {
		return i + j + 1
	}
	return len(page)
}

// rawTextEnd returns where tokenizing resumes after the text of an element
// whose content is text (name) that starts at i: after its end tag, or at the
// end of the page.
func rawTextEnd(page string, i int, name string) int {
	for {
		j := strings.Index(page[i:], "</")
		if j < 0 {
			return len(page)
		}
		i += j
		if endTagAt(page, i, name) {
			_, _, next, _ := readTag(page, i+2, false)
			return next
		}
		i += 2
	}
}

// scriptEnd returns where the text of a script element that starts at i
// ends, and where tokenizing resumes after its end tag; both are the end of
// the page when it ends first. It follows the tokenizer's script data
// states: "<!--" escapes the text, inside which "<script" starts a double
// escape that "</script" only leaves again, and "-->" returns to plain
// script data, where "</script" ends the element.
func scriptEnd(page string, i int) (textEnd, next int) {
	const (
		data = iota
		escaped
		doubleEscaped
	)
	n := len(page)
	state, dashes := data, 0
	for i < n {
		if state == data {
			j := strings.IndexByte(page[i:], '<')
			if j < 0 {
				return n, n
			}
			i += j
			switch {
			case endTagAt(page, i, "script"):
				_, _, next, _ = readTag(page, i+2, false)
				return i, next
			case strings.HasPrefix(page[i:], "<!--"):
				state, dashes = escaped, 2
				i += len("<!--")
			default:
				i++
			}
			continue
		}
		c := page[i]
		switch {
		case c == '-':
			dashes++
			i++
		case c == '>' && dashes >= 2:
			state, dashes = data, 0
			i++
		case c == '<' && state == escaped && endTagAt(page, i, "script"):
			_, _, next, _ = readTag(page, i+2, false)
			return i, next
		case c == '<' && state == escaped && startTagAt(page, i, "script"):
			state, dashes = doubleEscaped, 0
			i += len("<script")
		case c == '<' && state == doubleEscaped && endTagAt(page, i, "script"):
			state, dashes = escaped, 0
			i += len("</script")
		default:
			dashes = 0
			i++
		}
	}
	return n, n
}

// endTagAt reports whether page has the end tag of name at i: "</", the
// name in any case, and a space, '/' or '>' after it.
func endTagAt(page string, i int, name string) bool {
	return strings.HasPrefix(page[i:], "</") && nameAt(page, i+2, name)
}

// startTagAt reports whether page has the start tag of name at i.
func startTagAt(page string, i int, name string) bool {
	return strings.HasPrefix(page[i:], "<") && nameAt(page, i+1, name)
}

// nameAt reports whether page has the tag name name, in any case, at i,
// followed by a character that ends a tag name.
func nameAt(page string, i int, name string) bool {
	end := i + len(name)
	if end >= len(page) || !strings.EqualFold(page[i:end], name) {
		return false
	}
	c := page[end]
	return isTagSpace(c) || c == '/' || c == '>'
}

func isTagSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

func isASCIIAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// asciiLower lower-cases the ASCII letters of s, as the tokenizer does with
// tag and attribute names.
func asciiLower(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' }) {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// ---- what the page loads -----------------------------------------------------------

// scriptRef is a script the page loads from a URL: a script element's src or
// a modulepreload link's href.
type scriptRef struct {
	kind      string   // "script" or "modulepreload"
	src       string   // as written
	url       *url.URL // resolved against the page and its <base>; nil when it does not parse
	integrity string
	hasHash   bool // the integrity attribute is present
}

// name is how a finding names the script: its path on the target, or the
// URL it resolves to elsewhere.
func (r scriptRef) name(s *Scanner) string {
	switch {
	case r.url == nil:
		return fmt.Sprintf("%q", r.src)
	case s.sameOrigin(r.url):
		return r.url.EscapedPath()
	default:
		return r.url.String()
	}
}

// inlineScript is a script element with no src.
type inlineScript struct {
	nonce    string
	hasNonce bool
	text     string
}

// pageScripts sorts the page's tags into the scripts it loads by URL, its
// inline scripts, and the base URL they resolve against (the page's own, or
// the first <base href>).
func pageScripts(tags []pageTag, page *url.URL) (refs []scriptRef, inline []inlineScript, base *url.URL) {
	base = page
	for _, t := range tags {
		if href, ok := t.attrs["href"]; t.name == "base" && ok {
			if u, err := page.Parse(cleanURL(href)); err == nil {
				base = u
			}
			break
		}
	}
	resolve := func(src string) *url.URL {
		u, err := base.Parse(cleanURL(src))
		if err != nil {
			return nil
		}
		u.Fragment = ""
		return u
	}
	for _, t := range tags {
		switch t.name {
		case "script":
			kind := scriptKind(t.attrs["type"])
			src, external := t.attrs["src"]
			switch {
			case kind == "":
				// A data block: not run, and its src not fetched.
			case external && cleanURL(src) == "":
				// An empty src runs nothing.
			case external && (kind == "classic" || kind == "module"):
				integrity, has := t.attrs["integrity"]
				refs = append(refs, scriptRef{kind: "script", src: src, url: resolve(src), integrity: integrity, hasHash: has})
			case !external:
				nonce, has := t.attrs["nonce"]
				inline = append(inline, inlineScript{nonce: nonce, hasNonce: has, text: t.text})
			}
		case "link":
			href, ok := t.attrs["href"]
			if !ok || cleanURL(href) == "" || !slices.ContainsFunc(strings.Fields(t.attrs["rel"]), func(rel string) bool {
				return strings.EqualFold(rel, "modulepreload")
			}) {
				continue
			}
			integrity, has := t.attrs["integrity"]
			refs = append(refs, scriptRef{kind: "modulepreload", src: href, url: resolve(href), integrity: integrity, hasHash: has})
		}
	}
	return refs, inline, base
}

// cleanURL trims a URL attribute as a browser does: leading and trailing
// spaces and control characters go, and so do tabs and line breaks inside.
func cleanURL(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

// scriptKind returns what a script element of this type attribute is: a
// "classic" or "module" script, an "importmap" or "speculationrules" block,
// which also need the nonce inline, or "" for a data block a browser does
// not run.
func scriptKind(typ string) string {
	essence, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(typ)), ";")
	switch essence = strings.TrimSpace(essence); essence {
	case "", "application/ecmascript", "application/javascript", "application/x-ecmascript", "application/x-javascript",
		"text/ecmascript", "text/javascript", "text/javascript1.0", "text/javascript1.1", "text/javascript1.2",
		"text/javascript1.3", "text/javascript1.4", "text/javascript1.5", "text/jscript", "text/livescript",
		"text/x-ecmascript", "text/x-javascript":
		return "classic"
	case "module", "importmap", "speculationrules":
		return essence
	}
	return ""
}

// sameOrigin reports whether u is on the target's origin: the same scheme,
// host name and port.
func (s *Scanner) sameOrigin(u *url.URL) bool {
	return strings.EqualFold(u.Scheme, s.base.Scheme) && originHostPort(u) == originHostPort(s.base)
}

// originHostPort returns u's host name, lower case, and port, the scheme's
// default when it names none.
func originHostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

// cspNonces returns the script nonces the Content-Security-Policy headers
// announce.
func cspNonces(h http.Header) []string {
	var nonces []string
	for _, p := range parseCSP(h.Values("Content-Security-Policy")) {
		sources, ok := p["script-src-elem"]
		if !ok {
			_, sources, _ = p.effective("script-src", true)
		}
		for _, src := range sources {
			if len(src) > len("'nonce-'") && strings.EqualFold(src[:len("'nonce-")], "'nonce-") && strings.HasSuffix(src, "'") {
				nonces = append(nonces, src[len("'nonce-"):len(src)-1])
			}
		}
	}
	return nonces
}

// ---- integrity metadata ------------------------------------------------------------

// integrityAlgorithm is a hash algorithm browsers support for integrity.
type integrityAlgorithm struct {
	name string
	new  func() hash.Hash
}

// integrityAlgorithms are the supported algorithms, weakest first.
var integrityAlgorithms = []integrityAlgorithm{
	{"sha256", sha256.New},
	{"sha384", sha512.New384},
	{"sha512", sha512.New},
}

// strongestHashes parses an integrity value as a browser does: of its
// whitespace-separated hashes, only those of the strongest supported
// algorithm count, and the resource must match one of them. It returns that
// algorithm and those digests ("" and none when no hash uses a supported
// algorithm); a digest that is not base64 is kept as nil, which nothing
// matches.
func strongestHashes(value string) (alg string, digests [][]byte) {
	rank := -1
	for _, token := range strings.Fields(value) {
		token, _, _ = strings.Cut(token, "?")
		name, encoded, ok := strings.Cut(token, "-")
		if !ok {
			continue
		}
		i := slices.IndexFunc(integrityAlgorithms, func(a integrityAlgorithm) bool { return strings.EqualFold(a.name, name) })
		switch {
		case i < 0 || i < rank:
			continue
		case i > rank:
			rank, digests = i, nil
		}
		digests = append(digests, decodeDigest(encoded))
	}
	if rank < 0 {
		return "", nil
	}
	return integrityAlgorithms[rank].name, digests
}

// decodeDigest decodes a base64 digest, in the standard or URL-safe
// alphabet, padded or not; nil when it is none of them.
func decodeDigest(encoded string) []byte {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(encoded); err == nil {
			return b
		}
	}
	return nil
}

// digestOf hashes body with the named integrity algorithm.
func digestOf(alg string, body []byte) []byte {
	for _, a := range integrityAlgorithms {
		if a.name == alg {
			h := a.new()
			_, _ = h.Write(body)
			return h.Sum(nil)
		}
	}
	return nil
}

// kulaETag reports whether etag is Kula's tag for a static file (a weak tag
// of the first 16 bytes of the SHA-256 of the bytes it serves) and, if so,
// whether body is those bytes.
func kulaETag(etag string, body []byte) (matches, known bool) {
	tag, ok := strings.CutPrefix(etag, `W/"`)
	if !ok || !strings.HasSuffix(tag, `"`) {
		return false, false
	}
	want, err := hex.DecodeString(strings.TrimSuffix(tag, `"`))
	if err != nil || len(want) != 16 {
		return false, false
	}
	sum := sha256.Sum256(body)
	return string(sum[:16]) == string(want), true
}

// ---- the check -----------------------------------------------------------------------

// runIntegrityChecks judges SRI-001 and SRI-002 over the UI page.
func runIntegrityChecks(s *Scanner) []Finding {
	both := func(st Status, detail string) []Finding {
		return []Finding{finding("SRI-001", SevInfo, st, detail), finding("SRI-002", SevInfo, st, detail)}
	}
	page := s.fetchBounded(s.urlFor("/"), maxPageBytes)
	contentType := ""
	if page.header != nil {
		contentType = page.header.Get("Content-Type")
	}
	switch {
	case page.err != nil:
		return both(StatusError, "could not GET the UI page: "+page.err.Error())
	case hostCheckRefusal(httpResult{status: page.status, body: string(page.body)}):
		return both(StatusSkip, s.refusedHostReason())
	case page.status != http.StatusOK:
		return both(StatusSkip, fmt.Sprintf("/ answered HTTP %d, so there is no UI page whose scripts to check (is web.ui off?).", page.status))
	case page.readErr != nil:
		return both(StatusError, "could not read the UI page: "+page.readErr.Error())
	case page.truncated:
		return both(StatusError, fmt.Sprintf("the UI page is larger than %d MiB; its scripts were not read.", maxPageBytes>>20))
	case !strings.Contains(strings.ToLower(contentType), "html"):
		return both(StatusSkip, fmt.Sprintf("/ is not an HTML page (Content-Type %q), so it loads no scripts.", contentType))
	}

	tags, more := scanPageTags(string(page.body), maxPageTags)
	if more {
		return both(StatusError, fmt.Sprintf("the UI page has more than %d script, link and base tags; it was not judged.", maxPageTags))
	}
	pageURL, err := url.Parse(s.urlFor("/"))
	if err != nil {
		return both(StatusError, "could not parse the page URL: "+err.Error())
	}
	refs, inline, base := pageScripts(tags, pageURL)
	if len(refs) == 0 && len(inline) == 0 {
		return both(StatusSkip, "the UI page loads no scripts.")
	}
	return []Finding{s.scriptsCarryIntegrity(refs, inline, base, cspNonces(page.header)), s.scriptsMatchIntegrity(refs)}
}

// scriptsCarryIntegrity judges SRI-001: every script the page loads by URL is
// same-origin with a supported integrity hash, and every inline script
// carries a nonce the CSP announces.
func (s *Scanner) scriptsCarryIntegrity(refs []scriptRef, inline []inlineScript, base *url.URL, nonces []string) Finding {
	var foreign, unhashed, unnonced []string
	algs := map[string]int{}
	for _, r := range refs {
		alg, _ := strongestHashes(r.integrity)
		switch {
		case r.url == nil || !s.sameOrigin(r.url):
			foreign = append(foreign, r.kind+" "+r.name(s))
		case !r.hasHash:
			unhashed = append(unhashed, r.kind+" "+r.name(s)+" (no integrity)")
		case alg == "":
			unhashed = append(unhashed, fmt.Sprintf("%s %s (integrity %q has no sha256, sha384 or sha512 hash)", r.kind, r.name(s), truncate(r.integrity, 40)))
		default:
			algs[alg]++
		}
	}
	nonceNote := ""
	if len(nonces) == 0 {
		nonceNote = "; the CSP announces no nonce (see HDR-003), so the inline scripts were not judged"
	} else {
		for i, script := range inline {
			excerpt := truncate(strings.Join(strings.Fields(script.text), " "), 40)
			switch {
			case !script.hasNonce:
				unnonced = append(unnonced, fmt.Sprintf("inline script #%d (%q) carries no nonce", i+1, excerpt))
			case !slices.Contains(nonces, script.nonce):
				unnonced = append(unnonced, fmt.Sprintf("inline script #%d (%q) carries a nonce the CSP does not announce", i+1, excerpt))
			}
		}
	}

	baseNote := ""
	if base != nil && !s.sameOrigin(base) {
		baseNote = " The page's <base href> points to " + base.String() + ", which moves every relative script there."
	}
	if problems := slices.Concat(foreign, unhashed, unnonced); len(problems) > 0 {
		sev := SevMedium
		if len(foreign) > 0 {
			sev = SevHigh
		}
		return finding("SRI-001", sev, StatusFail,
			fmt.Sprintf("the UI page loads scripts a browser cannot check: %s.%s Kula's own page names only same-origin scripts, each with a sha384 hash, and gives its inline scripts the CSP nonce; a proxy or CDN in front may inject or rewrite them.",
				strings.Join(problems, "; "), baseNote)).
			withRemediation("Turn off script injection and HTML rewriting (analytics snippets, Rocket Loader, minification of HTML) for Kula in the proxy or CDN in front, and do not let it cache the page, which carries a fresh nonce on every response.")
	}
	var used []string
	for _, a := range integrityAlgorithms {
		if algs[a.name] > 0 {
			used = append(used, a.name)
		}
	}
	return finding("SRI-001", SevMedium, StatusPass,
		fmt.Sprintf("the UI page's %d scripts and module preloads are same-origin with %s integrity, and its %d inline scripts carry the CSP nonce%s.",
			len(refs), strings.Join(used, "/"), len(inline), nonceNote))
}

// scriptsMatchIntegrity judges SRI-002: each same-origin script with a
// supported integrity hash, fetched as a browser fetches it, matches it.
func (s *Scanner) scriptsMatchIntegrity(refs []scriptRef) Finding {
	// The scripts to fetch, once each, in page order, with every integrity
	// the page names them with.
	var order []string
	byURL := map[string][]scriptRef{}
	for _, r := range refs {
		if alg, _ := strongestHashes(r.integrity); r.url == nil || !s.sameOrigin(r.url) || alg == "" {
			continue
		}
		key := r.url.String()
		if _, seen := byURL[key]; !seen {
			order = append(order, key)
		}
		byURL[key] = append(byURL[key], r)
	}
	switch {
	case len(order) == 0:
		return finding("SRI-002", SevInfo, StatusSkip,
			"no same-origin script on the UI page carries an integrity hash to compare (see SRI-001).")
	case len(order) > maxAssetFetches:
		return finding("SRI-002", SevInfo, StatusError,
			fmt.Sprintf("the UI page names %d scripts, more than the %d the scanner fetches; they were not compared.", len(order), maxAssetFetches))
	}

	var mismatched, evidence, unreadable, unfetched []string
	matched := 0
	for _, key := range order {
		group := byURL[key]
		name := group[0].name(s)
		target := *group[0].url
		target.Scheme, target.Host = s.base.Scheme, s.base.Host
		f := s.fetchBounded(target.String(), maxAssetBytes)
		switch {
		case f.err != nil:
			unfetched = append(unfetched, name+" ("+f.err.Error()+")")
			continue
		case f.status != http.StatusOK:
			unfetched = append(unfetched, fmt.Sprintf("%s (HTTP %d)", name, f.status))
			continue
		case f.readErr != nil:
			unreadable = append(unreadable, name+": "+f.readErr.Error())
			continue
		case f.truncated:
			unreadable = append(unreadable, fmt.Sprintf("%s: larger than %d MiB, not hashed", name, maxAssetBytes>>20))
			continue
		}
		ok := true
		for _, r := range group {
			alg, digests := strongestHashes(r.integrity)
			got := digestOf(alg, f.body)
			if !slices.ContainsFunc(digests, func(d []byte) bool { return string(d) == string(got) }) {
				ok = false
				evidence = append(evidence, mismatchEvidence(name, alg, got, r.integrity, f))
			}
		}
		if ok {
			matched++
		} else {
			mismatched = append(mismatched, name)
		}
	}

	notes := ""
	if len(unfetched) > 0 {
		notes = "; not fetched: " + strings.Join(unfetched, ", ")
	}
	switch {
	case len(mismatched) > 0:
		return finding("SRI-002", SevHigh, StatusFail,
			fmt.Sprintf("%d of the %d scripts the UI page names do not match their integrity hash as served, so a browser refuses them and the dashboard breaks: %s. Kula hashes the bytes it serves; something between it and the scanner changed them%s.",
				len(mismatched), len(order), strings.Join(mismatched, ", "), notes)).
			withEvidence("%s", strings.Join(evidence, " | ")).
			withRemediation("Turn off asset rewriting for Kula in the proxy or CDN in front (minification, script injection, Rocket Loader, re-encoding); Kula minifies its own assets before it hashes them.")
	case len(unreadable) > 0:
		return finding("SRI-002", SevInfo, StatusError,
			"these scripts could not be read whole, so their hashes were not compared: "+strings.Join(unreadable, ", ")+notes+".")
	case matched == 0:
		return finding("SRI-002", SevInfo, StatusError,
			"none of the scripts the UI page names could be fetched"+notes+".")
	}
	return finding("SRI-002", SevHigh, StatusPass,
		fmt.Sprintf("the %d scripts the UI page names match their integrity hashes as served, after content decoding%s.", matched, notes))
}

// mismatchEvidence says how a script's bytes differ from the hash the page
// lists: what was received, and, when Kula's ETag came with it, whether the
// bytes are still the ones Kula served (then the page's hash was changed)
// or not (then the script was).
func mismatchEvidence(name, alg string, got []byte, listed string, f fetched) string {
	received := fmt.Sprintf("%d bytes", len(f.body))
	if f.encoding != "" {
		received += ", " + f.encoding + "-decoded"
	}
	ev := fmt.Sprintf("%s: received %s hashing to %s-%s; the page lists %q", name, received, alg,
		base64.StdEncoding.EncodeToString(got), truncate(listed, 120))
	if declared := f.header.Get("Content-Length"); declared != "" && f.encoding == "" && declared != fmt.Sprint(len(f.body)) {
		ev += "; Content-Length says " + declared
	}
	if same, known := kulaETag(f.header.Get("ETag"), f.body); known {
		if same {
			ev += "; the bytes still match the ETag Kula sent, so the page's hash, not the script, was changed"
		} else {
			ev += "; the bytes no longer match the ETag Kula sent: the script was changed after Kula served it"
		}
	}
	return ev
}
