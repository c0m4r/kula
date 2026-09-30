package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"kula/internal/config"
)

// embeddedWOFF2 returns the URL path of an embedded WOFF2 font, or "" when
// the tree ships none (addons/packaging/remove_fonts.sh deletes them all).
func embeddedWOFF2(t *testing.T) string {
	t.Helper()
	var found string
	err := fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if err == nil && found == "" && !d.IsDir() && strings.HasSuffix(path, ".woff2") {
			found = strings.TrimPrefix(path, "static")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Every font a stylesheet points at must be embedded, or the dashboard
// silently falls back to system fonts. The packaging helpers remove the game
// stylesheet and the fonts, so the sheets and fonts are taken from the tree.
func TestStylesheetFontsAreEmbedded(t *testing.T) {
	fontURL := regexp.MustCompile(`url\('(fonts/[^']+)'\)`)
	sheets, err := fs.Glob(staticFS, "static/*.css")
	if err != nil || len(sheets) == 0 {
		t.Fatalf("no embedded stylesheets (err %v)", err)
	}
	_, statErr := fs.Stat(staticFS, "static/fonts")
	fontsShipped := statErr == nil
	for _, sheet := range sheets {
		css, err := staticFS.ReadFile(sheet)
		if err != nil {
			t.Fatal(err)
		}
		matches := fontURL.FindAllStringSubmatch(string(css), -1)
		// Guards the pattern itself: a sheet that stops matching it would
		// otherwise pass without checking anything.
		if len(matches) == 0 && fontsShipped {
			t.Errorf("%s references no fonts", sheet)
		}
		for _, match := range matches {
			if _, err := fs.Stat(staticFS, "static/"+match[1]); err != nil {
				t.Errorf("%s references %s, which is not embedded: %v", sheet, match[1], err)
			}
		}
	}
}

func TestWOFF2FontServedWithoutGzip(t *testing.T) {
	font := embeddedWOFF2(t)
	if font == "" {
		t.Skip("no WOFF2 font is embedded (fonts removed for packaging)")
	}
	s := NewServer(config.WebConfig{UI: true, EnableCompression: true}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})
	handler := s.buildHandler()

	req := httptest.NewRequest(http.MethodGet, font, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET font = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "font/woff2" {
		t.Errorf("Content-Type = %q, want font/woff2", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none for an already-compressed font", got)
	}
	if !strings.HasPrefix(rec.Body.String(), "wOF2") {
		t.Error("font body is not WOFF2")
	}

	// Other assets are still compressed.
	req = httptest.NewRequest(http.MethodGet, "/style.css", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("style.css Content-Encoding = %q, want gzip", got)
	}
}

// Chart.js must stay off the render path: every Chart.js script is deferred,
// and deferred scripts run in document order before the app module that
// uses the Chart global.
func TestChartJSScriptsDeferred(t *testing.T) {
	page, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	source := string(page)
	scripts := regexp.MustCompile(`<script\b[^>]*>`).FindAllStringIndex(source, -1)
	appModule, chartScripts := -1, 0
	for _, loc := range scripts {
		tag := source[loc[0]:loc[1]]
		if strings.Contains(tag, `src="js/app/main.js"`) {
			appModule = loc[0]
		}
		if !strings.Contains(tag, `src="js/chartjs/`) {
			continue
		}
		chartScripts++
		if !regexp.MustCompile(`\sdefer[\s>]`).MatchString(tag) {
			t.Errorf("Chart.js script is render-blocking, want defer: %s", tag)
		}
		if appModule >= 0 {
			t.Errorf("Chart.js script loads after the app module: %s", tag)
		}
	}
	if chartScripts == 0 || appModule < 0 {
		t.Fatalf("found %d Chart.js scripts and app module at %d", chartScripts, appModule)
	}
}

// The theme is resolved by a nonce'd inline script at the top of <body>, from
// the server default rendered into the page, before anything paints.
func TestIndexResolvesThemeBeforePaint(t *testing.T) {
	for _, tc := range []struct{ theme, want string }{
		{"light", `data-default-theme="light"`},
		{`"><script>alert(1)</script>`, `data-default-theme="&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;"`},
	} {
		s := NewServer(config.WebConfig{UI: true, Security: config.SecurityConfig{Headers: true}}, config.GlobalConfig{DefaultTheme: tc.theme}, nil, nil, t.TempDir(), config.OllamaConfig{})
		rec := httptest.NewRecorder()
		s.securityMiddleware(http.HandlerFunc(s.handleIndex)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		body := rec.Body.String()

		start := strings.Index(body, "<body ")
		if start < 0 || !strings.HasPrefix(body[start:], "<body "+tc.want+">") {
			t.Fatalf("default theme %q: body tag not rendered as %s", tc.theme, tc.want)
		}
		nonce := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(rec.Header().Get("Content-Security-Policy"))
		if nonce == nil {
			t.Fatal("CSP has no nonce")
		}
		// The template HTML-escapes the base64 nonce ("+" becomes "&#43;").
		rest := html.UnescapeString(body[start:])
		script := strings.Index(rest, `<script nonce="`+nonce[1]+`">`)
		content := strings.Index(rest, "<div")
		if script < 0 || content < 0 || script > content {
			t.Fatalf("theme script (at %d) must precede the first element (at %d) and carry the CSP nonce", script, content)
		}
		if !strings.Contains(rest[script:content], "classList.add('light-mode')") {
			t.Error("theme script does not apply the light theme")
		}
	}
}

// Static assets revalidate with an ETag of the served bytes: a match is a 304
// with no body (and no gzip framing), anything else is the full asset.
func TestStaticAssetsRevalidate(t *testing.T) {
	paths := []string{"/js/app/main.js", "/style.css"}
	if font := embeddedWOFF2(t); font != "" {
		paths = append(paths, font)
	}
	for _, compress := range []bool{true, false} {
		s := NewServer(config.WebConfig{UI: true, MinifyAssets: true, EnableCompression: compress}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})
		handler := s.buildHandler()
		get := func(path, ifNoneMatch string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			if ifNoneMatch != "" {
				req.Header.Set("If-None-Match", ifNoneMatch)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec
		}

		for _, path := range paths {
			served, err := s.readStatic("static" + path)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(served)
			want := `W/"` + hex.EncodeToString(sum[:16]) + `"`

			rec := get(path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("compress=%v GET %s = %d", compress, path, rec.Code)
			}
			etag := rec.Header().Get("ETag")
			if etag != want {
				t.Errorf("compress=%v %s ETag = %q, want %q (hash of the served bytes)", compress, path, etag, want)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Errorf("compress=%v %s Cache-Control = %q, want no-cache", compress, path, got)
			}
			body := rec.Body.Bytes()
			if rec.Header().Get("Content-Encoding") == "gzip" {
				zr, err := gzip.NewReader(bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if body, err = io.ReadAll(zr); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(body, served) {
				t.Errorf("compress=%v %s body differs from the served asset", compress, path)
			}

			for _, match := range []string{etag, strings.TrimPrefix(etag, "W/"), `"other", ` + etag, "*"} {
				rec = get(path, match)
				if rec.Code != http.StatusNotModified {
					t.Errorf("compress=%v %s If-None-Match %s = %d, want 304", compress, path, match, rec.Code)
				}
				if rec.Body.Len() != 0 || rec.Header().Get("Content-Encoding") != "" {
					t.Errorf("compress=%v %s 304 carries a body (%d bytes) or Content-Encoding %q", compress, path, rec.Body.Len(), rec.Header().Get("Content-Encoding"))
				}
				if rec.Header().Get("ETag") != etag {
					t.Errorf("compress=%v %s 304 without its ETag", compress, path)
				}
			}
			if rec = get(path, `W/"stale"`); rec.Code != http.StatusOK {
				t.Errorf("compress=%v %s stale If-None-Match = %d, want 200", compress, path, rec.Code)
			}
		}

		rec := get("/style.css", "")
		vary := strings.Join(rec.Header().Values("Vary"), ",")
		if compress != strings.Contains(vary, "Accept-Encoding") {
			t.Errorf("compress=%v Vary = %q", compress, vary)
		}
	}

	// The tag follows the served bytes, so minifying changes it.
	minified := NewServer(config.WebConfig{UI: true, MinifyAssets: true}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})
	plain := NewServer(config.WebConfig{UI: true}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})
	if minified.etags["static/js/app/main.js"] == plain.etags["static/js/app/main.js"] {
		t.Error("minified and original main.js share an ETag")
	}
}

func TestEtagMatches(t *testing.T) {
	const tag = `W/"abc"`
	for header, want := range map[string]bool{
		`W/"abc"`:         true,
		`"abc"`:           true,
		`"x", W/"abc"`:    true,
		`*`:               true,
		`"abcd"`:          false,
		`W/"ab"`:          false,
		`"x",  "y"`:       false,
		`W/"abc"garbage"`: false,
	} {
		if got := etagMatches(header, tag); got != want {
			t.Errorf("etagMatches(%q) = %v, want %v", header, got, want)
		}
	}
}
