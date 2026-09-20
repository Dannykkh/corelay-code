package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
)

func authTestConfig(t *testing.T) {
	t.Helper()
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	t.Setenv("CORELAY_ACCESS_TOKEN", "")
	t.Setenv("ANICLEW_ACCESS_TOKEN", "")
	t.Setenv("CORELAY_BIND", "127.0.0.1")
}

func TestAuthMiddlewareRequiresTokenAndFailsClosedOnBrokenConfig(t *testing.T) {
	authTestConfig(t)
	called := false
	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/private", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatalf("missing token status=%d nextCalled=%v, want 401 and no dispatch", rec.Code, called)
	}

	if err := os.WriteFile(config.ConfigPath(), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/private", nil)
	req.Header.Set("X-Access-Token", "anything")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("broken config status=%d nextCalled=%v, want 503 and no dispatch", rec.Code, called)
	}
}

func TestAuthMiddlewareAcceptsHeaderAndSameSiteCookieButNotQueryToken(t *testing.T) {
	authTestConfig(t)
	if _, err := config.Update(func(cfg *config.Config) error {
		cfg.AccessToken = "private-token"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	handler := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for name, prepare := range map[string]func(*http.Request){
		"header": func(r *http.Request) { r.Header.Set("X-Access-Token", "private-token") },
		"cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "corelay-token", Value: "private-token"}) },
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/private", nil)
			prepare(req)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/api/private?token=private-token", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query credential status=%d, want 401", rec.Code)
	}
}

func TestCORSMiddlewareRejectsCrossOriginActualRequests(t *testing.T) {
	authTestConfig(t)
	called := false
	handler := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/private", strings.NewReader("{}"))
	req.Header.Set("Origin", "https://attacker.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("cross-origin status=%d nextCalled=%v", rec.Code, called)
	}

	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/private", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://127.0.0.1:4000")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "http://127.0.0.1:4000" || rec.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Fatalf("same-origin response status=%d headers=%v", rec.Code, rec.Header())
	}
}

func TestBrowserBootstrapIssuesHTTPOnlyCookieOnlyForLoopbackOrigin(t *testing.T) {
	authTestConfig(t)
	srv := &Server{}
	challenge := issueBootstrapChallenge(t, srv)
	handler := http.HandlerFunc(srv.handleBrowserBootstrap)
	request := func(remote, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/bootstrap", strings.NewReader(`{"challenge":"`+challenge+`"}`))
		req.RemoteAddr = remote
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	denied := request("192.0.2.2:1234", "http://127.0.0.1:4000")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("remote bootstrap status=%d", denied.Code)
	}
	rec := request("127.0.0.1:1234", "http://127.0.0.1:4000")
	if rec.Code != http.StatusOK {
		t.Fatalf("local bootstrap status=%d body=%s", rec.Code, rec.Body.String())
	}
	cookie := rec.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != "corelay-token" || !cookie[0].HttpOnly || cookie[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("bootstrap cookies=%+v", cookie)
	}
	cfg, exists, err := config.LoadChecked()
	if err != nil || !exists || cfg.AccessToken == "" || cookie[0].Value != cfg.AccessToken {
		t.Fatalf("persisted bootstrap token missing or mismatched: exists=%v err=%v", exists, err)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response["ok"] != true {
		t.Fatalf("bootstrap response=%s err=%v", rec.Body.String(), err)
	}
	if strings.Contains(rec.Body.String(), cfg.AccessToken) {
		t.Fatal("bootstrap response body exposed the access token")
	}
}

func bootstrapTestRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/api/bootstrap", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "http://127.0.0.1:4000")
	return req
}

