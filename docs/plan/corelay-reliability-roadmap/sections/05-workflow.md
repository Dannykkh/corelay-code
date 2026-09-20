# S05 작업·단계·계획

상태: PASS
연결: F09,F14,F15
선행: S03,S04

## 수정 시작점과 소유 범위

internal/workstream; internal/agent/team_plan.go,team.go,plan.go,planmode.go,plan_anchor.go; internal/server/server.go; internal/agent/sessions.go

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] 기존 Workstream/TeamPlan/Plan의 production caller를 추적하고 단일 소유 상태를 결정했다. Workstream이 workspace별 목표·진행의 canonical owner이며, revisioned Plan/Stage는 그 하위 객체가 된다. TeamPlan은 실행 adapter/value로 유지하고, global `activePlan`, 미연결 `PlanModeManager`, 대화별 `/plan` directive는 새 owner를 우회하지 않도록 호환 연결 또는 명시적 폐기 대상으로 둔다. 새 병렬 계획 엔진을 만들지 않는다.

- B. Session→Workstream→Plan revision/stage 참조와 소속 검사를 구현한다. global activePlan 경로는 연결 검증 후 제거 또는 compatibility adapter로 바꾼다.

- C. [PASS] 단계 transition, approval revision, acceptance evidence를 저장한다. 일반 chat/Team/daemon이 같은 상태를 사용한다.

- D. [PASS] resume/handoff에서 목표·결정·현재 단계·미완료·검증을 복원한다. Workstream context 2000 byte cap에서도 핵심 참조를 보존한다.

## 완료 조건

Q07. 두 세션이 다른 계획을 승인해도 섞이지 않는다. 수정된 plan은 이전 approval로 실행 불가. failed verification이 completed로 표시되지 않는다.

## 회귀·제약

기존 TeamPlan file scope/capacity/dependency 검사를 제거 금지. plan mode는 full에서도 mutation 금지.

## 검증 기록

각 하위 단계의 조사·수정·검사·exit code·미실행 이유를 여기에 추가한다.

### A — PASS

- 기준 commit: `82d033bf4490033d782c97566877fed02536799e`.
- 조사 범위: Workstream schema/store/API 및 Agent·Team·Chronos·KAIROS caller; TeamPlan schema/validation·CLI/API/worker/receipt caller; Session PlanID/StageID validation; global `activePlan`, `PlanModeManager`, `/plan` directive, PlanAnchor usage.
- 확인: Workstream은 workspace-local `.corelay/workstreams`의 JSON/timeline을 소유하고 API CRUD/handoff, durable Agent/Team/Chronos, KAIROS가 사용한다. Session은 `WorkstreamID/PlanID/StageID`만 보유하며 현재는 stage→plan 형식 조건과 server-side Workstream 소속 확인만 있다. Workstream에 하위 Plan/Stage store는 없다.
- 확인: TeamPlan은 CLI/API/worker가 검증·실행하는 execution value이며 task file scope/capacity/dependency/stage 검사를 갖지만 Session/Workstream revision·approval에 binding되지 않는다. global `activePlan`은 `/api/plan` 조회/승인 경로를 제외하면 생성 경로가 없고 mutex/workspace/session scope가 없다. `PlanModeManager`는 production caller가 없고 저장 오류 및 후속 상태 저장도 처리하지 않는다. `/plan` directive는 현재 turn의 read-only tool allowlist/prompt만 설정하며 plan approval/progress에 연결되지 않는다. Chat의 “승인 & 실행”은 durable approval API가 아닌 다음 turn의 일반 실행 요청이다. PlanAnchor는 실행 prompt projection으로 유지한다.
- 소유 결정: Workstream 아래 revisioned Plan/Stage를 하나만 두고 Session은 workspace와 Workstream 소속까지 확인한 ID를 참조한다. TeamPlan은 그 state로부터 검증된 실행 adapter로 유지한다. global/미연결 계획 경로는 후속 B에서 adapter로 연결하거나 폐기하여 durable owner 밖에서 승인·상태를 만들지 않는다.
- `rg` caller/schema 검색 및 관련 source read: PASS (exit 0). 기존 회귀: Workstream store/API/Agent/Team/Chronos/KAIROS 테스트, TeamPlan file-scope/capacity/dependency 테스트, PlanAnchor 테스트의 존재를 확인.
- 동작 변경 없음. 새 동작 테스트: NOT RUN (분석·소유 결정 단계이며 B에서 persistence/association 경로 구현 후 회귀 검증 예정).
- 다음: B — revisioned Plan/Stage owner를 만들고 Session→Workstream→Plan 참조를 같은 workspace와 revision으로 검증한다.

### B — PASS

