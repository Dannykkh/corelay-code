# Agent Capability Absorption Specification

> 상태: Proposed
> 작성일: 2026-08-11
> 범위: Corelay Code 단일 agent kernel의 clean-room 기능 흡수
> 결정: unified single kernel

## 1. 목적

Corelay Code를 시장 차별화를 위한 별도 제품이 아니라, 검증된 agent harness
기법을 하나의 자체 runtime에 누적하는 개인용 통합 coding-agent
platform으로 발전시킨다.

기능 흡수는 외부 저장소의 코드를 가져오는 일이 아니다. Phase 1 감사에서
확인한 행동, 실패 조건, 입력과 출력 계약을 Corelay Code의 기존 단일
RunLoopWithOptions 안에서 독립적으로 설계하고 구현하며, 각 기능이 실제
실행 경로에 연결되고 테스트와 receipt로 증명될 때 완료로 본다.

## 2. 근거 기준선

### 2.1 Corelay Code 현재 상태

- 단일 agent loop는 internal/agent/loop.go:290-1224에 존재한다.
- provider 공통 계약은 internal/types/provider.go:40-45에 존재한다.
- 현재 ModelProfile은 tool budget과 temperature만 제공한다:
  internal/agent/profiles.go:5-15.
- context window는 loop에서 200000으로 고정되어 있다:
  internal/agent/loop.go:458-460.
- persistent SessionStore와 실행용 LoopRegistry는 서로 분리되어 있다:
  internal/agent/sessions.go:58-68,
  internal/agent/loop_registry.go:39-75.
- proxy ingress는 Anthropic Messages 경로만 제공한다:
  internal/server/server.go:177-181.
- parallel-safe tool 경로는 serial 경로의 permission 결정을 거치지 않는다:
  internal/agent/loop.go:1026-1080.
- ask 결정은 실제 사용자 승인을 받지 않고 allow rule을 저장할 수 있다:
  internal/agent/loop.go:1091-1113.
- OS process sandbox는 없다.
- SessionStore는 외부 ID를 파일명에 직접 사용하고 비원자적으로 저장한다:
  internal/agent/sessions.go:217-263.

### 2.2 행동 영감

Phase 1 감사에서 다음 행동을 확인했다. 링크는 provenance와 독립 검증을
위한 출처이며, 구현 코드를 복사하거나 포함하는 근거가 아니다.

- Open Interpreter:
  https://github.com/openinterpreter/openinterpreter
  - prompt, tool schema, message conversion, response post-processing을
    모델별 harness로 교체
  - model-aware compaction
  - session resume와 fork
  - ACP surface
  - OS sandbox
  - automatic empirical capability profiler는 없음
  - exact Edit 중심이라 Corelay Code의 fuzzy hint와 lint rollback보다 복구 폭이
    좁음
- SmallCode:
  https://github.com/Doorman11991/smallcode
  - deterministic tool routing
  - 16K 이하 context용 2-stage routing
  - Hermes, Liquid, fenced JSON, bare JSON tool-call recovery
  - plan anchor
  - read-before-write
  - patch-first edit와 rollback
  - fingerprint repetition detection과 early stop
  - contract와 Definition of Done
  - OS sandbox는 없음
  - 공개 benchmark는 광범위한 동등성 증거로 쓰기에는 약함

## 3. 목표

1. 모든 own-agent 실행을 하나의 kernel과 하나의 상태 모델로 유지한다.
2. provider와 model마다 prompt, tools, protocol, context, recovery,
   verification 정책을 조합 가능한 HarnessProfile로 만든다.
3. 모델 이름 휴리스틱을 empirical CapabilityProfiler로 보완한다.
4. tool 실행은 serial과 parallel 모두 동일한 permission, approval,
   validator, ownership, sandbox gate를 통과시킨다.
5. durable session이 create, resume, fork, interrupt, recovery를 지원한다.
6. Anthropic Messages, OpenAI Chat Completions, OpenAI Responses, ACP를
   canonical kernel request와 event로 변환한다.
7. 작은 모델의 malformed call, plan drift, stale edit, repetition,
   context overflow를 bounded recovery로 흡수한다.
8. 완료 판정을 verified, partially-verified, unverified, blocked로
   구분하고 redacted evidence receipt를 남긴다.
9. 모든 외부 영감은 clean-room provenance로 추적한다.

## 4. 비목표

- 외부 저장소를 fork, vendor, subtree, submodule, binary embed하지 않는다.
- 외부 source file이나 함수 본문을 번역하거나 줄 단위로 재작성하지 않는다.
- provider별로 별도 agent loop를 만들지 않는다.
- UI별, protocol별, model별로 독립 상태 머신을 만들지 않는다.
- benchmark 수치만 맞추기 위해 실제 안전성과 완료 계약을 축소하지 않는다.
- 기존 Corelay Code fuzzy edit, hint, lint rollback을 exact edit로 퇴행시키지
  않는다.