func issueBootstrapChallenge(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleBrowserBootstrapChallenge(rec, bootstrapTestRequest(""))
	var response struct {
		Challenge string `json:"challenge"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil || len(response.Challenge) != 43 {
		t.Fatalf("challenge issuance status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("challenge response cookie/cache contract")
	}
	return response.Challenge
}

func TestBrowserBootstrapRejectsExpiredReplayAndConcurrentExchange(t *testing.T) {
	authTestConfig(t)
	srv := &Server{}
	nonce := issueBootstrapChallenge(t, srv)
	if _, exists, err := config.LoadChecked(); exists || err != nil {
		t.Fatalf("issuance wrote config: %v %v", exists, err)
	}
	srv.bootstrap.mu.Lock()
	entry := srv.bootstrap.challenges[nonce]
	entry.expires = time.Now().Add(-time.Second)
	srv.bootstrap.challenges[nonce] = entry
	srv.bootstrap.mu.Unlock()
	rec := httptest.NewRecorder()
	srv.handleBrowserBootstrap(rec, bootstrapTestRequest(`{"challenge":"`+nonce+`"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired status=%d", rec.Code)
	}
	if _, exists, err := config.LoadChecked(); exists || err != nil {
		t.Fatalf("expired exchange wrote config: %v %v", exists, err)
	}
	nonce = issueBootstrapChallenge(t, srv)
	start, results := make(chan struct{}), make(chan int, 8)
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			rec := httptest.NewRecorder()
			srv.handleBrowserBootstrap(rec, bootstrapTestRequest(`{"challenge":"`+nonce+`"}`))
			results <- rec.Code
		}()
	}
	close(start)
	success := 0
	for i := 0; i < 8; i++ {
		status := <-results
		if status == http.StatusOK {
			success++
		} else if status != http.StatusUnauthorized {
			t.Errorf("exchange status=%d", status)
		}
	}
	if success != 1 {
		t.Fatalf("successful concurrent exchanges=%d", success)
	}
	rec = httptest.NewRecorder()
	srv.handleBrowserBootstrap(rec, bootstrapTestRequest(`{"challenge":"`+nonce+`"}`))
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("replay status=%d", rec.Code)
	}
}

