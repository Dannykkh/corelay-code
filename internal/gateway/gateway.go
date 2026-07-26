package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// User represents a team member.
type User struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Token     string   `json:"token"`
	Role      string   `json:"role"` // "admin", "developer", "viewer"
	AllowedProviders []string `json:"allowedProviders,omitempty"` // empty = all
	Enabled   bool     `json:"enabled"`
}

// AuditEntry records an API call.
type AuditEntry struct {
	Time     time.Time `json:"time"`
	UserID   string    `json:"userId"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Role     string    `json:"role"` // routing role
	Tokens   int       `json:"tokens"`
	Masked   bool      `json:"masked"` // PII was masked
}

// Gateway manages team access and auditing.
type Gateway struct {
	mu     sync.RWMutex
	users  map[string]*User // token -> user
	audit  []AuditEntry
	dir    string
}

func New(baseDir string) *Gateway {
	dir := filepath.Join(baseDir, "gateway")
	os.MkdirAll(dir, 0755)

	g := &Gateway{
		users: make(map[string]*User),
		audit: make([]AuditEntry, 0, 10000),
		dir:   dir,
	}
	g.load()
	return g
}

// AddUser creates a new team user.
func (g *Gateway) AddUser(name, role string, allowed []string) *User {
	g.mu.Lock()
	defer g.mu.Unlock()

	token := generateToken(name)
	user := &User{
		ID:               fmt.Sprintf("user_%s", hashStr(name)[:8]),
		Name:             name,
		Token:            token,
		Role:             role,
		AllowedProviders: allowed,
		Enabled:          true,
	}
	g.users[token] = user
	g.save()
	log.Printf("[Gateway] User added: %s (%s)", name, role)
	return user
}

// Authenticate validates a request token and returns the user.
func (g *Gateway) Authenticate(r *http.Request) (*User, error) {
	token := r.Header.Get("X-Proxy-Token")
	if token == "" {
		// Try bearer
		auth := r.Header.Get("X-Team-Auth")
		if strings.HasPrefix(auth, "Bearer ") {
			token = auth[7:]
		}
	}

	if token == "" {
		return nil, nil // no team auth = passthrough
	}

	g.mu.RLock()
	user, ok := g.users[token]
	g.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("invalid token")
	}
	if !user.Enabled {
		return nil, fmt.Errorf("user disabled")
	}
	return user, nil
}

// CheckProvider returns true if the user is allowed to use this provider.
func (g *Gateway) CheckProvider(user *User, provider string) bool {
	if len(user.AllowedProviders) == 0 {
		return true // all allowed
	}
	for _, p := range user.AllowedProviders {
		if p == provider {
			return true
		}
	}
	return false
}

// RecordUsage appends an audit entry for a completed request.
func (g *Gateway) RecordUsage(userID, provider, model, role string, tokens int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.audit = append(g.audit, AuditEntry{
		Time:     time.Now(),
		UserID:   userID,
		Provider: provider,
		Model:    model,
		Role:     role,
		Tokens:   tokens,
	})

	// Trim old entries
	if len(g.audit) > 10000 {
		g.audit = g.audit[len(g.audit)-5000:]
	}
}

// GetUsers returns all users.
func (g *Gateway) GetUsers() []*User {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var result []*User
	for _, u := range g.users {
		result = append(result, u)
	}
	return result
}

// GetAudit returns recent audit entries.
func (g *Gateway) GetAudit(limit int) []AuditEntry {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if limit <= 0 || limit > len(g.audit) {
		limit = len(g.audit)
	}
	start := len(g.audit) - limit
	result := make([]AuditEntry, limit)
	copy(result, g.audit[start:])
	return result
}

// MaskPII replaces sensitive patterns in text.
func MaskPII(text string) (string, bool) {
	masked := false
	// Simple patterns — in production, use regex
	patterns := []string{"sk-", "AKIA", "ghp_", "ghu_", "password=", "secret="}
	for _, p := range patterns {
		if strings.Contains(text, p) {
			idx := strings.Index(text, p)
			end := idx + len(p) + 20
			if end > len(text) { end = len(text) }
			text = text[:idx] + "[REDACTED]" + text[end:]
			masked = true
		}
	}
	return text, masked
}

func (g *Gateway) load() {
	path := filepath.Join(g.dir, "users.json")
	data, err := os.ReadFile(path)
	if err != nil { return }
	var users []*User
	json.Unmarshal(data, &users)
	for _, u := range users {
		g.users[u.Token] = u
	}
}

func (g *Gateway) save() {
	path := filepath.Join(g.dir, "users.json")
	var users []*User
	for _, u := range g.users {
		users = append(users, u)
	}
	data, _ := json.MarshalIndent(users, "", "  ")
	os.WriteFile(path, data, 0644)
}

func generateToken(seed string) string {
	return "ccp_" + hashStr(seed + time.Now().String())[:32]
}

func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
