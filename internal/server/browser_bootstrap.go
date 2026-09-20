package server

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const browserBootstrapTTL = time.Minute
const maxBrowserBootstrapChallenges = 128

type browserBootstrapChallenge struct {
	expires time.Time
	origin  string
	host    string
}

type browserBootstrapState struct {
	mu         sync.Mutex
	challenges map[string]browserBootstrapChallenge
}

func browserBootstrapRequestAllowed(r *http.Request) bool {
	host, err := url.Parse("http://" + r.Host)
	return err == nil && isLoopbackHost(host.Hostname()) && isLoopbackListener() &&
		isLoopbackRemote(r.RemoteAddr) && requestOriginAllowed(r, r.Header.Get("Origin"))
}

func (s *Server) handleBrowserBootstrapChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !browserBootstrapRequestAllowed(r) {
		writeError(w, http.StatusForbidden, "Browser bootstrap is available only from the local application origin")
		return
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		writeError(w, http.StatusServiceUnavailable, "Could not initialize browser challenge")
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(secret)
	now := time.Now()
	s.bootstrap.mu.Lock()
	if s.bootstrap.challenges == nil {
		s.bootstrap.challenges = make(map[string]browserBootstrapChallenge)
	}
	for key, challenge := range s.bootstrap.challenges {
		if !now.Before(challenge.expires) {
			delete(s.bootstrap.challenges, key)
		}
	}
	if len(s.bootstrap.challenges) >= maxBrowserBootstrapChallenges {
		s.bootstrap.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "Too many pending browser challenges")
		return
	}
	s.bootstrap.challenges[nonce] = browserBootstrapChallenge{
		expires: now.Add(browserBootstrapTTL), origin: strings.ToLower(r.Header.Get("Origin")), host: strings.ToLower(r.Host),
	}
	s.bootstrap.mu.Unlock()
	writeJSON(w, map[string]any{"challenge": nonce, "expiresInSeconds": int(browserBootstrapTTL.Seconds())})
}

// Consume under one lock before credential persistence, including when a
// later persistence failure prevents the exchange from completing.
func (s *browserBootstrapState) consume(nonce string, r *http.Request, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	challenge, exists := s.challenges[nonce]
	if !exists || !now.Before(challenge.expires) {
		delete(s.challenges, nonce)
		return false
	}
	if challenge.origin != strings.ToLower(r.Header.Get("Origin")) || challenge.host != strings.ToLower(r.Host) {
		return false
	}
	delete(s.challenges, nonce)
	return true
}
