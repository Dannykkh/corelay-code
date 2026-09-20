# 검증과 완료 판정

## 최신 Argos 판정 — 2026-09-20

전체 CONDITIONAL / S13.A 로컬 PASS. snapshot-only 판정을 실제 compaction·durable project/session switch·Write 후 cancel/reconcile/resume 검증으로 교체했다. 정상·오답·재실행 거절을 각 3회 이상 실행했다. bootstrap은 단기 일회성 교환, ACP workflow 세션은 명시 거절로 보완했다. 압축 중 최신 요청 누락도 수정했다. 상세 실행 범위와 미검증 항목은 [verify-report.md](verify-report.md)를 따른다.

최종 통합: `go test ./... -count=1`, `go build ./...`, `go vet ./...` exit 0. 후속 false-done 지표 보완 후 profiler 전체 재검증도 exit 0. Web build/lint, 새 embedded bundle의 bootstrap/S10 실제 Chromium 검사, ACP 전체 회귀 PASS. coding assertion의 실제 Linux bubblewrap 격리 실패/성공 경로 PASS. 선택적 도구 미설치 skip과 hosted/live 미실행은 전체 성공으로 환산하지 않는다.

## 계획 작성 시점 — 역사적 기록 (2026-09-13)

제품 수정/런타임 검증: NOT RUN (이번 요청은 구현 계획).
이전 웹수집 작업의 통과 기록은 이번 전체 개선 검증으로 간주하지 않는다.
문서의 링크/섹션 의존성/결함 매핑은 작성 후 별도 검사한다.

현재 단계별 구현·검증 결과는 아래 S01~S13 기록을 정본으로 사용한다.

### 계획 문서 검사 결과 — 2026-09-13

- PowerShell로 Markdown 22개 / 구현 섹션 13개 / 내부 상대 링크 존재 여부 검사: PASS (exit 0).
- F01~F31이 추적표와 섹션에 모두 연결되는지 검사: PASS, 누락 0.
- git diff --check: PASS (exit 0). 새 문서는 위 별도 검사에 포함했다.
- 섹션 순서는 plan.md 선행 표와 대조했다. 제품 테스트는 구현하지 않았으므로 NOT RUN이다.

## 단계별 검사

1. 결함 fixture: 실제 handler/dispatcher/store/provider adapter를 통해 실패를 재현.
2. 수정 후 해당 test와 패키지 suite.
3. 경계 변경 시 기존 durable hard-crash, permission, sandbox, tool-result ownership tests.
4. 프론트 변경 시 lint/build 및 실제 렌더/상호작용 테스트.
5. G1/G2/G3 시점에서 영향 범위 통합 검사.

Go 기본 명령 (제품 루트):
go test ./internal/config ./internal/server ./internal/agent ./internal/workstream
go test ./...
go vet ./...
go build ./...
레이스 검사는 지원 toolchain/CGO 환경에서 go test -race를 실행; 불가 시 이유와 Linux CI 실행을 남긴다.

Web 루트:
npm ci (lockfile과 설치 상태에 따라 필요한 때)
npm run lint
npm run build
S10/S13에서 추가한 E2E script 명령을 실제 package.json 이름으로 기록한다.
Windows bash fixture가 WSL bash로 잘못 잡히면 테스트 프로세스 PATH에 Git for Windows bin을 우선한다.
전역 PATH는 수정하지 않는다. live Chromium, Node, Git, Go 버전을 결과에 남긴다.

## 통합 시나리오

