package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kula-szpiegula/internal/config"

	"golang.org/x/crypto/argon2"
)

// AuthManager handles authentication validation and sessions.

type AuthManager struct {
	mu         sync.RWMutex
	cfg        config.AuthConfig
	storageDir string
	sessions   map[string]*session
	Limiter    *RateLimiter
	trustProxy bool
}

// RateLimiter tracks recent rapid login attempts by IP.
type RateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

type session struct {
	username  string
	ip        string
	userAgent string
	createdAt time.Time
	expiresAt time.Time
}

// sessionData is used for JSON serialization of sessions.
type sessionData struct {
	Token     string    `json:"token"`
	Username  string    `json:"username"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func NewAuthManager(cfg config.AuthConfig, storageDir string, trustProxy bool) *AuthManager {
	return &AuthManager{
		cfg:        cfg,
		storageDir: storageDir,
		sessions:   make(map[string]*session),
		Limiter: &RateLimiter{
			attempts: make(map[string][]time.Time),
		},
		trustProxy: trustProxy,
	}
}

// Allow checks if the given IP has exceeded 5 login attempts in the last 5 minutes.
func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-5 * time.Minute)

	var recent []time.Time
	for _, t := range rl.attempts[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}

	if len(recent) >= 5 {
		return false
	}

	rl.attempts[ip] = append(recent, now)
	return true
}

// HashPassword creates an Argon2id hash with the given salt and parameters.
func HashPassword(password, salt string, params config.Argon2Config) string {
	keyLen := uint32(32)

	hash := argon2.IDKey([]byte(password), []byte(salt), params.Time, params.Memory, params.Threads, keyLen)
	return hex.EncodeToString(hash)
}

// hashToken returns a SHA-256 hash of the session token.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// GenerateSalt creates a random 32-byte hex salt.
func GenerateSalt() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ValidateCredentials checks username and password against config.
func (a *AuthManager) ValidateCredentials(username, password string) bool {
	if !a.cfg.Enabled {
		return true
	}

	if subtle.ConstantTimeCompare([]byte(username), []byte(a.cfg.Username)) != 1 {
		return false
	}

	hash := HashPassword(password, a.cfg.PasswordSalt, a.cfg.Argon2)
	return subtle.ConstantTimeCompare([]byte(hash), []byte(a.cfg.PasswordHash)) == 1
}

// CreateSession creates a new authenticated session.
func (a *AuthManager) CreateSession(username, ip, userAgent string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	token, err := generateToken()
	if err != nil {
		return "", err
	}
	hashedToken := hashToken(token)
	a.sessions[hashedToken] = &session{
		username:  username,
		ip:        ip,
		userAgent: userAgent,
		createdAt: time.Now(),
		expiresAt: time.Now().Add(a.cfg.SessionTimeout),
	}

	return token, nil
}

// ValidateSession checks if a session token is valid and matches client fingerprint.
func (a *AuthManager) ValidateSession(token, ip, userAgent string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	hashedToken := hashToken(token)
	sess, ok := a.sessions[hashedToken]
	if !ok {
		return false
	}

	if time.Now().After(sess.expiresAt) {
		delete(a.sessions, hashedToken)
		return false
	}

	if sess.ip != ip || sess.userAgent != userAgent {
		return false
	}

	// Sliding expiration
	sess.expiresAt = time.Now().Add(a.cfg.SessionTimeout)

	return true
}

// RevokeSession manually destroys a session by its token.
func (a *AuthManager) RevokeSession(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	hashedToken := hashToken(token)
	delete(a.sessions, hashedToken)
}

// AuthMiddleware protects routes when auth is enabled.
func (a *AuthManager) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.Enabled {
			next.ServeHTTP(w, r)
			return
		}

		ip := getClientIP(r, a.trustProxy)
		userAgent := r.UserAgent()

		// Check cookie
		cookie, err := r.Cookie("kula_session")
		if err == nil && a.ValidateSession(cookie.Value, ip, userAgent) {
			next.ServeHTTP(w, r)
			return
		}

		// Check Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader != "" && len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			token := authHeader[7:]
			if a.ValidateSession(token, ip, userAgent) {
				next.ServeHTTP(w, r)
				return
			}
		}

		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	})
}

// CleanupSessions removes expired sessions and stale rate limiter entries periodically.
func (a *AuthManager) CleanupSessions() {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	for token, sess := range a.sessions {
		if now.After(sess.expiresAt) {
			delete(a.sessions, token)
		}
	}

	// Purge stale rate limiter entries
	a.Limiter.mu.Lock()
	defer a.Limiter.mu.Unlock()
	cutoff := now.Add(-5 * time.Minute)
	for ip, attempts := range a.Limiter.attempts {
		var recent []time.Time
		for _, t := range attempts {
			if t.After(cutoff) {
				recent = append(recent, t)
			}
		}
		if len(recent) == 0 {
			delete(a.Limiter.attempts, ip)
		} else {
			a.Limiter.attempts[ip] = recent
		}
	}
}

// LoadSessions loads sessions from disk.
func (a *AuthManager) LoadSessions() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	path := filepath.Join(a.storageDir, "sessions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No sessions to load
		}
		return err
	}

	var saved []sessionData
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}

	now := time.Now()
	for _, sd := range saved {
		if now.Before(sd.ExpiresAt) {
			// In hashed version, sd.Token is actually the hash
			a.sessions[sd.Token] = &session{
				username:  sd.Username,
				ip:        sd.IP,
				userAgent: sd.UserAgent,
				createdAt: sd.CreatedAt,
				expiresAt: sd.ExpiresAt,
			}
		}
	}

	return nil
}

// SaveSessions writes active sessions to disk.
func (a *AuthManager) SaveSessions() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	var toSave []sessionData
	now := time.Now()
	for hashedToken, sess := range a.sessions {
		if now.Before(sess.expiresAt) {
			toSave = append(toSave, sessionData{
				Token:     hashedToken,
				Username:  sess.username,
				IP:        sess.ip,
				UserAgent: sess.userAgent,
				CreatedAt: sess.createdAt,
				ExpiresAt: sess.expiresAt,
			})
		}
	}

	data, err := json.Marshal(toSave)
	if err != nil {
		return err
	}

	path := filepath.Join(a.storageDir, "sessions.json")
	return os.WriteFile(path, data, 0600)
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand.Read failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ValidateOrigin checks if the request's Origin or Referer header matches the host.
// This is a defense-in-depth measure against CSRF for state-modifying requests.
func (a *AuthManager) ValidateOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}

	if origin == "" {
		return false
	}

	u, err := url.Parse(origin)
	if err != nil {
		return false
	}

	return strings.EqualFold(u.Host, r.Host)
}

// CSRFMiddleware enforces origin validation for state-modifying requests.
func (a *AuthManager) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !a.ValidateOrigin(r) {
				http.Error(w, `{"error":"invalid origin"}`, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// PrintHashedPassword generates and prints a hash for a password using the given Argon2 parameters.
func PrintHashedPassword(password string, params config.Argon2Config) {
	salt, err := GenerateSalt()
	if err != nil {
		fmt.Printf("Error generating salt: %v\n", err)
		return
	}

	hash := HashPassword(password, salt, params)
	fmt.Printf("Password hash algorithm: Argon2id\n")
	fmt.Printf("Time: %d, Memory: %d KB, Threads: %d\n", params.Time, params.Memory, params.Threads)
	fmt.Printf("Password hash: %s\n", hash)
	fmt.Printf("Salt: %s\n", salt)
	fmt.Println("\nAdd these to your config.yaml under web.auth:")
	fmt.Printf("  password_hash: \"%s\"\n", hash)
	fmt.Printf("  password_salt: \"%s\"\n", salt)
	fmt.Printf("  argon2:\n")
	fmt.Printf("    time: %d\n", params.Time)
	fmt.Printf("    memory: %d\n", params.Memory)
	fmt.Printf("    threads: %d\n", params.Threads)
}
