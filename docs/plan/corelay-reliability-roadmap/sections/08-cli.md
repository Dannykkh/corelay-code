# S08 CLI·운영

상태: PASS
연결: F22,F23,F31
선행: S01,S03,S04

## 수정 시작점과 소유 범위

cmd/proxy/chat.go,tui_commands.go,main.go; internal/server lifecycle/session API; internal/config

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] plain/one-shot/TUI가 동일 durable create/append/resume/fork 계약을 사용한다. session flag 미지원 경로를 없앤다.

- B. [PASS] 단일 명령에서 server 연결 또는 관리되는 child 시작을 구현한다. 명시 remote URL은 자동 local fallback 금지. port 충돌 시 프로세스 identity/credential 확인.

- C. [PASS] human/json/jsonl 출력 계약과 stable event version, stdout/stderr 분리, 실패/cancel exit code를 정의한다. stream split/재연결 시 중복 실행 방지.

- D. [PASS] version/doctor 구현: CLI/server/build commit, browser/Git/shell/LSP/MCP capability, 권한 한계를 비민감하게 출력. child 종료와 기존 외부 서버 보존 테스트.

## 완료 조건

Q10/Q16. standalone invocation, managed child cleanup, concurrent CLI startup single owner, 잘못된 endpoint, ctrl-c, failed run nonzero. JSON stdout에 banner/progress prose가 섞이지 않는다.

## 회귀·제약

기존 사용자의 서버 프로세스를 종료하거나 port를 강제로 해제하지 않는다. version/doctor가 credential을 출력하지 않는다.

## 검증 기록

