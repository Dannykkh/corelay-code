# S13 통합 평가·릴리즈 준비

상태: IN_PROGRESS (Argos 2026-09-20: A 로컬 acceptance PASS, B/C/D 구현 PASS, E 문서 보완; hosted OS verification NOT RUN)
연결: F30,F31
선행: S01~S12

## 수정 시작점과 소유 범위

cmd/corelaycode-profile; internal/observability/regression.go; internal/server regression runner; .github/workflows/ci.yml,release.yml; scripts/build-release.sh; README.md/docs

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] 여섯 과제에 실제 acceptance를 연결했다. coding은 snapshot 일치와 필수 격리 내 Go assertion 실행, lifecycle은 compaction 뒤 결정 복원·durable A/B/A 선택·Write 후 취소/재조정/재개를 검증한다. deterministic provider 회귀의 PASS이며 live 모델 품질 수치를 의미하지 않는다.

- B. [PASS] Q01~Q16과 migration/OS matrix를 CI job으로 연결했다. Windows/Linux 필수, 배포 지원 macOS smoke를 같은 entrypoint로 기록한다. 로컬 Windows Git Bash와 workflow 정적 검증은 통과했지만 hosted Linux/macOS/Windows runner 실행은 외부 게이트로 남아 있다.

- C. [PASS] build-time version/commit을 CLI/server/ACP에 일관 주입하고 checksums/패키지 스모크를 준비한다.

- D. [PASS] update는 현재 배포 채널을 먼저 확인하고 명시 사용자 명령, artifact 무결성, Windows 실행 중 exe 교체/rollback을 설계·테스트한다. 자동 background update 기본 비활성.

- E. [PASS] README/agent-operating-model/web-fetch/API/계획 상태를 최종 실제 동작에 맞게 수정한다. G1/G2/G3 결과와 known limitations를 정리한다.

## 완료 조건

전체 로드맵 PASS는 섹션별 증거, 필수 검사, Q01~Q16 결과가 있을 때만. live 검증 미실행/지원불가 capability는 명시. 배포 자체는 수행하지 않음.

## 회귀·제약

실패한 테스트를 skip으로 바꾸어 성공률을 올리지 않는다. 비용/성공률 측정 없이 성능 향상 수치를 만들지 않는다.

## 검증 기록

### Argos 후속 수정 — 2026-09-20

- A04: production executor가 coding fixture snapshot 일치 후 `probe_verification.go`의 실제 `go test -json` 결과까지 검사한다. 전용 임시 workspace, 필수 filesystem/network/process 격리, network deny를 사용하며 격리 불가·테스트 실패·취소·pass assertion 부재는 성공으로 기록하지 않는다.
- 검증: Windows profiler 전체 회귀 PASS. privileged `golang:1.25-bookworm` + bubblewrap 실제 격리에서 실패 assertion 거절과 정상 assertion 통과를 모두 확인했다. runner 주입으로 host fallback 거절, 취소, 도구 부재도 검증했다.
- 평가 계약 변경을 구분하기 위해 default probe plan을 v4로 변경했다. 이전 snapshot-only v3 결과와 동일 plan으로 비교하지 않는다.
- A05: production compactor 뒤 정답 없는 후속 질문에서 최신 결정 JSON을 검사한다. 별도 durable session A/B/A를 매회 reopen하고 실제 Read·정확한 답·다른 transcript 불변을 검사한다. 실제 Write 후 취소→journal/checkpoint 확인→store reopen→미조정 저장 거절→receipt reconcile→Read 재개를 실행하며 재실행을 차단한다.
- 정상/오답 category별 각 3회, 복구는 marker-only·잘못된 결과·Write 재시도 각 3회 PASS. 전체 lifecycle latency를 집계하며 원문은 저장하지 않고 digest evidence만 기록한다. store reopen은 별도 프로세스 강제 종료와 구분한다.
- A08: 이 검증 중 압축이 마지막 user 요청을 생략하는 실제 결함을 발견해 수정했다. 두 번 압축해도 마지막 위치에 한 번 유지하는 회귀와 compaction 관련 전체 테스트 PASS.

### Argos 재판정 — 2026-09-20