- proxy 경로가 own-agent의 edit recovery를 자동으로 얻는다고 주장하지
  않는다. loop 소유권이 다른 경로는 capability를 명시적으로 구분한다.

## 5. 아키텍처 불변식

| ID | 불변식 |
|---|---|
| INV-01 | RunLoopWithOptions가 유일한 own-agent kernel이다. |
| INV-02 | 한 run은 시작 시 immutable CompiledHarness를 하나만 선택한다. |
| INV-03 | protocol adapter는 kernel 밖에서 canonical request와 event로 변환한다. |
| INV-04 | tool schema를 노출한 catalog와 실제 executor dispatch는 같은 snapshot을 사용한다. |
| INV-05 | permission과 sandbox gate 이전에는 어떤 tool도 실행하지 않는다. |
| INV-06 | ask는 실제 approval 결과 없이는 allow로 승격되지 않는다. |
| INV-07 | context 계산은 system, tools, messages, injected context, output reserve를 모두 포함한다. |
| INV-08 | compaction은 tool pair, active plan, edited files, verification state를 보존한다. |
| INV-09 | durable session ID와 active run ID는 서로 다른 개념이다. |
| INV-10 | receipt에는 raw secret, raw credential, 전체 prompt를 저장하지 않는다. |
| INV-11 | recovery와 retry에는 명시적 budget과 typed terminal state가 있다. |
| INV-12 | source inspiration은 behavior contract와 provenance로만 사용한다. |

## 6. 기능 요구사항

| ID | 요구사항 | 완료 증거 |
|---|---|---|
| FR-001 | HarnessResolver는 provider, model, observed capability를 입력받아 CompiledHarness를 반환해야 한다. | resolver table test와 unknown-model fallback test |
| FR-002 | HarnessProfile은 system prompt, tool policy, context policy, parser policy, recovery policy, verification policy, protocol preference를 표현해야 한다. | schema validation test와 profile snapshot |
| FR-003 | profile resolution precedence는 explicit run override, persisted empirical profile, configured profile, conservative fallback 순이어야 한다. | precedence table test |
| FR-004 | ToolCatalog는 core, optional, MCP, plugin tool을 immutable snapshot으로 조합해야 한다. | advertised tool과 executable tool의 집합 동등성 test |
| FR-005 | context planner는 완성된 request의 입력 예산을 산출하고 provider/model별 window와 output reserve를 적용해야 한다. | 16K, 32K, 128K, 1M fixture test |
| FR-006 | compaction은 structured session snapshot을 만들고 LLM summary 실패 시 deterministic fallback을 사용해야 한다. | summary success, empty, timeout, fallback golden test |
| FR-007 | tool router는 deterministic relevance routing과 context가 16K 이하일 때 2-stage selection을 지원해야 한다. | repeatable routing fixture |
| FR-008 | tool-call parser는 native structured call, Hermes, Liquid, fenced JSON, bare JSON을 bounded cascade로 처리해야 한다. | format별 golden fixture와 malformed negative fixture |
| FR-009 | leaked or truncated tool call recovery는 catalog에 존재하는 tool과 유효한 schema만 실행 후보로 인정해야 한다. | unknown tool, invalid args, truncation test |
| FR-010 | plan anchor는 active plan, current step, completion contract를 매 iteration에 안정적으로 보존해야 한다. | long-run drift regression |
| FR-011 | edit pipeline은 read-before-write, patch-first, exact edit, fuzzy hint, syntax/lint rollback을 순서가 명시된 waterfall로 제공해야 한다. | stale edit, parse break, rollback fixture |
| FR-012 | repetition guard는 normalized action fingerprint로 반복을 탐지하고 bounded retry 후 typed stop을 반환해야 한다. | equivalent-call fingerprint test |
| FR-013 | serial과 parallel tool은 동일한 common safety pipeline을 통과해야 한다. | parallel permission-deny regression |
| FR-014 | ask decision은 UI, CLI, ACP에서 실제 approval round trip을 수행해야 한다. | approve, deny, timeout, disconnect integration test |
| FR-015 | shell, process, filesystem mutation은 지원 OS에서 sandbox policy를 적용하고 준비 실패 시 fail closed해야 한다. | Windows, Linux, macOS adapter contract test |
| FR-016 | DurableSession은 create, load, append, resume, fork, interrupt, recover를 지원해야 한다. | crash-recovery와 fork isolation test |
| FR-017 | session write는 opaque validated ID와 atomic replace를 사용하고 workspace boundary를 검증해야 한다. | traversal, collision, partial-write regression |
| FR-018 | large tool result는 durable blob으로 offload하고 digest와 bounded preview만 context에 남겨야 한다. | round-trip, cleanup, resume test |
| FR-019 | Anthropic Messages, OpenAI Chat Completions, OpenAI Responses ingress가 canonical request로 수렴해야 한다. | request and streaming golden tests |
| FR-020 | ACP adapter는 session, stream, tool approval, cancel을 canonical operation에 매핑해야 한다. | ACP conformance fixture |
| FR-021 | CapabilityProfiler는 isolated probe workspace에서 protocol, tool, context, edit, repetition 능력을 반복 측정해야 한다. | deterministic fake-provider probe suite |
| FR-022 | profiler는 confidence와 provenance가 있는 CapabilityProfile을 저장하고 holdout 실패 시 quarantine해야 한다. | stale profile, low confidence, regression test |
| FR-023 | EvidenceGate는 verified, partially-verified, unverified, blocked 상태를 반환해야 한다. | 상태 전이 table test |
| FR-024 | receipt는 prompt나 secret 대신 redacted metadata, artifact digest, command exit status를 저장해야 한다. | secret corpus redaction test와 digest verification |
| FR-025 | 모든 absorbed capability는 source inspiration, behavior contract, independent implementation, tests를 provenance record로 남겨야 한다. | capability matrix와 provenance audit |

