package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"kula/internal/config"
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

// TestValidateCredentialsDisabled pins that disabled auth validates nothing:
// answering true let /api/login mint real sessions for any name while auth was
// off, and those sessions stayed valid once it was turned on.
func TestValidateCredentialsDisabled(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{Enabled: false}, "", false, config.SecurityConfig{OriginValidation: true})
	if am.ValidateCredentials("any", "any") {
		t.Error("With auth disabled, ValidateCredentials must return false")
	}
}

// TestLoadSessionsDropsUnconfiguredUsers covers sessions persisted for a name
// the config no longer has: a removed account, or one minted by an older
// version's auth-off login. They must not survive a restart.
func TestLoadSessionsDropsUnconfiguredUsers(t *testing.T) {
	tmpDir := t.TempDir()
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		Username:       "admin",
		Users:          []config.UserConfig{{Username: "alice"}},
		SessionTimeout: time.Hour,
	}, tmpDir, false, config.SecurityConfig{OriginValidation: true})
	removed := NewAuthManager(config.AuthConfig{Enabled: true, Username: "bob"}, "", false, config.SecurityConfig{})

	now := time.Now().UTC().Format(time.RFC3339)
	saved := `[` +
		`{"token":"primary","username":"admin",` + credentialJSON(t, am, "admin") + `"created_at":"` + now + `","expires_at":"2999-01-01T00:00:00Z"},` +
		`{"token":"extra","username":"alice",` + credentialJSON(t, am, "alice") + `"created_at":"` + now + `","expires_at":"2999-01-01T00:00:00Z"},` +
		`{"token":"removed","username":"bob",` + credentialJSON(t, removed, "bob") + `"created_at":"` + now + `","expires_at":"2999-01-01T00:00:00Z"},` +
		`{"token":"nameless","username":"","created_at":"` + now + `","expires_at":"2999-01-01T00:00:00Z"}` +
		`]`
	if err := os.WriteFile(filepath.Join(tmpDir, "sessions.json"), []byte(saved), 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	if err := am.LoadSessions(); err != nil {
		t.Fatalf("LoadSessions error: %v", err)
	}

	am.mu.RLock()
	defer am.mu.RUnlock()
	for _, kept := range []string{"primary", "extra"} {
		if _, ok := am.sessions[kept]; !ok {
			t.Errorf("session %q of a configured user was not restored", kept)
		}
	}
	for _, dropped := range []string{"removed", "nameless"} {
		if _, ok := am.sessions[dropped]; ok {
			t.Errorf("session %q of an unconfigured user was restored", dropped)
		}
	}
}

// credentialJSON returns the credential field of a session am issued for user,
// with its trailing comma, for hand-written sessions.json fixtures.
func credentialJSON(t *testing.T, am *AuthManager, user string) string {
	t.Helper()
	fingerprint, ok := am.credentialFingerprint(user)
	if !ok {
		t.Fatalf("%q is not configured", user)
	}
	return `"credential":"` + fingerprint + `",`
}

