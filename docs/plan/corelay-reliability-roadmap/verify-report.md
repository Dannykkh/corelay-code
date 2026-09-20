# Argos 검증 보고서

작성: 2026-09-20 / source: codex
대상: corelay-reliability-roadmap / main / HEAD 82d033bf4490033d782c97566877fed02536799e + 현재 미커밋 변경.

## 판정: CONDITIONAL — 로컬 결함 수정 완료, 미검증 범위 유지

앞선 전체 목표 완료 보고를 철회한다. TODO 상태가 없다는 사실은 요구사항 충족 증거가 아니다. S13.A는 실제 compaction, durable session 선택, 쓰기 후 취소·재조정·재개를 수행하도록 보완했고 정상/부정 회귀를 각각 반복 검증했다. S01 bootstrap은 단기 일회성 교환으로 수정했다. ACP는 workflow 결합 세션을 명시적으로 거절하며 workflow 실행 지원 자체는 미완료다.

전체 충족률은 산정하지 않는다. 전 영역 실행·요구사항별 증거가 부족한 상태에서 PASS 라벨 수로 백분율을 만들지 않는다. 기존 테스트 성공은 검사한 범위에만 적용한다.

## 이번 발견과 자동 수정

| ID | 심각도 / 상태 | 근거와 영향 | 조치·검증 |
|---|---|---|---|
| A01 | High / 수정 | `internal/workstream/store.go:22`: 요청별 Store가 별도 mutex로 동일 JSON/tmp를 갱신해 충돌·변경 유실 가능 | canonical workspace별 in-process mutex 공유. `store_test.go:14` 두 Store의 동시 부분 PATCH를 수정 전 rename sharing violation/file-not-found로 재현; 수정 후 20회 × 10 runs에서 두 필드와 timeline 보존, package PASS |
| A02 | Medium / 수정 | `internal/agent/lsp.go:568`: push 진단 URI를 무시하여 다른 파일 오류가 요청 파일 오류로 반환 | active document URI별 필터 및 빈 결과/미수신 구분. `lsp_test.go:24` protocol 3사례 수정 전 FAIL→수정 후 PASS |
| A03 | High / 수정 | `.github/workflows/ci.yml:43`: GITHUB_PATH 등록 직후 같은 step에서 gopls 호출. fresh runner에서 명령을 못 찾거나 기존 다른 버전 실행 | 현재 shell PATH export 추가, actionlint PASS. hosted run은 NOT RUN |
| A04 | Medium / 수정 | `cmd/corelaycode-profile/fixtures.go:149`: 신규 feature 테스트 signature 오류와 assertion 없는 bug/failing-test fixture | authored assertion 및 production `probe_verification.go` 연결. snapshot 일치 후 별도 workspace에서 required sandbox/network deny로 `go test -json` 실행. 실제 테스트 pass 이벤트 없거나 격리 불가·실패·취소면 성공 거절. Windows 회귀 및 privileged Linux bubblewrap 실제 assertion 실패→성공 PASS |
| A05 | High / 수정 | 기존 파일 snapshot·marker·읽기/쓰기 횟수만으로 현실 과제 성공 판정 | v4 production lifecycle dispatch: compaction 후 최신 결정 JSON 검증, 별도 durable A/B/A workspace·transcript 검증, 실제 Write 후 cancel→store reopen→checkpoint receipt reconcile→Read 재개. category별 정상/오답·재실행 거절 최소 3회 PASS. snapshot-only trace는 무조건 실패 |
| A06 | Supply-chain / 수정 | npm audit: High 6, Moderate 2, Low 1의 영향 dependency records | 허용 semver 범위 내 `npm audit fix --ignore-scripts`; lockfile 갱신. `npm ci --ignore-scripts`, 재audit 모두 성공/0건. 빌드·린트 PASS, embedded assets 재생성 |
| A07 | 계약 차이 / 수정 | `contracts.md` 일회성·단기 bootstrap 요구와 반복 가능한 cookie 발급이 불일치 | 60초 challenge→단일 교환, Host/Origin 결합, 최대 128개 pending, 원자적 consume. expiry/replay/concurrency/malformed/config 실패 검증 PASS. 프론트 동시 교환 병합, 새 embedded bundle Rod browser PASS |
| A08 | High / 수정 | `internal/agent/compact.go`가 최근 보존 구간 안의 최신 사용자 요청도 이미 복사했다고 간주해 생략 | 앞쪽에 실제 복사한 경우에만 생략하도록 조건 수정. `compact_latest_request_test.go`에서 두 번 압축해도 최신 요청이 마지막 user turn에 한 번 보존됨을 검증. compaction 관련 전체 회귀 PASS |
| A09 | High / 수정 | `internal/agent/mcp_remote.go`: session-loss 재연결 중 `tools/list`도 404이면 같은 mutex 재획득으로 timeout 후에도 반환되지 않음 | 내부 재연결 재진입 차단, 취소·deadline·client 종료를 받는 채널로 재연결 소유권 대기. 연속 session-loss 및 동시 대기 취소 회귀 수정 전 FAIL→후 PASS. 원격 MCP 10회 반복·Linux race 3회 PASS |
| A10 | Medium / 수정 | `web/src/pages/Chat.tsx`, `web/src/lib/sse.ts`: 완료 이벤트 없는 EOF에 오류 표시 없음; 미완성 done frame도 EOF에서 dispatch | 부분 응답 보존·연결 중단/세션 확인 안내, 실패 후 legacy 성공 저장 차단. 불완전 SSE frame 폐기. SSE 5개 단위 검사, 실제 Chromium의 빈 EOF/부분 응답/미완성 done과 저장 revision 불변 검사 PASS |

