package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kula/internal/config"
)

func TestTemplateInjection(t *testing.T) {
	s := NewServer(config.WebConfig{}, config.GlobalConfig{}, nil, nil, t.TempDir())
	
	// Create a recorder to capture the response
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	
	// Wrap with securityMiddleware to get the nonce
	handler := s.securityMiddleware(http.HandlerFunc(s.handleIndex))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", rec.Code)
	}

	body := html.UnescapeString(rec.Body.String())

	// Verify nonce is in CSP header
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "nonce-") {
		t.Errorf("CSP header missing nonce: %s", csp)
	}

	// Extract nonce from CSP
	parts := strings.Split(csp, "'nonce-")
	if len(parts) < 2 {
		t.Fatalf("Could not parse nonce from CSP: %s", csp)
	}
	nonce := strings.Split(parts[1], "'")[0]

	// Verify nonce is injected into HTML
	if !strings.Contains(body, `nonce="`+nonce+`"`) {
		t.Errorf("HTML body missing injected nonce %s", nonce)
	}

	// Verify SRI is injected into HTML
	sri := s.sriHashes["js/app/main.js"]
	if sri == "" {
		t.Error("SRI hash for js/app/main.js is empty in server")
	}
	if !strings.Contains(body, `integrity="`+sri+`"`) {
		t.Errorf("HTML body missing injected SRI %s", sri)
	}
}

func TestGameTemplateInjection(t *testing.T) {
	s := NewServer(config.WebConfig{}, config.GlobalConfig{}, nil, nil, t.TempDir())
	
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/game.html", nil)
	
	handler := s.securityMiddleware(http.HandlerFunc(s.handleGame))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", rec.Code)
	}

	body := html.UnescapeString(rec.Body.String())
	
	// Verify SRI for game.js
	sri := s.sriHashes["game.js"]
	if sri == "" {
		t.Error("SRI hash for game.js is empty in server")
	}
	if !strings.Contains(body, `integrity="`+sri+`"`) {
		t.Errorf("Game HTML body missing injected SRI %s", sri)
	}
}

func TestHandleHealth(t *testing.T) {
	s := NewServer(config.WebConfig{}, config.GlobalConfig{}, nil, nil, t.TempDir())

	for _, path := range []string{"/health", "/status"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)

			http.HandlerFunc(s.handleHealth).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("Expected status 200 for %s, got %d", path, rec.Code)
			}
			if rec.Body.String() != "kula is healthy" {
				t.Fatalf("Expected body %q for %s, got %q", "kula is healthy", path, rec.Body.String())
			}
		})
	}
}
