package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
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
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
)

func TestManagedChatReusesOnlyAuthenticatedCorelayServer(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	const token = "managed-test-token"
	server := httptest.NewServer(managedServerTestHandler(t, token, "corelaycode"))
	defer server.Close()

	var starts atomic.Int32
	lease, err := ensureManagedLocalChatServerWithStarter(
		context.Background(), server.URL,
		config.Config{DefaultProvider: "ollama", DefaultModel: "local"},
		"ollama", "local", token, server.Client(),
		func(int, string, config.Config, string, string) (*managedServerChild, error) {
			starts.Add(1)
			return nil, errors.New("unexpected child start")
		},
	)
	if err != nil {
		t.Fatalf("reuse Corelay server: %v", err)
	}
	if starts.Load() != 0 {
		t.Fatalf("managed child starts = %d, want 0", starts.Load())
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("release external server lease: %v", err)
	}
	response, err := server.Client().Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("existing server was stopped: %v", err)
	}
	_ = response.Body.Close()
}

func TestManagedChatLeavesUnidentifiedPortOwnerUntouched(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	server := httptest.NewServer(managedServerTestHandler(t, "", "other-service"))
	defer server.Close()
	var starts atomic.Int32

	_, err := ensureManagedLocalChatServerWithStarter(
		context.Background(), server.URL,
		config.Config{DefaultProvider: "ollama", DefaultModel: "local"},
		"ollama", "local", "", server.Client(),
		func(int, string, config.Config, string, string) (*managedServerChild, error) {
			starts.Add(1)
			return nil, errors.New("must not start over an occupied port")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "identity and credentials could not be confirmed") {
		t.Fatalf("occupied endpoint error = %v, want identity conflict", err)
	}
	if starts.Load() != 0 {
		t.Fatalf("managed child starts = %d, want 0", starts.Load())
	}
	response, err := server.Client().Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("foreign port owner was disturbed: %v", err)
	}
	_ = response.Body.Close()
}

func TestManagedChatRequiresMatchingCredentialForCorelayPort(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	server := httptest.NewServer(managedServerTestHandler(t, "server-token", "corelaycode"))
	defer server.Close()
	var starts atomic.Int32

	_, err := ensureManagedLocalChatServerWithStarter(
		context.Background(), server.URL,
		config.Config{DefaultProvider: "ollama", DefaultModel: "local"},
		"ollama", "local", "different-token", server.Client(),
		func(int, string, config.Config, string, string) (*managedServerChild, error) {
			starts.Add(1)
			return nil, errors.New("must not replace an authenticated server")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "identity and credentials could not be confirmed") {
		t.Fatalf("credential mismatch error = %v, want identity conflict", err)
	}
	if starts.Load() != 0 {
		t.Fatalf("managed child starts = %d, want 0", starts.Load())
	}
	response, err := server.Client().Get(server.URL + "/health")
	if err != nil {
		t.Fatalf("Corelay port owner was disturbed: %v", err)
	}
	_ = response.Body.Close()
}

func TestManagedChatProbeDoesNotForwardCredentialsAcrossRedirect(t *testing.T) {
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	var forwarded atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != "" {
			forwarded.Add(1)
		}
		writeChatJSON(w, map[string]string{"name": "corelaycode"})
	}))
	defer attacker.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/", http.StatusFound)
	}))
	defer server.Close()
	var starts atomic.Int32

	_, err := ensureManagedLocalChatServerWithStarter(
		context.Background(), server.URL,
		config.Config{DefaultProvider: "ollama", DefaultModel: "local"},
		"ollama", "local", "secret-token", server.Client(),
		func(int, string, config.Config, string, string) (*managedServerChild, error) {
			starts.Add(1)
			return nil, errors.New("must not start over an occupied port")
		},
	)
	if err == nil || starts.Load() != 0 {
		t.Fatalf("redirecting endpoint result = %v starts=%d, want conflict without child", err, starts.Load())
	}
	if forwarded.Load() != 0 {
		t.Fatalf("managed probe forwarded credentials %d time(s) across redirect", forwarded.Load())
	}
}