A06은 설치 패키지에 대한 advisory 결과이며 제품의 원격 악용 가능 취약점 9건을 재현했다는 뜻이 아니다. Vite 문제는 개발 서버 조건에 해당하며 Go가 제공하는 정적 bundle 서버와 구분한다. 공식 근거: https://github.com/vitejs/vite/security/advisories/GHSA-fx2h-pf6j-xcff (영향 8.0.0–8.0.15; Windows 파일 경로 검증). 현재 lockfile Vite 8.3.0.


## A05 실행 범위와 한계

- 결정 보존: 변경된 정책을 포함한 긴 대화와 structured decision state를 production `BuildDeterministicCompaction`으로 압축한 뒤 새 질문을 보낸다. 질문에는 정답 값을 넣지 않고 최신 결정 JSON을 검사한다. 자동 압축 임계치나 live 모델 품질 점수 측정은 아니다.
- 프로젝트 전환: 별도 workspace와 durable session A/B를 저장하고 매회 새 SessionStore로 A→B→A를 선택한다. 실제 Read, 정확한 파일 답, 양방향 transcript 누출 부재, 선택하지 않은 세션 불변, 원본 파일 보존을 확인한다.
- 복구: 실제 Read/Write 후 이벤트 수신 시 취소한다. 쓰기 전 journal을 저장하고 postimage 기반 checkpoint 증거를 확인한다. store를 다시 열어 미조정 SaveExpected 거절→receipt로 명시 조정→Read만으로 재개하며 재실행 시도를 거절한다. store 재개방은 별도 OS 프로세스 강제 종료와 구분한다. OS hard-crash는 기존 server 회귀의 별도 증거다.
- 전체 lifecycle 시간을 latency로 기록하고 실제 run·compaction·receipt는 digest로만 프로파일에 남긴다. 원문 응답은 메모리 내 판정에만 사용한다.

## Module Coverage

