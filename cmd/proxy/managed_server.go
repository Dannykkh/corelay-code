package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
)

const (
	managedServerStartupTimeout = 25 * time.Second
	managedServerLeaseInterval  = time.Second
	managedServerLeaseExpiry    = 6 * time.Second
	managedServerIdleTimeout    = 2 * time.Second
)

var launchManagedChatServerChild = startManagedServerChild

type managedServerLease struct {
	path      string
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func (l *managedServerLease) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		close(l.stop)
		<-l.done
		l.closeErr = os.Remove(l.path)
		if errors.Is(l.closeErr, os.ErrNotExist) {
			l.closeErr = nil
		}
	})
	return l.closeErr
}

func newManagedServerLease(dir string) (*managedServerLease, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create managed server lease directory: %w", err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("create managed server lease identity: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("client-%d-%s.lease", os.Getpid(), hex.EncodeToString(random[:])))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("create managed server lease: %w", err)
	}
	if _, err := io.WriteString(file, "corelay-managed-chat\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write managed server lease: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close managed server lease: %w", err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("initialize managed server lease heartbeat: %w", err)
	}
	lease := &managedServerLease{path: path, stop: make(chan struct{}), done: make(chan struct{})}
	go lease.heartbeat()
	return lease, nil
}

func (l *managedServerLease) heartbeat() {
	defer close(l.done)
	ticker := time.NewTicker(managedServerLeaseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-ticker.C:
			_ = os.Chtimes(l.path, now, now)
		}
	}
}

func managedServerHasLiveLease(dir string, now time.Time, leaseExpiry time.Duration) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lease") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) <= leaseExpiry {
			return true
		}
	}
	return false
}

func waitForManagedServerIdle(ctx context.Context, dir string, idleTimeout, leaseExpiry time.Duration) bool {
	lock := waitForManagedServerIdleLock(ctx, dir, idleTimeout, leaseExpiry)
	if lock == nil {
		return false
	}
	_ = lock.Close()
	return true
}