## 7. 주요 시나리오

### 7.1 새 모델 연결

1. 사용자가 provider와 model을 등록한다.
2. CapabilityProfiler가 isolated probe를 실행한다.
3. probe가 protocol 형식, 안정적인 tool 수, context ceiling, edit 방식,
   repetition 성향을 측정한다.
4. confidence가 충분하면 empirical CapabilityProfile을 저장한다.
5. holdout 검증에 실패하면 profile을 quarantine하고 conservative fallback을
   사용한다.
6. 이후 run은 해당 profile을 CompiledHarness로 해석한다.

### 7.2 coding run

1. ingress adapter가 요청을 canonical request로 변환한다.
2. durable session을 create, resume 또는 fork한다.
3. HarnessResolver가 immutable CompiledHarness를 선택한다.
4. ToolCatalog와 ContextPlan을 만든다.
5. provider response를 parser cascade로 해석한다.
6. tool call은 common safety pipeline과 sandbox를 통과한다.
7. 결과와 checkpoint를 atomic session append로 저장한다.
8. completion contract와 EvidenceGate를 평가한다.
9. redacted receipt와 terminal state를 반환한다.

### 7.3 오류와 복구

- protocol parsing 실패: 다음 parser format으로 이동하되 budget 소진 시
  malformed_response로 종료한다.
- context 초과: structured compaction 후 재계산하며 fallback도 실패하면
  context_blocked로 종료한다.
- approval 거절이나 timeout: tool_denied result를 session에 기록한다.
- sandbox 준비 실패: tool을 실행하지 않고 sandbox_unavailable로 종료한다.
- 반복 action: fingerprint threshold에서 correction을 한 번 제공하고 이후
  repeated_action으로 중단한다.
- client disconnect: active run을 취소하고 durable session을 interrupted로
  checkpoint한다.
- verification 부족: 완료를 blocked 또는 unverified로 분류하며 성공으로
  위장하지 않는다.

## 8. 품질 요구사항

| ID | 요구사항 |
|---|---|
| NFR-01 | 동일 input, profile, seed에서 deterministic policy 결과가 재현되어야 한다. |
| NFR-02 | 모든 retry, recovery, compaction, approval에는 상한이 있어야 한다. |
| NFR-03 | persisted profile과 session은 versioned schema와 migration을 가져야 한다. |
| NFR-04 | Windows, Linux, macOS의 sandbox 차이는 동일한 fail-closed contract 뒤에 숨긴다. |
| NFR-05 | protocol adapter golden fixtures는 외부 network 없이 실행 가능해야 한다. |
| NFR-06 | profiler probe는 사용자 workspace를 수정하지 않아야 한다. |
| NFR-07 | clean-room provenance 누락은 capability 완료 gate를 실패시켜야 한다. |
| NFR-08 | 기존 own-agent API는 migration 기간 동안 canonical adapter를 통해 호환되어야 한다. |

## 9. 완료 정의

capability 하나는 다음을 모두 만족해야 absorbed 상태가 된다.

1. behavior contract가 capability matrix에 기록되어 있다.
2. 기존 single kernel 실제 경로에 연결되어 있다.
3. profile이나 policy로 활성화와 선택이 가능하다.
4. happy path, malformed input, timeout, cancellation, retry exhaustion test가
   있다.
5. receipt 또는 trace에서 선택과 결과를 관찰할 수 있다.
6. source inspiration과 independent implementation provenance가 있다.
7. relevant regression suite가 통과한다.

전체 목표 완료는 capability-matrix.md의 target 항목이 모두 Verified이고,
P0 security 회귀가 없으며, 동일 조건 replay가 terminal state와 artifact
digest를 재현할 때만 선언한다.