| 모듈 | 경로 / 적용 |
|---|---|
| Argos | `C:/Users/Administrator/.codex/skills/argos/SKILL.md`, references/verify-protocol.md 읽음 |
| code-reviewer | `C:/Users/Administrator/.codex/.olympus/source-skills/code-reviewer/SKILL.md`, references/security-audit.md 읽음. source-only 보안 계약 적용 |
| 기능·품질 검사 | Argos가 명시한 두 독립 native subagent. 읽기 전용 검토 후 메인이 소유 파일을 지정해 bounded healer 위임. 공유 보고서는 메인만 작성 |
| 일반 diff 리뷰 | 전체 roadmap/코드베이스 감리 분기에서 spec 중심 native 작업자 검사 사용; `codex review` 별도 중복 실행 없음 |
| flow-verifier | flow-diagrams 없음; 미적용 |
| frontend-design / ui-ux-auditor | design-system.md 및 루트 DESIGN.md 없음; Phase 6 기준 문서 부재로 미적용. UI가 없다는 뜻은 아님 |

## Phase 0 — CPS

spec.md에 Context Map/Problem Statement 쌍 없음: 정식 CPS 체크는 SKIP. spec의 명시 설계 선택 10개와 contracts/13개 sections를 실제 감사 입력으로 사용했다.

## Phase 1 — 기능·품질 대조

| 영역 | 실제 코드 근거 / 판정 범위 |
|---|---|
| S01 설정·인증 | config.go:172,258 Update + OS lock/atomic replace. auth middleware/Origin/bootstrap API focused tests PASS. A07 수정·새 Web bundle 실제 브라우저 PASS. Windows 사용자 ACL 실제 격리 증거 미확인 |
| S02 실행 정책 | executionpolicy/policy.go:50,79,108 기본 workspace/child 축소; agent/loop.go:760 snapshot 검증. 코드 존재와 전체 조합의 OS 격리 검증을 구분 |
| S03 프로젝트·세션 | session_workflow_binding.go:76 소속 검증, server.go:3534 production 연결. workspace 거부/approval 교차 요청의 직접 API 회귀 PASS |
| S04 복구 | checkpoint_undo.go:123,266,538 preflight 및 적용 직전 검사. 외부 프로세스의 최종 검사 이후 경합은 기존 명시 제한 |
| S05 작업·단계 | plans.go:334,355,407 CAS; workstream_plan_execution.go:295 receipt/evidence gate. A01 수정. 공유 mutex는 프로세스 간 lock을 의미하지 않음 |
| S06 문맥 | compact.go:276,540,682 snapshot reconstruction 확인. profiler가 production compactor를 호출하며 A08 최신 요청 누락 회귀도 추가 |
| S07 스킬 | skill_settings.go:25 workspace 설정; skill_descriptors.go:450 shortlist; skill_reader.go:54,124 digest/UTF-8/범위. 전체 policy 조합은 기존 회귀 범위 참조 |
| S08–S10 CLI/이미지/UI | 기존 durable/managed-server/image 경로와 실제 Rod UI 테스트 확인. 새 bundle에서도 프로젝트/파일/stale stream E2E PASS; interactive terminal/OS clipboard 전체 사용자 평가 NOT RUN |
| S11 웹/MCP | 기존 Rod supervisor와 MCP stdio/remote fixtures 존재. 이번 감사는 외부 MCP·browser package download·live provider 미실행 |
| S12 LSP | 실제 URI 진단 결함 A02 수정 및 protocol 회귀 PASS. 이번 installed gopls 검사는 실행 파일 부재로 SKIP |
| S13 평가·CI | A03/A04/A05 수정. 실제 acceptance 회귀 PASS. hosted OS matrix NOT RUN |

ACP 계약 공백 보완: workflow lifecycle 통합 전까지 WorkstreamID/PlanID/PlanRevision/StageID가 있는 세션을 Load/Prompt에서 명시적으로 거절한다. `workflow_binding_test.go`에서 runner 호출 0, durable revision/history 보존, runtime 미시작을 확인했다. 일반 ACP 세션 전체 회귀 PASS. 이는 ACP workflow 지원 완료가 아니며 Web/API 경로를 사용해야 한다.

## Phase 2 — 런타임

실행일 2026-09-20, Windows, Go toolchain go1.26.1, 설치된 Chrome/Chromium, fake providers/임시 workspace.