// waitForManagedServerIdleLock serializes the final no-lease check with new
// clients. The caller keeps the lock until the server has stopped accepting
// requests, so a client cannot attach to a process that is already shutting
// down.
func waitForManagedServerIdleLock(ctx context.Context, dir string, idleTimeout, leaseExpiry time.Duration) *managedServerStartLock {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var idleSince time.Time
	for {
		if managedServerHasLiveLease(dir, time.Now(), leaseExpiry) {
			idleSince = time.Time{}
		} else if idleSince.IsZero() {
			idleSince = time.Now()
		} else if time.Since(idleSince) >= idleTimeout {
			lockPath := filepath.Join(filepath.Dir(dir), "startup.lock")
			lock, err := acquireManagedServerStartLock(ctx, lockPath)
			if err != nil {
				return nil
			}
			if managedServerHasLiveLease(dir, time.Now(), leaseExpiry) {
				_ = lock.Close()
				idleSince = time.Time{}
				continue
			}
			return lock
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func managedServerStatePaths(baseDir, endpointKey string) (string, string) {
	root := filepath.Join(baseDir, "managed-chat")
	endpointDir := filepath.Join(root, endpointKey)
	return filepath.Join(endpointDir, "startup.lock"), filepath.Join(endpointDir, "leases")
}

func managedEndpointKey(address string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(address)))
	return hex.EncodeToString(digest[:12])
}

func ensureManagedLocalChatServer(
	ctx context.Context,
	baseURL string,
	cfg config.Config,
	provider, model, token string,
	client *http.Client,
) (*managedServerLease, error) {
	return ensureManagedLocalChatServerWithStarter(ctx, baseURL, cfg, provider, model, token, client, launchManagedChatServerChild)
}

type managedServerChildStarter func(int, string, config.Config, string, string) (*managedServerChild, error)

func ensureManagedLocalChatServerWithStarter(
	ctx context.Context,
	baseURL string,
	cfg config.Config,
	provider, model, token string,
	client *http.Client,
	startChild managedServerChildStarter,
) (*managedServerLease, error) {
	address, port, err := managedLoopbackAddress(baseURL)
	if err != nil {
		return nil, err
	}
	baseDir, err := filepath.Abs(config.BaseDir())
	if err != nil {
		return nil, fmt.Errorf("resolve Corelay config directory: %w", err)
	}
	key := managedEndpointKey(address)
	lockPath, leaseDir := managedServerStatePaths(baseDir, key)
	startupCtx, cancel := context.WithTimeout(ctx, managedServerStartupTimeout)
	defer cancel()
	lock, err := acquireManagedServerStartLock(startupCtx, lockPath)
	if err != nil {
		return nil, fmt.Errorf("wait for local Corelay server startup: %w", err)
	}
	defer lock.Close()

	ready, probeErr := probeManagedCorelay(startupCtx, baseURL, address, token, client)
	if ready {
		return newManagedServerLease(leaseDir)
	}
	if probeErr != nil {
		return nil, probeErr
	}
	if provider == "" || model == "" {
		return nil, errors.New("no Corelay Code server is running and no provider/model is configured; start Corelay Code once to choose a provider and model")
	}

	lease, err := newManagedServerLease(leaseDir)
	if err != nil {
		return nil, err
	}
	if startChild == nil {
		_ = lease.Close()
		return nil, errors.New("managed server starter is unavailable")
	}
	child, err := startChild(port, leaseDir, cfg, provider, model)
	if err != nil {
		_ = lease.Close()
		return nil, err
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case childErr := <-child.done:
			ready, verifyErr := probeManagedCorelay(startupCtx, baseURL, address, token, client)
			if ready {
				return lease, nil
			}
			_ = lease.Close()
			if verifyErr != nil {
				return nil, verifyErr
			}
			if childErr == nil {
				childErr = errors.New("managed server exited before becoming ready")
			}
			return nil, fmt.Errorf("managed Corelay server did not become ready: %w", childErr)
		case <-ticker.C:
			ready, _ := probeManagedCorelay(startupCtx, baseURL, address, token, client)
			if ready {
				return lease, nil
			}
		case <-startupCtx.Done():
			child.stop()
			_ = lease.Close()
			return nil, fmt.Errorf("managed Corelay server did not become ready: %w", startupCtx.Err())
		}
	}
}

func managedLoopbackAddress(raw string) (string, int, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "http") || parsed.Hostname() == "" {
		return "", 0, errors.New("managed Corelay startup requires an HTTP loopback URL")
	}
	host := strings.Trim(parsed.Hostname(), "[]")
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", 0, errors.New("managed Corelay startup is limited to loopback addresses")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", 0, errors.New("managed Corelay startup URL must contain only a loopback host and port")
	}
	portText := parsed.Port()
	if portText == "" {
		portText = "80"
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, errors.New("managed Corelay startup URL has an invalid port")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), port, nil
}

func probeManagedCorelay(ctx context.Context, baseURL, address, token string, client *http.Client) (bool, error) {
	if token == "" {
		token = resolveAccessTokenForURL("", baseURL)
	}
	dialer := net.Dialer{Timeout: 350 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		if isConnectionRefused(err) {
			return false, nil
		}
		return false, fmt.Errorf("local endpoint %s could not be checked safely: %w", address, err)
	}
	_ = conn.Close()

	probeClient := client
	if probeClient == nil {
		probeClient = &http.Client{Timeout: 2 * time.Second, CheckRedirect: rejectManagedProbeRedirect}
	} else {
		copyClient := *probeClient
		if copyClient.Timeout == 0 || copyClient.Timeout > 2*time.Second {
			copyClient.Timeout = 2 * time.Second
		}
		copyClient.CheckRedirect = rejectManagedProbeRedirect
		probeClient = &copyClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/", nil)
	if err != nil {
		return false, err
	}
	if token != "" {
		request.Header.Set("X-Access-Token", token)
	}
	response, err := probeClient.Do(request)
	if err != nil {
		return false, managedEndpointConflict(address)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, managedEndpointConflict(address)
	}
	var identity struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&identity); err != nil || identity.Name != "corelaycode" {
		return false, managedEndpointConflict(address)
	}
	configRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/config", nil)
	if err != nil {
		return false, err
	}
	if token != "" {
		configRequest.Header.Set("X-Access-Token", token)
	}
	configResponse, err := probeClient.Do(configRequest)
	if err != nil {
		return false, managedEndpointConflict(address)
	}
	_ = configResponse.Body.Close()
	if configResponse.StatusCode != http.StatusOK {
		return false, managedEndpointConflict(address)
	}
	return true, nil
}

func rejectManagedProbeRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func managedEndpointConflict(address string) error {
	return fmt.Errorf("local endpoint %s is occupied but its Corelay identity and credentials could not be confirmed; the existing process was left untouched", address)
}

type managedServerChild struct {
	cmd    *exec.Cmd
	done   chan error
	stopFn func()
}

func startManagedServerChild(port int, leaseDir string, cfg config.Config, provider, model string) (*managedServerChild, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Corelay executable: %w", err)
	}
	return startManagedServerChildWithExecutable(executable, port, leaseDir, cfg, provider, model)
}

func startManagedServerChildWithExecutable(executable string, port int, leaseDir string, cfg config.Config, provider, model string) (*managedServerChild, error) {
	args := []string{"-port", strconv.Itoa(port), "-provider", provider, "-model", model}
	if cfg.RouterEnabled {
		args = append(args, "-router")
	}
	cmd := exec.Command(executable, args...)
	cmd.Env = replaceProcessEnv(os.Environ(), map[string]string{
		"CORELAY_BIND":                 "127.0.0.1",
		"CORELAY_MANAGED_LEASE_DIR":    leaseDir,
		"CORELAY_MANAGED_CHILD":        "1",
		"CORELAY_MANAGED_IDLE_TIMEOUT": managedServerIdleTimeout.String(),
	})
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	configureManagedServerChildCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start managed Corelay server: %w", err)
	}
	child := &managedServerChild{cmd: cmd, done: make(chan error, 1)}
	go func() { child.done <- cmd.Wait() }()
	return child, nil
}

func (c *managedServerChild) stop() {
	if c == nil {
		return
	}
	if c.stopFn != nil {
		c.stopFn()
		return
	}
	if c.cmd == nil || c.cmd.Process == nil {
		return
	}
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Kill()
	<-c.done
}

func replaceProcessEnv(current []string, overrides map[string]string) []string {
	result := make([]string, 0, len(current)+len(overrides))
	for _, value := range current {
		name, _, ok := strings.Cut(value, "=")
		if !ok {
			result = append(result, value)
			continue
		}
		for key := range overrides {
			if strings.EqualFold(name, key) {
				goto skipped
			}
		}
		result = append(result, value)
	skipped:
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}

func managedChildLeaseSettings() (string, time.Duration, bool) {
	dir := strings.TrimSpace(os.Getenv("CORELAY_MANAGED_LEASE_DIR"))
	if dir == "" || os.Getenv("CORELAY_MANAGED_CHILD") != "1" {
		return "", 0, false
	}
	idleTimeout, _ := time.ParseDuration(os.Getenv("CORELAY_MANAGED_IDLE_TIMEOUT"))
	if idleTimeout <= 0 {
		idleTimeout = managedServerIdleTimeout
	}
	return dir, idleTimeout, true
}

func waitForManagedChildIdle(ctx context.Context, dir string, idleTimeout time.Duration) *managedServerStartLock {
	return waitForManagedServerIdleLock(ctx, dir, idleTimeout, managedServerLeaseExpiry)
}
