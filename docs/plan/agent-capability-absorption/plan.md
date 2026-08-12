# Agent Capability Absorption Implementation Plan

> 작성일: 2026-08-11
> 선행 문서: spec.md
> 전략: 안전 기반을 먼저 고치고 기존 single kernel에 계약을 점진적으로 주입

## 1. 실행 원칙

- 새 agent loop를 만들지 않는다.
- 먼저 behavior-preserving seam을 만들고 다음 단계에서 policy를 교체한다.
- 각 단계는 독립 test gate와 rollback point를 가진다.
- P0 safety가 닫히기 전에는 parallel execution, plugin execution,
  automatic profiling의 범위를 넓히지 않는다.
- 외부 프로젝트는 행동 영감과 test scenario의 출처일 뿐 source dependency가
  아니다.

## 2. 단계와 의존성

| 단계 | 목적 | 선행 | 종료 gate |
|---|---|---|---|
| Phase 0 | 현재 P0 안전 결함 제거 | 없음 | common safety, real approval, sandbox contract, safe atomic session |
| Phase 1 | single kernel composition contracts 도입 | Phase 0 | 기존 behavior parity와 seam tests |
| Phase 2 | harness, context, compaction 흡수 | Phase 1 | model-aware request budget와 profile resolution |
| Phase 3 | small-model reliability 흡수 | Phase 2 | parser, routing, plan, edit, repetition regression |
| Phase 4 | durable session, protocols, ACP 흡수 | Phase 1, 2 | resume/fork와 protocol conformance |
| Phase 5 | empirical capability profiler | Phase 2, 3, 4 | isolated probes와 holdout validation |
| Phase 6 | evidence, provenance, replay closure | 전체 | matrix 전 항목 Verified |

## 3. Phase 0 — Safety and durability foundation

### P0-01 Common tool safety pipeline

작업:

1. serial과 parallel 분기 전에 schema validation, permission decision,
   approval, validator, ownership, isolation을 공통으로 실행한다.
2. permission 결과와 sandbox plan을 포함한 PreparedToolCall만 scheduler가
   받도록 한다.
3. parallel-safe 판정은 permission 통과 이후에만 수행한다.
4. blocked call도 정상적인 typed tool_result로 model과 receipt에 전달한다.

검증:

- deny된 Read와 Bash가 serial, parallel 양쪽에서 실행되지 않는다.
- mixed batch에서 하나가 deny되어도 다른 승인 call의 ordering은 보존된다.
- pre/post hook과 evidence가 success, denied, failed 모두 한 번씩 기록된다.

관련 현재 지점:

- internal/agent/loop.go:1011-1080
- internal/agent/loop.go:1091-1167
- internal/agent/tools.go:134-207

### P0-02 Real approval

작업:

1. ApprovalBroker 계약을 정의한다.
2. Web SSE, CLI, ACP adapter가 동일 approval request와 response를 사용한다.
3. approve, deny, timeout, client disconnect를 명시한다.
4. ask를 자동 allow로 persist하는 현재 동작을 제거한다.
5. persistence는 explicit remember 선택이 있을 때만 수행한다.

검증:

- approval 응답 전 tool process가 시작되지 않는다.
- timeout과 disconnect는 deny-equivalent이며 재실행되지 않는다.
- session resume 후 pending approval을 중복 실행하지 않는다.

### P0-03 Sandbox contract

작업:

1. SandboxPolicy와 SandboxExecutor를 정의한다.
2. shell, process, external plugin, filesystem mutation의 isolation level을
   profile과 permission으로 결정한다.
3. 지원 OS별 adapter를 독립 구현한다.
4. required sandbox 준비 실패는 fail closed한다.
5. read-only in-process tool만 명시적 정책으로 sandbox를 생략할 수 있다.

검증:

- workspace 밖 write, child-process escape, inherited secret env를 fixture로
  검사한다.
- Windows, Linux, macOS adapter contract test를 동일 suite로 실행한다.

### P0-04 Safe durable session persistence

작업:

1. 외부 ID를 opaque validated ID로 제한한다.
2. workspace key는 canonical path digest로 만든다.
3. temp write, flush, atomic replace 순서로 저장한다.
4. version과 monotonic revision으로 lost update를 막는다.
5. corrupt file은 원본을 보존하고 recovery state로 격리한다.

