package web

import (
	"kula-szpiegula/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var defaultArgonParams = config.Argon2Config{
	Time:    1,
	Memory:  64 * 1024,
	Threads: 4,
}

func TestHashPasswordDeterminism(t *testing.T) {
	hash1 := HashPassword("testpass", "salt123", defaultArgonParams)
	hash2 := HashPassword("testpass", "salt123", defaultArgonParams)
	if hash1 != hash2 {
		t.Errorf("HashPassword not deterministic: %q != %q", hash1, hash2)
	}
}

func TestHashPasswordDifferentSalts(t *testing.T) {
	hash1 := HashPassword("testpass", "salt1", defaultArgonParams)
	hash2 := HashPassword("testpass", "salt2", defaultArgonParams)
	if hash1 == hash2 {
		t.Error("Same password with different salts should produce different hashes")
	}
}

func TestHashPasswordDifferentPasswords(t *testing.T) {
	hash1 := HashPassword("pass1", "same_salt", defaultArgonParams)
	hash2 := HashPassword("pass2", "same_salt", defaultArgonParams)
	if hash1 == hash2 {
		t.Error("Different passwords with same salt should produce different hashes")
	}
}

func TestHashPasswordLength(t *testing.T) {
	hash := HashPassword("test", "salt", defaultArgonParams)
	// Argon2id produces 256-bit hash = 32 bytes = 64 hex chars based on keyLen=32
	if len(hash) != 64 {
		t.Errorf("Hash length = %d, want 64 hex chars (Argon2id 256-bit)", len(hash))
	}
}

func TestGenerateSalt(t *testing.T) {
	salt1, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt() error: %v", err)
	}
	salt2, err := GenerateSalt()
	if err != nil {
		t.Fatalf("GenerateSalt() error: %v", err)
	}
	if salt1 == salt2 {
		t.Error("Two GenerateSalt() calls should produce different values")
	}
	// 32 bytes = 64 hex chars
	if len(salt1) != 64 {
		t.Errorf("Salt length = %d, want 64 hex chars", len(salt1))
	}
}

func TestValidateCredentialsDisabled(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{Enabled: false}, "", false)
	if !am.ValidateCredentials("any", "any") {
		t.Error("With auth disabled, ValidateCredentials should return true")
	}
}

func TestValidateCredentialsCorrect(t *testing.T) {
	salt, _ := GenerateSalt()
	hash := HashPassword("secret", salt, defaultArgonParams)
	am := NewAuthManager(config.AuthConfig{
		Enabled:      true,
		Username:     "admin",
		PasswordHash: hash,
		PasswordSalt: salt,
		Argon2:       defaultArgonParams,
	}, "", false)
	if !am.ValidateCredentials("admin", "secret") {
		t.Error("Valid credentials should pass")
	}
}

func TestValidateCredentialsWrong(t *testing.T) {
	salt, _ := GenerateSalt()
	hash := HashPassword("secret", salt, defaultArgonParams)
	am := NewAuthManager(config.AuthConfig{
		Enabled:      true,
		Username:     "admin",
		PasswordHash: hash,
		PasswordSalt: salt,
		Argon2:       defaultArgonParams,
	}, "", false)
	if am.ValidateCredentials("admin", "wrong") {
		t.Error("Wrong password should fail")
	}
	if am.ValidateCredentials("wrong", "secret") {
		t.Error("Wrong username should fail")
	}
}

func TestSessionLifecycle(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Hour,
	}, "", false)

	token, err := am.CreateSession("admin", "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	if token == "" {
		t.Fatal("CreateSession returned empty token")
	}
	if !am.ValidateSession(token, "127.0.0.1", "test-agent") {
		t.Error("Newly created session should be valid")
	}
	if am.ValidateSession("invalid_token", "127.0.0.1", "test-agent") {
		t.Error("Invalid token should not validate")
	}
}

func TestSessionExpiry(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Millisecond, // very short timeout
	}, "", false)

	token, _ := am.CreateSession("admin", "127.0.0.1", "test-agent")
	time.Sleep(5 * time.Millisecond)
	if am.ValidateSession(token, "127.0.0.1", "test-agent") {
		t.Error("Expired session should not validate")
	}
}

func TestCleanupSessions(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Millisecond,
	}, "", false)

	_, _ = am.CreateSession("user1", "127.0.0.1", "test-agent")
	_, _ = am.CreateSession("user2", "127.0.0.1", "test-agent")
	time.Sleep(5 * time.Millisecond)
	am.CleanupSessions()

	am.mu.RLock()
	count := len(am.sessions)
	am.mu.RUnlock()

	if count != 0 {
		t.Errorf("After cleanup, sessions count = %d, want 0", count)
	}
}

