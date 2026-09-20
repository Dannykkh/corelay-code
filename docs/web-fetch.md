# Web page reading

`WebFetch` is a registered agent tool. When the model emits a call, the existing
permission, offline-mode, and dispatcher path executes it and returns content.
Providing a URL alone does not force a model tool call; ask the agent to read it.
When a tool call is emitted, the dispatcher applies the same project, session,
permission, offline, and cancellation boundaries as other tools. A direct URL
in a chat message remains model context until the model selects `WebFetch`.

## Default behavior

The default `auto` mode tries HTTP first. If HTTP fails, or a short HTML page
contains executable scripts and appears to need rendering, it launches local
headless Chrome/Chromium through Rod and reads the rendered document. Ordinary
static pages stay on the faster HTTP path. `WebResearch` uses the same automatic
fetch selection unless its Ollama provider is explicitly selected.

```json
{"url":"https://example.com/docs","prompt":"설치","max_chars":12000}
```

The heuristic is intentionally conservative: a long page with partly dynamic
content may not trigger automatic rendering. The model can request rendering
explicitly:

```json
{"url":"https://example.com/app","provider":"browser","prompt":"installation"}
```

`provider=direct` forces HTTP-only fetching. `provider=ollama` retains the existing
Ollama integration. The earlier `crawl4ai` server adapter has been replaced by the
local browser provider; no Python, Docker, browser server address, or API token
is required for local browser reading.

## Browser preparation and lifecycle

Corelay first finds an installed browser using Rod's executable discovery.
`CORELAY_BROWSER_PATH` optionally selects a specific local executable. An invalid
explicit path fails instead of silently choosing another executable.

If no browser is installed, Corelay uses Rod's Chromium downloader and caches
its package-pinned revision under the OS user cache directory, `corelay/browser`.
Downloads use the upstream Google Chromium snapshot host. First-use preparation
has a two-minute deadline; later requests reuse the cached browser. The executable
still requires a supported OS and browser system libraries. For minimal Linux
containers, provide an installed Chromium and the required system libraries.

Each rendered fetch owns a headless browser process and a temporary profile.
Existing personal browser profiles, cookies, extensions, and login sessions are
not reused. Rod's leakless launcher supervises the process; request completion,
failure, or cancellation closes/kills it and removes its profile. At most two
rendered fetches run concurrently. Browser downloads from visited pages are denied.
The Chromium sandbox stays enabled, including on Linux; a host unable to run it
fails explicitly rather than automatically using `--no-sandbox`.

There is no persistent browser server. Optional executable configuration is
operator-controlled and is never accepted as a model tool argument. Only HTTP(S)
page URLs without embedded credentials are accepted by auto/browser modes.
`CORELAY_OFFLINE` prevents tool execution and browser downloading.

## Extraction and limits

Both providers convert the DOM to Markdown, preferring `main`, then `article`,
then the body. Headings, lists, tables, links, and fenced code indentation are
preserved; navigation and hidden attributes are removed. This is a conservative
document converter, not an exact representation of all CSS layout.

The browser waits for page load and a quiet DOM, with a minimum 1.5-second
settling window and an eight-second stability deadline. If the deadline is
reached, it takes one bounded snapshot and returns it with a limitation notice
when the snapshot contains at least 40 runes of readable text. A continuously
changing page with no meaningful bounded text returns an explicit error.
Rendering has a 30-second deadline; the outer WebFetch call allows up to three
minutes including first-use preparation. HTTP-first attempts in auto mode have a
ten-second deadline. Late content, virtualized lists, scroll-triggered content,
logins, and CAPTCHAs are not automatically handled.

Open shadow roots and up to three accessible same-origin iframe documents
(depth below two) are included in browser snapshots. Closed shadow roots and
cross-origin frame contents are not included by this snapshot implementation.
Rendered HTML is capped at 2 MiB and 20,000 elements. These are extraction bounds,
not a browser process memory or total network-byte quota. HTTP retains its
existing 2 MiB per-response and bounded iframe traversal limits.

HTTP iframe collection checks the selecting document's host, scheme, and effective
port both for the initial frame URL and before every redirect request. Embedded
credentials are rejected. The existing Naver subdomain exception requires the
same scheme and effective port. A blocked frame is omitted; the parent document
can still be returned. Redirect chains remain limited to ten requests.
Explicit top-level URLs retain normal redirect behavior. This frame extraction
boundary is not a general private-network firewall: top-level local URLs remain
supported, and Chromium uses its own subresource networking and same-origin DOM
rules. DNS rebinding and browser-wide network isolation are outside this fix.

An optional `prompt` selects relevant Markdown blocks using BM25-style scoring
before the output is truncated. Two-character Korean query words are supported
without an extra LLM call or morphological analyzer. Fenced code stays in one
selection block. Without matches, the beginning of the document is returned.
`max_chars` defaults to 12,000 and is capped at 30,000 for content; metadata and
the truncation notice are additional. A single over-budget block is truncated.

## References and scope

[Rod v0.116.2](https://github.com/go-rod/rod/tree/v0.116.2) provides the Go browser
controller and launcher. Its README motivated the choice of browser discovery,
downloads, cancellation, and process supervision. Corelay uses its public APIs;
no Rod source is copied into the product. Builds require Go 1.26.8 or later.

[Crawl4AI revision 862f6bc](https://github.com/unclecode/crawl4ai/tree/862f6bccb9c063f49b9d42701baa0eea17a4993f)
remains a behavior reference for Markdown and relevance filtering. The Go
converter and selector are independent implementations. The previous external
Crawl4AI adapter and its server configuration are superseded by local rendering.
Site-wide BFS, arbitrary script arguments, and model-backed extraction remain
outside this change.

## Verification

`go test ./internal/agent -run '^TestBrowser' -count=1 -v` exercises actual local
Chrome when installed: direct versus rendered output, automatic fallback,
JavaScript-generated Korean text, tables, same-origin iframe and open-shadow-root
content, final URL after redirect, HTTP errors, cancellation, temporary-profile
cleanup, oversized DOM rejection, offline gating, static-page HTTP behavior, and
a meaningful bounded snapshot from a continuously mutating DOM. Browser-dependent
tests skip when no executable is installed and never initiate a browser download
in CI. Other extraction tests cover long-page tail selection.

On 2026-09-13, actual installed Chrome was used on Windows. Automatic browser
download on a machine with no installed browser was NOT RUN; the local executable
path, browser control, and cleanup were exercised. These tests do not measure an
external model's likelihood of choosing the WebFetch tool.

On 2026-09-20, the installed Chrome Rod suite was rerun after the LSP and
release-roadmap changes; it passed without contacting a provider or downloading
Chromium. Browser-less first use, CAPTCHA/login flows, and the model's decision
to emit the tool call remain outside local evidence.

On 2026-09-20, HTTP frame redirect regression tests verified that disallowed
hosts/ports receive zero requests, credential-bearing redirects are rejected,
and allowed multi-hop frames and explicit top-level redirects still work.
Scheme/default-port and Naver-subdomain cases are covered separately. The
project now builds with Go 1.26.8 and x/net v0.56.0; govulncheck v1.8.0 reports
no known vulnerabilities for the scanned code and module graph.