| 명령 | 결과 |
|---|---|
| `go build ./...` | exit 0 |
| `go vet ./...` | exit 0 |
| `npm run build`, `npm run lint` | 갱신 전·후 exit 0; JS chunk 약 1.28 MB 경고 유지 |
| `npm ci --ignore-scripts`, `npm audit` | exit 0, dependency advisory 0 |
| `go test ./internal/workstream -count=1` | exit 0; concurrency focused `-count=10`도 exit 0 |
| `go test ./internal/agent -run LSP -count=1` | exit 0; installed gopls는 미설치 skip |
| `go test ./cmd/corelaycode-profile -count=1` | exit 0; authored Go fixture 3종 assertion failure→repair pass 포함 |
| server 인증/Origin/bootstrap/workspace/approval focused tests | exit 0, 10 top-level tests |
| server WorkstreamPlan/Session binding + S10 browser focused tests | exit 0, 8 top-level tests |
| `actionlint` CI/release, `git diff --check` | exit 0; actionlint는 이전 CI 수정 시 실행, 이번 후속은 workflow 미변경. roadmap 상대 링크 23개 문서 검사 PASS |
| `go test ./... -count=1` | exit 0. A04/A05/A07/A08 및 ACP guard 수정 후 전체 package suite PASS; agent 132.320s, server 20.734s, profiler 14.820s. 후속 false-done 지표 보존 보완 후 profiler 전체 재검증 9.004s PASS. 미설치 도구 관련 skip을 전체 플랫폼 검증으로 간주하지 않음 |

## Phase 3·4 — API / QA

별도 api-spec.md, qa-scenarios.md 없음: 정식 전수 자동 대조는 SKIP. 대체 정본 contracts.md와 validation.md Q01–Q16을 사용했다. Q01–Q16 전체 OS 조합 완료로 환산하지 않는다.

| 직접 API 계약 | 정상/변조 및 전후 상태 근거 | 결과 |
|---|---|---|
| API-AUTH | auth_middleware_test.go:23,50 정상 header/cookie 204; 무토큰 401, 손상 config 503, next handler 미실행 | PASS 범위 |
| API-OBJECT | project_workspace_scope_test.go:34 정상 등록 workspace 파일; 미등록 거부; session_scope_q04_test.go:151 타 workspace session의 approval 해결 거부 | PASS 범위; 다중 계정 tenancy는 제품 계약 아님 |
| API-FIELD | SessionAPIEnforcesRevisionedWorkstreamPlanBinding/ValidatesWorkstreamBinding: workflow 소속·revision·provider/model 보존 | PASS 범위; 모든 endpoint mass-assignment 전수 검증 NOT RUN |
| API-FLOW | WorkstreamPlanAPIApprovalBindsRevisionAndUpdateRevokesIt 및 StageReconcileRequiresStoppedRunAcknowledgement: 승인 revision 변경·활성 run 선행 검사 | PASS 범위 |
| INPUT-SCHEMA / TEXT | session/plan 제한 decoder, 구조/ID 검사 확인 | 전 API의 null/길이/Unicode 경계 전수 실행 NOT RUN |
| INPUT-SINK | FileHandlersUseRegisteredClientWorkspace의 정상 read/write와 미등록 scope 거부; BrowserBootstrapRejectsDNSRebindingHost에서 config 미생성 확인 | PASS 범위; 전체 sink 경계 전수 검증 NOT RUN |

UI 없는 직접 호출의 HTTP 상태와 handler 미실행/저장 상태를 확인했다. 광범위 API 계약 전체 PASS는 부여하지 않는다.

## Phase 5·6 — 도면·디자인

flow-diagrams 및 디자인 명세 없음: 기준 문서 대조 SKIP. 실제 Web은 존재하며 Rod에서 프로젝트 전환, 파일 읽기, stale response 차단을 실행했다. 9영역 전체 시각/접근성 점수는 산정하지 않았다.

## Phase 7 — 보안

신뢰 경계: 브라우저/CLI→HTTP 인증·Origin→workspace/session/plan→run policy→tool supervisor→파일/외부 provider. 명시 full에도 HTTP 인증 경로 유지. npm supply-chain 결과는 A06 참조.