func TestAuthMiddlewareDisabled(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{Enabled: false}, "", false)
	handler := am.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Auth disabled: status = %d, want 200", rec.Code)
	}
}

func TestAuthMiddlewareNoToken(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Hour,
	}, "", false)
	handler := am.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("No token: status = %d, want 401", rec.Code)
	}
}

func TestAuthMiddlewareValidCookie(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Hour,
	}, "", false)
	token, _ := am.CreateSession("admin", "127.0.0.1", "mock-agent")

	handler := am.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("User-Agent", "mock-agent")
	req.AddCookie(&http.Cookie{Name: "kula_session", Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Valid cookie: status = %d, want 200", rec.Code)
	}
}

func TestAuthMiddlewareBearerToken(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Hour,
	}, "", false)
	token, _ := am.CreateSession("admin", "127.0.0.1", "mock-agent")

	handler := am.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("User-Agent", "mock-agent")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Bearer token: status = %d, want 200", rec.Code)
	}
}

func TestSessionHashingOnDisk(t *testing.T) {
	tmpDir := t.TempDir()

	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Hour,
	}, tmpDir, false)

	token, err := am.CreateSession("admin", "127.0.0.1", "agent")
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	if err := am.SaveSessions(); err != nil {
		t.Fatalf("SaveSessions error: %v", err)
	}

	// Read sessions.json directly
	data, err := os.ReadFile(filepath.Join(tmpDir, "sessions.json"))
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("sessions.json is empty")
	}

	if contains(string(data), token) {
		t.Error("sessions.json contains the plaintext token! hashing failed or not implemented for storage")
	}

	hashed := hashToken(token)
	if !contains(string(data), hashed) {
		t.Error("sessions.json does not contain the hashed token")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(s) > len(substr) && stringContains(s, substr)))
}

func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
func TestGetClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		trustProxy bool
		want       string
	}{
		{
			name:       "Direct connection, no trust",
			remoteAddr: "1.2.3.4:1234",
			xff:        "",
			trustProxy: false,
			want:       "1.2.3.4",
		},
		{
			name:       "XFF present, no trust",
			remoteAddr: "1.2.3.4:1234",
			xff:        "10.0.0.1",
			trustProxy: false,
			want:       "1.2.3.4",
		},
		{
			name:       "XFF present, trusted",
			remoteAddr: "1.2.3.4:1234",
			xff:        "10.0.0.1",
			trustProxy: true,
			want:       "10.0.0.1",
		},
		{
			name:       "XFF list, trusted",
			remoteAddr: "1.2.3.4:1234",
			xff:        "10.0.0.1, 10.0.0.2, 1.2.3.4",
			trustProxy: true,
			want:       "10.0.0.1",
		},
		{
			name:       "Invalid RemoteAddr format",
			remoteAddr: "not-an-ip",
			xff:        "",
			trustProxy: false,
			want:       "not-an-ip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}

			got := getClientIP(req, tt.trustProxy)
			if got != tt.want {
				t.Errorf("%s: getClientIP() = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}
func TestValidateOrigin(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{}, "", false)

	tests := []struct {
		name    string
		origin  string
		referer string
		host    string
		want    bool
	}{
		{
			name:   "Matching Origin",
			origin: "http://localhost:8080",
			host:   "localhost:8080",
			want:   true,
		},
		{
			name:   "Mismatched Origin",
			origin: "http://evil.com",
			host:   "localhost:8080",
			want:   false,
		},
		{
			name:    "Matching Referer fallback",
			referer: "http://localhost:8080/dashboard",
			host:    "localhost:8080",
			want:    true,
		},
		{
			name:   "Empty headers now reject",
			origin: "",
			host:   "localhost:8080",
			want:   false,
		},
		{
			name:   "Prefix bypass attempt",
			origin: "http://localhost:8080.evil.com",
			host:   "localhost:8080",
			want:   false,
		},
		{
			name:   "Scheme mismatch robustness",
			origin: "https://localhost:8080", // Browser sends https, internal might be http
			host:   "localhost:8080",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.referer != "" {
				req.Header.Set("Referer", tt.referer)
			}

			if got := am.ValidateOrigin(req); got != tt.want {
				t.Errorf("ValidateOrigin() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCSRFMiddleware(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{}, "", false)
	handler := am.CSRFMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("Blocked invalid origin", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/test", nil)
		req.Host = "localhost:8080"
		req.Header.Set("Origin", "http://evil.com")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Invalid origin POST: status = %d, want 403", rec.Code)
		}
	})

	t.Run("Blocked empty origin", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/api/test", nil)
		req.Host = "localhost:8080"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("Empty origin POST: status = %d, want 403", rec.Code)
		}
	})

	t.Run("Allowed GET with empty origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/test", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET should be allowed: status = %d, want 200", rec.Code)
		}
	})
}
