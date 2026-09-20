# S03 프로젝트·세션

상태: PASS
연결: F07,F08,F09,F10
선행: S01,S02

## 수정 시작점과 소유 범위

internal/agent/sessions.go; internal/server/agent_session.go,server.go; internal/agent/context.go; cmd/proxy/chat.go; web/src/pages/Chat.tsx,Workspace.tsx

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] Session project/workstream/policy/model 목표 필드와 migration을 contracts.md대로 추가한다.

- B. [PASS] durable request workspace는 세션에서 resolve하고 명시 불일치 거절. workstream 소속은 세션 연결 변경과 durable 실행 경계에서 검증하고, 실행 모델도 session target을 사용한다. 기존 registry barrier와 revision 검사를 보존한다.

- C. [PASS] 프로젝트 전환은 클라이언트 선택 상태 변경으로 제한한다. 새 세션 default와 기존 세션 snapshot을 구분한다.

- D. [PASS] ancestor→workspace→파일 하위 AGENTS 적용 순서를 구현했다. User-level `~/.claude/CLAUDE.md` defaults 다음에 filesystem ancestor→workspace 지침을 합성하고, path-aware built-in tools는 선택 workspace 안의 target scope 지침을 반환해 모델이 확인하기 전 실행되지 않게 한다. 같은 디렉터리는 `CLAUDE.md` 후 `AGENTS.md` 순서로 제시하고, 직접 충돌은 `AGENTS.md` 우선임을 본문 뒤의 effective precedence 요약과 계약 문구에 기록했다. sibling·무관 descendant는 초기/좁은 경로 범위에서 제외한다.

  `Read`, `Write`, `Edit`, `NotebookRead`, `ImageRead`, `PDFRead`, `Diff`, `GitDiff`, `GitCommit`, `LS`, `Glob`, `Grep`, `RepoMap`에 경로별 disclosure gate를 연결했다. 같은 dispatcher batch에서 새 scope를 먼저 만난 tool 호출은 실행 대신 지침을 반환하고, 재시도 batch에서만 실행한다. 재귀 탐색은 64 instruction files / 128 KiB / 20,000 entries 제한 안에서 완료해야 하며 초과·탐색 실패 시 fail-closed한다. RepoMap만 숨김 디렉터리를 건너뛰며, Glob/Grep/GitDiff는 숨김 scope를 포함한다. Grep brace/exclusion glob은 공통 검색 범위까지 제한 스캔한다. GitDiff와 GitCommit의 경로는 literal path만 허용하고 glob·Git pathspec magic은 공통 resolver에서 거절한다. `GitCommit scope=staged`는 전체 workspace 지침 scope를 확인하며, workspace가 Git 저장소 루트와 일치할 때만 허용한다.

  회귀 근거: ancestor/workspace/nested 순서, CLAUDE/AGENTS 우선순위, 상대 경로·symlink·Windows case, direct read/write 및 batch 재시도, Notebook/PDF/LS/Glob/Grep/RepoMap/GitDiff/GitCommit scopes, hidden/brace/exclusion globs, external Full-mode 경로, 탐색 한도 fail-closed를 검증했다. Q04 증거도 same-basename 프로젝트 격리, SessionStore symlink alias, 서로 다른 persisted model의 동시 세션, 다른 workspace 세션의 approval ID 거절, legacy fixture·실패 저장 시 bytes 보존 테스트로 보완했다. `go test ./internal/agent ./internal/server -run 'Test(LoadProjectContext|NestedProject|ProjectInstruction|ScopedProjectInstructions|RecursiveProjectInstruction|GitDiff|SessionStoreIsolatesRealWorkspacesWithSameBasename|SessionStoreTreatsDirectorySymlinkAsCanonicalWorkspace|SessionStorePreservesOriginalWhenAtomicWriteFails|ConcurrentDurableSessionsUseTheirPersistedModels|ApprovalCannotBeResolvedFromAnotherWorkspaceSession)' -count=1` exit 0; `go test ./... -count=1` exit 0; 독립 검토 차단 사항 없음; `git diff --check` exit 0.

  범위 제한: nested scope gate는 명시적 경로를 제공하는 built-in file/search/diff tools에 적용한다. 임의 Bash 명령 및 경로를 제공하지 않는 opaque MCP/plugin 호출은 하위 instruction scope를 신뢰성 있게 추론할 수 없어 이 gate의 적용 대상이 아니다. 이를 보안 경계라고 주장하지 않으며, workspace/ancestor prompt context는 유지한다.

