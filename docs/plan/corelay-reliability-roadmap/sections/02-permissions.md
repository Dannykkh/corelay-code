# S02 권한·명령 분류

상태: PASS
연결: F05,F06
선행: S01

## 수정 시작점과 소유 범위

internal/agent/permission.go,concurrency.go,tool_dispatch.go,run_options.go,sandbox_bridge.go; internal/approval; internal/sandbox; internal/hooks; internal/acpbridge; internal/kairos

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. read-only/workspace/full 및 runtime capability를 분리한 공통 정책 resolver를 만든다. 기존 허용 설정 migration을 명시한다.

- B. Bash/Git 병렬 분류와 승인 분류가 공통 command effect 정보를 사용하게 한다. shell compound/인용/리다이렉트/unknown은 보수적으로 판정한다.

- C. tool dispatcher부터 process supervisor/MCP/plugin/hook/team/daemon까지 실효 정책 snapshot을 전달한다. 재진입마다 글로벌 설정을 다시 읽지 않는다.

- D. 세 모드의 API/CLI 입력과 현재 모드 출력을 제공한다. full 선택 시 실제 외부 경로/명령 허용까지 검증한다. 관리자 권한 자동 상승은 하지 않는다.

## 완료 조건

Q03. find -delete, python 쓰기, git branch 생성, 리다이렉트는 read-only parallel로 판정하지 않는다. full의 외부 temp 파일 변경은 개별 승인 없이 성공, read-only는 차단. child가 부모보다 권한을 올릴 수 없다.

## 회귀·제약

Windows Job Object를 파일/네트워크 격리로 표시 금지. HTTP 인증, session binding, 데이터 충돌 검사를 full로 우회 금지.

## 검증 기록