- 구현: Workstream별 `plans/{planId}.json`에 revisioned Plan definition과 Stage lifecycle 상태를 저장한다. Session/SessionSummary에 `planRevision`을 추가하고 workspace → Workstream → Plan revision → Stage chain을 세션 저장과 durable Agent 실행 전에 검사한다. 부분 업데이트는 기존 연결을 보존하고 Plan/Workstream 변경 시 이전 revision·stage의 자동 재사용을 막는다. 기존 global `activePlan` 및 Tool 정의를 제거하고 `/api/plan` 조회는 scope를 요구하도록 compatibility route로 바꿨다. unscoped approval route는 retired 응답을 준다.
- 프로젝트 경로는 plan API가 저장소에 닿기 전에 server default/등록 프로젝트인지 확인한다. 저장 데이터 손상은 `plan_corrupt` recovery 상태로 분리한다. TeamPlan의 기존 검증 경로를 새 저장 API에서도 재사용한다.
- 주요 변경 파일: `internal/workstream/plans.go`, `internal/workstream/paths.go`, `internal/server/workstream_plans_api.go`, `internal/server/session_workflow_binding.go`, `internal/server/workspace_scope.go`, `internal/server/server.go`, `internal/agent/sessions.go`, `internal/agent/plan.go`, `internal/agent/planmode.go`, `web/src/lib/sessions.ts` 및 회귀 테스트.
- 검증 중 수정: JSON atomic write가 definition formatting을 정규화하므로 store test는 의미상 JSON을 비교한다. Cross-workstream miss는 `ErrNotFound` wrapped error로 확인하도록 바꿨다. 새 workspace allowlist 순서에 맞게 S03의 기존 cross-workspace expectation을 403으로 수정했다.
- 잔여: C에서 approval revision과 stage transition/evidence를 저장하고 Team/chat/daemon caller를 공통 상태로 연결한다. `/plan`의 현재 UI는 대화 중 read-only 제안 흐름이므로 durable 승인처럼 표시하지 않도록 C/S10 연동에서 정리한다.
- `go test ./internal/workstream ./internal/server -count=1`: PASS (exit 0).
- `go test ./internal/agent -count=1`: PASS (exit 0).
- `go build ./...`: PASS (exit 0).
- `go vet ./internal/workstream ./internal/server ./internal/agent`: PASS (exit 0).
- `npm run build`: PASS (exit 0; 기존 번들 크기 안내만 출력). `npm run lint`: PASS (exit 0). `gofmt -d` 대상 Go 파일: PASS (차이 없음). `git diff --check`: PASS (exit 0).
- 독립 검토: PASS. workspace scope, same-ID cross-workstream 재결합, 손상 저장 오류, stale durable 실행 전 차단을 확인했다.
- 전체 `go test ./...`: NOT RUN (S05 C/D 통합 후 계획된 전체 suite에서 실행). durable Plan approval/UI 동작과 같은 Workstream에서 Plan을 명시적으로 해제하는 기능은 C의 소유 범위다.
- 다음: C — plan definition revision에 결합한 승인, CAS stage transition, acceptance evidence를 저장하고 caller/UI를 연결한다.

### C — PASS

- 구현: Workstream Plan에 CAS 기반 draft 수정·revision별 승인·stage 실행 attempt·완료 acceptance evidence를 저장한다. 정의 수정은 revision을 올리고 승인을 폐기하며, 실행 attempt가 생긴 정의는 수정할 수 없다. completed 전이는 서버가 실제로 저장한 정확한 Agent/Team receipt, 동일 revision·stage·run binding, 완료된 acceptance contract 및 통과 verification을 확인한 뒤에만 기록한다. 실행 중단으로 남은 `running` 상태는 자동 완료하거나 초기화하지 않는다.
- 연결: durable `/api/agent`, `/api/team`, `/api/chronos`는 저장된 Workstream Plan의 현재 승인 revision 및 실행 가능 stage만 사용하며, 요청의 Team task/verify 명령 override를 받지 않는다. KAIROS는 명시적으로 연결된 Plan의 canonical stage를 read-only observer 문맥으로만 제공하고 Plan 진행·검증 증거를 변경하지 않는다. Chat은 Workstream별 Plan 조회·승인·stage 표시, 세션 binding 저장/복원, 실행 전 live preflight를 제공하며 일반 `/plan` 제안은 저장 승인이 아님을 표시한다.
- 주요 변경: `internal/workstream/plans.go`, `internal/server/workstream_plans_api.go`, `internal/server/session_workflow_binding.go`, `internal/server/workstream_plan_execution.go`, `internal/server/server.go`, `internal/agent/run_options.go`, `internal/agent/receipt.go`, `internal/agent/loop.go`, `internal/agent/chronos.go`, `internal/agent/chronos_adapter.go`, `internal/kairos/daemon.go`, `web/src/lib/workstreams.ts`, `web/src/pages/Chat.tsx` 및 관련 테스트.
- 회귀: Workstream store transition/CAS/evidence tests, API approval/update stale revision test, Agent 미승인 provider 차단 및 receipt 없는 완료 방지, Team 정확한 Plan/stage+통과 verifier receipt 완료, Chronos 미승인 provider 차단, KAIROS observer/no-mutation/stale rejection tests를 추가했다. Plan-bound KAIROS `git-watch` 내장 작업은 observer 계약을 우회하지 못하도록 provider/실행 전에 거부한다.
- `go test ./internal/workstream ./internal/agent ./internal/server ./internal/kairos -count=1`: PASS (exit 0; Agent 패키지 114.5s, Server 5.7s 포함).
- `go build ./...`: PASS (exit 0). `go vet ./internal/workstream ./internal/agent ./internal/server ./internal/kairos`: PASS (exit 0).
- Chat UI: `npm run lint`, `npm run build`: PASS (exit 0; 기존 500kB 초과 번들 안내). `git diff --check`: PASS (exit 0).
- 전체 `go test ./...`: NOT RUN (D 구현과 S05 통합 후 실행 예정). CLI `team run`은 Workstream Plan binding 옵션이 없어 S05.C Team 연결 대상(API `/api/team`)에 포함하지 않았으며, 독립 CLI TeamPlan 실행은 기존 동작을 유지한다.
- 다음: D — handoff/context 및 세션 재개 정보에 정확한 Plan revision·stage·미완료·검증 상태를 복원하고 2000-byte 문맥 상한을 검증한다.