| QA | 입력/행동 | 통과 기준 |
|---|---|---|
| Q01 | fake-token 설정, 저장 중단/손상/동시 두 프로세스 갱신 | 이전 설정 보존 또는 명시 오류, 인증 우회/부분 성공 없음 |
| Q02 | 허용/불허 Origin, 무토큰 CLI, 정상 CLI, bootstrap 재사용 | 정해진 인증 계약 준수; 로컬 브라우저 테스트 별도 |
| Q03 | 세 권한 × Write/Bash/MCP/plugin/hook/team/daemon | 모든 진입점에서 같은 실효 권한; read-only 변형 변경 차단 |
| Q04 | A/B 프로젝트 두 탭과 CLI 동시 실행 | 대화·모델·권한·작업·파일·approval이 교차하지 않음 |
| Q05 | staged B 상태에서 A만 커밋, 실패/취소/partial staging | 커밋 A만 포함; B와 기존 index/worktree 보존 |
| Q06 | agent 편집 후 사용자 재편집, Undo와 재시작 | 충돌 감지; 사용자 변경 보존; 안전한 파일만 명시 복원 |
| Q07 | Workstream 2개, 단계 승인/실행 도중 강제 종료 | revision/단계/증거 복구; 미완료를 완료로 처리하지 않음 |
| Q08 | 장문 중간 지시 수정·5회 이상 읽기 필요 | 최신 결정과 필요한 추가 탐색 보존; 예산 무한화 없음 |
| Q09 | skill none/source/custom/동명이름/긴 본문/상대참조 | 설정과 실행 일치; 정확한 provenance; 정책 상향 불가 |
| Q10 | one-shot session→TUI resume→Web open/fork | transcript/revision/작업/모델 소속 일치 |
| Q11 | 실제 PNG를 UI와 ImageRead에서 전달 | fake provider가 실제 bytes/image block 수신; 미지원 모델 명시 오류 |
| Q12 | 같은 basename의 서로 다른 디렉터리 파일 열기 | 모델 호출 0회, 정확한 경로/내용; 누적 diff 복원 일치 |
| Q13 | JS 본문/긴 shell/계속 변하는 DOM/취소/브라우저 없음 | bounded 결과, 누수 없음, fallback 이유 표시 |
| Q14 | stdio/remote MCP 끊김·인증 실패·재연결 | 중복 실행/권한 누출 없음; 도구 목록 재조정 |
| Q15 | Go 정의/참조/진단, LSP 없음/죽음 | 경로 정확, 제한시간, RepoMap fallback |
| Q16 | 깨끗한 Windows/Linux 환경 설치→시작→종료→재시작 | credential/port/child 수명주기/버전 일관; 기존 외부 서버 보존 |

### Q11 — 2026-09-14

- PASS: Go 1.26.1/Windows amd64에서 `go test ./internal/agent ./internal/protocol ./internal/providers ./internal/server -count=1` (exit 0). 이 중 fake provider transport tests가 supported OpenAI/Anthropic request shape 및 ImageRead image block을 검사하고, server tests가 잘못된 MIME·한도 초과·다른 세션 blob·image resume·unsupported/unknown capability 오류를 검사한다.
- PASS: Chrome 152.0.7977.77 headless + Rod로 내장 UI에서 임시 2×2 PNG를 첨부하고 메시지를 전송했다. local fake provider에서 MIME `image/png`, 77 bytes, image block 1개, SHA-256 `302d727e5d93aba85c43facbef4446a23d9588db8cc47ef52bee3dfa332f092d`를 확인했다. fixture와 provider 수신 bytes의 digest가 일치했다. 임시 harness와 fixture는 검증 후 제거했다.
- PASS: `go vet ./internal/agent ./internal/protocol ./internal/providers ./internal/server`, `git diff --check` (각 exit 0).
- NOT RUN: live provider API와 브라우저 clipboard paste 상호작용. 브라우저 업로드, ImageRead, 명시적 미지원 모델 오류는 확인했으며 live service 의존은 두지 않았다.

### S10.A — 2026-09-20

