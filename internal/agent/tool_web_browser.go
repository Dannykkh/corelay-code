package agent

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const maxBrowserHTMLBytes = 2 * 1024 * 1024

// A page owns its process/profile; at most two such pages run at once.
var webBrowserSlots = make(chan struct{}, 2)

func autoWebFetch(ctx context.Context, target string) (webFetchResult, error) {
	if _, err := browserTargetURL(target); err != nil {
		return webFetchResult{}, err
	}
	directCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	result, directErr := directWebFetch(directCtx, target)
	cancel()
	if directErr == nil && !result.NeedsBrowser {
		return result, nil
	}
	if ctx.Err() != nil {
		return webFetchResult{}, ctx.Err()
	}
	rendered, err := browserWebFetch(ctx, target)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("automatic browser fallback failed: %w", err)
	}
	return rendered, nil
}

func webPageNeedsBrowser(rawHTML, markdown string) bool {
	text := strings.TrimSpace(htmlToText(markdown))
	if len([]rune(text)) >= 300 {
		return false
	}
	for _, tag := range regexp.MustCompile(`(?is)<script\b([^>]*)>`).FindAllStringSubmatch(rawHTML, -1) {
		kind := strings.ToLower(htmlAttrValue(tag[1], "type"))
		if kind == "" || kind == "module" || strings.Contains(kind, "javascript") {
			return true
		}
	}
	return false
}

func browserTargetURL(target string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, fmt.Errorf("web URL must use HTTP(S) without embedded credentials")
	}
	return u, nil
}

func installedWebBrowser() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CORELAY_BROWSER_PATH")); configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("CORELAY_BROWSER_PATH does not identify an executable browser")
		}
		return path, nil
	}
	if path, ok := launcher.LookPath(); ok {
		return path, nil
	}
	return "", nil
}

// WebBrowserInstalled reports whether Chromium or Chrome is already available.
// It never downloads or starts a browser.
func WebBrowserInstalled() bool {
	path, err := installedWebBrowser()
	return err == nil && path != ""
}

func resolveWebBrowser(ctx context.Context) (string, error) {
	path, err := installedWebBrowser()
	if err != nil || path != "" {
		return path, err
	}
	if OfflineMode() {
		return "", fmt.Errorf("browser download is disabled in offline mode")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate browser cache")
	}
	download := launcher.NewBrowser()
	download.Context = ctx
	download.RootDir = filepath.Join(cache, "corelay", "browser")
	// Prefer the upstream Chromium snapshot host instead of third-party mirrors.
	download.Hosts = []launcher.Host{launcher.HostGoogle}
	path, err = download.Get()
	if err != nil {
		return "", fmt.Errorf("cannot prepare Chromium; install Chrome/Chromium or retry the download: %w", err)
	}
	return path, nil
}