- A: PASS. `internal/agent/execution_policy.go`에 read-only/workspace/full 모드, revision이 있는 run snapshot, 별도 runtime capability snapshot, 자식 모드 제한 resolver를 추가했다. 하위가 부모보다 넓은 모드를 요청하면 거부하며, 자식 executor capability는 부모 값에서 상속하지 않고 실제 자식 런타임 값으로 기록한다.
- A migration: 기존 `safe`/`moderate`/`all`/`none`은 승인 임계값이지 외부 접근 권한으로 간주하지 않는다. 기존 설치와 미설정 신규 실행은 workspace로 매핑한다. legacy threshold 자체는 resolver가 변형하지 않는다. 알 수 없는 threshold와 mode는 fail-closed 오류다.
- A tests: `gofmt -w internal/agent/execution_policy.go internal/agent/execution_policy_test.go` — exit 0; `$env:PATH = 'C:\Program Files\Git\bin;' + $env:PATH; go test ./internal/agent` — PASS, exit 0. 정책 default/migration, explicit mode, capability 분리, child 상속·축소·상향 거부를 검증했다.
- B: PASS. `internal/agent/command_effect.go`를 추가해 Bash/Git 효과를 하나의 read-only/mutating/destructive/unknown 결과로 분류하고 `ClassifyDanger`와 `IsConcurrencySafe`가 같은 결과를 사용한다. compound/quote/subshell/redirect/expansion과 unknown 명령은 unknown으로 승인 요구 및 serial 처리한다. `find -delete`, Python 쓰기, Bash·구조화 Git branch 생성, redirect를 회귀에 포함했다.
- B 검증: `$env:PATH = 'C:\Program Files\Git\bin;' + $env:PATH; go test ./internal/agent -run 'Test(CommandEffect|IsConcurrencySafe|PartitionToolCalls|ClassifyDangerGitInvocationTable|UnknownCommand)'` — PASS, exit 0; 전체 `go test ./internal/agent` — PASS, exit 0; `git diff --check` — PASS, exit 0.
- B 재현→수정: 첫 전체 패키지 실행에서 unknown 등급을 모든 미등록 tool에 적용해 기존 MCP/plugin dynamic-call 경로가 승인 요청자로 넘어가는 회귀가 드러났다. unknown은 Bash/Git effect에만 적용하고 기타 tool 기존 분류를 유지한 뒤 전체 패키지 재검증 PASS.
- C: PASS. RunLoop가 요청 정책을 한 번 resolve하고 immutable snapshot과 실제 선택 executor capability를 tools/hooks/MCP/plugin/computer-use/team/child 경계까지 전달한다. 자식은 부모 모드보다 넓어질 수 없다. Approval broker의 full-mode grant는 명시 선택 provenance, policy revision, call/session/run/executor/input digest/source/expiry에 묶이고 한 번만 소비된다. Chronos adapter/API와 ACP durable session도 같은 snapshot 계약을 사용한다. Windows Job Object는 filesystem/network 격리로 표시하지 않고 실행기 capability를 그대로 보고한다.
- C 보강: 직접 호출 가능한 Read/Glob/Grep/Write/Edit 경로도 canonical target에 정책 검사를 적용하고 기본 `.env`/`.ssh` 차단을 유지한다. full은 workspace 경계만 완화하며 경로 문법·symlink·민감 경로 방어는 보존한다.
- D: PASS. `/api/agent`, `/api/team/execute`, `/api/chronos`는 mode만 받고 revision은 서버가 발급한다. 응답/SSE/추적 메타데이터에 실제 mode와 revision을 반환하며, 잘못된 mode는 실행 전 거부한다. CLI Chat/Team/Worker/TUI는 `--mode read-only|workspace|full`을 지원하고 유효 모드를 보여준다. ACP는 workspace 기본값, 명시적 session mode 변경, durable 저장·재개를 지원한다. Full의 외부 임시 파일 쓰기 및 read-only 차단을 실행 테스트로 확인했다. Full은 OS credential을 승격하지 않는다.
- UI 범위: Web Chat/Team의 모드 선택 UI 연결은 S10 대상이며 여기서는 API·CLI 계약까지 완료했다.
- 검증 기준 commit: `82d033bf4490033d782c97566877fed02536799e`.
- 초기 영향 패키지 검사: `$env:PATH = 'C:\Program Files\Git\bin;' + $env:PATH; go test ./internal/agent ./internal/approval ./internal/hooks ./internal/server ./internal/acpbridge ./internal/acp ./cmd/proxy ./internal/sandbox ./internal/processsupervisor -count=1` — PASS, exit 0. `git diff --check` — PASS, exit 0.
- 전체 저장소 검증 중 ACP conformance fixture가 `session/set_mode`를 거부하는 오래된 계약을 기대했고, 해당 fixture와 성공·저장·재개 검증을 갱신했다. 이 과정에서 ACP approval requester가 executor/policy provenance를 전달하지 않는 결함도 재현·수정했다.
- 최종 검토 보강: path-qualified `./cat`이 read-only로 오인되는 분류를 수정했다. Windows Job Object처럼 filesystem isolation이 없는 runner에서는 Workspace Bash/Git에 per-command approval을 요구하고, MCP 프로세스 시작을 막도록 했다. 테스트용 격리 runner는 실제 fixture capability를 명시한다.
- 잔여 검토 위험: versioned session JSON의 full-selection provenance는 구조 검증이며 저장소 파일에 대한 same-user tampering을 암호학적으로 인증하지 않는다. 제품 위협 모델은 같은 OS 사용자 악성 프로세스 격리를 주장하지 않으며, per-user session storage ACL을 전제로 한다. 공유/원격 저장소를 도입하면 별도 무결성 설계가 필요하다.
- 최종 전체 저장소 검증: `$env:PATH = 'C:\Program Files\Git\bin;' + $env:PATH; go test ./... -count=1` — PASS, exit 0 (2026-09-13). 첫 전체 실행에서 드러난 ACP fixture, approval provenance, 격리 능력을 빠뜨린 테스트 runner 문제를 수정한 뒤 재실행했다.
- 최종 정적 검사: `git diff --check` — PASS, exit 0.
