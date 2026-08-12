package tray

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Dannykkh/corelay-code/internal/sandbox"
)

const (
	defaultHostActionTimeout = 10 * time.Second
	maxNotificationTitle     = 256
	maxNotificationMessage   = 4 * 1024
)

// TrayConfig holds tray app settings.
type TrayConfig struct {
	Port     int
	Model    string
	Provider string
}

// HostExecutionOptions binds one fixed tray action to a supervised process
// boundary. It deliberately has no raw command override.
type HostExecutionOptions struct {
	Context       context.Context
	Runner        sandbox.Runner
	Policy        sandbox.Policy
	Timeout       time.Duration
	ObserveReport func(sandbox.Report)
}

// ShowNotification sends a system notification. Failure is reported without
// logging the title or message, which may contain user or model data.
func ShowNotification(title, message string) {
	if err := ShowNotificationWithOptions(title, message, defaultHostExecution()); err != nil {
		log.Printf("[Tray] Notification failed: %s", safeHostActionError(err))
	}
}

// ShowNotificationWithOptions sends a notification using fixed argv or a
// fixed script. On Windows the payload crosses only as environment values; on
// macOS it crosses as osascript argv. It is never interpolated into code.
func ShowNotificationWithOptions(title, message string, options HostExecutionOptions) error {
	if err := validateNotificationText(title, maxNotificationTitle); err != nil {
		return fmt.Errorf("invalid notification title")
	}
	if err := validateNotificationText(message, maxNotificationMessage); err != nil {
		return fmt.Errorf("invalid notification message")
	}
	command, err := notificationCommand(runtime.GOOS, title, message)
	if err != nil {
		return err
	}
	return executeHostAction(options, command)
}

// OpenBrowser opens the default browser to Corelay Code's loopback URL.
func OpenBrowser(port int) {
	if err := OpenBrowserWithOptions(port, defaultHostExecution()); err != nil {
		log.Printf("[Tray] Browser launch failed: %s", safeHostActionError(err))
	}
}

// OpenBrowserWithOptions validates the only variable URL component before
// invoking a fixed platform launcher.
func OpenBrowserWithOptions(port int, options HostExecutionOptions) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid browser port")
	}
	command, err := browserCommand(runtime.GOOS, port)
	if err != nil {
		return err
	}
	return executeHostAction(options, command)
}

func notificationCommand(platform, title, message string) (sandbox.CommandSpec, error) {
	command := sandbox.CommandSpec{Timeout: defaultHostActionTimeout}
	switch platform {
	case "windows":
		command.Path = "powershell"
		command.Args = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", windowsNotificationScript}
		command.Environment = sandbox.EnvironmentSpec{
			Inherit: hostActionInheritedEnvironment(platform),
			Set: map[string]string{
				"CORELAY_NOTIFICATION_TITLE":   title,
				"CORELAY_NOTIFICATION_MESSAGE": message,
			},
		}
	case "darwin":
		command.Path = "osascript"
		command.Args = []string{"-e", macNotificationScript, "--", title, message}
		command.Environment = sandbox.EnvironmentSpec{Inherit: hostActionInheritedEnvironment(platform)}
	case "linux":
		command.Path = "notify-send"
		command.Args = []string{"--app-name=Corelay Code", "--", title, message}
		command.Environment = sandbox.EnvironmentSpec{Inherit: hostActionInheritedEnvironment(platform)}
	default:
		return sandbox.CommandSpec{}, fmt.Errorf("unsupported notification platform")
	}
	return command, nil
}

func browserCommand(platform string, port int) (sandbox.CommandSpec, error) {
	url := fmt.Sprintf("http://localhost:%d/app", port)
	command := sandbox.CommandSpec{
		Timeout:     defaultHostActionTimeout,
		Environment: sandbox.EnvironmentSpec{Inherit: hostActionInheritedEnvironment(platform)},
	}
	switch platform {
	case "windows":
		command.Path = "rundll32"
		command.Args = []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		command.Path = "open"
		command.Args = []string{url}
	case "linux":
		command.Path = "xdg-open"
		command.Args = []string{url}
	default:
		return sandbox.CommandSpec{}, fmt.Errorf("unsupported browser platform")
	}
	return command, nil
}

