# 결함·요구 추적표

C = 읽은 코드로 동작/연결 누락 확인. R = 위험 시나리오이며 재현 필요. N = 신규 요구.
C도 실제 사용자 환경 장애 발생을 뜻하지 않는다. 구현 시작 시 최신 코드를 다시 확인한다.
숫자 위치는 감사 당시 위치이며 함수명을 우선 검색한다.

| ID | 구분 | 관찰과 근거 | 섹션 |
|---|---|---|---|
| F01 | C/R | config.Load 파싱 오류 무시, Save 직접 덮어쓰기, authMiddleware 빈 토큰 허용. internal/config/config.go:129, server/server.go:380 | S01 |
| F02 | C/R | localhost 기본이나 CORS 모든 출처 허용, optional auth. server/server.go:363,433 | S01 |
| F03 | C/R | .claude/settings.json을 필터 없이 prompt에 포함. agent/context.go:43 | S01 |
| F04 | C | provider/workspace/skill-source API 일부 Save 오류 무시. server/server.go:794,1067,1942 | S01 |
| F05 | C/R | Bash 위험 분류와 병렬 read-only 분류가 다른 규칙. permission.go:71, concurrency.go:51 | S02 |
| F06 | N | 명시적 full 모드와 실제 executor 제약을 끝까지 연결할 필요. run_options.go, sandbox_bridge.go, sandbox/job_windows.go | S02 |
| F07 | C/R | 서버 전역 workDir, Chat 요청은 workDir 생략. server.go:794,2834, Chat.tsx:356 | S03 |
| F08 | C | per-session provider/model 저장은 있으나 일반 실행 대상은 전역 설정 영향. server.go, cmd/proxy/tui_commands.go | S03 |
| F09 | C | Session에 WorkstreamID 없음; 요청에서 별도 선택. agent/sessions.go:244 | S03/S05 |
| F10 | C | 프로젝트 지침은 현재 폴더 중심, ancestor/nested scope 로더 없음. context.go:12 | S03 |
| F11 | C/R | GitCommit 지정 파일 stage 후 일반 commit, 기존 index 포함. tools_advanced.go:527 | S04 |
| F12 | C/R | Undo에 postimage 충돌 확인 부족, single checkpoint. checkpoint.go:21,182,206 | S04 |
| F13 | C | reconcile이 기록 확인/해제로 한정; 실제 변경 결과 자동 검증 아님. sessions.go:1352, tui_commands.go:289 | S04 |
| F14 | C | 전역 activePlan와 일반 loop 계획의 연결 부족. plan.go:26, server.go:1831, loop.go | S05 |
| F15 | C | Workstream/TeamPlan은 구현됨; 일반 세션·단계 재개 연결 부족. workstream/types.go, team_plan.go | S05 |
| F16 | C/R | 기본 read-only 탐색 가중치5 후 history 축약·도구 제거. loop.go:49,1245 | S06 |
| F17 | C/R | 압축 source는 오래된 메시지부터 제한, Decisions 전달 부족. compact.go:760, loop.go:1366 | S06 |
| F18 | C | SkillSource 저장하나 loop LoadSkills는 all 사용. context.go:70, loop.go:1072 | S07 |
| F19 | C | SkillDirs/MCPConfigPaths 소비처를 cmd/internal 검색에서 찾지 못함. config.go:41 | S07/S11 |
| F20 | C | 스킬 이름/설명 없이 개수만 모델에 안내. loop.go:1132 | S07 |
| F21 | C | 프로젝트 .claude만 자동 탐색, first-wins, 20KB 절단, 원본경로 없이 slash 확장. context.go, commands.go:58 | S07 |
| F22 | C | one-shot/plain session flag 영속 바인딩 누락. cmd/proxy/chat.go:55,82,486 | S08 |
| F23 | C | 별도 서버 시작 필요, 구조화 출력/운영 명령 부족. chat.go, main.go | S08 |
| F24 | C | 이미지 UI와 ImageRead가 실제 이미지 대신 placeholder/길이 전달. Chat.tsx:325, tools_advanced.go:161 | S09 |
| F25 | C | Explorer가 파일 열기를 모델 요청으로 수행, SSE 부분 줄/경로 처리 우려. Explorer.tsx:43,60,106 | S10 |
| F26 | C | diff 표시 최신 결과 중심, 누적 변경/선택 복원 부족. tui_commands.go:330, Chat.tsx:576 | S10 |
| F27 | C/R | browser auto 짧은 shell 기준, DOM mutation timeout이 좋은 콘텐츠도 실패시킬 가능성. tool_web_browser.go:44,207 | S11 |
| F28 | C/N | MCP stdio 중심, 원격 transport 추가 필요. mcp_runtime.go, mcp_client.go, acpbridge/mcp_validation_test.go | S11 |
| F29 | C/N | RepoMap은 존재; semantic LSP 생산 경로는 미확인. repo_map.go, 이전 reference-capability-absorption-v2/plan.md | S12 |
| F30 | C | profiler/regression은 존재; 현실 multi-file 작업 평가와 Web E2E 부족. cmd/corelaycode-profile/fixtures.go, .github/workflows/ci.yml, web/package.json | S13 |
| F31 | C | release 구성은 있으나 버전/doctor/update 통합 부족. scripts/build-release.sh, release.yml, server.go:1360 | S08/S13 |

