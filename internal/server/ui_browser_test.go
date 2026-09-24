package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

type s10BrowserProvider struct{}

func (s10BrowserProvider) Name() string              { return "ollama" }
func (s10BrowserProvider) DisplayName() string       { return "Ollama" }
func (s10BrowserProvider) Models() []types.ModelInfo { return nil }
func (s10BrowserProvider) Validate() error           { return nil }
func (s10BrowserProvider) StreamMessage(ctx context.Context, _ *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	ch := make(chan types.SSEEvent)
	go func() { defer close(ch); <-ctx.Done() }()
	return ch, nil
}

func TestS10BrowserUIProjectFilesAndStaleStreams(t *testing.T) {
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
	s.SetSessionStore(agent.NewSessionStore(t.TempDir()))
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
	var agentRequests atomic.Int32
	agentStarted := make(chan struct{})
	releaseFirstAgent := make(chan struct{})
	router := browser.HijackRequests()
	router.MustAdd(base+"/api/agent", func(h *rod.Hijack) {
		request := agentRequests.Add(1)
		if request == 1 {
			close(agentStarted)
			<-releaseFirstAgent
		}
		h.Response.SetHeader("Content-Type", "text/event-stream")
		payload := "data: {\"type\":\"text\",\"data\":\"FRESH_RESPONSE_MARKER\"}\n\n" +
			"data: {\"type\":\"diff\",\"data\":{\"file\":\"changed.txt\",\"diff\":\"+changed\"}}\n\n" +
			"data: {\"type\":\"undo_preview\",\"data\":{\"entries\":[{\"id\":\"cp-1\",\"path\":\"changed.txt\",\"status\":\"restorable\"}]}}\n\n" +
			"data: {\"type\":\"done\",\"data\":{}}\n\n" +
			"data: {\"type\":\"stream_end\",\"data\":{}}\n\n"
		if request == 1 {
			payload = strings.Replace(payload, "FRESH_RESPONSE_MARKER", "STALE_RESPONSE_MARKER", 1)
		}
		h.Response.SetBody(payload)
	})
	go router.Run()
	defer func() { _ = router.Stop() }()
	page := browser.MustPage(base + "/app")
	t.Log("page created")
	if err := page.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	t.Log("page loaded")
	page = page.Timeout(15 * time.Second)
	page.MustElementR("span", "Project")
	t.Log("project rail visible")
	var body string
	for i := 0; i < 100; i++ {
		body = page.MustEval(`() => document.body.innerText`).Str()
		if strings.Contains(body, "Alpha") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "Alpha") {
		t.Fatalf("body did not load Alpha: %s", body)
	}
	if !strings.Contains(body, "Permission unknown") || !strings.Contains(body, "Project") {
		t.Fatalf("context strip did not render: %s", body)
	}
	page.MustElement(`button[title="Files"], button[title="파일"]`).MustClick()
	time.Sleep(200 * time.Millisecond)
	body = page.MustEval(`() => document.body.innerText`).Str()
	if !strings.Contains(body, "readme.txt") {
		t.Fatalf("files view did not load readme.txt: %s", body)
	}
	page.MustElementR("button", "readme.txt").MustClick()
	time.Sleep(200 * time.Millisecond)
	body = page.MustEval(`() => document.body.innerText`).Str()
	if !strings.Contains(body, "alpha content") {
		t.Fatalf("read content missing: %s", body)
	}
	page.MustElementR("button", "blob.bin").MustClick()
	time.Sleep(200 * time.Millisecond)
	body = page.MustEval(`() => document.body.innerText`).Str()
	if !strings.Contains(body, "binary") || strings.Contains(body, "\nEdit\n") {
		t.Fatalf("binary viewer state invalid: %s", body)
	}
	page.MustElementR("button", "large.txt").MustClick()
	time.Sleep(200 * time.Millisecond)
	body = page.MustEval(`() => document.body.innerText`).Str()
	if !strings.Contains(body, "too_large") || !strings.Contains(body, "176K") {
		t.Fatalf("large-file viewer state invalid: %s", body)
	}
	selectProject := func(current, target string) {
		// Scope the interaction to the side-panel picker. A broad button text
		// selector can hit the status-bar copy while the side panel is loading.
		picker := "div.w-64 > div.border-b"
		page.MustElementR(picker+" > button", current).MustClick()
		// The dropdown is rendered by React after the header click. Waiting on
		// the actual project option avoids racing that render under the full suite.
		page.MustElementR(picker+" div.cursor-pointer", target).MustClick()
	}
	selectProject("Alpha", "Beta")
	for i := 0; i < 100; i++ {
		body = page.MustEval(`() => document.body.innerText`).Str()
		if strings.Contains(body, "Beta") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "Beta") {
		t.Fatalf("project switch did not render Beta: %s", body)
	}
	if err := page.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := page.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		body = page.MustEval(`() => document.body.innerText`).Str()
		if strings.Contains(body, "Beta") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "Beta") {
		t.Fatalf("refresh lost selected project: %s", body)
	}
	page.MustElement("textarea").MustInput("stale request")
	page.MustElementR("button", "전송|Send").MustClick()
	select {
	case <-agentStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("delayed agent request did not reach the browser harness")
	}
	selectProject("Beta", "Alpha")
	time.Sleep(500 * time.Millisecond)
	close(releaseFirstAgent)
	time.Sleep(500 * time.Millisecond)
	body = page.MustEval(`() => document.body.innerText`).Str()
	if strings.Contains(body, "STALE_RESPONSE_MARKER") {
		t.Fatalf("stale response reached the switched project: %s", body)
	}
	if strings.Contains(body, "Connection interrupted before the run completed") {
		t.Fatalf("cancelled previous project stream raised interruption notice in new project: %s", body)
	}
	if err := page.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := page.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		body = page.MustEval(`() => document.body.innerText`).Str()
		if strings.Contains(body, "Alpha") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "Alpha") {
		t.Fatalf("reload after stale response did not restore Alpha: %s", body)
	}
	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{Width: 420, Height: 800, DeviceScaleFactor: 1, Mobile: true}); err != nil {
		t.Fatal(err)
	}
	page.MustElement(`button[title="Chat"], button[title="채팅"]`).MustClick()
	textarea := page.MustElement("textarea")
	textarea.MustClick().MustInput("keyboard probe")
	value := page.MustEval(`() => document.querySelector('textarea')?.value || ''`).Str()
	if value != "keyboard probe" {
		t.Fatalf("keyboard input value=%q", value)
	}
	page.MustElementR("button", "전송|Send").MustClick()
	for i := 0; i < 100; i++ {
		body = page.MustEval(`() => document.body.innerText`).Str()
		if strings.Contains(body, "Changes") && strings.Contains(body, "Restore") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "Changes") || !strings.Contains(body, "changed.txt") || !strings.Contains(body, "Restore") {
		t.Fatalf("diff/restore UI did not render: %s", body)
	}
	page.MustElementR("button", "Restore|복원").MustClick()
	page.MustElement(`button[title="Theme"]`).MustClick()
	if got := page.MustEval(`() => document.documentElement.getAttribute('data-theme')`).Str(); got != "light" {
		t.Fatalf("theme toggle = %q, want light", got)
	}
}