- A PASS: 기준 commit `82d033bf4490033d782c97566877fed02536799e` (작업 트리의 사용자 변경은 보존하고 커밋하지 않음). plain/one-shot/TUI에서 `-session`을 처리하고 `-fork`를 연결했다. line client와 TUI가 공통 helper로 사용자 메시지를 저장하고, 저장 revision을 `/api/agent`의 `durableSessionId`/`expectedRevision`으로 묶는다. 실행 후 서버 canonical transcript와 revision을 다시 읽는다. `-workdir`를 생략하면 저장 세션 workspace를 채택하며, 명시 경로가 다르면 정규화 경로 비교 후 fork 또는 append 전에 중단한다. 독립 리뷰가 찾은 workspace mismatch 문제를 이 사전 검사와 CLI/TUI 회귀 테스트로 해결했다.
- 회귀 범위: 실제 `runChat` subprocess가 one-shot 생성, one-shot 재개, plain 재개, fork를 stateful session API fixture에 연결하고 ID/revision/transcript/workspace를 확인한다. workspace 충돌은 plain/one-shot/fork 및 TUI bootstrap/turn 준비에서 부모 revision·session 수·fork/save/start가 바뀌지 않는지 확인한다. plain append가 CAS 409를 만나면 stale cache를 버리고 다음 turn에서 최신 transcript/revision을 읽는 것도 검증한다. 기존 stream·approval 테스트 서버는 durable API fixture로 보강해 스트림 회귀도 실행한다.
- 최초 전체 패키지 실행 `go test ./cmd/proxy -count=1`: FAIL (exit 1). 새 production 경로가 stream 테스트 전 durable save/GET을 호출하지만 기존 테스트 server fixture가 해당 API를 제공하지 않아 요청이 스트림 handler에 도달하지 않았다. fixture를 durable save/GET 경로로 보강한 뒤 동일한 전체 패키지 명령 PASS (exit 0, 0.659s).
- focused 회귀 `go test ./cmd/proxy -run 'Test(ChatSessionSubprocessHelper|ChatOneShotPlainResumeAndForkUseDurableSessionContract|ChatRejectsExplicitWorkspaceMismatchBeforeSessionMutation|ChatReloadsSessionAfterCASConflictBeforeNextTurn|TUICommandLineForkUsesLoadedRevisionAndAdoptsSessionWorkspace|TUIRejectsExplicitWorkspaceMismatchBeforeForkOrAppend|TUIPreparesDurableTurnBeforeStartingAgent|TUIPropagatesRequestedModeAndShowsServerConfirmedMode|StreamTurnApprovalDecisions|ApprovalConflictCancelsTurnFailClosed|PlainStreamRejectsOversizedSSE|PlainStreamReturnsBoundedSanitizedHTTPError|PlainStreamSanitizesTextEvents|PlainStreamTextOnlyEOFIsUnknownTerminal|PlainStreamDoneReturnsSuccess|PlainOneShotUnknownTerminalExitsNonzero|ApprovalRequiresFutureExpiry|StreamTurnReturnsBlockedCompletionError)$' -count=1`: PASS (exit 0).
- 서버와의 기존 CAS/fork/workspace 경계 `go test ./internal/server -run 'Test(SessionAPICreateUpdateAndRenameRequireExpectedRevision|SessionAPIForkIsIsolatedAndRevisionChecked|AgentLoopRejectsExplicitDurableWorkspaceMismatchBeforeDispatch)$' -count=1`: PASS (exit 0, 0.323s).
- `git diff --check`: PASS (exit 0). 변경 Go 파일 `gofmt -d`: 출력 없음. UI/web build는 web 코드를 바꾸지 않아 NOT RUN.
- 독립 리뷰 `/root/s08a_trace`: workspace preflight가 CLI와 TUI에서 fork/save 전에 수행되고 생략 workdir는 기존 workspace를 채택하는 점을 확인했다. 리뷰가 찾은 CAS 409 후 plain session stale-cache 문제는 무효화·재조회로 수정했으며 최종 재검토 PASS.
- B PASS: 기준 commit `82d033bf4490033d782c97566877fed02536799e`. 변경: `cmd/proxy/managed_server.go`(신규), `managed_server_child_{unix,windows}.go`(신규), `managed_server_lock*.go`(신규), `managed_server_test.go`(신규), `managed_server_child_*_test.go`(신규), Unix Ctrl-C 통합 테스트(신규), `cmd/proxy/chat.go`, `tui.go`, `main.go`, `internal/server/server.go`, `README.md`. 기본 `chat`은 설정 포트의 loopback에서 인증된 Corelay 서버만 재사용하고 없으면 같은 실행 파일을 headless managed child로 시작한다. 명시한 `-url`은 연결 전용으로 유지한다. 알 수 없거나 인증이 맞지 않는 점유 포트는 손대지 않고, 프로세스 간 OS 파일 잠금으로 startup과 lease 생성을 직렬화한다. 마지막 lease 만료 뒤에도 child가 같은 잠금을 잡고 lease를 재검사하며, HTTP listener를 shutdown한 다음 잠금을 풀어 새 client가 종료 중 서버에 연결되지 않게 했다. Unix child는 별도 process group, Windows child는 `CREATE_NEW_PROCESS_GROUP|CREATE_NO_WINDOW`로 owner Ctrl-C 전파를 차단한다. 관리형 probe 및 chat/TUI 요청은 리다이렉트를 거부해 `X-Access-Token`이 다른 endpoint로 전달되지 않는다.
- B 완료 회귀: `go test ./cmd/proxy -count=1` PASS (exit 0, 14.041s); `go test ./cmd/proxy -run '^TestManagedChatConcurrentCLIProcessesShareOneChild$' -count=5` PASS (exit 0, 13.150s; 두 요청을 barrier로 겹치도록 고정); `go vet ./cmd/proxy` PASS; `go test ./internal/server -run 'Test(SessionAPICreateUpdateAndRenameRequireExpectedRevision|SessionAPIForkIsIsolatedAndRevisionChecked|AgentLoopRejectsExplicitDurableWorkspaceMismatchBeforeDispatch)$' -count=1` PASS (exit 0, 0.230s). 실제 production binary subprocess가 managed startup·authenticated readiness·마지막 lease 후 종료를 검증하고, 별도 CLI subprocess는 실패 시 exit 1 및 정리를 검증한다. remote URL fallback 금지, foreign/mismatched-auth port 보존, 인증 probe redirect 차단, chat ping credential redirect 차단도 테스트한다.
- 플랫폼 신호 검증: 현재 Windows에서 전체 CLI suite 및 `TestManagedServerChildUsesSeparateConsoleProcessGroup`가 PASS했다. Unix 소유 CLI 프로세스 그룹에 Ctrl-C를 보내고 두 번째 live lease 동안 server가 남는 통합 테스트를 추가했으며 `GOOS=linux GOARCH=amd64 go test -c ./cmd/proxy` 교차 컴파일은 PASS했다. Unix 테스트는 Windows 개발 호스트에서 실행하지 않아 NOT RUN; Windows 콘솔 Ctrl-C 이벤트 자체도 비대화형 테스트 환경에서 직접 송신하지 않았다.
- 추가 검증: `git diff --check` PASS; 변경 Go 파일 gofmt 완료. 독립 리뷰 `/root/s08b_final_review`에서 managed lifecycle·마지막 lease 잠금·프로세스 그룹·endpoint identity/auth·redirect 보호를 검토해 blocker 없음을 확인했다. 첫 동시 subprocess 테스트에서 확인된 간헐 재시작은 두 agent 요청을 barrier로 동시 진행하도록 만들어 5회 반복 통과했다.
- C PASS: 기준 commit `82d033bf4490033d782c97566877fed02536799e`. `-format human|json|jsonl`을 추가했다. JSON은 `schemaVersion: 1`이 있는 단일 최종 결과이고 JSONL은 `schemaVersion/runId/seq/event/data` envelope의 정규화 이벤트와 마지막 `result` 한 건을 출력한다. machine 형식은 one-shot만 허용하며 stdout에는 protocol 데이터만 둔다. JSONL은 allowlist만 내보내고 원시 `tool_input`, workdir, Workstream 제목/다음 행동을 제외한다. token은 text/error/trace ID/terminal reason에서 제거하고 terminal kind·policy·상태는 allowlist로 제한했다. 기본 HTTP transport는 변경을 일으키는 `/api/agent` POST를 재시도하지 않으며, `done` 없는 EOF는 `unknown_terminal` 실패로 끝난다. `signal.NotifyContext`가 준비/실행/최종 세션 동기화에 전달되며 exit code는 성공 0, 실패·blocked 1, usage 2, Ctrl-C 130이다. 내부 approval cancellation은 사용자 Ctrl-C로 오인하지 않고 실패 1로 구분한다.
- C 변경 파일: `cmd/proxy/chat.go`, `chat_output.go`(신규), `chat_output_test.go`(신규), `chat_transport.go`, `chat_session_test.go`, `README.md`. 실제 `runChat` subprocess에서 JSON/JSONL stdout의 JSON-only 형식, 버전/sequence, 마지막 단일 result, status banner 미출력, raw tool input·비밀값 제거, structured usage/startup/terminal failure, cancellation mapping, EOF 뒤 `/api/agent` POST 1회를 검증한다. SSE chunk 경계의 event ordering은 기존 transport 회귀도 함께 유지한다.
- C 완료 검증: `go test ./cmd/proxy -count=1` PASS (exit 0, 14.574s); `go vet ./cmd/proxy` PASS (exit 0); `gofmt -d cmd/proxy/chat.go cmd/proxy/chat_output.go cmd/proxy/chat_output_test.go cmd/proxy/chat_transport.go cmd/proxy/chat_session_test.go` 출력 없음; `git diff --check` PASS (exit 0). focused subprocess 회귀 `go test ./cmd/proxy -run 'TestChatMachineOutputReportsUsageAndStartupFailuresAsStructuredResults|TestManagedChatExplicitRemoteFailureNeverFallsBackToLocal' -count=1` PASS; 비밀값·JSONL·취소 동기화 회귀 `go test ./cmd/proxy -run 'TestChatMachineOutputDoesNotExposeServerTerminalKindOrStopReason|TestChatJSONLOneShotEmitsAllowlistedVersionedEventsAndOneResult|TestChatStreamRefreshFollowsInterruptContext' -count=1` PASS.
- 독립 리뷰 `/root/s08c_review`: stdout 세션 안내 혼입, untrusted trace ID/terminal 필드, refresh 중 취소, machine format의 unknown-flag 사용 오류를 발견했다. 각각 human 전용 출력, token redaction·terminal allowlist, parent cancellation context, ContinueOnError structured usage result로 보완한 뒤 최종 blocker 없음.
- 플랫폼 한계: 취소 context와 refresh 취소/130 분류를 테스트했다. Windows 비대화형 테스트 프로세스에 실제 console Ctrl-C 이벤트를 주입하는 것은 NOT RUN; OS 신호 자체 전달 검증은 S08.B의 플랫폼별 기록을 따른다.
- D PASS: `version`은 설정 파일을 읽거나 서버에 접속하지 않고 CLI version·build commit·Go runtime을 출력한다. `doctor`는 설치된 browser/Git/Bash, 실행 sandbox capability, 로컬 MCP 설정 개수, 설정된 Go/gopls executable availability와 RepoMap fallback, 현재 OS 계정 기준 full mode 제한과 서버 health/version/build commit을 비민감한 allowlist로 출력한다. 브라우저·MCP/server child·LSP child를 시작하지 않고, 관리 상태/lease도 만들지 않는다. `/health`는 토큰 없이 GET하고 `/`만 인증 GET하며, explicit non-loopback URL에는 `-token`으로 직접 받은 credential만 전송한다. redirect는 차단하고 URL·config 오류·응답 body·provider/model·MCP command/env/path를 출력하지 않는다. 서버 root version/commit은 공통 `internal/buildinfo` 값이며 make/release/Docker 빌드에서 주입한다.
- D 변경 파일: `cmd/proxy/diagnostics.go`, `diagnostics_test.go`, `main.go`; `internal/buildinfo/buildinfo.go`; `internal/agent/tool_web_browser.go`, `tool_web_browser_test.go`; `internal/server/server.go`, `server_root_test.go`; `Makefile`, `Dockerfile`, `docker-compose.yml`, `.github/workflows/release.yml`, `README.md`.
- D 검증: `go test ./cmd/proxy -count=1` PASS (exit 0, 21.422s); production CLI subprocess가 설정 손상과 무관한 offline version, linker version/commit, doctor GET-only/auth header, secret/path 비노출, remote token trust boundary, redirect 차단, managed child 상태 미생성, 외부 fixture 보존, unreachable endpoint를 확인했다. `go test ./cmd/proxy -run '^(TestVersionAndDoctorProductionSubcommands|TestResolveDoctorAccessTokenKeepsRemoteCredentialsExplicit)$' -count=1` PASS; `go test ./internal/server -run '^TestRootReportsBuildMetadata$' -count=1` PASS; `go test ./internal/agent -run '^TestWebBrowserInstalledIsReadOnlyAndDoesNotExposePath$' -count=1` PASS; `go vet ./cmd/proxy ./internal/server ./internal/agent` PASS; `GOOS=linux GOARCH=amd64 go test -c ./cmd/proxy` PASS; `docker compose config --quiet` PASS; `git diff --check` PASS; D Go 파일 gofmt 완료.
- D 잔여 검증: Docker image 자체 build/publish는 실행하지 않았다. 독립 리뷰 `/root/s08d_final_review`가 발견한 MCP in-process 상태 오표시, 원격 ambient token 전송, 서버 commit 누락은 각각 runtime unknown 출력, loopback-only ambient credential, root commit allowlist로 수정하고 회귀 테스트했다. 실제 production provider credential 및 외부 server와 연결하는 live 검사는 실행하지 않았다.
- S08 완료. 다음 순서: S09.A — canonical image block contract와 provider capability 및 MIME/byte/pixel limits를 확정한다.
