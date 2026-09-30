package web

import (
	"html"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"kula/internal/config"
)

// Every font a stylesheet points at must be embedded, or the dashboard
// silently falls back to system fonts.
func TestStylesheetFontsAreEmbedded(t *testing.T) {
	fontURL := regexp.MustCompile(`url\('(fonts/[^']+)'\)`)
	for _, sheet := range []string{"static/style.css", "static/game.css"} {
		css, err := staticFS.ReadFile(sheet)
		if err != nil {
			t.Fatal(err)
		}
		matches := fontURL.FindAllStringSubmatch(string(css), -1)
		if len(matches) == 0 {
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
	s := NewServer(config.WebConfig{UI: true, EnableCompression: true}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})
	handler := s.buildHandler()

	req := httptest.NewRequest(http.MethodGet, "/fonts/Inter/Inter-VariableFont_opsz,wght.woff2", nil)
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