// TestLoadSessionsDropsChangedCredentials pins the binding between a session
// and the credential it was issued for: changing an account's password or salt
// signs its sessions out at the next start, and so does a session saved without
// a binding — by an older version, which also issued sessions with auth off.
func TestLoadSessionsDropsChangedCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	params := config.Argon2Config{Time: 1, Memory: 8, Threads: 1}
	authWith := func(password, salt string) config.AuthConfig {
		return config.AuthConfig{
			Enabled:        true,
			Username:       "admin",
			PasswordHash:   HashPassword(password, salt, params),
			PasswordSalt:   salt,
			Argon2:         params,
			SessionTimeout: time.Hour,
		}
	}
	before := NewAuthManager(authWith("old secret", "salt"), tmpDir, false, config.SecurityConfig{OriginValidation: true})
	if _, err := before.CreateSession("admin"); err != nil {
		t.Fatal(err)
	}
	if err := before.SaveSessions(); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(tmpDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	restored := func(cfg config.AuthConfig) int {
		t.Helper()
		am := NewAuthManager(cfg, tmpDir, false, config.SecurityConfig{OriginValidation: true})
		if err := am.LoadSessions(); err != nil {
			t.Fatalf("LoadSessions: %v", err)
		}
		return len(am.sessions)
	}

	if n := restored(authWith("old secret", "salt")); n != 1 {
		t.Fatalf("same credential: restored %d session(s), want 1", n)
	}
	if n := restored(authWith("new secret", "salt")); n != 0 {
		t.Errorf("after a password change: restored %d session(s), want 0", n)
	}
	if n := restored(authWith("old secret", "new salt")); n != 0 {
		t.Errorf("after a salt change: restored %d session(s), want 0", n)
	}

	var sessions []map[string]any
	if err := json.Unmarshal(saved, &sessions); err != nil || len(sessions) != 1 {
		t.Fatalf("sessions.json = %s (%v)", saved, err)
	}
	delete(sessions[0], "credential")
	unbound, _ := json.Marshal(sessions)
	if err := os.WriteFile(filepath.Join(tmpDir, "sessions.json"), unbound, 0600); err != nil {
		t.Fatal(err)
	}
	if n := restored(authWith("old secret", "salt")); n != 0 {
		t.Errorf("session saved without a credential binding: restored %d, want 0", n)
	}
}