이하 이전 PASS는 당시 검사 범위의 역사적 기록이다. 최신 전체 판정은 `../verify-report.md`를 따른다.
감리 당시 executor는 read/write 횟수와 expected 파일 digest만 판정하며 테스트 실행, compaction, 실제 session/project switch, interruption/restart를 검증하지 않았다. 전체 목표 완료라는 당시 보고를 철회했다. 후속 수정은 위 기록을 따른다.

- 수정: CI gopls 설치 step에 현재 shell PATH export; Go 과제 3종에 실제 testing.T assertion과 authored fixture 실패→성공 회귀 추가.
- 당시 재개 조건(후속 구현 완료): 기존 supervisor/sandbox 경계 내 테스트 실행 evidence를 acceptance에 연결한다. 생성 코드의 무격리 실행은 금지한다.
- 당시 재개 조건(후속 구현 완료): 장문 결정 변경→compaction→후속 요청, 서로 다른 durable project/session 전환, 중단→재시작→reconcile을 실제 production 경로로 검증하고 최소 3회 반복 증거를 남긴다.
- 외부 배포는 spec의 제외 범위다. hosted OS matrix 미실행은 별도로 남기며 로컬 fixture로 대체 완료 처리하지 않는다.

### A — 2026-09-20

- PASS: `internal/capabilityprofile/plan.go`의 기본 plan v3에 여섯 validation category를 추가하고, `cmd/corelaycode-profile/fixtures.go`가 각 과제의 격리 multi-file fixture·승인된 mutation·최종 snapshot acceptance를 만든다. `agent_executor.go`는 marker만이 아니라 읽기 수, mutation 수, artifact snapshot을 함께 판정한다.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/capabilityprofile ./cmd/corelaycode-profile -count=1` (exit 0). `TestValidationFixtureCatalogHasExecutableAcceptanceArtifacts`가 여섯 과제의 fixture 수, workspace containment, expected snapshot digest를 확인했고 기존 profiler/CLI 테스트도 통과했다.
- NOT RUN / KNOWN: 실제 유료/외부 provider를 사용한 validation run은 수행하지 않았다. 중단 복구 fixture는 재시작 가능한 checkpoint 상태를 검사하는 deterministic acceptance이며, 강제 프로세스 종료와 OS matrix는 S13.B에서 다룬다.

### B — 2026-09-20

- PASS: `.github/workflows/ci.yml`의 기존 Windows/Linux/macOS matrix에 고정 `gopls v0.23.0` 설치와 `scripts/roadmap-qa.sh`를 연결했다. 이 entrypoint는 Q01~Q16 설명에 대응하는 전체 Go suite, 이름을 지정한 migration 회귀, sandbox/process 반복, vet를 같은 OS job에서 실행한다. Linux sandbox 설치와 별도 Ubuntu race/web job은 유지한다.
- PASS: Windows host의 Git Bash에서 `$env:GOTOOLCHAIN='go1.26.1'; bash scripts/roadmap-qa.sh`를 재실행해 exit 0으로 완료했다. entrypoint가 OS/Go/gopls 상태를 기록하고, 전체 `go test ./... -count=1`, 이름을 지정한 `Migration|Legacy` 회귀, sandbox/process 반복(`-count=3`), `go vet ./...`, 영속 `build-release-test.sh` fixture를 통과했다. Chronos full-mode 정책 fixture는 전체 정책 시스템 프롬프트가 16K context를 초과하던 테스트 harness를 32K로 조정한 뒤 실제 Write dispatch와 무승인 실행을 확인한다.
- PASS / RECHECK: 실제 Git Bash 실행 파일(`C:\Program Files\Git\bin\bash.exe`)로 roadmap QA를 재실행해 exit 0을 확인했다. 반복 실행에서 드러난 `TestWindowsJobRunnerKillsDescendantTreeOnTimeout`의 1.5초 setup deadline을 10초로 늘려 Job Object 준비 지연과 실제 descendant termination을 분리했고, 해당 테스트 `-count=3` 및 전체 entrypoint가 통과했다. WSL `bash`의 Go PATH 부재는 별도 환경 제한으로 남긴다.
- PASS / Q15: 제품 PATH를 바꾸지 않는 임시 `GOBIN`에 `gopls v0.23.0`을 설치하고 `go test ./internal/agent -run '^TestInstalledGoplsSemanticFixture$' -count=1 -v`를 실행해 exit 0(5.952s)을 확인했다. fixture 종료 후 임시 executable은 제거했다.
- PASS / BROWSER REGRESSION: 전체 suite에서 드러난 Rod 프로젝트 전환 race를 현재 선택과 대상 선택을 분리한 SidePanel selector 및 메뉴 행 렌더 대기로 보정했다. `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/server -run '^TestS10BrowserUIProjectFilesAndStaleStreams$' -count=5 -v`가 5회 모두 exit 0으로 통과했고, 이후 전체 Git Bash roadmap QA도 exit 0이다.
- PASS / LINUX RACE: privileged `golang:1.25-bookworm` container에 bubblewrap 0.8.0을 설치한 뒤 `go test -race ./internal/approval ./internal/agent ./internal/acpbridge ./internal/server -count=1`을 실행해 네 패키지 모두 exit 0을 확인했다. 기본 Docker seccomp 실행은 sandbox isolation capability 부족으로 fail-closed했으며, CI의 bubblewrap 설치 조건과 분리해 기록한다.
- PASS / LINUX ROADMAP QA: privileged `golang:1.25-bookworm` container에서 `GOTOOLCHAIN=auto`, `gopls v0.23.0`, bubblewrap 0.8.0을 준비하고 `bash scripts/roadmap-qa.sh`를 실행해 전체 Go suite, Q01~Q16 연결 회귀, sandbox/process 반복, vet가 exit 0으로 완료됐다. Go 1.25.14가 gopls 설치 중 Go 1.26.8 toolchain을 자동 선택하도록 CI 설치 단계도 고정했다. bubblewrap이 `/tmp`를 격리하면서 임시 MCP 실행 파일을 숨기던 경로를 읽기 전용 staging mount로 보완했고 ACP stdio MCP conformance가 같은 Linux 조건에서 통과했다. 이 결과는 privileged local container 증거이며 hosted runner 결과를 대신하지 않는다.
- PASS: `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 .github/workflows/ci.yml .github/workflows/release.yml` (exit 0), `bash -n` for all roadmap/release scripts (exit 0), Docker ShellCheck for the three scripts (exit 0), and Python YAML parse (exit 0) validated workflow syntax and expressions.
- NOT RUN / KNOWN: WSL system bash는 Windows Go PATH를 전달하지 않아 `go: command not found`(exit 127)였고 Git Bash 경로로 재실행해 통과시켰다. 해당 Git Bash 환경에는 `gopls`가 없어 Q15 installed-server test는 skip됐지만, 별도 임시 fixture는 위 Q15 기록대로 통과했다. 외부 CI runner의 Linux/macOS/Windows matrix는 이 세션에서 실행하지 않았다.

### C — 2026-09-20

- PASS: 공통 `internal/buildinfo`의 Version/Commit을 proxy server, ACP bridge, capability profiler가 사용하도록 연결했다. `Makefile`, `Dockerfile`, `scripts/build-release.sh`, `.github/workflows/release.yml`의 세 binary 빌드 모두 같은 `-ldflags`를 사용하고, release job은 5개 OS/architecture 조합의 세 binary(최소 15개)와 `checksums.txt`를 package smoke로 확인한다.
- PASS / RELEASE PACKAGE GUARD: release workflow와 `scripts/build-release.sh`가 지원 대상 15개 파일명을 각각 확인하고, 정확히 15개 artifact와 15줄 checksum만 생성하도록 강화했다. release script는 `set -euo pipefail`과 `CGO_ENABLED=0`을 사용해 frontend/cross-build 실패와 host cgo 차이를 조기에 드러낸다. 영속 `scripts/build-release-test.sh`를 `roadmap-qa.sh`에 연결해 Windows Git Bash와 Linux Docker에서 15개 artifact/checksum 성공, stale artifact 거부, frontend 실패 전파를 exit 0으로 확인했다. artifact 수집은 GNU 전용 `find -maxdepth` 없이 Bash glob을 사용해 macOS runner에서도 실행되도록 했다. fixture는 orchestration만 검증하며 실제 binary build 증거는 별도 package smoke다.
- PASS / PACKAGE SMOKE: 저장소를 변경하지 않는 임시 fixture 디렉터리에서 `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64` 각각 proxy/ACP/profile 3개를 `CGO_ENABLED=0`으로 빌드해 15개 binary와 15줄 `checksums.txt`를 생성했다. exit 0 후 산출물은 제거했다.
- PASS: `corelaycode-profile version`도 설정·provider·server 접속 없이 CLI version, commit, Go runtime을 출력한다. `$env:GOTOOLCHAIN='go1.26.1'; go test ./cmd/corelaycode-profile ./cmd/corelaycode-acp ./cmd/proxy ./internal/server -run 'Version|Doctor|Root|Capability' -count=1` (exit 0), `go test ./cmd/corelaycode-acp -count=1` (exit 0; advertised streamable HTTP fixture pinned to the implemented contract), `go vet ./cmd/corelaycode-profile ./cmd/corelaycode-acp ./cmd/proxy ./internal/server` (exit 0), `git diff --check` (exit 0)를 통과했다. linker smoke build로 profiler의 `v-test`/`abc123` 출력도 확인했다.
- PASS / CONTAINER SMOKE: Docker 29.5.3에서 `docker build --no-cache --build-arg CORELAY_VERSION=v-fixture --build-arg CORELAY_BUILD_COMMIT=local-fixture -t corelaycode:roadmap-smoke .`가 frontend build와 세 Go binary build를 포함해 exit 0으로 완료했다. `.dockerignore` 적용 후 context는 34.70KB였고, `docker run --rm corelaycode:roadmap-smoke version`이 `v-fixture`/`local-fixture` metadata를 출력했다. 임시 image tag는 확인 후 제거했다.
- NOT RUN: 실제 tag release와 GitHub artifact download/업로드, registry Docker publish는 수행하지 않았다. 외부 release channel 결과는 workflow 실행 후 별도로 기록해야 한다.

### D — 2026-09-20

- PASS: 자동 background update나 원격 release discovery는 추가하지 않았다. 명시적 `corelaycode update` 명령이 사용자가 제공한 로컬 artifact와 SHA-256만 검증하고, staged copy → target backup → atomic rename → 설치 후 digest 확인 순서로 교체한다. 이전 실행 파일은 `.previous` rollback copy로 보존하며 `corelaycode update -rollback`으로 명시적으로 복구할 수 있다.
- PASS: `internal/updater`는 regular non-symlink artifact, 정확한 64자리 SHA-256, 기존 backup 충돌, target/artifact path alias를 거부하고, 설치·rollback·tamper 회귀를 검증한다. `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/updater -count=1` (exit 0), `go test ./cmd/proxy -run '^TestRunUpdate' -count=1` (exit 0), `go vet ./internal/updater ./cmd/proxy` (exit 0), `git diff --check` (exit 0).
- PASS / PLATFORM: Windows 공유 삭제 금지 핸들을 실제 target에 열어 `TestInstallFailsClosedWhenWindowsTargetDeleteIsDenied`가 `ErrTargetInUse`, 기존 target 보존, backup 미생성을 확인했다. 현재 실행 파일을 대상으로 한 명시 update/rollback은 one-shot helper가 부모 종료를 기다린 뒤 교체하도록 구현했고, `TestWindowsSelfUpdateHelperReplacesAndRollsBackRunningExecutable`가 실제 `.exe` digest 교체와 rollback을 통과했다. 비-self target이 사용 중이면 기존 fail-closed 경계를 유지한다.
- NOT RUN / KNOWN: release channel metadata 서명, 원격 다운로드/TLS pinning, 실제 설치 서비스 중단·재시작은 이 로컬 범위에 없다. E에서 README와 known limitations에 이 수동 artifact 계약을 반영했다.

### E — 2026-09-20

- PASS: README의 native API/session/workstream contract, WebFetch tool-call semantics와 Rod limits, MCP/LSP boundaries, profiler version/validation fixtures, explicit update command를 실제 구현에 맞춰 동기화했다. `docs/agent-operating-model.md`에는 project/session epoch, run-owned MCP, semantic LSP fallback, update fail-closed 경계를 추가했다.
- PASS: G1/G2/G3 결과를 `review.md`에 기록했다. 승인·journal·session 경계와 CLI/Web 회귀는 부분 검토를 통과했으며, capability/release/update focused tests도 통과했다. 각 게이트의 hosted CI, interactive terminal, live provider, release publish 미실행은 명시적으로 남겼다.
- NOT RUN / KNOWN: 외부 CI의 Q01~Q16 OS matrix가 통과하기 전 S13 전체와 로드맵 전체를 PASS로 올리지 않는다. external release/channel, live provider/browser, interactive flow 검증 부재가 릴리즈 게이트의 잔여 조건이다.