## 재현 원칙

검토 정정: main loop는 DefaultPermissionConfig 이후 AutoApprove를 moderate로 설정한다
(internal/agent/loop.go:1998). 실제 기본을 safe라고 설명한 과거 응답은 정확하지 않다.
F05는 기본값과 별개로 변경 명령이 concurrency-safe 군에 들어가는 문제다.
file mutation batch의 기본 대상은 Write/Edit이며 NotebookEdit 포함으로 가정하지 않는다.

R 항목은 임시 디렉터리, 가짜 credential, 로컬 HTTP fixture에서 재현한다.
재현 실패 시 코드 경로와 환경을 남겨 NEEDS-REPRO로 기록한다. 장애가 없다고 단정하거나 임의 보안 패치를 넓히지 않는다.
F02는 서버 Origin 계약 테스트와 실제 브라우저 접근 검증을 분리한다.
설정 파일 실제 내용, API key, 원본 사용자 conversation은 증거에 복사하지 않는다.

S07.A 처리 기록: F18의 실행 consumer 누락은 API 목록과 RunLoop의 workspace policy resolver로 연결했다. F19 중 `SkillDirs`는 global/project additional roots로 소비되며, `MCPConfigPaths`는 S11에서 별도로 다룬다.

S11 처리 기록: F19의 `MCPConfigPaths`는 RunLoop와 legacy MCP loader의 병합 경로로 연결했다. F27은 Rod bounded DOM snapshot과 long-shell heuristic 회귀로 보완했다. F28은 stdio와 공통 executor catalog에 streamable HTTP를 추가하고, auth/header/cancel/session-loss reconnect/schema refresh를 로컬 fixture로 검증했다. legacy SSE, OAuth discovery/refresh, 외부 remote service는 capability limitation으로 남겼다.

S12 처리 기록: F29를 기존 `RepoMap`과 semantic `LSP`의 별도 capability로 분리했다. Go `gopls` JSON-RPC 경로는 configured executable/PATH 해석, workspace·Windows URI, full-document sync, timeout/cancel, supervisor lifecycle, definition/references/diagnostics를 제공하며 read-only catalog와 path policy를 통과한다. 서버 부재·crash·비 Go 대상은 semantic 결과로 위장하지 않고 structural RepoMap fallback을 반환한다. 실제 `gopls v0.23.0` fixture에서 Windows 긴 경로/공백 URI, 다중 파일 definition/references/type-error, full-mode 외부 프로젝트 루트 처리를 재현했고, Go 도구의 deny-by-default 환경 때문에 `GOCACHE`를 격리된 임시 경로로 명시해 semantic 분석이 안정적으로 시작되도록 했다.

S13.A 처리 기록: F30의 profiler 부족 범위를 별도 서버 없이 기본 capability profiler plan v3로 확장했다. 여섯 현실 과제는 multi-file bug, new feature+test, failing-test repair, decision retention, project switch, interrupt recovery이며, fixture별 approved mutation과 bounded multi-file snapshot acceptance를 가진다. 실제 provider/OS 종료는 호출하지 않았고, 여섯 fixture 계약과 기존 CLI/profiler suite를 통과시켰다.

S13.C/D 처리 기록: F31의 version/release 통합 범위를 공통 `internal/buildinfo` linker injection, profiler version command, ACP/server metadata, release checksum/package smoke로 닫았다. 업데이트는 원격 discovery나 background worker가 아니라 사용자가 artifact와 SHA-256을 직접 지정하는 `internal/updater` transaction으로 제한했다. staging과 post-install digest 검증, 기존 executable backup, 명시 rollback, symlink·digest·backup 충돌 거부를 구현했다. Windows 실행 중 다른 대상은 rename 실패를 target-in-use로 분류해 현재 파일을 보존하고, 현재 실행 파일은 one-shot helper가 부모 종료 뒤 교체한다. 실제 tag/Docker publish는 수행하지 않았다.
S13.C package smoke 보강: 임시 fixture에서 5개 OS/architecture × proxy/ACP/profile을 `CGO_ENABLED=0`으로 교차 빌드해 15개 binary와 checksums를 exit 0으로 만들고 산출물을 제거했다. 실제 tag/GitHub artifact publish/download와 Docker publish는 여전히 외부 게이트다.
S13.C release guard 보강: release workflow와 `scripts/build-release.sh`가 지원 대상 15개 artifact 이름·개수와 checksum 줄 수를 정확히 확인하도록 만들고, script cross-build를 `CGO_ENABLED=0`/`set -euo pipefail`로 고정했다. 영속 `scripts/build-release-test.sh`를 roadmap QA에 연결해 Windows Git Bash와 Linux Docker에서 script orchestration exit 0, 15개 checksum, stale/frontend 실패 거부를 확인했다. artifact 수집은 GNU 전용 `find -maxdepth` 대신 Bash glob을 사용해 macOS runner를 보존했다. 실제 tag publish/download는 여전히 외부 게이트다.
S13.C container smoke 보강: `.dockerignore`로 `.git`, docs, node_modules, 기존 webdist와 local artifacts를 build context에서 제외했다. Docker 29.5.3 `--no-cache` fixture version/commit build가 34.70KB context에서 frontend와 proxy/ACP/profile image build를 exit 0으로 완료했고 `docker run --rm ... version`이 `v-fixture`/`local-fixture` metadata를 출력했다. 임시 image tag는 제거했으며 registry publish는 수행하지 않았다.
S13.D Windows 경계 보완: `updater_windows_test.go`가 실제 Windows `CreateFile` 공유 모드(삭제 공유 없음)로 target을 잠근 뒤 Install이 `ErrTargetInUse`로 닫히고 target/backup 상태를 보존하는지 검증한다. `update_helper_windows.go`의 helper가 실행 중 현재 `.exe`를 부모 종료 뒤 교체하고 rollback하며, `TestWindowsSelfUpdateHelperReplacesAndRollsBackRunningExecutable`가 실제 digest를 확인한다.