검증:

- path traversal, ID collision, interrupted write, concurrent append,
  corrupt JSON을 회귀 테스트한다.

## 4. Phase 1 — Composition contracts

### P1-01 Harness contracts

도입할 최소 계약:

- HarnessProfile: 선언형 입력
- HarnessResolver: 선택 규칙
- CompiledHarness: 한 run의 immutable snapshot
- PromptPolicy
- ToolPolicy
- ContextPolicy
- ParserPolicy
- RecoveryPolicy
- VerificationPolicy

통합 지점:

- internal/agent/run_options.go:3-10
- internal/agent/profiles.go:5-52
- internal/agent/loop.go:290

gate:

- default CompiledHarness가 기존 tool budget, temperature, prompt 결과를
  snapshot test에서 보존한다.

### P1-02 ToolCatalog

작업:

1. core, advanced, computer-use, MCP, plugin source를 catalog entry로
   정규화한다.
2. MCP와 plugin discovery를 catalog snapshot 전으로 이동한다.
3. name collision과 incompatible schema를 reject한다.
4. advertised set과 executable set의 동등성을 검사한다.

gate:

- 첫 run에서 MCP tool이 보인다.
- 목록만 되던 plugin은 승인과 sandbox가 준비된 경우에만 executable이 된다.

### P1-03 Canonical state and terminal failure

작업:

1. run state에 iteration, active plan, context revision, tool fingerprint,
   verification status, session revision을 둔다.
2. error string 대신 typed FailureCode를 사용한다.
3. retry 가능 여부와 terminal 여부를 failure type이 결정한다.
4. SessionEnd hook은 defer 기반으로 모든 종료 경로에서 정확히 한 번 실행한다.

gate:

- normal completion, provider error, context blocked, repeated action,
  cancellation, panic recovery에서 lifecycle test가 통과한다.

## 5. Phase 2 — Harness, context, compaction

### P2-01 Profile resolution

1. 현재 substring ModelProfile을 configured fallback으로 이관한다.
2. explicit override와 empirical profile precedence를 추가한다.
3. provider/model capability metadata를 resolver 입력으로 사용한다.
4. selected profile ID와 version을 trace와 receipt에 기록한다.

### P2-02 Full request budgeting

1. provider model metadata에서 context window와 max output을 해석한다.
2. system prompt, tool schema, messages, project context, RAG, memory,
   plan anchor, output reserve를 모두 계측한다.
3. budget 초과 시 낮은 우선순위 injected context를 먼저 축소한다.
4. 그래도 초과하면 structured compaction을 실행한다.
5. 재계산 후에도 초과하면 context_blocked로 종료한다.

### P2-03 Structured compaction

보존 필드:

- user objective
- active plan과 current step
- files read and edited
- tool call과 result의 paired digest
- failed attempts와 correction
- verification evidence
- pending approval
- durable blob references

gate:

- 16K fixture에서 overflow 전에 compact한다.
- summarizer timeout과 empty output에서 deterministic fallback이 동작한다.
- compact 전후 tool pair와 completion contract가 유지된다.
- pre_compact와 post_compact hook이 모든 결과를 기록한다.

## 6. Phase 3 — Small-model reliability

### P3-01 Deterministic and staged tool routing

1. core tools는 항상 보존한다.
2. task relevance와 capability score로 optional tool을 정렬한다.
3. context window가 16K 이하이거나 profile이 요구하면 category selection과
   concrete tool selection의 2단계 routing을 사용한다.
4. 동일 input에서는 동일 catalog snapshot을 반환한다.

### P3-02 Multi-format tool-call parser

parser cascade:

1. provider native structured calls
2. declared profile format
3. Hermes-style envelope
4. Liquid-style action form
5. fenced JSON
6. bare JSON
7. bounded truncation repair

각 단계는 catalog membership와 JSON schema validation을 통과해야 한다.
실패한 parser의 원문을 실행하지 않으며 redacted digest만 trace에 남긴다.

### P3-03 Plan anchor and completion contract

1. active objective, current step, remaining steps, Definition of Done을
   structured state로 유지한다.