// TestLoadSessionsSkippedWhenAuthDisabled pins that a run with auth off loads
// no sessions, so the empty set saved at shutdown clears any stored earlier.
func TestLoadSessionsSkippedWhenAuthDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sessions.json")
	saved := `[{"token":"minted","username":"admin","created_at":"` + time.Now().UTC().Format(time.RFC3339) + `","expires_at":"2999-01-01T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(saved), 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	am := NewAuthManager(config.AuthConfig{Enabled: false, Username: "admin"}, tmpDir, false, config.SecurityConfig{OriginValidation: true})
	if err := am.LoadSessions(); err != nil {
		t.Fatalf("LoadSessions error: %v", err)
	}
	if n := len(am.sessions); n != 0 {
		t.Fatalf("auth disabled: loaded %d session(s), want 0", n)
	}
	if err := am.SaveSessions(); err != nil {
		t.Fatalf("SaveSessions error: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "[]" {
		t.Fatalf("no sessions saved as %q (%v), want []", data, err)
	}

	enabled := NewAuthManager(config.AuthConfig{Enabled: true, Username: "admin", SessionTimeout: time.Hour}, tmpDir, false, config.SecurityConfig{OriginValidation: true})
	if err := enabled.LoadSessions(); err != nil {
		t.Fatalf("LoadSessions error: %v", err)
	}
	if n := len(enabled.sessions); n != 0 {
		t.Errorf("after an auth-off run, enabling auth restored %d session(s), want 0", n)
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
	}, "", false, config.SecurityConfig{OriginValidation: true})
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
	}, "", false, config.SecurityConfig{OriginValidation: true})
	if am.ValidateCredentials("admin", "wrong") {
		t.Error("Wrong password should fail")
	}
	if am.ValidateCredentials("wrong", "secret") {
		t.Error("Wrong username should fail")
	}
}

func TestValidateCredentialsConstantTime(t *testing.T) {
	// An unknown username must trigger the same Argon2id work as a known one;
	// otherwise the login response time leaks which usernames exist.
	salt := "timingsalt"
	params := config.Argon2Config{Time: 1, Memory: 16 * 1024, Threads: 1}
	cfg := config.AuthConfig{
		Enabled:      true,
		Username:     "admin",
		PasswordHash: HashPassword("testpass", salt, params),
		PasswordSalt: salt,
		Argon2:       params,
	}
	am := NewAuthManager(cfg, t.TempDir(), false, config.SecurityConfig{})

	// Behavior must be unchanged by the constant-time fallback.
	if !am.ValidateCredentials("admin", "testpass") {
		t.Fatal("valid credentials should authenticate")
	}
	if am.ValidateCredentials("nobody", "testpass") {
		t.Fatal("unknown user must not authenticate")
	}

	// The minimum over several runs is a stable estimate of the unavoidable
	// Argon2id cost on each path.
	measure := func(user, pass string) time.Duration {
		best := time.Hour
		for i := 0; i < 7; i++ {
			start := time.Now()
			am.ValidateCredentials(user, pass)
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}

	known := measure("admin", "wrongpass")    // known user, wrong password
	unknown := measure("nobody", "wrongpass") // unknown user

	// Both paths run exactly one Argon2id hash, so their minima are the same
	// order of magnitude. Before the fix the unknown path skipped hashing and
	// was orders of magnitude faster; a generous 1/4 bound stays robust to CI
	// scheduling noise while failing hard for the old behavior.
	if unknown < known/4 {
		t.Errorf("unknown-user path too fast (%v) vs known-user path (%v): "+
			"username enumeration timing oracle likely present", unknown, known)
	}
}

func TestSessionLifecycle(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Hour,
	}, "", false, config.SecurityConfig{OriginValidation: true})

	token, err := am.CreateSession("admin")
	if err != nil {
		t.Fatalf("CreateSession error: %v", err)
	}
	if token == "" {
		t.Fatal("CreateSession returned empty token")
	}
	if !am.ValidateSession(token) {
		t.Error("Newly created session should be valid")
	}
	if am.ValidateSession("invalid_token") {
		t.Error("Invalid token should not validate")
	}
}

func TestSessionExpiry(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Millisecond, // very short timeout
	}, "", false, config.SecurityConfig{OriginValidation: true})

	token, _ := am.CreateSession("admin")
	time.Sleep(5 * time.Millisecond)
	if am.ValidateSession(token) {
		t.Error("Expired session should not validate")
	}
}

func TestCleanupSessions(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Millisecond,
	}, "", false, config.SecurityConfig{OriginValidation: true})

	_, _ = am.CreateSession("user1")
	_, _ = am.CreateSession("user2")
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
	am := NewAuthManager(config.AuthConfig{Enabled: false}, "", false, config.SecurityConfig{OriginValidation: true})
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
	}, "", false, config.SecurityConfig{OriginValidation: true})
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
	}, "", false, config.SecurityConfig{OriginValidation: true})
	token, _ := am.CreateSession("admin")

	handler := am.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.AddCookie(&http.Cookie{Name: "kula_session", Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Valid cookie: status = %d, want 200", rec.Code)
	}
}

func TestAuthMiddlewareValidCookieIgnoresClientChanges(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Hour,
	}, "", false, config.SecurityConfig{OriginValidation: true})
	token, _ := am.CreateSession("admin")

	handler := am.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
	req.RemoteAddr = "203.0.113.77:4321"
	req.Header.Set("User-Agent", "changed-agent")
	req.AddCookie(&http.Cookie{Name: "kula_session", Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("Changed client fingerprint: status = %d, want 200", rec.Code)
	}
}

func TestAuthMiddlewareBearerToken(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		SessionTimeout: time.Hour,
	}, "", false, config.SecurityConfig{OriginValidation: true})
	token, _ := am.CreateSession("admin")

	handler := am.AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/test", nil)
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
	}, tmpDir, false, config.SecurityConfig{OriginValidation: true})

	token, err := am.CreateSession("admin")
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

func TestLoadSessionsLegacyFingerprintFields(t *testing.T) {
	tmpDir := t.TempDir()
	am := NewAuthManager(config.AuthConfig{
		Enabled:        true,
		Username:       "admin",
		SessionTimeout: time.Hour,
	}, tmpDir, false, config.SecurityConfig{OriginValidation: true})
	legacyJSON := `[{"token":"hashed-token","username":"admin",` + credentialJSON(t, am, "admin") +
		`"ip":"127.0.0.1","user_agent":"legacy-agent","created_at":"2026-01-01T00:00:00Z","expires_at":"2999-01-01T00:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(tmpDir, "sessions.json"), []byte(legacyJSON), 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	if err := am.LoadSessions(); err != nil {
		t.Fatalf("LoadSessions error: %v", err)
	}

	am.mu.RLock()
	defer am.mu.RUnlock()
	sess, ok := am.sessions["hashed-token"]
	if !ok {
		t.Fatal("legacy session was not loaded")
	}
	if sess.username != "admin" {
		t.Errorf("loaded session username = %q, want admin", sess.username)
	}
}