func TestBrowserBootstrapMalformedExchangeDoesNotMutateOrConsume(t *testing.T) {
	authTestConfig(t)
	srv := &Server{}
	nonce := issueBootstrapChallenge(t, srv)
	valid := `{"challenge":"` + nonce + `"}`
	for _, body := range []string{"", "{}", "null", valid + "{}", `{"challenge":"` + nonce + `","extra":true}`, `{"challenge":"` + strings.Repeat("x", 1100) + `"}`} {
		rec := httptest.NewRecorder()
		srv.handleBrowserBootstrap(rec, bootstrapTestRequest(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("malformed status=%d", rec.Code)
		}
		if _, exists, err := config.LoadChecked(); exists || err != nil {
			t.Fatalf("malformed exchange wrote config: %v %v", exists, err)
		}
	}
	rec := httptest.NewRecorder()
	srv.handleBrowserBootstrap(rec, bootstrapTestRequest(valid))
	if rec.Code != http.StatusOK {
		t.Fatalf("valid challenge consumed by malformed input: %d", rec.Code)
	}
}

func TestBrowserBootstrapChallengeBoundAndOriginBinding(t *testing.T) {
	authTestConfig(t)
	srv := &Server{}
	nonce := issueBootstrapChallenge(t, srv)
	req := bootstrapTestRequest(`{"challenge":"` + nonce + `"}`)
	req.Host = "localhost:4000"
	req.Header.Set("Origin", "http://localhost:4000")
	rec := httptest.NewRecorder()
	srv.handleBrowserBootstrap(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("different origin status=%d", rec.Code)
	}
	for i := 1; i < maxBrowserBootstrapChallenges; i++ {
		issueBootstrapChallenge(t, srv)
	}
	rec = httptest.NewRecorder()
	srv.handleBrowserBootstrapChallenge(rec, bootstrapTestRequest(""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("capacity status=%d", rec.Code)
	}
	if len(srv.bootstrap.challenges) != maxBrowserBootstrapChallenges {
		t.Fatal("unbounded challenge state")
	}
	srv.bootstrap.mu.Lock()
	for key, challenge := range srv.bootstrap.challenges {
		challenge.expires = time.Now().Add(-time.Second)
		srv.bootstrap.challenges[key] = challenge
	}
	srv.bootstrap.mu.Unlock()
	issueBootstrapChallenge(t, srv)
	if len(srv.bootstrap.challenges) != 1 {
		t.Fatal("expired challenges were not reclaimed")
	}
}

func TestBrowserBootstrapChallengeRejectsNonlocalRequestsBeforeConfig(t *testing.T) {
	authTestConfig(t)
	srv := &Server{}
	for _, prepare := range []func(*http.Request){
		func(r *http.Request) { r.RemoteAddr = "192.0.2.1:1234" },
		func(r *http.Request) { r.Header.Del("Origin") },
		func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") },
		func(r *http.Request) { r.Host = "attacker.example"; r.Header.Set("Origin", "http://attacker.example") },
	} {
		req := bootstrapTestRequest("")
		prepare(req)
		rec := httptest.NewRecorder()
		srv.handleBrowserBootstrapChallenge(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("nonlocal challenge status=%d", rec.Code)
		}
	}
	if len(srv.bootstrap.challenges) != 0 {
		t.Fatal("rejected requests allocated challenges")
	}
	if _, exists, err := config.LoadChecked(); exists || err != nil {
		t.Fatalf("rejected issuance wrote config: %v %v", exists, err)
	}
}

func TestBrowserBootstrapPersistenceFailureStillConsumesChallenge(t *testing.T) {
	authTestConfig(t)
	srv := &Server{}
	nonce := issueBootstrapChallenge(t, srv)
	if err := os.WriteFile(config.ConfigPath(), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.handleBrowserBootstrap(rec, bootstrapTestRequest(`{"challenge":"`+nonce+`"}`))
	if rec.Code != http.StatusServiceUnavailable || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("broken config status=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.handleBrowserBootstrap(rec, bootstrapTestRequest(`{"challenge":"`+nonce+`"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("failed exchange replay status=%d", rec.Code)
	}
	data, err := os.ReadFile(config.ConfigPath())
	if err != nil || string(data) != "{" {
		t.Fatalf("corrupt config overwritten: %v", err)
	}
}

func TestEnsureServiceAccessTokenCreatesOnceAndPreservesConfiguredValue(t *testing.T) {
	authTestConfig(t)
	if err := ensureServiceAccessToken(); err != nil {
		t.Fatal(err)
	}
	first, exists, err := config.LoadChecked()
	if err != nil || !exists || len(first.AccessToken) < 40 {
		t.Fatalf("generated service token invalid: exists=%v err=%v len=%d", exists, err, len(first.AccessToken))
	}
	if err := ensureServiceAccessToken(); err != nil {
		t.Fatal(err)
	}
	second, _, err := config.LoadChecked()
	if err != nil || second.AccessToken != first.AccessToken {
		t.Fatalf("service token changed across startup: err=%v", err)
	}
}

func TestBrowserBootstrapRejectsDNSRebindingHost(t *testing.T) {
	authTestConfig(t)
	req := httptest.NewRequest(http.MethodPost, "http://attacker.example/api/bootstrap", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "http://attacker.example")
	rec := httptest.NewRecorder()
	http.HandlerFunc((&Server{}).handleBrowserBootstrap).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("DNS-rebinding bootstrap status=%d, want 403", rec.Code)
	}
	if _, exists, err := config.LoadChecked(); err != nil || exists {
		t.Fatalf("rebinding request wrote config: exists=%v err=%v", exists, err)
	}
}

func TestSetWorkspaceDoesNotSwitchOnConfigWriteFailure(t *testing.T) {
	authTestConfig(t)
	if err := os.WriteFile(config.ConfigPath(), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	srv := &Server{workDir: "before"}
	bodyBytes, err := json.Marshal(map[string]string{"path": workspace})
	if err != nil {
		t.Fatal(err)
	}
	body := strings.NewReader(string(bodyBytes))
	req := httptest.NewRequest(http.MethodPut, "/api/workspace", body)
	rec := httptest.NewRecorder()
	srv.handleSetWorkspace(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want save error", rec.Code, rec.Body.String())
	}
	srv.mu.RLock()
	workDir := srv.workDir
	srv.mu.RUnlock()
	if workDir != "before" {
		t.Fatalf("runtime workspace changed to %q after failed config update", workDir)
	}
}
