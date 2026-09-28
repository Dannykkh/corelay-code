package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/config"
	"github.com/Dannykkh/corelay-code/internal/types"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

// overhaulApprovalProvider asks for one Bash call whose effect is unknown, which
// needs explicit approval under the workspace mode's moderate auto-approve
// threshold whether or not filesystem isolation is available. Once the
// transcript holds that assistant turn it answers with a final text.
type overhaulApprovalProvider struct {
	mu    sync.Mutex
	calls int
}

func (*overhaulApprovalProvider) Name() string              { return "ollama" }
func (*overhaulApprovalProvider) DisplayName() string       { return "Ollama" }
func (*overhaulApprovalProvider) Models() []types.ModelInfo { return nil }
func (*overhaulApprovalProvider) Validate() error           { return nil }

func overhaulJSON(value interface{}) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func (p *overhaulApprovalProvider) StreamMessage(_ context.Context, req *types.MessagesRequest, _ *types.StreamOptions) (<-chan types.SSEEvent, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	finish := false
	for _, message := range req.Messages {
		if message.Role == "assistant" {
			finish = true
			break
		}
	}
	events := make(chan types.SSEEvent, 8)
	if !finish {
		events <- types.SSEEvent{Type: "content_block_start", ContentBlock: overhaulJSON(map[string]string{"type": "tool_use", "id": "call-overhaul-1", "name": "Bash"})}
		events <- types.SSEEvent{Type: "content_block_delta", Delta: overhaulJSON(map[string]string{"type": "input_json_delta", "partial_json": `{"command":"custom-tool --version"}`})}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_delta", Delta: overhaulJSON(map[string]string{"stop_reason": "tool_use"})}
	} else {
		events <- types.SSEEvent{Type: "content_block_start", ContentBlock: overhaulJSON(map[string]string{"type": "text"})}
		events <- types.SSEEvent{Type: "content_block_delta", Delta: overhaulJSON(map[string]string{"type": "text_delta", "text": "APPROVAL_FLOW_DONE"})}
		events <- types.SSEEvent{Type: "content_block_stop"}
		events <- types.SSEEvent{Type: "message_delta", Delta: overhaulJSON(map[string]string{"stop_reason": "end_turn"})}
	}
	events <- types.SSEEvent{Type: "message_stop"}
	close(events)
	return events, nil
}