// TestSessionAbsoluteLifetime pins the ceiling that stops sliding expiration from
// renewing a session forever: past created_at+session_max_lifetime the token is
// rejected and dropped even though it has been used continuously.
func TestSessionAbsoluteLifetime(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:            true,
		SessionTimeout:     time.Hour,
		SessionMaxLifetime: time.Hour,
	}, "", false, config.SecurityConfig{OriginValidation: true})

	token, _ := am.CreateSession("admin")
	if !am.ValidateSession(token) {
		t.Fatal("fresh session should validate")
	}

	// Age the login rather than sleeping through it: the ceiling is measured from
	// createdAt, so backdating reproduces the condition exactly without racing the
	// scheduler. That first ValidateSession already slid expiresAt to the deadline,
	// which the backdating leaves in the future — so the session is not idle-expired
	// and only the absolute ceiling can reject it.
	am.mu.Lock()
	am.sessions[hashToken(token)].createdAt = time.Now().Add(-2 * time.Hour)
	am.mu.Unlock()

	if am.ValidateSession(token) {
		t.Error("session outlived session_max_lifetime despite continuous use")
	}
	am.mu.RLock()
	_, still := am.sessions[hashToken(token)]
	am.mu.RUnlock()
	if still {
		t.Error("session past its absolute lifetime was not deleted")
	}
}

// TestSessionSlidingClampedToLifetime checks the sliding extension never writes an
// expiresAt beyond the absolute deadline, so what CleanupSessions and sessions.json
// see matches what ValidateSession will honour.
func TestSessionSlidingClampedToLifetime(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:            true,
		SessionTimeout:     time.Hour,
		SessionMaxLifetime: time.Minute,
	}, "", false, config.SecurityConfig{OriginValidation: true})

	token, _ := am.CreateSession("admin")
	if !am.ValidateSession(token) {
		t.Fatal("fresh session should validate")
	}

	am.mu.RLock()
	sess := am.sessions[hashToken(token)]
	expires, created := sess.expiresAt, sess.createdAt
	am.mu.RUnlock()

	if want := created.Add(time.Minute); expires.After(want) {
		t.Errorf("expiresAt = %v, want no later than the absolute deadline %v", expires, want)
	}
}

// TestSessionMaxLifetimeDisabled documents the opt-out: a zero session_max_lifetime
// keeps the old indefinitely renewable behaviour.
func TestSessionMaxLifetimeDisabled(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:            true,
		SessionTimeout:     time.Hour,
		SessionMaxLifetime: 0,
	}, "", false, config.SecurityConfig{OriginValidation: true})

	token, _ := am.CreateSession("admin")
	am.mu.Lock()
	am.sessions[hashToken(token)].createdAt = time.Now().Add(-365 * 24 * time.Hour)
	am.mu.Unlock()

	if !am.ValidateSession(token) {
		t.Error("with session_max_lifetime disabled, an old but active session should still validate")
	}
}

// TestCleanupSessionsAbsoluteLifetime covers the background sweeper: a session that
// is idle but past its absolute lifetime must not survive until someone tries it.
func TestCleanupSessionsAbsoluteLifetime(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{
		Enabled:            true,
		SessionTimeout:     time.Hour,
		SessionMaxLifetime: time.Minute,
	}, "", false, config.SecurityConfig{OriginValidation: true})

	token, _ := am.CreateSession("admin")
	am.mu.Lock()
	am.sessions[hashToken(token)].createdAt = time.Now().Add(-2 * time.Minute)
	am.mu.Unlock()

	am.CleanupSessions()

	am.mu.RLock()
	_, still := am.sessions[hashToken(token)]
	am.mu.RUnlock()
	if still {
		t.Error("CleanupSessions kept a session past its absolute lifetime")
	}
}

