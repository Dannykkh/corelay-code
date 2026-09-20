package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Dannykkh/corelay-code/internal/agent"
	"github.com/Dannykkh/corelay-code/internal/buildinfo"
	"github.com/Dannykkh/corelay-code/internal/config"
)

const doctorRequestTimeout = 2 * time.Second

func runVersion(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "version: does not accept arguments; see 'corelaycode help'")
		return 2
	}
	fmt.Printf("Corelay Code CLI version: %s\n", safeBuildMetadata(buildinfo.Version))
	fmt.Printf("Build commit: %s\n", safeBuildMetadata(buildinfo.Commit))
	fmt.Printf("Go runtime: %s\n", runtime.Version())
	return 0
}

func runDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	serverURL := fs.String("url", "", "Corelay server URL to check (default: configured local server)")
	tokenFlag := fs.String("token", "", "Server access token (ambient credentials are used only for loopback endpoints)")
	workDir := fs.String("workdir", "", "Workspace to inspect for MCP configuration (default: current directory)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "doctor: invalid arguments; see 'corelaycode help'")
		return 2
	}

	workDirValue := strings.TrimSpace(*workDir)
	if workDirValue == "" {
		workDirValue, _ = os.Getwd()
	}

	cfg, configExists, configErr := config.LoadChecked()
	fmt.Fprintln(os.Stdout, "Corelay Code doctor")
	fmt.Fprintf(os.Stdout, "  CLI version: %s\n", safeBuildMetadata(buildinfo.Version))
	fmt.Fprintf(os.Stdout, "  Build commit: %s\n", safeBuildMetadata(buildinfo.Commit))
	fmt.Fprintf(os.Stdout, "  Go runtime: %s\n", runtime.Version())
	switch {
	case configErr != nil:
		fmt.Fprintln(os.Stdout, "  Configuration: invalid (details withheld)")
	case configExists:
		fmt.Fprintln(os.Stdout, "  Configuration: available")
	default:
		fmt.Fprintln(os.Stdout, "  Configuration: missing (defaults in use)")
	}

	printLocalCapabilities(os.Stdout, workDirValue)

	baseURL := strings.TrimSpace(*serverURL)
	if baseURL == "" {
		port := cfg.Port
		if port == 0 {
			port = 4000
		}
		baseURL = "http://127.0.0.1:" + strconv.Itoa(port)
	}
	baseURL, err := normalizeDoctorURL(baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doctor: -url must be an HTTP(S) server URL without credentials, path, query, or fragment")
		return 2
	}
	token := resolveDoctorAccessToken(*tokenFlag, baseURL, cfg, configErr)
	status, version, serverCommit := inspectDoctorServer(baseURL, token, newChatHTTPClient(doctorRequestTimeout))
	if version != "" {
		fmt.Fprintf(os.Stdout, "  Corelay server: %s (version %s, build commit %s)\n", status, version, serverCommit)
	} else {
		fmt.Fprintf(os.Stdout, "  Corelay server: %s (version unknown)\n", status)
	}
	if configErr != nil || status != "healthy" || version == "" {
		return 1
	}
	return 0
}

func printLocalCapabilities(out io.Writer, workDir string) {
	if agent.WebBrowserInstalled() {
		fmt.Fprintln(out, "  Browser (installed): available")
	} else {
		fmt.Fprintln(out, "  Browser (installed): unavailable")
	}
	if _, err := exec.LookPath("git"); err == nil {
		fmt.Fprintln(out, "  Git: available")
	} else {
		fmt.Fprintln(out, "  Git: unavailable")
	}
	if _, err := exec.LookPath("bash"); err == nil {
		fmt.Fprintln(out, "  Shell (Bash): available")
	} else {
		fmt.Fprintln(out, "  Shell (Bash): unavailable")
	}
	lspExecutable := agent.ConfiguredLSPExecutable()
	if _, err := exec.LookPath(lspExecutable); err == nil {
		fmt.Fprintf(out, "  LSP (Go/gopls): available (%s)\n", filepath.Base(lspExecutable))
	} else {
		fmt.Fprintf(out, "  LSP (Go/gopls): unavailable (%s); RepoMap fallback\n", filepath.Base(lspExecutable))
	}

	mcpJSON := agent.LoadMCPConfig(workDir)
	if mcpJSON == "" {
		fmt.Fprintln(out, "  MCP: no configuration found")
	} else if parsed, err := agent.ParseMCPConfig(mcpJSON); err != nil {
		fmt.Fprintln(out, "  MCP: configuration invalid (details withheld)")
	} else {
		fmt.Fprintf(out, "  MCP: %d configured; runtime state unknown\n", len(parsed.MCPServers))
	}

	runner, policy := agent.DefaultSandboxExecution(workDir)
	caps := runner.Capabilities()
	formatBool := func(value bool) string {
		if value {
			return "yes"
		}
		return "no"
	}
	isolation := "limited"
	if caps.HasIsolation() {
		isolation = "available"
	}
	fmt.Fprintf(out, "  Workspace execution: runner=%s enforcement=%s isolation=%s\n", runner.Name(), policy.Enforcement, isolation)
	fmt.Fprintf(out, "  Sandbox capabilities: filesystem=%s network=%s process=%s environment=%s\n",
		formatBool(caps.FilesystemIsolation), formatBool(caps.NetworkIsolation),
		formatBool(caps.ProcessIsolation), formatBool(caps.EnvironmentIsolation))
	fmt.Fprintln(out, "  Full execution: current OS account privileges; sandbox disabled; no privilege elevation")
}

func normalizeDoctorURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("invalid server URL")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", errors.New("invalid server URL scheme")
	}
	if port := parsed.Port(); port != "" {
		parsedPort, err := strconv.Atoi(port)
		if err != nil || parsedPort < 1 || parsedPort > 65535 {
			return "", errors.New("invalid server URL port")
		}
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func resolveDoctorAccessToken(flagToken, baseURL string, cfg config.Config, configErr error) string {
	if flagToken != "" {
		return flagToken
	}
	if !isLoopbackServerURL(baseURL) {
		return ""
	}
	if token := resolveAccessToken(""); token != "" {
		return token
	}
	if configErr == nil {
		return cfg.AccessToken
	}
	return ""
}

func inspectDoctorServer(baseURL, token string, client *http.Client) (status, version, commit string) {
	var health struct {
		Status string `json:"status"`
	}
	if err := doctorGETJSON(client, baseURL, "/health", "", &health); err != nil || health.Status != "ok" {
		return "unavailable", "", ""
	}

	var root struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := doctorGETJSON(client, baseURL, "/", token, &root); err != nil || !strings.EqualFold(root.Name, "corelaycode") {
		return "healthy; identity unavailable", "", ""
	}
	version = safeDoctorVersion(root.Version)
	if version == "" {
		return "healthy; version unavailable", "", ""
	}
	return "healthy", version, safeBuildMetadata(root.Commit)
}

func doctorGETJSON(client *http.Client, baseURL, path, token string, target any) error {
	request, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	if err != nil {
		return errors.New("request unavailable")
	}
	if token != "" {
		request.Header.Set("X-Access-Token", token)
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("server request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return errors.New("server response unavailable")
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(target); err != nil {
		return errors.New("server response invalid")
	}
	return nil
}

func safeDoctorVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune(".+-_", character) {
			continue
		}
		return ""
	}
	return value
}

func safeBuildMetadata(value string) string {
	if safe := safeDoctorVersion(value); safe != "" {
		return safe
	}
	return "unknown"
}
