package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
)

func TestResolveAccessTokenForURLUsesSavedCredentialOnlyForLoopback(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	t.Setenv("CORELAY_ACCESS_TOKEN", "")
	t.Setenv("ANICLEW_ACCESS_TOKEN", "")
	if _, err := config.Update(func(cfg *config.Config) error {
		cfg.AccessToken = "local-service-secret"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := resolveAccessTokenForURL("", "http://localhost:4000"); got != "local-service-secret" {
		t.Fatalf("loopback credential = %q", got)
	}
	if got := resolveAccessTokenForURL("", "http://192.0.2.8:4000"); got != "" {
		t.Fatalf("remote URL inherited local credential %q", got)
	}
	if got := resolveAccessTokenForURL("explicit", "http://192.0.2.8:4000"); got != "explicit" {
		t.Fatalf("explicit credential = %q", got)
	}
}

func TestChatPingRejectsRedirectWithoutForwardingCredential(t *testing.T) {
	var forwarded atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "local-secret" {
			return
		}
		forwarded.Add(1)
	}))
	defer attacker.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL, http.StatusFound)
	}))
	defer redirector.Close()

	client := &chatClient{base: redirector.URL, accessToken: "local-secret", http: newChatHTTPClient(2 * time.Second)}
	err := client.ping()
	if err == nil || !strings.Contains(err.Error(), "redirects are disabled") {
		t.Fatalf("redirecting ping error = %v, want redirect rejection", err)
	}
	if got := forwarded.Load(); got != 0 {
		t.Fatalf("redirect target received X-Access-Token %d time(s), want zero", got)
	}
}