## 완료 조건

Q04. 같은 basename 프로젝트, Windows case/relative/symlink, 다른 모델의 동시 세션, 다른 프로젝트 approval id 재사용 거절. 예전 session fixture가 읽히고 쓰기 실패 시 보존된다.

## 회귀·제약

글로벌 provider lock만 풀고 공유 객체 race를 만들지 않는다. 세션 모델 미지원이면 조용히 다른 모델 사용 금지.

## 검증 기록

- A PASS: Session에 workstream/plan/stage 연결 필드를 추가하고 스키마 v2를 도입했다. v0/v1 읽기를 유지하고, 최초 유효 저장에서 v2로 원자 migration하며 실패 시 원본 bytes를 보존한다. v1의 execution policy·lifecycle·provider/model과 연결 필드의 부분 갱신 보존, fork 복사, stage 단독 지정 거부를 검증했다. `go test ./... -count=1` exit 0; `npm run build` exit 0; `git diff --check` exit 0.
- B PASS: durable 요청은 세션 workspace를 기본값으로 사용하고 명시 workspace 불일치는 `session_workspace_conflict`로 거부한다. 저장된 provider/model pair를 그대로 해석하고, 양쪽이 비어 있는 legacy 세션만 활성 기본값을 사용하며 불완전·알 수 없는 target은 거부한다. Workstream 연결은 세션 저장 시 같은 workspace 소속을 검사하고, durable 실행은 저장 연결을 기본 사용하며 다른 연결을 거부한다. 활성 durable run 중 세션 변경을 막고, WorkstreamStore의 경로 비교를 SessionStore와 같은 symlink/case 정규화 규칙으로 맞췄다. `go test ./... -count=1` exit 0; `git diff --check` exit 0.
- C PASS: App과 SidePanel의 프로젝트 전환을 브라우저 선택 상태로 제한하고 localStorage에 보존했다. 프로젝트 등록은 서버 전역 `WorkDir`를 바꾸지 않는다. 선택 workspace를 파일 트리·파일 읽기/쓰기·세션 목록·workstream 조회/생성에 전달한다. 새 durable 세션은 첫 저장에서 선택 workspace를 snapshot으로 기록하고, 기존 세션은 저장 workspace와 workstream 연결을 유지해 다음 실행도 그 snapshot을 사용한다. 파일 API는 서버 기본 workspace와 등록 프로젝트만 요청 root로 허용하고, symlink를 해석한 뒤 상대 경로가 root 안에 있는지 검사한다. 빈 초기 workspace에서 전체 세션을 조회하지 않으며, 빠른 프로젝트 전환 중 늦게 도착한 이전 응답은 폐기한다. 내구 세션 생성도 미등록 workspace를 거부한다. 변경 파일: `internal/server/server.go`, `internal/server/workspace_scope.go`, `internal/server/project_workspace_scope_test.go`, `internal/server/webdist/index.html`, `internal/server/webdist/assets/index-BvMZQMhS.js`, `internal/server/webdist/assets/index-ClIeEByk.css`, `web/src/App.tsx`, `web/src/components/SidePanel.tsx`, `web/src/lib/workstreams.ts`, `web/src/lib/workspace.ts`, `web/src/pages/Chat.tsx`. `go test ./internal/server -count=1` exit 0; `go test ./... -count=1` exit 0 (통합 suite); `npm run build` exit 0; `npm run lint` exit 0; `git diff --check` exit 0. React 상호작용 테스트 하네스가 없어 UI 동작은 API 회귀 테스트와 build/lint로 검증했다.
- 범위 결정: StageID는 PlanID를 요구하도록 구조 검증한다. Workstream의 workspace 소속은 S03.B에서 연결 변경 및 durable request 경계에 검사한다. Plan/Stage 존재·소속 검증은 해당 저장소와 workflow resolver가 마련되는 단계에서 수행한다. 현재 빈 연결 필드는 “미지정”으로 처리해 기존 값을 보존하며 명시적 연결 해제는 아직 지원하지 않는다.
- S03.B 경로 주의는 `internal/workstream/store.go`의 symlink/case 정규화와 회귀 테스트로 해소했다.
- 다음: S04.A에서 staged B와 지정 A 커밋의 선택 경로 보존과 전체 staged scope 제한을 이어서 구현·검증한다.
