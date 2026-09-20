package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Dannykkh/corelay-code/internal/config"
)

func TestVersionAndDoctorProductionSubcommands(t *testing.T) {
	binary := buildManagedProductionBinary(t)

	t.Run("version is offline and independent of malformed user config", func(t *testing.T) {
		configDir := t.TempDir()
		const configSecret = "VERSION_CONFIG_SECRET_SENTINEL"
		if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{"accessToken":"`+configSecret+`",`), 0600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runDiagnosticsBinary(t, binary, []string{"version"}, configDir, nil)
		if err != nil {
			t.Fatalf("version failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
		}
		for _, want := range []string{"Corelay Code CLI version: dev", "Build commit: unknown", "Go runtime: " + runtime.Version()} {
			if !strings.Contains(stdout, want) {
				t.Errorf("version output missing %q: %s", want, stdout)
			}
		}
		if strings.Contains(stdout+stderr, configSecret) {
			t.Fatal("version exposed configuration contents")
		}
	})

	t.Run("version and server build metadata honor linker values", func(t *testing.T) {
		binaryName := "corelaycode-buildinfo"
		if runtime.GOOS == "windows" {
			binaryName += ".exe"
		}
		binaryPath := filepath.Join(t.TempDir(), binaryName)
		build := exec.Command("go", "build", "-ldflags", "-X github.com/Dannykkh/corelay-code/internal/buildinfo.Version=v9.8.7-test -X github.com/Dannykkh/corelay-code/internal/buildinfo.Commit=abc123def456", "-o", binaryPath, ".")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build metadata fixture: %v\n%s", err, output)
		}
		configDir := t.TempDir()
		stdout, stderr, err := runDiagnosticsBinary(t, binaryPath, []string{"version"}, configDir, nil)
		if err != nil {
			t.Fatalf("linked version failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
		}
		for _, want := range []string{"Corelay Code CLI version: v9.8.7-test", "Build commit: abc123def456"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("linked version output missing %q: %s", want, stdout)
			}
		}
	})

	t.Run("doctor only reads safe endpoint fields and preserves external server", func(t *testing.T) {
		configDir := t.TempDir()
		workspace := t.TempDir()
		t.Setenv("CORELAY_CONFIG_DIR", configDir)
		const (
			accessSecret   = "DOCTOR_ACCESS_SECRET_SENTINEL"
			providerSecret = "DOCTOR_PROVIDER_SECRET_SENTINEL"
			modelSecret    = "DOCTOR_MODEL_SECRET_SENTINEL"
			hintSecret     = "DOCTOR_HINT_SECRET_SENTINEL"
			commandSecret  = "DOCTOR_COMMAND_SECRET_SENTINEL"
			argSecret      = "DOCTOR_ARG_SECRET_SENTINEL"
			envSecret      = "DOCTOR_ENV_SECRET_SENTINEL"
		)
		if err := os.WriteFile(filepath.Join(workspace, ".mcp.json"), []byte(fmt.Sprintf(`{"mcpServers":{"private-name":{"command":%q,"args":[%q],"env":{"PRIVATE_KEY":%q}}}}`, commandSecret, argSecret, envSecret)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := config.Save(config.Config{Port: 43219, AccessToken: accessSecret}); err != nil {
			t.Fatal(err)
		}

		var requestsMu sync.Mutex
		var requests []string
		serverPID := os.Getpid()
		fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestsMu.Lock()
			requests = append(requests, r.Method+" "+r.URL.Path)
			requestsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/health":
				if r.Method != http.MethodGet || r.Header.Get("X-Access-Token") != "" {
					http.Error(w, "unexpected health request", http.StatusBadRequest)
					return
				}
				_, _ = fmt.Fprintf(w, `{"status":"ok","provider":%q,"model":%q}`, providerSecret, modelSecret)
			case "/":
				if r.Method != http.MethodGet || r.Header.Get("X-Access-Token") != accessSecret {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				_, _ = fmt.Fprintf(w, `{"name":"corelaycode","version":"v7.8.9-test","commit":"server123abc","provider":%q,"model":%q,"hint":%q}`, providerSecret, modelSecret, hintSecret)
			default:
				http.NotFound(w, r)
			}
		}))
		defer fixture.Close()

		stdout, stderr, err := runDiagnosticsBinary(t, binary, []string{"doctor", "-url", fixture.URL, "-workdir", workspace}, configDir, nil)
		if err != nil {
			t.Fatalf("doctor failed: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
		}
		for _, want := range []string{
			"Corelay server: healthy (version v7.8.9-test, build commit server123abc)",
			"MCP: 1 configured; runtime state unknown",
			"LSP (Go/gopls):",
			"Full execution: current OS account privileges; sandbox disabled; no privilege elevation",
			"Browser (installed): ",
			"Git: ",
			"Shell (Bash): ",
			"Workspace execution: runner=",
			"Sandbox capabilities: filesystem=",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("doctor output missing %q: %s", want, stdout)
			}
		}
		for _, secret := range []string{accessSecret, providerSecret, modelSecret, hintSecret, commandSecret, argSecret, envSecret, "private-name", fixture.URL, workspace, configDir} {
			if strings.Contains(stdout+stderr, secret) {
				t.Errorf("doctor exposed sensitive or private value %q: stdout=%s stderr=%s", secret, stdout, stderr)
			}
		}

		requestsMu.Lock()
		gotRequests := append([]string(nil), requests...)
		requestsMu.Unlock()
		if fmt.Sprint(gotRequests) != "[GET /health GET /]" {
			t.Fatalf("doctor requests = %v, want only GET /health and GET /", gotRequests)
		}
		if _, err := os.Stat(filepath.Join(configDir, "managed-chat")); !os.IsNotExist(err) {
			t.Fatalf("doctor created managed server state: %v", err)
		}
		if os.Getpid() != serverPID {
			t.Fatal("test fixture process identity unexpectedly changed")
		}
		response, err := http.Get(fixture.URL + "/health")
		if err != nil {
			t.Fatalf("external server did not survive doctor: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("external server health after doctor = %d, want 200", response.StatusCode)
		}
	})

	t.Run("doctor rejects credential redirects and hides endpoint details", func(t *testing.T) {
		configDir := t.TempDir()
		t.Setenv("CORELAY_CONFIG_DIR", configDir)
		const accessSecret = "DOCTOR_REDIRECT_ACCESS_SECRET_SENTINEL"
		if err := config.Save(config.Config{Port: 43220, AccessToken: accessSecret}); err != nil {
			t.Fatal(err)
		}
		var redirectedRequests atomic.Int32
		attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			redirectedRequests.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer attacker.Close()
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"status":"ok"}`))
				return
			}
			if r.URL.Path == "/" {
				http.Redirect(w, r, attacker.URL, http.StatusFound)
				return
			}
			http.NotFound(w, r)
		}))
		defer redirector.Close()

		stdout, stderr, err := runDiagnosticsBinary(t, binary, []string{"doctor", "-url", redirector.URL}, configDir, nil)
		assertChatProcessExitCode(t, err, 1)
		if !strings.Contains(stdout, "Corelay server: healthy; identity unavailable (version unknown)") {
			t.Fatalf("redirecting doctor output = %q; stderr=%q", stdout, stderr)
		}
		if strings.Contains(stdout+stderr, accessSecret) || strings.Contains(stdout+stderr, attacker.URL) || strings.Contains(stdout+stderr, redirector.URL) {
			t.Fatalf("doctor leaked a token or endpoint: stdout=%q stderr=%q", stdout, stderr)
		}
		if got := redirectedRequests.Load(); got != 0 {
			t.Fatalf("redirect target received %d request(s), want zero", got)
		}

		privateURL := "http://private-user:private-password@127.0.0.1:43220"
		stdout, stderr, err = runDiagnosticsBinary(t, binary, []string{"doctor", "-url", privateURL}, configDir, nil)
		assertChatProcessExitCode(t, err, 2)
		if strings.Contains(stdout+stderr, "private-user") || strings.Contains(stdout+stderr, "private-password") {
			t.Fatalf("invalid URL credentials leaked: stdout=%q stderr=%q", stdout, stderr)
		}
	})

	t.Run("doctor reports unreachable server without raw transport errors", func(t *testing.T) {
		configDir := t.TempDir()
		port := freeManagedServerPort(t)
		baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
		stdout, stderr, err := runDiagnosticsBinary(t, binary, []string{"doctor", "-url", baseURL}, configDir, nil)
		assertChatProcessExitCode(t, err, 1)
		if !strings.Contains(stdout, "Corelay server: unavailable (version unknown)") {
			t.Fatalf("unreachable doctor output = %q; stderr=%q", stdout, stderr)
		}
		if strings.Contains(stdout+stderr, baseURL) || strings.Contains(stdout+stderr, "connect:") {
			t.Fatalf("unreachable doctor leaked endpoint or transport details: stdout=%q stderr=%q", stdout, stderr)
		}
	})
}