// TestOverhaulBrowserApprovalFlowAndDestinations drives the embedded Web bundle
// through a real approval raised by the server: the linked step card shows the
// decision, the activity bar badges it while the workspace is hidden, a denial
// lets the run finish without re-alerting, and the server memory page stays
// reachable from Settings.
func TestOverhaulBrowserApprovalFlowAndDestinations(t *testing.T) {
	bin, ok := launcher.LookPath()
	if !ok || bin == "" {
		t.Skip("no installed Chrome/Chromium")
	}
	t.Setenv("CORELAY_CONFIG_DIR", t.TempDir())
	t.Setenv("ANICLEW_CONFIG_DIR", "")
	t.Setenv("CORELAY_ACCESS_TOKEN", "")
	t.Setenv("ANICLEW_ACCESS_TOKEN", "")
	t.Setenv("CORELAY_BIND", "127.0.0.1")
	t.Setenv("CORELAY_MEMORY", "off")
	t.Setenv("CORELAY_AUTOSKILL", "off")
	t.Setenv("CORELAY_AUTOVERIFY", "off")
	workspace := t.TempDir()
	if _, err := config.Update(func(cfg *config.Config) error {
		cfg.WorkDir = workspace
		cfg.Projects = []config.Project{{Path: workspace, Name: "Overhaul"}}
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
	provider := &overhaulApprovalProvider{}
	s := New(provider, "fake", port)
	s.SetWorkDir(workspace)
	s.SetSessionStore(agent.NewSessionStore(t.TempDir()))
	go func() { _ = s.Start() }()
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

	u := launcher.New().Bin(bin).Headless(true).MustLaunch()
	browser := rod.New().ControlURL(u).MustConnect()
	defer browser.MustClose()
	page := browser.MustPage(base + "/app")
	if err := page.WaitLoad(); err != nil {
		t.Fatal(err)
	}
	// Each step gets its own deadline; one page-wide timeout would let slow setup
	// on a busy runner eat the time budget of later waits.
	step := func() *rod.Page { return page.Timeout(20 * time.Second) }
	bodyText := func() string { return page.MustEval(`() => document.body.innerText`).Str() }
	providerCalls := func() int {
		provider.mu.Lock()
		defer provider.mu.Unlock()
		return provider.calls
	}
	// waitFor reports what the page showed instead of a bare deadline panic, and
	// the provider call count tells a UI-binding problem (1 call, run waiting)
	// apart from a tool call that never needed approval (2 calls, run finished).
	waitFor := func(selector string) *rod.Element {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if has, element, _ := page.Has(selector); has {
				return element
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s did not appear (provider calls=%d); page text:\n%s", selector, providerCalls(), bodyText())
		return nil
	}

	// The app resolves the selected workspace asynchronously after load, and a
	// workspace change aborts the active run (Chat.tsx workspace epoch guard). On
	// a slow runner a send issued before that resolution was cancelled and no
	// approval appeared, so wait for the project to be selected first, as the S10
	// browser test does.
	projectReady := false
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if strings.Contains(bodyText(), "Overhaul") {
			projectReady = true
			break
		}
	}
	if !projectReady {
		t.Fatalf("workspace was not selected; page text:\n%s", bodyText())
	}

	step().MustElement("textarea").MustInput("run the probe tool")
	step().MustElementR("button", "전송|Send").MustClick()

	// The approval is linked to its step card by toolCallId and the card stays open.
	card := waitFor(`[data-pending-approval="true"]`)
	cardText := card.MustText()
	for _, want := range []string{"Bash", "Allow once", "Deny"} {
		if !strings.Contains(cardText, want) {
			t.Fatalf("pending approval card missing %q: %s", want, cardText)
		}
	}
	step().MustElementR("button", "승인 필요")

	// A pending decision is badged on the workspace while another page is shown.
	step().MustElement(`button[aria-label^="Settings"], button[aria-label^="설정"]`).MustClick()
	step().MustElement(`[data-approval-badge="chat"]`)
	step().MustElementR("button", "^Server Memory$").MustClick()
	step().MustElementR("h1", "^Memory$")
	step().MustElementR("button", "Run Dream Cycle")
	step().MustElement(`button[aria-label^="Workspace"], button[aria-label^="작업 공간"]`).MustClick()

	card = waitFor(`[data-pending-approval="true"]`)
	card.MustElementR("button", "^Deny$").MustClick()

	var body string
	for i := 0; i < 600; i++ {
		body = bodyText()
		if strings.Contains(body, "APPROVAL_FLOW_DONE") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(body, "APPROVAL_FLOW_DONE") {
		t.Fatalf("run did not continue after the denial: %s", body)
	}
	if has, _, _ := page.Has(`[data-pending-approval="true"]`); has {
		t.Fatalf("approval card still pending after the run finished: %s", body)
	}
	// A settled approval must not come back as a fresh alert once the tool result lands.
	alerts := page.MustEval(`() => Array.from(document.querySelectorAll('[role="alert"]')).map((el) => el.innerText).join('\n')`).Str()
	if strings.Contains(alerts, "Permission required") {
		t.Fatalf("settled approval re-rendered as an alert: %s", alerts)
	}
	if has, _, _ := page.Has(`[data-approval-badge="chat"]`); has {
		t.Fatal("approval badge remained after the decision")
	}
	// No verification ran, so the receipt must not look like a success.
	tone := step().MustElement(`[data-receipt-tone]`).MustAttribute("data-receipt-tone")
	if tone == nil || *tone == "success" {
		t.Fatalf("receipt tone for an unverified run = %v, body: %s", tone, body)
	}
}