- PASS: Web source and embedded bundle show one compact context strip for project/workstream/session/stage/effective permission. `Session.executionPolicy` is restored on load and replaced only by a validated server SSE policy; missing or invalid policy remains unknown. Existing workflow binding and approval controls remain connected.
- PASS: TUI SESSION rail displays persisted session/workstream/plan/stage metadata and validated permission mode/revision. Focused rail tests and `go test ./cmd/proxy -count=1` passed (exit 0, 28.121s).
- PASS: `npm run lint`, `npm run build`, `git diff --check` (each exit 0). Build warning is the existing single >500 kB JavaScript chunk.
- PASS: Rod browser E2E covers refresh and context strip; interactive terminal smoke and independent flow judgment remain for G2.

### S10.B — 2026-09-20

- PASS: The production `/api/file` handler now resolves the requested path beneath the registered workspace, returns a canonical slash-separated relative path, and handles directories with bounded child entries. Text reads use a 100,000-byte hard cap; binary, image, unsupported, too-large, and missing-file results have explicit response types/statuses. No agent/model call is involved.
- PASS: `TestReadFileUsesBoundedDirectResponseAndCanonicalPaths` covers two identical basenames in different directories, lexical normalization, directory entries, binary detection, bounded large-file preview, missing 404, and provider call count zero. `TestFileHandlersUseRegisteredClientWorkspace` continues to cover selected workspace and traversal isolation.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/server -run '^(TestReadFileUsesBoundedDirectResponseAndCanonicalPaths|TestFileHandlersUseRegisteredClientWorkspace)$' -count=1 -v` (exit 0), `go test ./internal/server -count=1` (exit 0, 20.360s), `go vet ./internal/server` (exit 0), `npm run lint` (exit 0), `npm run build` (exit 0), and `git diff --check` (exit 0).
- PASS: `web/src/lib/files.ts` is the shared caller for `App.tsx` and the previously model-backed `Explorer.tsx`; non-text responses are displayed with editing disabled. Embedded assets were rebuilt after the Web change.
- PASS: Authenticated Chromium E2E covers directory tree plus binary and large-file viewer states; independent flow judgment remains for G2.

### S10.C — 2026-09-20

- PASS: Agent RunLoop emits bounded structured `undo_preview`/`undo_result` events for the existing checkpoint list/select restore path. The Web Chat keeps up to 200 turn-tagged diff records, derives unique changed files, exposes checkpoint candidates, and only offers restore for `restorable` entries; conflicts/unavailable entries remain blocked. TUI `/diff` now summarizes all diff records and unique changed files for the run. CLI output normalization allowlists and sanitizes the structured undo events.
- PASS: Existing conflict-aware checkpoint test still restores a selected safe file while preserving the unselected user-conflicted file and now asserts structured preview/result events. TUI accumulation and CLI normalization have focused regression tests.
- PASS: `go test ./internal/agent -run '^TestUndoCheckpointSelectedRestorePreservesUnselectedConflict$' -count=1 -v`, focused `go test ./cmd/proxy` diff/normalizer tests, full `go test ./cmd/proxy -count=1` (exit 0, 29.086s), server affected regression (exit 0, 6.105s), `go vet ./internal/agent ./cmd/proxy ./internal/server`, Web lint/build, and `git diff --check` passed.
- HISTORICAL / RESOLVED IN S13: At the time of this S10.C run, full `go test ./internal/agent -count=1` also exercised a Chronos full-mode fixture that overflowed its 16K test context before dispatch. S13 raised that policy-forwarding fixture to 32K and the full suite now passes. Browser interaction for accumulated diffs and restore passed in S10.E; independent flow judgment remains for G2.

### S10.D — 2026-09-20

- PASS: `web/src/lib/sse.ts` now owns incremental SSE framing. It handles CRLF/LF/CR boundaries, comments, `event` and repeated `data` fields, decoder final flush, terminal frames without a trailing blank line, and abort cancellation. Chat, Team, Costs, and the compatibility `streamChat` API all use the shared stream helper.
- PASS: Chat captures the active project and epoch for each send. Project changes abort the active controller and invalidate runtime/workstream requests; stale stream frames cannot update messages, agent state, or the compatibility durable-session save. Durable session saves also check the same epoch before and after the network write. Workstream refreshes discard responses for an older project.
- PASS: `npm run lint`, `npm run build` (Vite 8.0.3; existing single >500 kB chunk warning), and `git diff --check` passed. A Node 22 `ReadableStream` check split UTF-8 bytes and SSE records across arbitrary chunks and verified multiline/terminal parsing (exit 0).
- PASS: Authenticated Chromium E2E covers a network-delayed project switch, stale stream cancellation, and mobile/keyboard interaction; independent flow judgment remains for G2.

### S10.E — 2026-09-20

- PASS: `internal/server/ui_browser_test.go` starts the real production server and embedded Web bundle, obtains the loopback bootstrap cookie, and drives Chrome 152.0.7977.77 headless through Rod. It verifies project/workspace refresh persistence, bounded text/binary/too-large file viewer states, context strip, mobile viewport, keyboard input, and theme toggle.
- PASS: The same browser run uses a deterministic fake SSE response only for `/api/agent`. A delayed first response is released after switching projects and its stale marker never reaches the new screen. A subsequent diff plus structured undo preview renders the Changes/Restore controls and the restore action can be invoked. No live provider or paid credential is used.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/server -run '^TestS10BrowserUIProjectFilesAndStaleStreams$' -count=1 -v` (exit 0, 5.99s), full `go test ./internal/server -count=1` (exit 0, 37.075s), `go vet ./internal/server`, Web lint/build, embedded bundle sync, and `git diff --check` passed.
- NOT RUN: External provider, OS clipboard paste, and automatic Chromium download when no browser is installed. PNG upload/ImageRead browser acceptance is recorded under S09/Q11.