// TestLoadSessionsEnforcesAbsoluteLifetime makes sure a restart is not a way to
// launder an over-age session: sessions.json carries expires_at, but created_at is
// what decides whether the session may come back at all.
func TestLoadSessionsEnforcesAbsoluteLifetime(t *testing.T) {
	tmpDir := t.TempDir()
	am := NewAuthManager(config.AuthConfig{
		Enabled:            true,
		Username:           "admin",
		SessionTimeout:     time.Hour,
		SessionMaxLifetime: 7 * 24 * time.Hour,
	}, tmpDir, false, config.SecurityConfig{OriginValidation: true})
	bound := credentialJSON(t, am, "admin")
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	saved := `[` +
		`{"token":"aged","username":"admin",` + bound + `"created_at":"` + old + `","expires_at":"2999-01-01T00:00:00Z"},` +
		`{"token":"undated","username":"admin",` + bound + `"expires_at":"2999-01-01T00:00:00Z"},` +
		`{"token":"fresh","username":"admin",` + bound + `"created_at":"` + time.Now().UTC().Format(time.RFC3339) + `","expires_at":"2999-01-01T00:00:00Z"}` +
		`]`
	if err := os.WriteFile(filepath.Join(tmpDir, "sessions.json"), []byte(saved), 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	if err := am.LoadSessions(); err != nil {
		t.Fatalf("LoadSessions error: %v", err)
	}

	am.mu.RLock()
	defer am.mu.RUnlock()
	if _, ok := am.sessions["aged"]; ok {
		t.Error("session older than session_max_lifetime was restored from disk")
	}
	if _, ok := am.sessions["undated"]; ok {
		t.Error("session with no created_at was restored; its lifetime cannot be enforced")
	}
	sess, ok := am.sessions["fresh"]
	if !ok {
		t.Fatal("session within its absolute lifetime was not restored")
	}
	if want := sess.createdAt.Add(7 * 24 * time.Hour); sess.expiresAt.After(want) {
		t.Errorf("restored expiresAt = %v, want no later than %v", sess.expiresAt, want)
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
		proxyLine  string
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
			want:       "1.2.3.4",
		},
		{
			name:       "Invalid RemoteAddr format",
			remoteAddr: "not-an-ip",
			xff:        "",
			trustProxy: false,
			want:       "not-an-ip",
		},
		{
			// A proxy that adds its own line leaves the client's first.
			name:       "Client line, then the proxy's, trusted",
			remoteAddr: "1.2.3.4:1234",
			xff:        "203.0.113.9",
			proxyLine:  "198.51.100.7",
			trustProxy: true,
			want:       "198.51.100.7",
		},
		{
			// Each made-up value would be a limiter key of its own.
			name:       "Not an address, trusted",
			remoteAddr: "1.2.3.4:1234",
			xff:        "10.0.0.1, attempt-42",
			trustProxy: true,
			want:       "1.2.3.4",
		},
		{
			name:       "Address with a port, trusted",
			remoteAddr: "1.2.3.4:1234",
			xff:        "[2001:DB8::1]:4711",
			trustProxy: true,
			want:       "2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				req.Header.Set("X-Forwarded-For", tt.xff)
			}
			if tt.proxyLine != "" {
				req.Header.Add("X-Forwarded-For", tt.proxyLine)
			}

			got := getClientIP(req, tt.trustProxy)
			if got != tt.want {
				t.Errorf("%s: getClientIP() = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestLoginClientKey verifies what the login limiters count as one client: an
// IPv4 address, or the /64 of an IPv6 address.
func TestLoginClientKey(t *testing.T) {
	for ip, want := range map[string]string{
		"203.0.113.7":          "203.0.113.7",
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8:1:2::1":      "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"::1":                  "::/64",
		"fe80::1%eth0":         "fe80::/64",
		"::ffff:203.0.113.7":   "203.0.113.7",
		// IPv4 clients through a NAT64 translator, each by its own address.
		"64:ff9b::203.0.113.7": "203.0.113.7",
		"64:ff9b::c633:6408":   "198.51.100.8",
		"64:ff9b:0:0:1::1":     "64:ff9b::/64", // outside the /96
		// Not an address: counted as given.
		"not-an-ip":      "not-an-ip",
		"203.0.113.7:80": "203.0.113.7:80",
		"":               "",
	} {
		if got := loginClientKey(ip); got != want {
			t.Errorf("loginClientKey(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestValidateOrigin(t *testing.T) {
	am := NewAuthManager(config.AuthConfig{}, "", false, config.SecurityConfig{OriginValidation: true})

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
	am := NewAuthManager(config.AuthConfig{}, "", false, config.SecurityConfig{OriginValidation: true})
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

func TestRateLimiterCapsDistinctKeys(t *testing.T) {
	rl := &RateLimiter{attempts: make(map[string][]time.Time), limit: loginIPAttemptLimit}

	// Fill the limiter to capacity with distinct, fresh keys.
	for i := 0; i < maxRateLimiterKeys; i++ {
		if !rl.Allow("ip-" + strconv.Itoa(i)) {
			t.Fatalf("key %d should be allowed while under the cap", i)
		}
	}

	// A brand-new key must be refused now that the map is saturated with fresh
	// (non-purgeable) entries: fail closed rather than grow without bound.
	if rl.Allow("overflow-ip") {
		t.Fatal("new key should be denied when the limiter is saturated with fresh entries")
	}
	if len(rl.attempts) > maxRateLimiterKeys {
		t.Fatalf("map grew past cap: got %d keys, want <= %d", len(rl.attempts), maxRateLimiterKeys)
	}

	// An already-tracked key is still served — the cap never evicts existing keys.
	if !rl.Allow("ip-0") {
		t.Fatal("already-tracked key should still be allowed under the cap")
	}
}

func TestRateLimiterReclaimsStaleKeys(t *testing.T) {
	rl := &RateLimiter{attempts: make(map[string][]time.Time), limit: loginIPAttemptLimit}

	// Saturate the map with entries that are already outside the 5-minute window.
	stale := time.Now().Add(-10 * time.Minute)
	for i := 0; i < maxRateLimiterKeys; i++ {
		rl.attempts["ip-"+strconv.Itoa(i)] = []time.Time{stale}
	}

	// A new key trips the cap, which purges the stale entries and admits the key.
	if !rl.Allow("fresh-ip") {
		t.Fatal("new key should be admitted after stale entries are purged")
	}
	if len(rl.attempts) > maxRateLimiterKeys {
		t.Fatalf("map grew past cap after purge: got %d keys", len(rl.attempts))
	}
}

// TestUserLimiterKeyedPerIP asserts that exhausting the per-user login limiter from
// one address does not lock the same account out from another one (K-03).
func TestUserLimiterKeyedPerIP(t *testing.T) {
	rl := &RateLimiter{attempts: make(map[string][]time.Time), limit: loginUserAttemptLimit, failOpen: true}

	// Attacker burns the whole window against a known account name.
	for i := 0; i < loginUserAttemptLimit; i++ {
		if !rl.Allow(userLimiterKey("Admin", "10.0.0.9")) {
			t.Fatalf("attempt %d from the attacker should be allowed", i)
		}
	}
	if rl.Allow(userLimiterKey("admin", "10.0.0.9")) {
		t.Fatalf("attacker should be throttled after %d attempts (username is case-folded)", loginUserAttemptLimit)
	}

	// The real operator, on a different address, is unaffected.
	if !rl.Allow(userLimiterKey("admin", "192.168.1.5")) {
		t.Fatal("same username from a different IP must not be locked out")
	}
}

// TestUserLimiterFailsOpenWhenSaturated asserts that filling the per-user limiter
// with attacker-chosen usernames degrades to "no per-user throttle" rather than to
// a login outage; the IP limiter still fails closed (K-03).
func TestUserLimiterFailsOpenWhenSaturated(t *testing.T) {
	rl := &RateLimiter{attempts: make(map[string][]time.Time), limit: loginUserAttemptLimit, failOpen: true}

	for i := 0; i < maxRateLimiterKeys; i++ {
		if !rl.Allow(userLimiterKey("user-"+strconv.Itoa(i), "10.0.0.9")) {
			t.Fatalf("key %d should be allowed while under the cap", i)
		}
	}

	if !rl.Allow(userLimiterKey("operator", "192.168.1.5")) {
		t.Fatal("saturated per-user limiter must fail open, not deny every new login")
	}
	if len(rl.attempts) > maxRateLimiterKeys {
		t.Fatalf("map grew past cap: got %d keys, want <= %d", len(rl.attempts), maxRateLimiterKeys)
	}
}

// TestUserLimiterKeyBoundsUsername asserts the attacker-controlled half of the key
// is truncated and cannot forge another caller's IP prefix.
func TestUserLimiterKeyBoundsUsername(t *testing.T) {
	long := strings.Repeat("a", maxLimiterUsernameLen*4)
	key := userLimiterKey(long, "10.0.0.9")
	if want := len("10.0.0.9") + 1 + maxLimiterUsernameLen; len(key) != want {
		t.Fatalf("key length = %d, want %d", len(key), want)
	}
	if got, want := userLimiterKey("10.0.0.9\x00admin", "1.2.3.4"), "1.2.3.4\x0010.0.0.9\x00admin"; got != want {
		t.Fatalf("key = %q, want %q (IP prefix must stay unforgeable)", got, want)
	}
}

// A login whose hashing panics (net/http recovers the handler) must still
// give its Argon2id slot back, or a few such requests would leave every later
// login refused as busy. Argon2 time 0 makes argon2.IDKey panic.
func TestLoginReleasesHashSlotOnPanic(t *testing.T) {
	s := NewServer(config.WebConfig{Auth: config.AuthConfig{Enabled: true, Username: "admin",
		Argon2: config.Argon2Config{Time: 0, Memory: 1024, Threads: 1}}}, config.GlobalConfig{}, nil, nil, t.TempDir(), config.OllamaConfig{})
	for i := 0; i <= cap(s.auth.hashSlots); i++ {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("hashing with argon2 time 0 did not panic; the test no longer exercises a panic")
				}
			}()
			req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"x"}`))
			req.RemoteAddr = "192.0.2." + strconv.Itoa(i+1) + ":1234"
			s.handleLogin(httptest.NewRecorder(), req)
		}()
		if held := len(s.auth.hashSlots); held != 0 {
			t.Fatalf("after panicking login %d, %d hash slot(s) still held", i+1, held)
		}
	}
}

// TestPasswordHashConcurrencyBounded pins the global Argon2id bound: the
// per-address limiters alone let many sources keep hundreds of 32 MiB hashes
// in flight at once.
func TestPasswordHashConcurrencyBounded(t *testing.T) {
	n := passwordHashConcurrency(0)
	if n < 1 || n > maxConcurrentPasswordHashes {
		t.Fatalf("passwordHashConcurrency(0) = %d, want 1..%d", n, maxConcurrentPasswordHashes)
	}
	// The bound is memory as well as hashes: raising argon2.memory lowers it,
	// down to one hash however large one is.
	for memoryKiB, limit := range map[uint32]int{32 << 10: 4, 64 << 10: 2, 256 << 10: 1, 4 << 20: 1} {
		if got := passwordHashConcurrency(memoryKiB); got < 1 || got > min(n, limit) {
			t.Errorf("passwordHashConcurrency(%d KiB) = %d, want 1..%d", memoryKiB, got, min(n, limit))
		}
		if got := passwordHashConcurrency(memoryKiB); got*int(memoryKiB)<<10 > passwordHashMemoryBudget && got > 1 {
			t.Errorf("passwordHashConcurrency(%d KiB) = %d exceeds the memory budget", memoryKiB, got)
		}
	}

	am := NewAuthManager(config.AuthConfig{Enabled: true}, "", false, config.SecurityConfig{OriginValidation: true})
	if got := cap(am.hashSlots); got != n {
		t.Fatalf("hash slots = %d, want %d", got, n)
	}

	releases := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		release, ok := am.acquireHashSlot(context.Background())
		if !ok {
			t.Fatalf("slot %d of %d refused while capacity remained", i+1, n)
		}
		releases = append(releases, release)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := am.acquireHashSlot(ctx); ok {
		t.Fatal("acquired a hash slot beyond the concurrency bound")
	}

	releases[0]()
	release, ok := am.acquireHashSlot(ctx)
	if !ok {
		t.Fatal("a released slot could not be reacquired")
	}
	release()
	for _, r := range releases[1:] {
		r()
	}
	if got := len(am.hashSlots); got != 0 {
		t.Errorf("slots still held after every release: %d", got)
	}
}