### D — PASS

- 구현: Workstream의 결정 사항을 bounded durable field로 추가하고 변경 기록을 남긴다. handoff는 요청의 정확한 Plan ID를 우선 사용하며 현재 stage·definition/state revision·미완료 task·attempt/receipt/evidence·verification·복구 지시를 복원한다. Plan 선택이 없으면 실행 중/실패/승인/초안/완료 우선순위로 가장 관련 있는 계획을 고른다.
- 복구: 실행 프로세스가 사라져 `running`으로 남은 stage는 명시적 사용자 확인과 정확한 run/revision/stateRevision/stage binding을 요구하는 reconcile 경로로만 `failed` 처리한다. 같은 서버의 실제 실행 중 stage는 거부하며, 복구가 acceptance evidence를 만들거나 성공으로 바꾸지 않는다. Handoff는 `pending`을 포함해 모든 미완료 stage의 task와 acceptance criteria를 출력한다.
- 문맥: Plan-bound Workstream context를 2000-byte hard cap 안에 유지하고, 길이가 긴 설명보다 workspace/Workstream/Plan/revision/stage와 최근 상태·증거 참조를 보존한다. Agent/Team/Chronos는 `BeginPlanStage`의 반환 상태로 다시 렌더링해 provider가 running state revision과 attempt/run ID를 받는다. Chronos도 canonical Plan 조회 뒤 context를 만들고 실행 시작 후 다시 렌더링한다. Chat handoff는 선택 workspace와 Plan ID를 전달한다. `docs/workstreams.md`에 API와 복구 계약을 문서화했다.
- 주요 변경: `internal/workstream/{types.go,store.go,context.go,handoff.go,plans.go}`와 테스트, `internal/server/{server.go,workstream_plans_api.go}`와 API 테스트, Agent/Team/Chronos/KAIROS 호출자·테스트, `web/src/{lib/workstreams.ts,pages/Chat.tsx}`, `internal/server/webdist/*`, `README.md`, `docs/workstreams.md`.
- 회귀·독립 검토: `TestGenerateHandoffIncludesPendingStageTasksAndAcceptance`, Agent/Team/Chronos prompt의 running state revision·attempt ID 검사를 추가했다. 독립 재검토에서 앞서 지적된 pending handoff·Chronos 문맥 순서·실행 전 revision 문제 해소를 확인했고 추가 차단 결함을 찾지 못했다.
- 검증: `go test ./internal/workstream ./internal/server -count=1`: PASS (exit 0); 전체 `go test ./... -count=1`: PASS (exit 0, 수정 후 재실행). `npm run lint`, `npm run build`: PASS (exit 0; 기존 500kB 초과 bundle 안내). 최신 Vite index/asset을 `internal/server/webdist`에 동기화하고 embed reference 존재를 확인: PASS. `go build ./...`: PASS (exit 0); `go vet ./internal/workstream ./internal/agent ./internal/server ./internal/kairos`: PASS (exit 0); `git diff --check`: PASS (exit 0).
- 미실행: 실제 브라우저에서 Chat 상호작용 end-to-end 검사는 NOT RUN (프로젝트에 해당 테스트 하네스가 없어 API 회귀, TypeScript build/lint로 검증). 사용자 데이터가 있는 외부 workspace의 복구 테스트는 하지 않았다.
- 완료/다음: S05 전체 PASS. 다음 선행 가능 TODO는 S06.A — read-only weighted budget 종료 시 fake provider로 실제 prompt/tool payload를 재현한다.