### S11.B — 2026-09-20

- PASS: The Rod settle script keeps a 1.5-second quiet window and an eight-second bounded deadline. At the deadline, `browserWebFetch` captures one snapshot; meaningful text (at least 40 runes) is returned with `LimitNotice`, while an empty/meaningless changing DOM fails explicitly. `formatFetchResult` exposes the notice without including page script input.
- PASS: `TestBrowserFetchChangingDOMReturnsBoundedMeaningfulSnapshot` uses real Chrome 152.0.7977.77 and a page that mutates an attribute every 25ms. It verifies readable Markdown and the `Limit: DOM kept changing` metadata. `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run '^TestBrowser' -count=1` passed (exit 0, 33.968s); the suite also covers cancellation, redirect/HTTP error, offline gating, oversized DOM rejection, profile/slot cleanup, and direct/browser/auto selection.
- NOT RUN: Browser-less first-use Chromium download and remote MCP transport remain under S11.D.

### S11.C — 2026-09-20

- PASS: `LoadMCPConfigWithPaths` now merges user settings, configured `MCPConfigPaths` in listed order, workspace settings, `mcp.json`, and `.mcp.json` with explicit specificity precedence. Duplicate server names are replaced only by the later source; unrelated definitions and environment maps remain available. Supplemental files are bounded to 256 KiB and 64 paths, and explicit missing/malformed paths fail closed.
- PASS: RunLoop reads the validated application config and passes its supplemental paths into the run-owned MCP spec resolver. Legacy list/start/connect and enhanced manager paths use the same merged loader. Specs are cloned before runtime creation; no process-global registry is used to populate a run catalog.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run 'MCP' -count=1` (exit 0, 1.486s). The focused fixtures verify precedence, environment preservation, two workspace registries without leakage, and real RunLoop advertisement from a configured supplemental file.
- NOT RUN: Remote MCP transport/auth/reconnect/schema refresh is recorded under S11.D.

### S11.D — 2026-09-20

- PASS: `MCPServerSpec.Type=http` now shares the run-owned MCP catalog and dispatcher with stdio. The streamable HTTP client sends the fixed MCP `2025-06-18` protocol header, explicit configured auth/custom headers, JSON or SSE-framed responses, and optional `Mcp-Session-Id`. Endpoint credentials, redirects, reserved headers, invalid status, and oversized responses fail closed.
- PASS: Request contexts cancel the HTTP request. A 404/410 session loss performs one serialized initialize→tools/list refresh and retries the original call; generic network/5xx failures are not automatically retried because the server may already have executed a side effect. `notifications/tools/list_changed` marks the catalog stale and the next tool call refreshes its schema.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run '^TestMCPRemote|^TestRunLoopConsumesConfiguredMCPConfigPaths$' -count=1` (exit 0), `go test ./internal/acpbridge -run '^TestACP(AdvertisesAndConvertsStreamableHTTPMCP|RejectsUnsupportedAndDuplicateMCPDeclarations)$' -count=1` (exit 0), `go test ./internal/acp -run '^TestStableV1StdioMCPAcceptsPATHCommandWithoutAdvertisingRemoteTransports$' -count=1` (exit 0), `go vet ./internal/agent ./internal/acpbridge ./internal/acp` (exit 0), and `git diff --check` (exit 0).
- HISTORICAL / RESOLVED IN S13: The full affected package command at the time completed with the same non-MCP Chronos fixture context overflow in `internal/agent`; `internal/acpbridge` and `internal/acp` passed. S13 raised the policy-forwarding fixture harness to 32K, and the full suite now passes without changing the MCP path.
- NOT RUN: External remote service, OAuth issuer discovery/token refresh, legacy SSE transport, and actual browser download with no installed browser. Local HTTP auth, JSON/SSE response framing, cancellation, reconnect, schema refresh, and offline/fail-closed browser gating were used instead; no paid credential or live service was contacted.

