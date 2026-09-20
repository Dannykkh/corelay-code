//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/config"
)

func TestManagedServerSurvivesOwnerProcessGroupInterruptWhileAnotherLeaseIsLive(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CORELAY_CONFIG_DIR", configDir)
	port := freeManagedServerPort(t)
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	const token = "managed-owner-interrupt-token"
	cfg := config.Config{
		Port: port, DefaultProvider: "ollama", DefaultModel: "local",
		AccessToken: token, Providers: map[string]config.ProviderSettings{},
	}
	if err := config.Save(cfg); err != nil {
		t.Fatalf("write isolated config: %v", err)
	}

	binaryPath := buildManagedProductionBinary(t)
	owner := exec.Command(binaryPath, "chat", "-plain", "-quiet")
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create owner CLI stdin pipe: %v", err)
	}
	defer stdinWriter.Close()
	owner.Stdin = stdinReader
	owner.Stdout = io.Discard
	owner.Stderr = io.Discard
	configureManagedServerChildCommand(owner)
	if err := owner.Start(); err != nil {
		_ = stdinReader.Close()
		t.Fatalf("start owner CLI process: %v", err)
	}
	_ = stdinReader.Close()
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- owner.Wait() }()
	ownerReaped := false
	t.Cleanup(func() {
		if !ownerReaped {
			_ = owner.Process.Kill()
			<-ownerDone
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ready := false
	var lastProbeErr error
	readyDeadline := time.Now().Add(35 * time.Second)
	for time.Now().Before(readyDeadline) {
		ready, lastProbeErr = probeManagedCorelay(ctx, baseURL, address, token, &http.Client{Timeout: 2 * time.Second})
		if ready {
			break
		}
		select {
		case ownerErr := <-ownerDone:
			ownerReaped = true
			t.Fatalf("owner CLI exited before server was ready: %v", ownerErr)
		case <-ctx.Done():
			t.Fatalf("owner CLI did not start managed server: %v", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !ready {
		t.Fatalf("owner CLI server did not become ready: %v", lastProbeErr)
	}

	secondLease, err := ensureManagedLocalChatServer(ctx, baseURL, cfg, "ollama", "local", token, &http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("attach second client lease: %v", err)
	}
	defer secondLease.Close()

	if err := syscall.Kill(-owner.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatalf("send Ctrl-C to owner process group: %v", err)
	}
	select {
	case <-ownerDone:
		ownerReaped = true
	case <-time.After(5 * time.Second):
		t.Fatal("owner CLI did not exit after its process group received Ctrl-C")
	}

	ready, err = probeManagedCorelay(ctx, baseURL, address, token, &http.Client{Timeout: 2 * time.Second})
	if err != nil || !ready {
		t.Fatalf("server stopped while second lease remained live: ready=%t err=%v", ready, err)
	}
	if err := secondLease.Close(); err != nil {
		t.Fatalf("release second client lease: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if dialErr != nil {
			return
		}
		_ = conn.Close()
		select {
		case <-ctx.Done():
			t.Fatalf("managed child remained after final lease close: %v", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("managed server did not stop after the second lease closed")
}
