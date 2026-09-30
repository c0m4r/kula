package web

import (
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