- 최초 감사의 Secret archaeology: `git ls-files '.env*'`와 `git log --all --format ... --name-only -- '.env' '.env.*'`에 결과 없음. 비밀값 본문 출력 안 함. gitleaks/trufflehog 미설치로 현재/전체 이력 redacting scanner NOT RUN; 시크릿 없음 판정 금지.
- 최초 감사의 Go advisory scanner: govulncheck 미설치로 NOT RUN이었다. 아래 보안 후속에서 실제 실행 및 패치 후 재검증으로 대체했다.
- CI/CD: push/PR test와 tag release 분리, release 권한 선언 확인. actions tag references를 immutable SHA/provenance 보장으로 주장하지 않음. hosted/publish NOT RUN.
- STRIDE: Spoofing은 auth/Origin focused PASS, Tampering은 workflow CAS 및 A01 수정, Repudiation은 receipt 경로 정적 확인, Information Disclosure는 workspace/digest 경계 부분 검증, DoS는 body/tool budget 일부만 확인, Elevation은 policy snapshot/child 축소 정적 확인. 전체 위협 제거 인증 아님.
- API 7-3a 필수 전수 실행 공백 때문에 다른 결함이 없어도 최대 CONDITIONAL. 로컬 결함 수정 이후에도 전수 검증 공백으로 최종 CONDITIONAL 유지.

## Phase 8 — 도메인사전

docs/domain-dictionary.md 읽음. DurableSession/ActiveRun, Workstream/Plan/Stage, HarnessProfile/CapabilityProfile 책임 구분을 확인했다. 금지 구문 검색 결과 `unsafe tool` 테스트 입력과 `concurrent-safe tools` 주석은 문맥상 위반 아님. 영문 식별자 30개와 모든 UI 한글의 전수 매핑은 UNVERIFIED; 전체 용어 준수율을 보고하지 않는다.

## 재개할 작업

1. live provider의 성능·품질 평가는 NOT RUN. 현재 증거는 deterministic provider와 실제 kernel/store/sandbox 실행 경로에 대한 회귀다.
2. ACP workflow lifecycle 지원은 후속 기능이다. 현재 결합 세션은 load/prompt 전에 거절하며 일반 ACP만 지원한다.
3. hosted OS matrix와 미검증 API·OS ACL 검증. Go advisory와 secret scanner의 후속 실행 결과는 아래 참조. 외부 배포 자체는 spec 제외 범위이므로 자동 실행하지 않는다.

A01–A12 로컬 결함 및 의존성 취약점을 수정했다. ACP workflow 지원 및 미실행 API·플랫폼·보안 검증을 완료로 표시하지 않는다.

## 추가 런타임 오류 수정 — 2026-09-20

앞선 전체 suite 성공 이후 별도 장애 주입으로 A09/A10을 재현했다. 기존 테스트 성공을 모든 실행 경로의 무결함으로 해석하지 않는다. 두 재현을 영속 테스트로 추가했다.

- MCP: 재연결의 도구 목록 조회에서 다시 세션 만료가 발생하면 한 번의 복구 시도를 실패로 끝낸다. 다른 재연결 소유자를 기다리는 요청도 취소할 수 있다. `TestMCPRemoteReconnectDiscoveryLossHonorsCancellation`, `TestMCPRemoteConcurrentReconnectWaitHonorsCancellation` 및 정상 reconnect 회귀 PASS. Linux Docker에서 `go test -race ./internal/agent -run TestMCPRemote -count=3` PASS.
- Web: SSE 끝의 미완성 이벤트는 성공 근거가 될 수 없다. `npm test` 5개 PASS, 실제 Chromium `TestBrowserChatReportsInterruptedStreamWithoutSavingSuccess` 3개 시나리오 PASS. 부분 응답 보존·오류 안내와 함께 저장 revision이 증가하지 않는지 검사한다. 기존 프로젝트 전환 테스트는 새 오류 안내가 이전 stream 때문에 나타나지 않는지도 확인한다.
- 지속 검증: CI web job에 `npm run test:sse`를 연결했다. Web lint/build, embedded bundle 재생성, actionlint PASS. 외부 hosted CI·live MCP 서버의 실제 장애는 미실행이며 로컬 HTTP fixture와 실제 브라우저 증거다.
- 최종 관련 패키지 검사: `go test ./internal/agent ./internal/acpbridge ./internal/server ./cmd/proxy -count=1` exit 0 (각 140.876s / 11.619s / 24.201s / 33.164s). `go build ./...`, 해당 네 패키지 `go vet`, `git diff --check` exit 0. 이 후속에서는 이전 전체 suite를 다시 실행했다고 주장하지 않는다.


