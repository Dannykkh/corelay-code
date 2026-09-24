package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

func TestBrowserChatReportsInterruptedStreamWithoutSavingSuccess(t *testing.T) {
	partial := "data: {\"type\":\"text\",\"data\":\"PARTIAL_ONLY_RESPONSE\"}\n\n"
	for name, payload := range map[string]string{
		"partial-only":      partial,
		"unterminated-done": partial + "data: {\"type\":\"done\",\"data\":{}}",
		"empty-eof":         "",
	} {
		t.Run(name, func(t *testing.T) { checkBrowserChatInterruptedStream(t, payload) })
	}
}

func checkBrowserChatInterruptedStream(t *testing.T, payload string) {
	bin, ok := launcher.LookPath()
	if !ok || bin == "" {
		t.Skip("no installed Chrome/Chromium")
	}
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	t.Setenv("CORELAY_ACCESS_TOKEN", "")
	t.Setenv("ANICLEW_ACCESS_TOKEN", "")
	t.Setenv("CORELAY_BIND", "127.0.0.1")
	a := t.TempDir()
	b := t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "readme.txt"), []byte("alpha content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "blob.bin"), []byte{0, 1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "large.txt"), []byte(strings.Repeat("large\n", 30000)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Update(func(cfg *config.Config) error {
		cfg.WorkDir = a
		cfg.Projects = []config.Project{{Path: a, Name: "Alpha"}, {Path: b, Name: "Beta"}}
		cfg.DefaultProvider = "ollama"
		cfg.DefaultModel = "fake"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	portLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portLn.Addr().(*net.TCPAddr).Port
	_ = portLn.Close()
	s := New(s10BrowserProvider{}, "fake", port)
	s.SetWorkDir(a)
	sessionStore := agent.NewSessionStore(t.TempDir())
	s.SetSessionStore(sessionStore)
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	defer func() { _ = s.Shutdown(context.Background()) }()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}
	ready := false
	for i := 0; i < 100; i++ {
		resp, getErr := client.Get(base + "/health")
		if getErr == nil {
			_ = resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("server did not become ready")
	}
	t.Log("server ready", base)
	u := launcher.New().Bin(bin).Headless(true).MustLaunch()
	t.Log("browser launched")
	browser := rod.New().ControlURL(u).MustConnect()
	t.Log("browser connected")
	defer browser.MustClose()

	router := browser.HijackRequests()
	router.MustAdd(base+"/api/agent", func(h *rod.Hijack) {
		h.Response.SetHeader("Content-Type", "text/event-stream")
		h.Response.SetBody(payload)
	})
	go router.Run()
	defer func() { _ = router.Stop() }()
	page := browser.MustPage(base + "/app").Timeout(20 * time.Second)
	page.MustWaitLoad()
	page.MustElement("textarea").MustInput("stream truncation probe")
	page.MustElementR("button", "전송|Send").MustClick()
	var body string
	for i := 0; i < 300; i++ {
		body = page.MustEval(`() => document.body.innerText`).Str()
		if strings.Contains(body, "Connection interrupted before the run completed") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if (payload != "" && !strings.Contains(body, "PARTIAL_ONLY_RESPONSE")) || !strings.Contains(body, "Connection interrupted before the run completed") || !strings.Contains(body, "Reload this session") {
		t.Fatalf("missing partial output/interruption notice: %s", body)
	}
	page.MustElementR("button", "전송|Send")
	// Only the pre-run user request may have been saved. The truncated done
	// frame cannot authorize the compatibility successful-transcript save.
	summaries := sessionStore.List(a)
	if len(summaries) != 1 {
		t.Fatalf("sessions=%d", len(summaries))
	}
	saved, err := sessionStore.Get(summaries[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 1 {
		t.Fatalf("interrupted response committed session revision %d", saved.Revision)
	}
	for _, message := range saved.Messages {
		if strings.Contains(message.Content, "PARTIAL_ONLY_RESPONSE") {
			t.Fatal("partial response committed as successful transcript")
		}
	}
}
