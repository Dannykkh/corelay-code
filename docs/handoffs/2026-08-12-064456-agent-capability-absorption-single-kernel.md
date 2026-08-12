# Handoff: Agent capability absorption single kernel

## Session Metadata
- Created: 2026-08-12 06:44:56 +09:00
- Project: D:\git\aniclew
- Branch: main
- Base commit: 5b1cf79

## Current State Summary
AniClew가 Open Interpreter, SmallCode, OpenClaw 계열의 유용한 기능을 자체 하네스에 흡수하는 장기 작업을 계속하고 있다. 이전 핸드오프 이후 run-owned MCP, provider target-bound transport, capability profiler, durable large-result storage, CompletionContract, file-tool boundary가 구현·검증됐다. 현재 핵심은 CAP-001 단일 agent kernel 수렴과 CAP-044 종료 생명주기 정규화다. Chronos와 SubAgent의 별도 provider/parser/dispatcher loop는 제거됐고, Team·Bridge도 typed terminal reducer를 사용한다. ACP consumer와 공통 terminal finalizer는 병렬 작업 중이므로 목표는 아직 완료되지 않았다.

## Work Completed
- [x] run-owned stdio MCP를 RunLoop와 ACP session 수명에 연결하고 세션 격리·close-once·비밀 비누출 검증
- [x] provider target-bound HTTP transport와 fail-closed capability-profiler runtime 경계 구현
- [x] durable large tool result offload, canonical reference, HTTP/ACP resume·fork·delete composition 구현
- [x] CompletionContract, ReportCompletion, evidence index, false-completion prose 차단, typed terminal persistence 구현
- [x] Chronos phase controller와 SubAgent task controller를 `RunMode`로 축소하고 `RunLoopWithOptions` 단일 호출로 수렴
- [x] Team과 Bridge의 blocked/incomplete/error/cancel/no-terminal 판정을 공통 typed terminal 의미에 맞춤

### Files Modified
| File | Changes |
|------|---------|
| internal/agent/run_mode.go | provider/tool loop를 소유하지 않는 phase-only `RunMode` 계약 |
| internal/agent/chronos_adapter.go | Chronos를 공통 RunLoop에 연결하는 adapter |
| internal/agent/chronos.go | configuration/state만 남기고 별도 mini-loop 제거 |
| internal/agent/subagent.go | task RunMode와 event reducer를 사용한 단일 RunLoop 호출 |
| internal/agent/team.go | truthful terminal reducer와 bounded partial result |
| internal/agent/bridge.go | typed terminal 판정과 실패 시 half-turn rollback |
| internal/server/chronos_evidence.go | max-cycle/blocked RunMode terminal을 실패 trace로 분류 |
| internal/agent/sessions.go | completion terminal metadata를 durable session에 원자 저장 |

### Decisions Made
| Decision | Rationale |
|----------|-----------|
| Chronos/SubAgent는 새 kernel이 아니라 `RunMode` controller로 표현 | context, tools, safety, hooks, evidence, receipt, terminal semantics의 단일 정본을 유지하기 위해서다 |
| mode iteration limit는 harness maximum을 완화하지 않는 상한으로 적용 | 작은 모델 안전 프로필을 작업 모드가 우회하지 못하게 한다 |
| `done`을 성공과 동일시하지 않는다 | blocked/incomplete completion도 transcript commit이 가능하며, 성공 여부는 typed terminal metadata가 결정한다 |
| terminal event 이후 관찰된 cancel은 이미 선형화된 정상 결과를 뒤집지 않는다 | event 소비 시점의 race로 성공을 실패로 바꾸지 않기 위해서다 |
| recorder, hooks, receipt는 adapter가 아니라 공통 RunLoop가 소유 | 중복 실행과 ingress별 의미 드리프트를 막기 위해서다 |

## Pending Work
### Immediate Next Steps
1. ACP bridge consumer가 blocked/incomplete/context/max-iteration/cancel/no-terminal을 같은 typed terminal 의미로 처리하도록 마감한다.
2. 공통 RunLoop terminal finalizer를 도입해 SessionEnd, recorder terminal, receipt, typed terminal event, channel close 순서를 정확히 한 번으로 고정한다.
3. CAP-001/CAP-044 capability matrix, provenance ledger, agent-kernel flow diagram을 실제 구현·테스트 상태로 갱신한다.
4. 모든 worker 종료 뒤 Go 전체 test/vet/build, Web build/changed-file lint, provenance와 Mermaid 구조 검증을 실행한다.
5. 남은 Partial/Connected capability를 다시 감사하고 실제 기능 공백만 다음 구현 파동으로 보낸다.

### Blockers/Open Questions
- [ ] ACP의 legacy synthetic done 경로를 protocol 호환을 깨지 않으면서 typed durable policy로 교체해야 한다.
- [ ] common finalizer 이전의 일부 preflight 경로는 RunFailed가 RunStarted 없이 기록되고, UI command 경로는 recorder terminal이 빠질 수 있다.
- [ ] typed terminal이 모든 client/runner에서 성공 판정의 정본인지 전체 소비자 검색과 E2E로 재확인해야 한다.
- [ ] 공식 ACP full conformance suite는 존재하지 않아 Connected를 Verified로 올릴 별도 repo-contained scenario가 필요하다.

## Context for Resuming
### Important Context
- 공유 worktree는 매우 dirty하며 현재 변경 전체가 하나의 capability-absorption 작업이다. reset, checkout, broad cleanup을 하면 안 된다.
- 마지막 통합 focused gate는 SubAgent·Team·Bridge count=3 PASS다. Chronos, SubAgent, Team은 각각 full `internal/agent`와 vet를 소유 worker가 통과시켰다.
- ACP terminal worker는 `internal/acpbridge/backend.go`와 전용 테스트만 소유한다. terminal-finalizer worker는 `internal/agent/loop.go`, `run_options.go`, 신규 terminal helper/tests만 소유한다.
- `RunMode`는 phase/message/continue/finish만 결정하고 provider, parser, dispatcher, context planning, hooks, evidence, persistence를 실행하지 않는다.
- Session terminal metadata는 transcript와 같은 CAS에 저장되며 blocked/incomplete는 resume 후에도 보존된다.

### Potential Gotchas
- common `done`은 commit 가능성을 뜻하므로 typed blocked/incomplete를 정상 성공으로 축약하면 안 된다.
- 정상 `done`을 소비한 뒤 context가 취소될 수 있으므로 late cancel이 이미 확정된 결과를 뒤집으면 안 된다.
- test fixture의 전역 HOME, memory, autoskill, autoverify 설정이 작은 context 모델을 의도치 않게 overflow시킬 수 있어 격리 helper를 사용해야 한다.
- 병렬 worker 중간 compile 오류는 소유 worker의 compile-safe 신호 전까지 다른 기능 회귀로 단정하지 않는다.
- 이번 압축 시점에 `memory/{gotchas,learned}/observations.jsonl`은 2026-05-30 이후 변경되지 않아 새로 정제할 session observation은 0개였다.
