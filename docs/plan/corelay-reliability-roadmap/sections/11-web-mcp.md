# S11 웹수집·MCP

상태: PASS
연결: F19,F27,F28
선행: S02,S03,S07

## 수정 시작점과 소유 범위

internal/agent/tool_web_browser.go,tool_web.go,mcp_runtime.go,mcp_client.go; internal/acpbridge; docs/web-fetch.md

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] 기존 미커밋 Rod 구현을 먼저 diff/테스트 확인한다. 자동 render 판단은 긴 JS shell과 충분한 static 본문 fixture를 추가해 보완한다.

  - 초기 확인: `internal/agent/tool_web_browser.go`는 Rod v0.116.2로 설치 브라우저 발견/고정된 Chromium download, 임시 profile, download deny, file/ftp 차단, 2-slot 제한, 30초 render/8초 DOM quiet/2 MiB·20,000 element bounded snapshot, open shadow와 제한된 same-origin iframe 추출, context cancel cleanup을 이미 production 경로에 연결하고 있었다. `tool_web_browser_test.go`는 rendered JS/Korean/table/iframe/open-shadow, redirect/404, cancel, offline, oversized DOM, static HTTP fallback을 실제 fixture로 검사했다.
  - 보완: `TestBrowserAutoKeepsLongJavaScriptShellWhenStaticBodyIsSufficient`를 추가해 긴 hydration shell이 있어도 충분한 server-rendered 본문은 browser fallback을 선택하지 않고 direct HTTP 결과를 유지하는 production `autoWebFetch` 경로를 고정했다. `CORELAY_BROWSER_PATH`가 잘못된 상태에서도 browser를 호출하지 않는 fixture다.
  - 검증: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run '^TestBrowser' -count=1 -v` — PASS, exit 0 (Chrome 152.0.7977.77 headless, 27.192s). 추가 focused heuristic/installed-path tests — PASS, exit 0. Rod profile/slot cleanup, cancel, redirect/HTTP error, offline, oversized DOM, direct/browser/auto selection을 모두 실제 경로로 확인했다.
  - NOT RUN: browser 없는 환경의 실제 Chromium 다운로드만 외부 네트워크 의존으로 실행하지 않았다. DOM 변동과 remote MCP는 후속 B/D에서 검증했다.

- B. [PASS] DOM이 계속 변해도 bounded 시점에 의미 있는 snapshot이 있으면 결과와 제한을 반환한다. 의미 있는 텍스트가 없는 변동 DOM은 오류로 닫고, 시간초과/취소/download/profile/slot cleanup을 검증했다.

  - 구현: Rod 안정화 대기는 1.5초 quiet window와 8초 deadline을 사용한다. deadline에 도달하면 DOM snapshot을 한 번 읽고, 읽을 수 있는 본문이 40 rune 이상이면 `LimitNotice`를 붙여 반환한다. 본문이 없으면 명시 오류를 반환하며, 기존 2 MiB/20,000 element와 HTTP status/URL bounds는 그대로 적용된다. `formatFetchResult`도 제한을 `Limit:` 메타데이터로 표시한다.
  - 검증: `TestBrowserFetchChangingDOMReturnsBoundedMeaningfulSnapshot`가 25ms마다 attribute를 바꾸는 실제 headless Chrome 페이지에서 bounded Markdown과 제한 메타데이터를 확인했다. 같은 `^TestBrowser` 실행은 PASS, exit 0 (Chrome 152.0.7977.77, 33.968s)였고 cancel, redirect/HTTP error, offline, oversized DOM, direct/browser/auto 선택 및 profile/slot cleanup도 함께 통과했다.
  - 로컬 대체: `CORELAY_OFFLINE`과 잘못된 browser path에서 다운로드를 시작하지 않고 명시 오류로 닫는 first-use gate를 기존 offline fixture에서 확인했다. 실제 Chromium package 다운로드는 외부 네트워크 의존으로 실행하지 않았다.

- C. [PASS] `MCPConfigPaths` 소비와 merge precedence를 구현하고 각 workspace/run의 server/env/tool registry를 분리한다.

  - 구현: 사용자 settings < 전역 설정에 지정한 `mcpConfigPaths`(목록 순서) < workspace `.claude/settings.json` < `mcp.json` < `.mcp.json` 순으로 MCP server를 병합한다. 동일 이름은 더 구체적인 뒤쪽 선언이 대체하고 다른 server는 유지한다. supplemental path는 workspace 기준 상대 경로 또는 절대 경로이며 파일 크기 256 KiB, path 64개, 명시 경로 누락/손상은 fail-closed다. RunLoop는 검증된 config의 paths를 `WorkspaceMCPServerSpecsWithPaths`로 전달하고, legacy connect/list/manager 경로도 같은 loader를 사용한다.
  - 격리: 병합 결과는 실행 전에 복제된 run-owned `MCPServerSpec`으로 변환되고 기존 immutable runtime catalog에만 연결된다. process-global client registry는 workspace/run tool catalog에 참여하지 않는다.
  - 검증: `TestLoadMCPConfigWithPathsMergesBySpecificityAndKeepsWorkspacesIsolated`, `TestWorkspaceMCPServerSpecsWithPathsConsumesSupplementalConfig`, `TestRunLoopConsumesConfiguredMCPConfigPaths`가 precedence, 서로 다른 workspace의 server 비혼합, env 보존, 실제 RunLoop 첫 catalog 광고를 확인했다. `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run 'MCP' -count=1` — PASS, exit 0 (1.486s).

- D. [PASS] 원격 MCP streamable HTTP를 공식 2025-06-18 transport 계약에 맞춰 명시 transport/auth config, cancel, session-loss reconnect, SSE/JSON response, schema refresh를 stdio와 공통 dispatcher에 연결했다. ACP는 HTTP capability를 광고하고 legacy SSE는 unsupported로 fail-closed한다.

  - 구현: `MCPServerSpec.Type=http`은 HTTP(S) endpoint와 bounded custom headers만 받는다. `MCP-Protocol-Version: 2025-06-18`, `Accept: application/json, text/event-stream`, optional `Mcp-Session-Id`를 사용하고 redirect, reserved header, embedded credential, 401/403을 거부한다. POST context cancellation은 네트워크 요청을 중단하며 404/410 session loss에서만 새 initialize→tools/list 후 원래 call을 한 번 재시도해 tool side effect 중복을 피한다. 응답 안의 `notifications/tools/list_changed`는 다음 tool call 전에 schema를 재조회한다.
  - 회귀: `TestMCPRemoteHTTPAuthenticatesRefreshesSchemaAndReconnects`, `TestMCPRemoteHTTPReconnectsAfterSessionLoss`, `TestMCPRemoteHTTPCancellationAbortsRequest`, `TestMCPRuntimeOwnsRemoteHTTPWithoutFilesystemRunner`가 로컬 httptest fixture에서 auth, JSON/SSE 응답, 취소, schema refresh, session reconnect, remote workspace 격리를 확인했다. ACP descriptor/validation도 HTTP 지원과 legacy SSE 거절을 확인한다.
  - 미실행: 외부 remote service, OAuth discovery/token refresh, browser 없는 환경의 실제 Chromium 다운로드는 네트워크/자격 증명 의존으로 실행하지 않았다. 정적 HTTP auth와 로컬 transport lifecycle만 검증했다.

## 완료 조건

Q13/Q14. fake stdio/로컬 remote fixture 통과. browser 없는 fresh install은 offline/fail-closed first-use gate를 별도 검증하며, 실제 Chromium package download는 외부 네트워크 의존으로 실행하지 않는다. 서로 다른 프로젝트 MCP env/credential가 섞이지 않는다.

## 회귀·제약

외부 Crawl4AI 서버 의존을 다시 강제하지 않는다. cross-origin iframe/closed shadow/CAPTCHA 처리 성공을 과장하지 않는다.

## 검증 기록

A~D PASS. 외부 서비스/OAuth와 browser download는 로컬 대체 검증만 수행했다. 브라우저 구현과 자동 render heuristic, 변동 DOM bounded snapshot, MCP config registry 격리, streamable HTTP 공통 dispatcher 회귀가 통과했다.

### 런타임 후속 수정 — 2026-09-20

A09: 재연결 중 도구 목록 조회 실패의 재귀 lock과 대기 취소 누락을 수정했다. 수정 전 실패 재현, 수정 후 MCP 반복 및 Linux race 검증은 [감리 보고서](../verify-report.md)의 추가 런타임 기록을 따른다.