func TestManagedChatConcurrentStartupUsesOneChildAndLeaseShutdown(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	port := freeManagedServerPort(t)
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	var starts atomic.Int32
	var childMu sync.Mutex
	var child *managedServerChild
	starter := func(port int, leaseDir string, _ config.Config, _, _ string) (*managedServerChild, error) {
		starts.Add(1)
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		server := &http.Server{Handler: managedServerTestHandler(t, "concurrent-token", "corelaycode")}
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		var stopOnce sync.Once
		started := &managedServerChild{
			done: done,
			stopFn: func() {
				stopOnce.Do(func() { _ = server.Close() })
			},
		}
		go func() {
			if waitForManagedServerIdle(context.Background(), leaseDir, 300*time.Millisecond, time.Second) {
				_ = server.Close()
			}
		}()
		childMu.Lock()
		child = started
		childMu.Unlock()
		return started, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	leases := make([]*managedServerLease, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for index := range leases {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			leases[index], errs[index] = ensureManagedLocalChatServerWithStarter(
				ctx, baseURL,
				config.Config{Port: port, DefaultProvider: "ollama", DefaultModel: "local"},
				"ollama", "local", "concurrent-token", &http.Client{Timeout: time.Second}, starter,
			)
		}(index)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ensure %d: %v", index, err)
		}
	}
	if starts.Load() != 1 {
		t.Fatalf("child starts = %d, want one owner", starts.Load())
	}
	for _, lease := range leases {
		if lease == nil {
			t.Fatal("concurrent ensure returned a nil client lease")
		}
	}
	for _, lease := range leases {
		if err := lease.Close(); err != nil {
			t.Fatalf("close client lease: %v", err)
		}
	}
	childMu.Lock()
	started := child
	childMu.Unlock()
	select {
	case serveErr := <-started.done:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Fatalf("managed child serve error = %v", serveErr)
		}
	case <-time.After(3 * time.Second):
		started.stop()
		t.Fatal("managed server stayed alive after its final client lease closed")
	}
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(baseURL, "http://"), 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("managed server listener remained open after idle shutdown")
	}
}

func TestManagedChatConcurrentCLIProcessesShareOneChild(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	port := freeManagedServerPort(t)
	if err := config.Save(config.Config{
		Port: port, DefaultProvider: "ollama", DefaultModel: "local",
		Providers: map[string]config.ProviderSettings{},
	}); err != nil {
		t.Fatalf("write isolated config: %v", err)
	}
	key := managedEndpointKey(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	_, leaseDir := managedServerStatePaths(configDir, key)
	stopMarker := filepath.Join(leaseDir, "cli-test-child-stopped")
	startMarker := filepath.Join(leaseDir, "cli-test-child-started")
	args := []string{"-quiet", "-p", "concurrent managed request"}
	env := map[string]string{
		"CORELAY_CONFIG_DIR":                 configDir,
		"CORELAY_MANAGED_SERVER_TEST_MODE":   "1",
		"CORELAY_MANAGED_TEST_TOKEN":         "managed-cli-race-token",
		"CORELAY_MANAGED_TEST_STOP_FILE":     stopMarker,
		"CORELAY_MANAGED_TEST_START_FILE":    startMarker,
		"CORELAY_MANAGED_TEST_AGENT_BARRIER": "2",
	}
	outputs := make([]string, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for index := range outputs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			outputs[index], errs[index] = runChatSubprocessEnvResult(t, args, "", env)
		}(index)
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil || !strings.Contains(outputs[index], "reply-") {
			t.Fatalf("concurrent CLI %d failed: %v\n%s", index, err, outputs[index])
		}
	}
	started, err := os.ReadFile(startMarker)
	if err != nil {
		t.Fatalf("read child startup marker: %v", err)
	}
	if got := strings.Count(string(started), "started\n"); got != 1 {
		t.Fatalf("managed child start attempts = %d, want exactly one", got)
	}
	waitForManagedTestChildStop(t, stopMarker)
}