## 보안 후속 수정 — 2026-09-20

- A11 / High / 수정: HTTP iframe의 최초 주소만 검사하여 같은 호스트의 redirect가 다른 호스트·포트의 로컬 fixture를 읽을 수 있었다. `tool_web_redirect_test.go`의 실제 HTTP 회귀가 수정 전 다른 호스트·포트·embedded credentials 세 경우에서 실패했다. iframe별 client의 CheckRedirect가 선택한 부모 문서 기준으로 매 요청 전에 scheme/effective port/host를 검사하고 credentials 및 10회 이상 redirect를 거절한다. 기존 Naver 하위 도메인 예외도 scheme/port를 유지해야 한다. 차단 대상은 내용 필터링뿐 아니라 요청 횟수 0으로 검증한다. 정상 nested frame 및 명시적 top-level redirect는 유지한다.
- A12 / Supply-chain / 수정: Go 1.26.1 + x/net v0.35.0에서 govulncheck v1.8.0이 reachable advisory 23건을 보고했다(실제 exploit 23건을 뜻하지 않음). go.mod 최소 버전을 Go 1.26.8로 올리고 x/net v0.56.0으로 갱신했다. v0.55.0은 호출하지 않는 DNS parser advisory가 남아 최종 채택하지 않았다. [공식 GO-2026-5942](https://pkg.go.dev/vuln/GO-2026-5942) 및 [GO-2026-5028](https://pkg.go.dev/vuln/GO-2026-5028) 참조.
- CI/release는 setup-go의 go-version-file로 go.mod를 읽고, Docker builder는 golang:1.26.8-alpine을 사용한다. CI build job에 고정 govulncheck v1.8.0을 연결했다. 기존에 배포된 바이너리는 자동으로 교체되지 않는다.
- 최종 의존성에서 `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` exit 0, `No vulnerabilities found.`; `go build ./...`, `go vet ./...`, `git diff --check` exit 0. CI/release actionlint v1.7.7 exit 0.
- 앞선 보안 감사에서 Gitleaks 공식 Docker image를 network none / repo readonly / redacted metadata-only 보고서로 실행했다. 현재 source 8.05MB와 Git 162 commits 16.87MB에서 각각 후보 12건(exit 1)을 보고했고 모두 테스트 파일에 위치했다. 운영 credential 유출로 확인한 항목은 없으나, 이를 모든 비밀정보의 부재 보장으로 간주하지 않는다. 현재 source의 node_modules, .git, web/dist는 제외했다.
- 한계: 이 수정은 HTTP 자동 iframe 경계다. 명시적 top-level 로컬 URL은 허용하며 Chromium 전체 network isolation, DNS rebinding 방어를 추가한 것은 아니다. hosted CI·Docker image 재빌드/배포·API 전수·OS ACL 검증은 이번 후속에서 NOT RUN. 전체 판정은 CONDITIONAL을 유지한다.

- 최종 x/net v0.56.0 기준 `TestWebFrameRedirectBoundary` 7개 HTTP 시나리오, `TestWebFrameOriginPolicy` 9개 경계 사례, `TestWebTopLevelRedirectStillWorks` 모두 exit 0. 최종 Go 1.26.8 / x/net v0.56.0에서 `go test ./... -count=1` 전체 exit 0 (agent 172.222s, server 40.976s, ACP bridge 23.259s, proxy 39.689s). 환경별 skip을 외부 OS 검증으로 환산하지 않는다.