2. 매 iteration prompt에는 전체 conversation 재주입 대신 compact anchor를
   넣는다.
3. model의 done 주장보다 VerificationPolicy와 EvidenceGate가 우선한다.

### P3-04 Edit waterfall

순서:

1. read-before-write 확인
2. patch-first 적용
3. exact edit
4. Corelay Code fuzzy candidate와 numbered hint
5. syntax or lint gate
6. 실패 시 rollback
7. bounded correction feedback

원칙:

- fallback이 원본보다 넓은 범위를 조용히 덮어쓰지 않는다.
- rollback 실패는 별도 terminal failure다.

### P3-05 Repetition and early stop

1. tool name, normalized args, target revision으로 action fingerprint를 만든다.
2. 동일 failure fingerprint가 threshold를 넘으면 correction hint를 준다.
3. budget 소진 후 repeated_action으로 종료한다.
4. 성공, file revision 변화, explicit user approval은 fingerprint state를
   합리적으로 reset한다.

## 7. Phase 4 — Durable sessions, protocols, ACP

### P4-01 Durable session lifecycle

operations:

- create
- append
- resume
- fork
- interrupt
- recover
- close

fork는 parent revision을 기록하고 이후 log와 blob namespace를 분리한다.
active run ID는 cancellation용이며 durable session ID를 대체하지 않는다.

### P4-02 Large result offload

현재 호출되지 않는 SessionMemory 개념을 DurableBlobStore에 통합한다.
context에는 bounded preview, byte size, content digest, blob reference만
남긴다. resume과 fork 후에도 참조가 유효해야 하며 retention policy로
정리한다.

### P4-03 Protocol adapters

구현 순서:

1. Anthropic Messages ingress를 기존 canonical behavior의 golden baseline으로
   고정한다.
2. OpenAI Chat Completions ingress와 streaming egress를 추가한다.
3. OpenAI Responses ingress와 event mapping을 추가한다.
4. Gemini tool use와 tool result round trip을 완성한다.
5. parallel tool-call block index와 unique ID를 검증한다.
6. ACP session, cancel, approval, stream operation을 canonical operation에
   연결한다.

## 8. Phase 5 — Empirical CapabilityProfiler

probe categories:

- native structured tool call
- Hermes, Liquid, fenced, bare format stability
- maximum reliable tool count
- 2-stage routing benefit
- practical context ceiling
- patch, exact edit, fuzzy correction success
- repetition and truncation frequency
- plan-anchor retention

실행 원칙:

- isolated disposable workspace
- fixed fixtures, seeds, repeats
- raw trace와 scoring 분리
- confidence threshold
- holdout validation
- regression 시 quarantine
- manual override는 provenance와 expiry를 요구

## 9. Phase 6 — Evidence, provenance, replay

작업:

1. EvidenceGate 기본 정책과 terminal state를 명확히 한다.
2. receipt에 harness profile, capability profile, context budget,
   parser path, recovery count, sandbox result, approval result,
   artifact digest를 추가한다.
3. raw secret과 prompt를 redaction corpus로 검사한다.
4. source inspiration과 independent implementation provenance를 capability별로
   연결한다.
5. captured fixture로 regression replay를 수행한다.
6. capability matrix의 상태를 Missing, Partial, Connected, Verified로
   자동 산출할 verifier를 만든다.

## 10. 검증 명령과 gate

각 phase에서 최소 다음을 실행한다.

    go test -count=1 ./...
    go vet ./...
    go build ./...

추가 gate:

- protocol golden suite
- parser malformed corpus
- session crash and traversal suite
- sandbox contract suite
- profiler deterministic fixture suite
- secret redaction corpus
- git diff --check
- Mermaid structural validation

## 11. 완료 순서 체크리스트

- [ ] Phase 0 P0 safety gate
- [ ] Phase 1 composition parity gate
- [ ] Phase 2 16K and 32K context gate
- [ ] Phase 3 small-model recovery gate
- [ ] Phase 4 resume, fork, protocol, ACP gate
- [ ] Phase 5 profiler holdout gate
- [ ] Phase 6 evidence and provenance gate
- [ ] capability matrix 전 항목 Verified
- [ ] single kernel invariant audit
- [ ] clean-room provenance audit