func executeHostAction(options HostExecutionOptions, command sandbox.CommandSpec) error {
	if options.Context == nil {
		options.Context = context.Background()
	}
	if options.Runner == nil || options.Policy.Enforcement == "" {
		return fmt.Errorf("host action sandbox is not configured")
	}
	if options.Timeout <= 0 || options.Timeout > 30*time.Second {
		return fmt.Errorf("host action timeout is invalid")
	}
	command.Timeout = options.Timeout
	result, report := options.Runner.Run(options.Context, options.Policy, command)
	if options.ObserveReport != nil {
		options.ObserveReport(report)
	}
	if report.Failure != sandbox.FailureNone || !result.Started {
		return hostActionFailure{code: report.Failure}
	}
	if result.ExitCode != 0 {
		return hostActionFailure{code: sandbox.FailureExecutionFailed}
	}
	return nil
}

func defaultHostExecution() HostExecutionOptions {
	runner := sandbox.NewAutoRunner()
	policy := sandbox.Policy{
		Enforcement: sandbox.EnforcementPreferred,
		Required: sandbox.Capabilities{
			ProcessTreeKill:      true,
			EnvironmentFiltering: true,
			Timeouts:             true,
		},
	}
	if runner.Capabilities().FilesystemIsolation {
		if workDir, err := os.Getwd(); err == nil {
			if canonical, evalErr := filepath.EvalSymlinks(filepath.Clean(workDir)); evalErr == nil {
				policy.Workspace = canonical
				policy.WorkspaceAccess = sandbox.WorkspaceReadOnly
			}
		}
	}
	return HostExecutionOptions{
		Context: context.Background(),
		Runner:  runner,
		Policy:  policy,
		Timeout: defaultHostActionTimeout,
	}
}

type hostActionFailure struct{ code sandbox.FailureCode }

func (e hostActionFailure) Error() string {
	if e.code == sandbox.FailureNone {
		return "host_action_failed"
	}
	return string(e.code)
}

func safeHostActionError(err error) string {
	if failure, ok := err.(hostActionFailure); ok {
		return failure.Error()
	}
	return "host_action_rejected"
}

func validateNotificationText(value string, maxBytes int) error {
	if strings.IndexByte(value, 0) >= 0 || len(value) > maxBytes || !utf8.ValidString(value) {
		return fmt.Errorf("invalid notification text")
	}
	return nil
}

func hostActionInheritedEnvironment(platform string) []string {
	switch platform {
	case "windows":
		return []string{"PATH", "PATHEXT", "SystemRoot", "WINDIR", "TEMP", "TMP"}
	case "darwin":
		return []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TZ"}
	default:
		return []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TZ", "DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"}
	}
}

const windowsNotificationScript = `$title = $env:CORELAY_NOTIFICATION_TITLE
if ($null -eq $title) { $title = $env:ANICLEW_NOTIFICATION_TITLE }
$message = $env:CORELAY_NOTIFICATION_MESSAGE
if ($null -eq $message) { $message = $env:ANICLEW_NOTIFICATION_MESSAGE }
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType=WindowsRuntime] > $null
$template = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)
$text = $template.GetElementsByTagName("text")
$text.Item(0).AppendChild($template.CreateTextNode($title)) > $null
$text.Item(1).AppendChild($template.CreateTextNode($message)) > $null
$toast = [Windows.UI.Notifications.ToastNotification]::new($template)
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier("Corelay Code").Show($toast)`

const macNotificationScript = `on run argv
display notification (item 2 of argv) with title (item 1 of argv)
end run`