### S11.A — 2026-09-20

- PASS: Existing Rod v0.116.2 browser fetch path was traced from `WebFetch` through `autoWebFetch`/`browserWebFetch`. Production bounds and cleanup cover executable discovery/download, isolated temporary profile, browser download denial, file/ftp blocking, two concurrent slots, 30-second render/8-second quiet DOM deadline, 2 MiB/20,000-element snapshot, open shadow/same-origin iframe extraction, cancellation, and slot/profile cleanup.
- PASS: `TestBrowserAutoKeepsLongJavaScriptShellWhenStaticBodyIsSufficient` adds a long hydration shell plus sufficient static body fixture. The production auto selector stays on direct HTTP and succeeds even with an invalid browser path, proving a large script shell alone does not force rendering.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run '^TestBrowser' -count=1 -v` (exit 0, Chrome 152.0.7977.77 headless, 27.192s), focused heuristic tests (exit 0), and `git diff --check` passed.
- NOT RUN: Browser-less first-use's actual Chromium package download; offline/fail-closed gating is covered by the S11 browser fixtures and S11.D record.

### S12 — 2026-09-20

- PASS: `internal/agent/lsp.go` adds the read-only Go `LSP` tool with configured executable selection (`lspExecutable`, `CORELAY_LSP_EXECUTABLE`, compatibility env, then PATH `gopls`), bounded JSON-RPC framing, semantic definition/references/diagnostics, full-document synchronization, cancellation, timeout, and supervisor lifecycle. `RepoMap` remains structural and is never relabeled as semantic.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run 'LSP|ToolSchema|PathSafety|ContextScope|ToolCatalog' -count=1` (exit 0), `go test ./cmd/proxy -run 'Doctor|Diagnostics' -count=1` (exit 0), and `go vet ./internal/agent ./internal/config ./cmd/proxy` (exit 0). The local helper process verified initialize/didOpen/definition/references/diagnostics, cancellation, shutdown, and a space-containing URI; missing-server output verified the labeled RepoMap fallback.
- PASS: README, `docs/lsp.md`, config schema, doctor capability output, tool catalog, dispatcher, path policy, plan mode, and built-in read-only agent roles are synchronized.
- PASS: A temporary validation-only `gopls v0.23.0` executable in a separate `GOBIN` passed `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run '^TestInstalledGoplsSemanticFixture$' -count=1 -v` (exit 0). The fixture covered multi-file definition, references, type-error diagnostics, Windows long-path/space URI handling, and a full-mode external project rooted outside the selected workspace. Corelay Code still never installs gopls automatically; the temporary executable is removed after validation.
- HISTORICAL / RESOLVED IN S13: At the time of this S12 run, full `go test ./internal/agent -count=1` also exercised a non-LSP Chronos fixture that overflowed its 16K test context before dispatch. S13 raised that policy-forwarding fixture to 32K and the full suite now passes. User-installed gopls versions may differ in pull/push diagnostic timing; the bounded notification cache and fallback remain the compatibility path.