S13.B 보완 기록: Windows Git Bash에서 `bash scripts/roadmap-qa.sh`를 실행해 OS/Go/gopls 상태, 전체 Go suite, 명명된 migration 회귀, sandbox/process 반복, `go vet ./...`를 exit 0으로 확인했다. 처음 Chronos full-mode 정책 fixture가 16K context에서 전체 정책 시스템 프롬프트를 담지 못해 dispatch 전에 overflow된 것을 확인했고, 정책 경계 자체를 바꾸지 않고 fixture harness를 32K로 조정했다. 로컬 Git Bash에는 gopls가 없어 installed-server test는 skip됐고 S12의 별도 임시 `gopls v0.23.0` fixture가 semantic 경로를 증명한다. WSL system bash의 `go: command not found`는 Windows Go PATH를 전달하지 않는 환경 차이이며 Git Bash 경로에서는 통과했다. hosted OS matrix는 여전히 미실행이다.
S13.B 최신 재검증: 실제 `C:\Program Files\Git\bin\bash.exe scripts/roadmap-qa.sh`가 exit 0으로 전체 suite, Migration/Legacy, sandbox/process `-count=3`, vet, build-release fixture를 통과했다. 반복 실행에서 Job Object 준비 전 1.5초 deadline이 setup timeout을 유발해 `internal/sandbox/job_windows_test.go`의 descendant timeout fixture 예산을 10초로 조정했고, 해당 테스트 `-count=3`도 통과했다. WSL bash의 Go PATH 부재와 hosted Linux/macOS/Windows matrix 미실행은 그대로 외부 환경 제한이다.
S13.B 브라우저 회귀 보완: 전체 suite에서 드러난 `TestS10BrowserUIProjectFilesAndStaleStreams`의 Rod 프로젝트 전환 race를 현재 프로젝트와 대상 프로젝트를 분리한 SidePanel selector 및 메뉴 행 렌더 대기로 수정했다. 해당 테스트 `-count=5`와 후속 Windows Git Bash 전체 roadmap QA가 모두 exit 0이다.
S13.B Q15 보강: 제품 PATH를 오염하지 않는 임시 `GOBIN`에 `gopls v0.23.0`을 설치해 `TestInstalledGoplsSemanticFixture`를 exit 0(5.952s)으로 실행하고 임시 executable을 제거했다. 로컬 roadmap entrypoint는 gopls 미설치 상태에서도 나머지 suite를 통과하며, hosted matrix는 별도 게이트다.
S13.B Linux race 보강: bubblewrap staging 보완 후 privileged `golang:1.25-bookworm` + bubblewrap 0.8.0 조건에서 CI race 대상 네 패키지를 Go 1.25.14로 재실행해 exit 0을 확인했다. 기본 Docker seccomp는 sandbox isolation을 제공하지 않아 fail-closed했으며, 이는 capability 환경 차이다.
S13.B Linux roadmap 재검증: privileged `golang:1.25-bookworm`에서 `GOTOOLCHAIN=auto`, gopls v0.23.0, bubblewrap 0.8.0을 준비하고 `bash scripts/roadmap-qa.sh`를 exit 0으로 완료했다. `/tmp` tmpfs가 go test 임시 MCP 실행 파일을 숨기던 bubblewrap mount 순서를 읽기 전용 `/run` staging bind로 보완하고, ACP stdio MCP fixture marker를 workspace 안에 두어 보안 경계를 유지했다. 이 증거는 hosted Linux runner 실행과 동일하지 않다.
S13.B workflow 정적 검증: `actionlint v1.7.7`이 CI/release workflow 두 파일의 YAML과 GitHub expressions를 exit 0으로 확인했고, 모든 roadmap/release script의 `bash -n`, Docker ShellCheck, Python YAML parse도 통과했다. 이는 hosted runner 결과를 대신하지 않으며 로컬 syntax gate다.