func TestManagedChatProductionServerChildLifecycle(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	port := freeManagedServerPort(t)
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	const token = "managed-production-child-token"
	cfg := config.Config{
		Port: port, DefaultProvider: "ollama", DefaultModel: "local",
		AccessToken: token, Providers: map[string]config.ProviderSettings{},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("write isolated config: %v", err)
	}

	binaryPath := buildManagedProductionBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	var child *managedServerChild
	lease, err := ensureManagedLocalChatServerWithStarter(
		ctx, baseURL, cfg, "ollama", "local", token,
		&http.Client{Timeout: 2 * time.Second},
		func(port int, leaseDir string, cfg config.Config, provider, model string) (*managedServerChild, error) {
			var startErr error
			child, startErr = startManagedServerChildWithExecutable(binaryPath, port, leaseDir, cfg, provider, model)
			return child, startErr
		},
	)
	if err != nil {
		t.Fatalf("start production managed server: %v", err)
	}
	if child == nil {
		t.Fatal("production managed server starter returned no child process")
	}
	ready, err := probeManagedCorelay(ctx, baseURL, "127.0.0.1:"+strconv.Itoa(port), token, &http.Client{Timeout: 2 * time.Second})
	if err != nil || !ready {
		t.Fatalf("production server readiness = %t, %v", ready, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("release production server lease: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case childErr := <-child.done:
			if childErr != nil {
				t.Fatalf("production managed server child exited with error: %v", childErr)
			}
			conn, dialErr := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
			if dialErr == nil {
				_ = conn.Close()
				t.Fatal("production managed server listener remained open after child exit")
			}
			return
		default:
		}
		select {
		case <-ctx.Done():
			t.Fatalf("production managed server did not shut down: %v", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	child.stop()
	t.Fatal("production managed server stayed alive after the final lease closed")
}

func buildManagedProductionBinary(t *testing.T) string {
	t.Helper()
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancelBuild()
	binaryName := "corelaycode"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(t.TempDir(), binaryName)
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binaryPath, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build production Corelay child: %v\n%s", err, output)
	}
	return binaryPath
}

func TestManagedChatExplicitRemoteFailureNeverFallsBackToLocal(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	localPort := freeManagedServerPort(t)
	remotePort := freeManagedServerPort(t)
	if localPort == remotePort {
		remotePort = freeManagedServerPort(t)
	}
	if err := config.Save(config.Config{
		Port: localPort, DefaultProvider: "ollama", DefaultModel: "local",
		AccessToken: "configured-token", Providers: map[string]config.ProviderSettings{},
	}); err != nil {
		t.Fatalf("write isolated config: %v", err)
	}
	output, err := runChatSubprocessEnvResult(t, []string{
		"-url", "http://127.0.0.1:" + strconv.Itoa(remotePort), "-p", "explicit remote",
	}, "", map[string]string{"CORELAY_CONFIG_DIR": configDir})
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(output, "Cannot reach Corelay Code") {
		t.Fatalf("explicit remote result = %v, output=%q; want unreachable error and exit 1", err, output)
	}
	listener, listenErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if listenErr != nil {
		t.Fatalf("explicit remote URL started local server on configured port: %v", listenErr)
	}
	_ = listener.Close()
}

func TestManagedChatStartsAndCleansItsServerChild(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	port := freeManagedServerPort(t)
	const token = "managed-child-test-token"
	if err := config.Save(config.Config{
		Port: port, DefaultProvider: "ollama", DefaultModel: "local",
		Providers: map[string]config.ProviderSettings{},
	}); err != nil {
		t.Fatalf("write isolated config: %v", err)
	}
	key := managedEndpointKey(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	_, leaseDir := managedServerStatePaths(configDir, key)
	stopMarker := filepath.Join(leaseDir, "test-child-stopped")
	output, err := runChatSubprocessEnvResult(t, []string{"-quiet", "-p", "managed child request"}, "", map[string]string{
		"CORELAY_CONFIG_DIR":               configDir,
		"CORELAY_MANAGED_SERVER_TEST_MODE": "1",
		"CORELAY_MANAGED_TEST_TOKEN":       token,
		"CORELAY_MANAGED_TEST_STOP_FILE":   stopMarker,
	})
	if err != nil {
		t.Fatalf("managed chat subprocess: %v\n%s", err, output)
	}
	if !strings.Contains(output, "reply-1") {
		t.Fatalf("managed child did not serve the one-shot request: %s", output)
	}
	waitForManagedTestChildStop(t, stopMarker)
	if err := os.Remove(stopMarker); err != nil {
		t.Fatalf("reset child stop marker: %v", err)
	}
	output, err = runChatSubprocessEnvResult(t, []string{"-quiet", "-p", "managed failed request"}, "", map[string]string{
		"CORELAY_CONFIG_DIR":               configDir,
		"CORELAY_MANAGED_SERVER_TEST_MODE": "1",
		"CORELAY_MANAGED_TEST_TOKEN":       token,
		"CORELAY_MANAGED_TEST_STOP_FILE":   stopMarker,
		"CORELAY_MANAGED_TEST_FAIL_AGENT":  "1",
	})
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(output, "Error:") {
		t.Fatalf("failed managed chat result = %v, output=%q; want exit 1", err, output)
	}
	waitForManagedTestChildStop(t, stopMarker)
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("managed server listener survived failed-run cleanup: %v", err)
	}
	_ = listener.Close()
}

func waitForManagedTestChildStop(t *testing.T, stopMarker string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopMarker); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(stopMarker); err != nil {
		t.Fatalf("managed server child did not stop after final lease: %v", err)
	}
}

func TestManagedServerProcessHelper(t *testing.T) {
	if os.Getenv("CORELAY_MANAGED_SERVER_TEST_CHILD") != "1" {
		return
	}
	port, err := strconv.Atoi(os.Getenv("CORELAY_MANAGED_TEST_PORT"))
	if err != nil || port < 1 {
		t.Fatalf("invalid test child port: %v", err)
	}
	testToken := os.Getenv("CORELAY_MANAGED_TEST_TOKEN")
	if _, err := config.Update(func(cfg *config.Config) error {
		if strings.TrimSpace(cfg.AccessToken) == "" {
			cfg.AccessToken = testToken
		}
		return nil
	}); err != nil {
		t.Fatalf("initialize test child credential: %v", err)
	}
	if marker := os.Getenv("CORELAY_MANAGED_TEST_START_FILE"); marker != "" {
		if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
			t.Fatalf("create child start marker directory: %v", err)
		}
		file, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatalf("open child start marker: %v", err)
		}
		_, writeErr := file.WriteString("started\n")
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("write child start marker: %v", errors.Join(writeErr, closeErr))
		}
	}
	workspace, err := os.Getwd()
	if err != nil {
		t.Fatalf("get test child workspace: %v", err)
	}
	fixture := &chatSessionAPIFixture{workspace: workspace, sessions: make(map[string]*agent.Session)}
	var handler http.Handler = managedChatTestChildHandler(t, fixture, os.Getenv("CORELAY_MANAGED_TEST_TOKEN"))
	if barrierCount, _ := strconv.Atoi(os.Getenv("CORELAY_MANAGED_TEST_AGENT_BARRIER")); barrierCount > 1 {
		var requests atomic.Int32
		barrier := make(chan struct{})
		baseHandler := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/agent" {
				if requests.Add(1) >= int32(barrierCount) {
					select {
					case <-barrier:
					default:
						close(barrier)
					}
				}
				select {
				case <-barrier:
				case <-time.After(10 * time.Second):
					http.Error(w, "timed out waiting for concurrent CLI requests", http.StatusGatewayTimeout)
					return
				}
			}
			baseHandler.ServeHTTP(w, r)
		})
	}
	server := &http.Server{Handler: handler}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("listen in test child: %v", err)
	}
	leaseDir := os.Getenv("CORELAY_MANAGED_LEASE_DIR")
	idleTimeout, _ := time.ParseDuration(os.Getenv("CORELAY_MANAGED_IDLE_TIMEOUT"))
	go func() {
		if waitForManagedServerIdle(context.Background(), leaseDir, idleTimeout, managedServerLeaseExpiry) {
			_ = server.Close()
		}
	}()
	serveErr := server.Serve(listener)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		t.Errorf("test managed child serve: %v", serveErr)
	}
	if marker := os.Getenv("CORELAY_MANAGED_TEST_STOP_FILE"); marker != "" {
		if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
			t.Errorf("create child stop marker directory: %v", err)
		} else if err := os.WriteFile(marker, []byte("stopped\n"), 0600); err != nil {
			t.Errorf("write child stop marker: %v", err)
		}
	}
}

