# Handoff: AniClew 에이전트 하네스 기능 흡수

## Session Metadata
- Created: 2026-08-12 00:47:53 KST
- Project: `D:\git\aniclew`
- Branch: `main`
- Base commit: `5b1cf79`
- Goal: `모두 구현하자` (active; 완료 처리 금지)

## Current State Summary
AniClew의 정체성을 작은·로컬 모델이 실제 코딩 작업을 완수하도록 만드는 독립 agent harness/runtime으로 확정했다. Open Interpreter, SmallCode, OpenClaw와 기능이 겹치는 것은 허용하며, 기존 `RunLoopWithOptions`·provider·tool composition을 유지한 채 유용한 기능을 모두 흡수하는 중이다. capability matrix와 구현 도면을 만든 뒤 tool recovery, permission/approval, sandbox, durable session, response policy, receipts를 대폭 구현했다. 현재 공유 worktree에는 여러 worker의 미커밋 변경이 함께 있으며, 세션 HTTP/Web 마감과 내부 변경 도구 sandbox/CAS 작업이 진행 중이다.

## Work Completed
- [x] 45개 capability matrix, 구현 계획, 도메인 사전, 4개 Mermaid 흐름도 작성
- [x] HarnessProfile 기반과 모델별 tool budget/temperature/context 계약 연결
- [x] Hermes/Liquid/fenced/bare/native tool-call parser, strict limits·ambiguity·correction cap·fuzz 검증
- [x] 공통 tool dispatcher에 schema, catalog identity, path, permission, approval, provenance, scheduling 순서 통합
- [x] approval broker와 CLI/Web inline allow-once/deny UX 구현
- [x] Linux bubblewrap, macOS Seatbelt, Windows Job Object 기반 truthful sandbox 및 Bash/단기 subprocess 연결
- [x] crypto session IDs, canonical workspace digest, atomic revision save, fork/lifecycle/interruption store 구현
- [x] receipt redaction, command digest, artifact hash/status, atomic writer 구현
- [x] `/undo` manifest/preflight/rollback 및 top-level user consent 경계 강화
- [x] main/Chronos/SubAgent response policy 및 sandbox 정책 전파

### Files Modified
| File | Changes |
|------|---------|
| `internal/agent/loop.go` | unified harness/profile, recovery, dispatcher, approval, sandbox, evidence 연결 |
| `internal/agent/toolrecover.go` | bounded multi-format parser와 typed trace |
| `internal/agent/tool_dispatch.go` | 공통 authorization, schema/provenance, ordered execution |
| `internal/agent/sessions.go` | secure IDs, revision CAS, fork/lifecycle/interruption |
| `internal/approval/*` | one-shot approval broker와 session cancellation |
| `internal/sandbox/*` | OS별 secure runner와 honest capabilities |
| `internal/server/server.go` | approval/session API와 composition |
| `cmd/proxy/chat.go` | streaming approval UX와 fail-closed cleanup |
| `web/src/pages/Chat.tsx` | inline approval 및 durable-session revision 상태 |
| `docs/plan/agent-capability-absorption/*` | capability matrix, 계획, 구현 메모, 흐름도 |

### Decisions Made
| Decision | Rationale |
|----------|-----------|
| 하나의 agent kernel만 확장 | 같은 역할의 병렬 loop/fork가 정책과 안전 경계를 갈라놓는 것을 방지 |
| 외부 프로젝트는 동작 참고로만 사용 | 개인용이라 중복은 문제없지만 독립 Go 구현과 기존 composition을 보존 |
| approval과 sandbox 분리 | 사용자 동의는 격리를 대신하지 않으며 둘 다 fail-closed여야 함 |
| 모든 tool shape를 immutable catalog에 바인딩 | recovered/native/MCP/plugin 호출의 schema·executor 교체 TOCTOU 방지 |
| session revision과 interruption reconcile 강제 | ambiguous side effect 후 자동 재실행과 transcript overwrite 방지 |

## Pending Work
### Immediate Next Steps
1. 세션 API/Web revision·fork·interrupt·reconcile·close 마감 후 Go/Web 전체 게이트 실행
2. lint/Edit/Write/worktree 내부 mutation을 sandbox와 revision-safe rollback으로 완결
3. system/tools/RAG/memory/output reserve를 포함하는 전체 요청 context planner와 structured compaction 구현
4. `/api/agent`를 durable session load/append/checkpoint/resume/fork에 실제 연결
5. OpenAI Chat/Responses ingress, Gemini tool history, ACP lifecycle/permission/streaming adapter 구현
6. automatic capability profiler, holdout/quarantine, benchmark/ablation/replay 추가

### Blockers/Open Questions
- [ ] `WirePolicy`와 `EditPolicy`가 실행 경로에서 아직 완전히 소비되지 않음
- [ ] ReadLedger 검사와 실제 write 사이 CAS/다중 파일 transaction이 완결되지 않음
- [ ] MCP long-lived process, computer-use, hooks/Kairos/tray subprocess 신뢰 경계가 남음
- [ ] large tool-result offload와 full-request protocol-aware token estimator가 없음
- [ ] Web `dist`를 `internal/server/webdist`에 아직 동기화하지 않음

## Context for Resuming
### Important Context
- shared worktree이므로 다른 worker 소유 파일을 덮어쓰거나 reset하지 않는다.
- 안정 시점에 여러 차례 `go test ./...`, `go vet ./...`, `go build ./...`가 통과했지만 현재 진행 중 변경까지 포함한 최종 전체 검증은 아직 아니다.
- 현재 기본 방향은 Open Interpreter를 복제하는 것이 아니라 모델 실패 흡수, 실측 프로필, 안전한 tool execution, 증거 기반 완료를 강화하는 것이다.
- 다음 context planner는 기존 main loop 요청 구성 지점에만 연결하고 별도 loop를 만들지 않는다.

### Potential Gotchas
- 공유 worktree 중간 상태의 compile/test 실패를 최종 회귀로 단정하지 말고 owner 마감 뒤 재검증한다.
- `ask`를 자동 allow/persist하지 않는다. approval broker가 없으면 실행하지 않는다.
- sandbox `Preferred`도 요구 capability를 만족하지 못하면 host 실행으로 fallback하지 않는다.
- 정상 SSE 종료와 approval resolve POST 사이 cleanup race를 되살리지 않는다.
- version 없는 기존 undo manifest는 안전상 거부되는 호환성 변경이다.
