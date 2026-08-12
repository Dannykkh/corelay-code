# Handoff: Agent capability absorption runtime boundaries

## Session Metadata
- Created: 2026-08-12 04:30:13 +09:00
- Project: D:\git\aniclew
- Branch: main
- Base commit: 5b1cf79

## Current State Summary
AniClew를 자체 agent loop와 tool runtime을 소유하는 통합 하네스로 유지하면서 Open Interpreter, SmallCode, OpenClaw의 유용한 동작을 기존 composition point에 흡수하고 있다. 공통 permission/approval/sandbox, multi-format parser, context planner와 structured compaction, protocol adapters, executable plugins, durable session observer는 구현과 focused regression을 마쳤다. 현재 empirical capability profiler의 실제 안전 실행 경로, provider 목적지 고정 transport, ACP용 run-owned stdio MCP를 병렬 구현 중이다. 전체 목표와 goal은 아직 완료 상태가 아니다.

## Work Completed
- [x] serial/parallel 공통 tool authorization, real approval broker, OS process sandbox와 path/session safety 연결
- [x] HarnessProfile, model-aware context planning, structured compaction, deterministic/two-stage routing, multi-format parser 연결
- [x] Anthropic/OpenAI Chat/OpenAI Responses/Gemini protocol adapter와 ACP stable-v1 core/bridge/CLI 추가
- [x] executable plugin discovery, immutable identity, sandbox, per-call approval proof를 RunLoop/Chronos/SubAgent/Team에 전파
- [x] HTTP와 ACP가 공유하는 redacted `DurableRunObserver`, reconciliation marker, runtime quarantine 경계 추가

### Files Modified
| File | Changes |
|------|---------|
| internal/agent/durable_run.go | HTTP/ACP 공통 assistant/tool lifecycle observer와 ambiguity/reconciliation 판정 |
| internal/server/agent_session.go | 공통 observer 기반 CAS checkpoint, marker retry, runtime quarantine |
| internal/acpbridge/backend.go | durable observer 재사용, cancel/error/max-turn transcript와 reconciliation 처리 |
| internal/agent/plugin_execution.go | sandboxed exec-form plugin runtime과 immutable content identity |
| internal/agent/context_plan.go | 완성 request 기준 context budget과 deterministic reduction |
| internal/protocol/ | canonical ingress/egress adapter registry |
| internal/capabilityprofile/ | empirical plan, scoring, holdout, immutable profile store 기반 |
| docs/plan/agent-capability-absorption/ | capability matrix, provenance ledger, flow diagrams |

### Decisions Made
| Decision | Rationale |
|----------|-----------|
| 기능 중복을 제품 경쟁 관점에서 제거하지 않는다 | 개인용 통합 하네스이므로 검증된 기능 흡수 자체가 목표다 |
| 별도 provider/CLI agent loop를 만들지 않는다 | `RunLoopWithOptions`와 기존 catalog/dispatcher/session composition을 단일 정본으로 유지한다 |
| approval과 sandbox를 독립 경계로 유지한다 | 사용자 동의는 OS 격리를 대체하지 못한다 |
| HTTP와 ACP durable transcript는 공통 observer를 사용한다 | transport별 lifecycle drift와 side-effect 유실을 방지한다 |
| profiler provider 호출은 exact target proof 없이는 fail closed한다 | tool subprocess sandbox만으로 host provider egress를 격리했다고 주장할 수 없다 |
| MCP는 process-global client 대신 run-owned runtime으로 이동한다 | ACP 세션 격리와 close/cancel 수명을 증명하기 위해 필요하다 |

## Pending Work
### Immediate Next Steps
1. capability profiler runtime과 provider target-bound HTTP transport를 결합해 실제 provider probe를 안전하게 실행한다.
2. run-owned stdio MCP를 RunLoop와 ACP session config에 연결하고 first-request visibility, two-session isolation, close-once를 검증한다.
3. stale capability matrix와 provenance ledger를 실제 코드/test 상태로 재감사하고 누락 capability record를 채운다.
4. 모든 병렬 worker 종료 후 Go 전체 test/vet/build, Web build/changed lint, diff, Mermaid gate를 실행한다.
5. remaining Connected/Partial 항목인 unified kernel, large-result production composition, repetition progress reset, complete hook lifecycle을 다음 구현 파동에서 닫는다.

### Blockers/Open Questions
- [ ] provider target-bound application transport가 profiler의 target proof를 충족하지만 full-process OS network isolation과 동일하다고 과장하지 않아야 한다.
- [ ] ACP stable v1의 client-provided stdio MCP는 run-owned ownership 구현과 conformance E2E가 끝나기 전까지 Verified가 아니다.
- [ ] `capability-matrix.md`와 `provenance-ledger.json`은 현재 구현보다 뒤처져 있으며 전 항목 Verified gate가 아직 없다.
- [ ] Chronos/SubAgent가 별도 loop control flow를 보유해 INV-01 single-kernel 불변식의 최종 감사와 정리가 필요하다.

## Context for Resuming
### Important Context
- 공유 worktree는 매우 dirty하며 모든 변경이 같은 기능 흡수 작업에 속한다. 관련 없는 변경으로 간주해 reset/checkout하면 안 된다.
- 마지막 안정 전체 gate는 active profiler/provider/MCP 파동 전 시점의 PASS다. 최종 결과에는 모든 worker가 compile-safe를 선언한 뒤 새 전체 gate가 필요하다.
- executable plugin은 main loop뿐 아니라 Chronos/SubAgent/Team에도 전파됐고 default discovery와 explicit configuration의 실패 의미가 다르다.
- durable observer는 raw tool input을 저장하지 않고 tool call ID와 input digest만 저장한다. reused/overlapping ID ambiguity는 reconciliation을 강제한다.
- 현재 active workers: capability profiler runtime, provider target-bound transport, run-owned MCP.

### Potential Gotchas
- 병렬 worker의 중간 compile 오류를 다른 기능의 회귀로 오판하지 않는다. 소유 worker의 compile-safe 신호 뒤 전체 gate를 다시 실행한다.
- `SessionMemory` 구현과 context offload seam은 있어도 production composition에서 store가 주입되지 않으면 CAP-036을 Verified로 올릴 수 없다.
- `RunGuard.Reset`은 현재 production 호출이 없어 성공/진전/approval reset 계약이 미완이다.
- process-local runtime quarantine은 persistence 실패 시 안전한 차단이지만 restart를 넘는 durable recovery 증거는 아니다.
- raw MCP command, args, env, provider endpoint, credentials는 session, ACP outbound, receipt, trace에 기록하면 안 된다.