func startManagedServerTestChild(port int, leaseDir string, _ config.Config, _, _ string) (*managedServerChild, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, "-test.run=^TestManagedServerProcessHelper$")
	cmd.Env = replaceProcessEnv(os.Environ(), map[string]string{
		"CORELAY_MANAGED_SERVER_TEST_CHILD": "1",
		"CORELAY_MANAGED_TEST_PORT":         strconv.Itoa(port),
		"CORELAY_MANAGED_LEASE_DIR":         leaseDir,
		"CORELAY_MANAGED_CHILD":             "1",
		"CORELAY_MANAGED_IDLE_TIMEOUT":      managedServerIdleTimeout.String(),
	})
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	configureManagedServerChildCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	child := &managedServerChild{cmd: cmd, done: make(chan error, 1)}
	go func() { child.done <- cmd.Wait() }()
	return child, nil
}

func managedChatTestChildHandler(t *testing.T, fixture *chatSessionAPIFixture, token string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Access-Token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/":
			writeChatJSON(w, map[string]string{"name": "corelaycode"})
		case "/api/config":
			writeChatJSON(w, map[string]string{"provider": "fixture", "model": "fixture-model"})
		case "/health":
			writeChatJSON(w, map[string]string{"status": "ok"})
		case "/api/agent":
			if os.Getenv("CORELAY_MANAGED_TEST_FAIL_AGENT") == "1" {
				http.Error(w, "fixture agent failure", http.StatusBadGateway)
				return
			}
			fixture.ServeHTTP(w, r)
		default:
			fixture.ServeHTTP(w, r)
		}
	})
}

func managedServerTestHandler(t *testing.T, token, name string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("X-Access-Token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/":
			_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
		case "/api/config":
			_ = json.NewEncoder(w).Encode(map[string]string{"provider": "fixture"})
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	})
}

func freeManagedServerPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free loopback port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