## 측정

S13에서 기존 profiler/regression runner에 현실 과제를 추가한다.
최소 6종: 다중파일 버그, 신규기능+테스트, 실패테스트 수리, 장문 결정 보존,
프로젝트 전환, 중단 복구. 각 과제 acceptance를 실행 가능한 검사로 정의한다.
기준/수정 버전은 같은 fixture/seed/target/budget으로 비교한다.
성공률, tool parse failure, 무증거 완료, wall time, tokens/cost(관측 가능할 때), 안전성 실패를 기록한다.
fixture별 최소 3회 반복은 품질 추세 확인용이며 통계적 우월성의 증명이라고 주장하지 않는다.
단일 marker edit 통과를 전체 coding 능력 증명으로 쓰지 않는다.

### S13.A — 2026-09-20

- PASS: 기본 capability probe plan v3에 여섯 validation category를 연결했다. 각 fixture는 disposable workspace에서 실제 `RunLoop` executor가 읽고 쓰는 multi-file 작업(결정 보존은 읽기 artifact)을 준비하고, approved mutation과 최종 파일 snapshot digest를 acceptance로 사용한다. project switch는 두 project 디렉터리와 선택 결과를, interrupt recovery는 resumable checkpoint 상태를 검사한다.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/capabilityprofile ./cmd/corelaycode-profile -count=1` (exit 0). 신규 `TestValidationFixtureCatalogHasExecutableAcceptanceArtifacts`는 여섯 task의 artifact 개수, workspace containment, expected snapshot digest, marker를 확인했고 기존 profiler/CLI suite도 통과했다.
- NOT RUN / KNOWN: 외부 provider를 호출하는 실제 비용 측정과 OS 강제 종료는 수행하지 않았다. S13.B에서 CI OS matrix와 Q01~Q16 연결을 다룬다.

### S13.B — 2026-09-20

- PASS: `.github/workflows/ci.yml`의 기존 `ubuntu-latest`/`macos-latest`/`windows-latest` matrix에 validation entrypoint를 연결했다. 각 runner는 제품 설치를 바꾸지 않는 별도 `GOBIN`에 `gopls v0.23.0`을 준비한 뒤 `scripts/roadmap-qa.sh`로 Q01~Q16에 대응하는 `go test ./...`, 이름을 지정한 migration 회귀, 반복 sandbox/process 계약, `go vet ./...`를 실행한다. Ubuntu race와 Web lint/build job은 별도로 유지한다.
- PASS: `bash -n scripts/roadmap-qa.sh` (exit 0), `git diff --check` (exit 0).
- PASS: Windows host의 실제 Git Bash(`C:\Program Files\Git\bin\bash.exe`)에서 `$env:GOTOOLCHAIN='go1.26.1'; bash scripts/roadmap-qa.sh`를 재실행해 exit 0으로 완료했다. entrypoint가 OS/Go/gopls 상태를 기록하고, 전체 `go test ./... -count=1`, 이름을 지정한 `Migration|Legacy` 회귀, `go test ./internal/sandbox ./internal/processsupervisor -count=3`, `go vet ./...`, 영속 `build-release-test.sh` fixture를 통과했다. 반복 실행에서 드러난 `TestWindowsJobRunnerKillsDescendantTreeOnTimeout`의 1.5초 setup deadline은 10초로 조정해 Job Object 준비 지연과 실제 descendant termination을 분리했고, 해당 테스트 `-count=3`도 exit 0이다. 별도 임시 `GOBIN`의 `gopls v0.23.0`으로 `TestInstalledGoplsSemanticFixture`도 exit 0(5.952s)이며, fixture 종료 후 executable을 제거했다. Chronos full-mode 정책 fixture는 전체 정책 시스템 프롬프트가 16K 고정 context를 초과하던 테스트 harness를 32K로 조정한 뒤 실제 Write dispatch와 무승인 실행을 확인한다.
- PASS / BROWSER REGRESSION: 전체 suite에서 드러난 Rod 프로젝트 전환 race를 현재 선택과 대상 선택을 분리한 SidePanel selector 및 메뉴 행 렌더 대기로 보정했다. `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/server -run '^TestS10BrowserUIProjectFilesAndStaleStreams$' -count=5 -v`가 5회 모두 exit 0으로 통과했고, 이후 전체 Git Bash roadmap QA도 exit 0이다.
- PASS: bubblewrap staging 보완 후 privileged `golang:1.25-bookworm` Docker container에 bubblewrap 0.8.0을 설치하고 Go 1.25.14로 `go test -race ./internal/approval ./internal/agent ./internal/acpbridge ./internal/server -count=1`을 재실행해 모두 exit 0을 확인했다. privileged 없는 기본 Docker 실행은 sandbox isolation capability 부족으로 fail-closed했으며 hosted runner와 동일한 결과로 간주하지 않는다.
- PASS / LINUX ROADMAP QA: privileged `golang:1.25-bookworm`에서 `GOTOOLCHAIN=auto`, 임시 `gopls v0.23.0`, bubblewrap 0.8.0을 준비한 뒤 `bash scripts/roadmap-qa.sh`를 실행해 전체 Go suite와 Q01~Q16 연결 회귀, sandbox/process 반복, vet를 exit 0으로 완료했다. Go 1.25.14의 gopls 설치는 Go 1.26.8 자동 toolchain 선택으로 고정했고, `/tmp` 격리로 숨겨지던 임시 MCP 실행 파일은 읽기 전용 staging mount로 보존했다. ACP stdio MCP conformance도 같은 조건에서 통과했다. privileged local container 결과이며 hosted runner 결과를 대신하지 않는다.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 .github/workflows/ci.yml .github/workflows/release.yml` (exit 0), `bash -n` for all roadmap/release scripts (exit 0), Docker ShellCheck for the three scripts (exit 0), and Python YAML parse (exit 0) validated the workflow syntax and expressions.
- NOT RUN / KNOWN: WSL의 system bash는 Windows Go PATH를 전달하지 않아 `go: command not found`(exit 127)였고, Git Bash 경로로 재실행해 통과시켰다. 해당 Git Bash 환경에는 `gopls`가 없어 roadmap entrypoint의 Q15 installed-server test는 skip됐지만, 별도 임시 fixture는 위 S13.B 기록대로 통과했다. 외부 hosted CI matrix와 명시 OS별 runner 결과는 이 세션에서 실행하지 않았다.