func runDiagnosticsBinary(t *testing.T, binary string, args []string, configDir string, overrides map[string]string) (string, string, error) {
	t.Helper()
	command := exec.Command(binary, args...)
	envOverrides := map[string]string{
		"CORELAY_CONFIG_DIR":   configDir,
		"ANICLEW_CONFIG_DIR":   "",
		"CORELAY_ACCESS_TOKEN": "",
		"ANICLEW_ACCESS_TOKEN": "",
		"CORELAY_BROWSER_PATH": "",
	}
	for key, value := range overrides {
		envOverrides[key] = value
	}
	command.Env = replaceProcessEnv(os.Environ(), envOverrides)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func TestResolveDoctorAccessTokenKeepsRemoteCredentialsExplicit(t *testing.T) {
	const environmentSecret = "DOCTOR_ENV_TOKEN_SENTINEL"
	const configuredSecret = "DOCTOR_CONFIG_TOKEN_SENTINEL"
	t.Setenv("CORELAY_ACCESS_TOKEN", environmentSecret)
	t.Setenv("ANICLEW_ACCESS_TOKEN", "")
	cfg := config.Config{AccessToken: configuredSecret}
	if got := resolveDoctorAccessToken("", "https://example.invalid", cfg, nil); got != "" {
		t.Fatalf("remote URL inherited ambient token %q", got)
	}
	if got := resolveDoctorAccessToken("explicit-token", "https://example.invalid", cfg, nil); got != "explicit-token" {
		t.Fatalf("explicit remote token = %q, want explicit-token", got)
	}
	if got := resolveDoctorAccessToken("", "http://localhost:4000", cfg, nil); got != environmentSecret {
		t.Fatalf("loopback token = %q, want environment token", got)
	}
	t.Setenv("CORELAY_ACCESS_TOKEN", "")
	if got := resolveDoctorAccessToken("", "http://127.0.0.1:4000", cfg, nil); got != configuredSecret {
		t.Fatalf("loopback token fallback = %q, want configured token", got)
	}
}