func browserWebFetch(parent context.Context, target string) (webFetchResult, error) {
	u, err := browserTargetURL(target)
	if err != nil {
		return webFetchResult{}, err
	}
	if OfflineMode() {
		return webFetchResult{}, fmt.Errorf("browser fetch is disabled in offline mode")
	}
	select {
	case webBrowserSlots <- struct{}{}:
		defer func() { <-webBrowserSlots }()
	case <-parent.Done():
		return webFetchResult{}, parent.Err()
	}
	prepareCtx, prepareCancel := context.WithTimeout(parent, 2*time.Minute)
	bin, err := resolveWebBrowser(prepareCtx)
	prepareCancel()
	if err != nil {
		return webFetchResult{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	profile, err := os.MkdirTemp("", "corelay-web-browser-")
	if err != nil {
		return webFetchResult{}, fmt.Errorf("cannot prepare browser profile")
	}
	// The profile is an exclusively owned MkdirTemp result, never a user-supplied path.
	defer os.RemoveAll(profile)
	l := launcher.New().Context(ctx).Bin(bin).HeadlessNew(true).Leakless(true).
		NoSandbox(false).UserDataDir(profile).
		Set("remote-debugging-address", "127.0.0.1").
		Set("disable-extensions").Set("disable-sync").Set("disable-background-networking")
	endpoint, err := l.Launch()
	if err != nil {
		l.Kill()
		return webFetchResult{}, fmt.Errorf("cannot launch headless Chromium: %w", err)
	}
	defer l.Cleanup()
	defer l.Kill()
	browser := rod.New().ControlURL(endpoint).Context(ctx)
	if err := browser.Connect(); err != nil {
		return webFetchResult{}, fmt.Errorf("cannot connect to headless Chromium: %w", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = browser.Context(closeCtx).Close()
	}()
	if err := (proto.BrowserSetDownloadBehavior{Behavior: proto.BrowserSetDownloadBehaviorBehaviorDeny}).Call(browser); err != nil {
		return webFetchResult{}, fmt.Errorf("cannot disable browser downloads: %w", err)
	}
	page, err := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return webFetchResult{}, err
	}
	if err := (proto.NetworkEnable{}).Call(page); err != nil {
		return webFetchResult{}, err
	}
	if err := (proto.NetworkSetBlockedURLs{Urls: []string{"file://*", "ftp://*"}}).Call(page); err != nil {
		return webFetchResult{}, err
	}
	if err := page.Navigate(u.String()); err != nil {
		return webFetchResult{}, fmt.Errorf("browser navigation failed: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return webFetchResult{}, fmt.Errorf("browser page load failed: %w", err)
	}
	waitState, err := page.Eval(webBrowserWaitScript)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("browser page did not settle: %w", err)
	}
	var waitOutcome struct {
		Settled  bool `json:"settled"`
		TimedOut bool `json:"timed_out"`
	}
	if err := waitState.Value.Unmarshal(&waitOutcome); err != nil {
		return webFetchResult{}, fmt.Errorf("invalid browser settle result")
	}
	snapshot, err := page.Eval(webBrowserSnapshotScript)
	if err != nil {
		return webFetchResult{}, fmt.Errorf("cannot read rendered page: %w", err)
	}
	var rendered struct {
		HTML      string `json:"html"`
		URL       string `json:"url"`
		Status    int    `json:"status"`
		Oversized bool   `json:"oversized"`
	}
	if err := snapshot.Value.Unmarshal(&rendered); err != nil {
		return webFetchResult{}, fmt.Errorf("invalid browser snapshot")
	}
	if rendered.Oversized || len(rendered.HTML) > maxBrowserHTMLBytes {
		return webFetchResult{}, fmt.Errorf("rendered page exceeds 2 MiB or 20000 element limit")
	}
	if rendered.Status >= 400 {
		return webFetchResult{}, fmt.Errorf("browser target HTTP %d", rendered.Status)
	}
	finalURL, err := browserTargetURL(rendered.URL)
	if err != nil {
		return webFetchResult{}, err
	}
	content := webHTMLToMarkdown(rendered.HTML, finalURL)
	if strings.TrimSpace(content) == "" {
		return webFetchResult{}, fmt.Errorf("browser returned no readable content")
	}
	limitNotice := ""
	if waitOutcome.TimedOut {
		readable := strings.TrimSpace(htmlToText(content))
		if len([]rune(readable)) < 40 {
			return webFetchResult{}, fmt.Errorf("browser DOM kept changing and no meaningful bounded snapshot was available")
		}
		limitNotice = "DOM kept changing; returned a bounded snapshot at the stability deadline"
	}
	published, updated := extractHTMLDates(rendered.HTML)
	return webFetchResult{URL: u.String(), FinalURL: finalURL.String(), Title: extractHTMLTitle(rendered.HTML),
		Status: rendered.Status, ContentType: "text/html", Content: content,
		PublishedAt: published, UpdatedAt: updated, Links: extractLinks(rendered.HTML, finalURL), Source: "browser", LimitNotice: limitNotice}, nil
}

// Wait at least one second for a quiet DOM. A continuously mutating page gets
// one bounded snapshot at the deadline; the caller labels that limitation and
// rejects it only when the captured document has no meaningful text.
const webBrowserWaitScript = `() => new Promise((resolve) => {
 let quiet;
 const started = Date.now();
 const finish = (timedOut) => { clearTimeout(quiet); clearTimeout(deadline); observer.disconnect(); resolve({settled: !timedOut, timed_out: timedOut}); };
 const schedule = () => { clearTimeout(quiet); quiet = setTimeout(finish, Math.max(750, 1500 - (Date.now() - started))); };
 const observer = new MutationObserver(schedule);
 const deadline = setTimeout(() => finish(true), 8000);
 observer.observe(document.documentElement, {subtree:true, childList:true, characterData:true, attributes:true});
 quiet = setTimeout(() => finish(false), 1500);
})`

// Serialize open shadow roots and readable same-origin frames without exposing page JS as tool input.
const webBrowserSnapshotScript = `() => {
 let frames = 0, elements = 0;
 const copy = (root, depth) => {
   if (depth > 3) return document.createDocumentFragment();
   const clone = root.nodeType === 11 ? document.createDocumentFragment() : root.cloneNode(true);
   if (root.nodeType === 11) root.childNodes.forEach(child => clone.appendChild(child.cloneNode(true)));
   const sources = [root, ...root.querySelectorAll('*')];
   const targets = [clone, ...clone.querySelectorAll('*')];
   elements += sources.length;
   if (elements > 20000) throw new Error('element limit');
   sources.forEach((source, i) => {
     if (source.tagName === 'A' && source.href) targets[i].setAttribute('href', source.href);
     if (source.shadowRoot) targets[i].appendChild(copy(source.shadowRoot, depth + 1));
     if (source.tagName === 'IFRAME' && frames < 3 && depth < 2) {
       try { const doc = source.contentDocument;
         if (doc && doc.documentElement) { frames++; const section = document.createElement('section'); section.appendChild(copy(doc.documentElement, depth + 1)); targets[i].replaceWith(section); }
       } catch (_) {}
     }
   });
   return clone;
 };
 try {
   if (document.documentElement.outerHTML.length > 2097152) return {oversized:true};
   const html = copy(document.documentElement, 0).outerHTML;
   if (html.length > 2097152) return {oversized:true};
   return {html, url:location.href, status:performance.getEntriesByType('navigation')[0]?.responseStatus || 0};
 } catch (e) { if (e.message === 'element limit') return {oversized:true}; throw e; }
}`