### S13.C/D — 2026-09-20

- PASS: `internal/buildinfo`를 proxy/server, ACP, profiler가 공유하고 Makefile/Docker/release build의 linker flags를 통일했다. release workflow는 5개 target × 3개 binary package smoke와 `checksums.txt` 생성을 포함한다. 임시 release fixture에서도 `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64` × proxy/ACP/profile을 `CGO_ENABLED=0`으로 교차 빌드해 15개 binary와 15줄 checksum을 exit 0으로 확인한 뒤 산출물을 제거했다. `corelaycode-profile version` linker smoke, ACP advertised streamable HTTP fixture, 영향 package version/doctor/root/capability tests, `go vet`, `git diff --check`가 통과했다.
- PASS / RELEASE PACKAGE GUARD: release workflow와 `scripts/build-release.sh`가 5개 OS/architecture × 3개 binary의 정확한 15개 이름과 15줄 checksum을 검증하도록 강화했다. script는 frontend/cross-build 실패를 `set -euo pipefail`로 중단하고 모든 cross-build에 `CGO_ENABLED=0`을 적용한다. 영속 `scripts/build-release-test.sh`를 `roadmap-qa.sh`에 연결해 Windows Git Bash와 Linux Docker에서 15개 artifact/checksum 성공, stale artifact 거부, frontend 실패 전파를 확인했다. artifact 수집은 GNU 전용 `find -maxdepth` 없이 Bash glob을 사용해 macOS runner에서도 실행되도록 했다. `actionlint`와 `bash -n`도 exit 0이다. fixture는 script orchestration만 검증하며 실제 cross-build는 package smoke에서 검증했다.
- PASS: Docker 29.5.3에서 `docker build --no-cache --build-arg CORELAY_VERSION=v-fixture --build-arg CORELAY_BUILD_COMMIT=local-fixture -t corelaycode:roadmap-smoke .`가 frontend와 proxy/ACP/profile binary를 포함해 exit 0으로 완료했다. `.dockerignore` 적용 후 context는 34.70KB였고, `docker run --rm corelaycode:roadmap-smoke version`이 `v-fixture`/`local-fixture` metadata를 출력했다. 임시 image tag를 제거했다.
- PASS: 명시적 `corelaycode update -artifact <path> -sha256 <digest>`와 `internal/updater`의 local-only transaction을 추가했다. SHA-256 mismatch, symlink, existing backup, install, rollback fixture 및 CLI install이 통과했다. Windows 공유 삭제 금지 핸들을 실제로 열어 `ErrTargetInUse`와 target/backup 보존을 확인하는 `TestInstallFailsClosedWhenWindowsTargetDeleteIsDenied`도 통과했다. 현재 실행 파일은 one-shot helper가 부모 종료 후 교체하며, `go test ./cmd/proxy -run '^TestWindowsSelfUpdateHelperReplacesAndRollsBackRunningExecutable$' -count=1 -v` (exit 0, 5.964s)가 실제 digest 교체와 rollback을 확인했다. fixture는 부모 테스트 프로세스의 Go toolchain을 상속해 hosted matrix의 선언 버전에 맞춰 빌드한다. background update와 remote discovery는 없다.
- NOT RUN / KNOWN: 실제 GitHub tag release, artifact publish/download, Docker publish, hosted runner에서의 self-replacement는 수행하지 않았다. 비-self target이 사용 중인 경우에는 fail-closed로 유지되며 server 중지 후 재시도한다.

### S13.E — 2026-09-20

- PASS: README, `docs/agent-operating-model.md`, `docs/web-fetch.md`, API surface table, and roadmap README now describe the implemented Web/Rod, MCP, LSP, project/session, profiler, build metadata, and explicit update contracts.
- PASS: G1/G2/G3 focused gate evidence is recorded in `review.md`; commands were run locally with fake providers/fixtures and no paid credentials.
- NOT RUN / KNOWN: The roadmap remains IN_PROGRESS until hosted Linux/macOS/Windows Q01~Q16 CI is executed. The full local suite and the Git Bash `scripts/roadmap-qa.sh` entrypoint are green after the Chronos fixture context correction. Interactive user-flow judgment, live provider/browser-download, release publish, and hosted runner self-replacement remain outside this validation; the local Windows helper integration test is green.

## 릴리즈 게이트

필수 로컬/CI 테스트 PASS, Q01~Q16 결과, migration fixture 보존, README/API 사용법 일치.
live 외부 서비스 검증 미실행은 별도 capability limitation으로 명시.
Critical/High 회귀 또는 불명확한 migration 손상은 릴리즈 준비 완료를 차단한다.
배포/게시/업데이트 실제 수행은 이 계획의 검증과 별개다.
