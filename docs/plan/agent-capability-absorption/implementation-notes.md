# Agent Capability Absorption Implementation Notes

> 작성일: 2026-08-11
> 성격: 구현자가 따라야 할 composition, migration, test, provenance 메모
> 제약: Go와 TypeScript source는 이 설계 단계에서 변경하지 않는다.

## 1. Existing composition points

기존 구조를 대체하지 않고 다음 지점을 확장한다.

| Composition point | 현재 역할 | 최소 확장 |
|---|---|---|
| internal/agent/run_options.go:3-10 | run별 option 전달 | CompiledHarness, DurableSession, ApprovalBroker 추가 |
| internal/agent/profiles.go:5-52 | local model tool budget와 temperature | 선언형 HarnessProfile과 resolver fallback으로 승격 |
| internal/agent/loop.go:290 | 유일한 own-agent kernel 진입 | 시작 시 profile과 session snapshot을 고정 |
| internal/agent/loop.go:324 | tool schema snapshot | MCP와 plugin discovery 이후 ToolCatalog build |
| internal/agent/loop.go:458-460 | compaction 설정 | ContextPlan에서 model-aware 값 주입 |
| internal/agent/loop.go:666-678 | 최종 provider request 구성 | 완성 request 기준 budget 계측과 adapter 적용 |
| internal/agent/loop.go:814-838 | leaked tool recovery | profile-driven parser cascade 호출 |
| internal/agent/loop.go:1011-1189 | tool partition과 실행 | common safety pipeline 뒤 scheduler 분기 |
| internal/types/provider.go:40-45 | provider transport 계약 | breaking change 없이 capability descriptor를 별도 resolver로 제공 |
| internal/providers/registry.go:33-82 | provider factory | ProviderResolver의 default implementation |
| internal/server/server.go:177-181 | HTTP protocol ingress | ProtocolAdapter registry에 route 추가 |
| internal/server/server.go:2077-2226 | own-agent API composition | durable session, harness, approval broker 주입 |
| internal/agent/sessions.go:58-291 | JSON conversation persistence | versioned append store로 migration |
| internal/agent/loop_registry.go:39-208 | active run cancel과 drain | durable session과 분리 유지 |

## 2. Target component boundaries

### 2.1 AgentKernel

AgentKernel은 orchestration만 소유한다.

- durable session snapshot 읽기
- CompiledHarness 해석
- ContextPlan과 ToolCatalog 구성
- provider call
- parser and recovery
- tool safety pipeline 위임
- terminal state와 receipt 결정

AgentKernel이 직접 소유하지 않는 것:

- provider wire JSON
- OS sandbox command construction
- session file naming
- UI approval transport
- model-name pattern table
- plugin shell execution

### 2.2 HarnessProfile and CompiledHarness

HarnessProfile은 저장 가능한 선언이고 CompiledHarness는 한 run에 고정된
실행 snapshot이다.

권장 개념 계약:

    HarnessProfile
      ID
      Version
      Match constraints
      Prompt policy reference
      Tool policy reference
      Context policy reference
      Parser policy reference
      Recovery policy reference
      Verification policy reference
      Protocol preference
      Provenance reference

    CompiledHarness
      Resolved profile identity
      Immutable policy values
      Capability profile revision
      Tool catalog revision
      Context limits

CompiledHarness는 function closure나 process handle을 persistence하지 않는다.
persisted form은 ID와 version이며 runtime에서 검증 후 compile한다.

### 2.3 CapabilityProfile

CapabilityProfile은 HarnessProfile과 다르다.

- HarnessProfile: 어떻게 실행할지 정하는 policy
- CapabilityProfile: model이 실측에서 무엇을 안정적으로 할 수 있었는지에
  대한 evidence

CapabilityProfile 권장 필드:

    Provider and model identity
    Probe suite version
    Observed protocol formats
    Reliable tool count
    Practical context ceiling
    Preferred edit format
    Repetition and truncation rates
    Confidence and sample count
    Holdout status
    Created, expires, quarantined
    Raw trace digest and provenance

HarnessResolver가 CapabilityProfile을 policy 선택 입력으로 사용한다.

## 3. Resolution order

한 run의 profile은 다음 우선순위로 한 번만 선택한다.

1. explicit run override
2. compatible non-quarantined empirical profile
3. configured provider or model profile
4. current name-based profile migrated as conservative fallback
5. generic safe fallback

선택 후에는 run 중 config나 profiler 결과가 바뀌어도 CompiledHarness를
교체하지 않는다. 새 결과는 다음 run부터 적용한다.

## 4. Context planning

### 4.1 Budget equation

ContextPlan은 다음 식을 사용한다.

    usable input
      = model context window
      - reserved output
      - provider protocol overhead
      - safety margin

    request input
      = system prompt
      + tool schemas
      + messages
      + project instructions
      + plan anchor
      + RAG context
      + recalled memory
      + workstream context

request input이 usable input을 넘으면 다음 순서로 줄인다.

1. low-relevance optional RAG
2. old recalled memory
3. optional tool schemas through ToolRouter
4. large tool result를 blob reference로 offload
5. structured compaction

user objective, active plan, pending approval, edited file revision,
verification evidence는 임의로 제거하지 않는다.

### 4.2 Token estimation

- provider tokenizer가 있으면 해당 tokenizer를 사용한다.
- 없으면 protocol-aware conservative estimator를 사용한다.
- chars divided by four는 최종 fallback일 뿐 primary estimator가 아니다.
- estimate source와 confidence를 trace에 남긴다.
- provider가 actual input usage를 반환하면 estimate calibration에 사용하되
  같은 run의 immutable budget을 소급 변경하지 않는다.

### 4.3 Structured compaction

summary text 하나로 모든 상태를 대체하지 않는다.

    CompactionSnapshot
      Objective
      Active plan and current step
      Decisions
      Files and revisions
      Tool interaction digests
      Failed actions and correction
      Verification state
      Pending approval
      Durable blob references

LLM summary는 이 snapshot의 narrative field를 보조할 수 있으나 필수 state를
생성하거나 삭제할 권한이 없다. timeout, empty output, invalid shape에서는
deterministic fallback snapshot을 사용한다.

## 5. Tool catalog and routing

### 5.1 Build order

1. load core tool descriptors
2. discover configured MCP servers
3. connect and fetch MCP descriptors
4. load plugin descriptors without executing them
5. validate names and schemas
6. reject collisions
7. apply air-gap and permission visibility policy
8. apply profile tool routing
9. freeze immutable ToolCatalog

같은 catalog가 model에 보낸 schema와 runtime dispatch lookup을 제공한다.
schema에는 있지만 실행할 수 없거나, 실행할 수 있지만 schema에 없는 tool을
허용하지 않는다.

### 5.2 Deterministic routing

ranking input:

- core tool guarantee
- task token relevance
- model empirical reliability
- current phase such as plan or execute
- air-gap and sandbox availability
- context budget

tie-break는 stable catalog order를 사용한다. 16K 이하 2-stage mode에서는
첫 단계가 category만 고르고 두 번째 단계가 해당 category의 concrete tool만
노출한다.

## 6. Tool-call parsing

parser는 실행기가 아니다. parser output은 항상 catalog membership와 JSON
schema validation을 통과해야 PreparedToolCall 후보가 된다.

권장 cascade:

1. provider native structured event
2. profile-declared native text form
3. Hermes-style envelope
4. Liquid-style action form
5. fenced JSON
6. bare JSON
7. bounded truncation repair

각 parser는 다음 결과 중 하나를 반환한다.

- Parsed: valid candidate
- NotApplicable: 다음 parser로 이동
- Malformed: 해당 format이 감지됐지만 복구 실패
- Rejected: unknown tool, invalid schema, unsafe ambiguity

복수 parser가 서로 다른 call을 해석하면 실행하지 않고 ambiguous_tool_call로
model에 correction을 요구한다.

## 7. Common tool safety pipeline

PreparedToolCall이 scheduler에 들어가기 전에 다음을 순서대로 실행한다.

1. catalog identity and schema validation
2. argument normalization
3. permission decision
4. explicit approval if ask
5. command and path validators
6. workspace scope and team ownership
7. sandbox plan and readiness
8. concurrency classification
9. execution
10. rollback when supported
11. post-hook and evidence

parallel과 serial은 8번 이후에만 갈라진다. 따라서 parallel-safe라는 이유로
permission, approval, sandbox를 생략할 수 없다.

### 7.1 ApprovalBroker

Approval request 권장 필드:

    Approval ID
    Durable session ID and revision
    Active run ID
    Tool name
    Redacted normalized input
    Danger level
    Scope
    Expiry
    Remember option allowed

approve response는 동일 session revision과 approval ID에 한 번만 소비된다.
timeout, client disconnect, stale revision은 deny-equivalent다.

### 7.2 SandboxExecutor

SandboxExecutor는 OS별 구현을 숨기는 공통 계약이다.

    Prepare policy and workspace -> SandboxPlan or typed error
    Execute prepared call -> result
    Cancel active execution
    Cleanup

required isolation 준비가 실패하면 host process에서 fallback 실행하지 않는다.
plugin command는 일반 Bash보다 더 약한 경로가 아니라 같은 sandbox contract를
사용한다.

## 8. Edit waterfall

EditPolicy는 한 번의 edit request에 사용할 방법과 fallback 순서를 명시한다.

1. target file revision과 prior Read evidence 확인
2. patch-first
3. exact match edit
4. fuzzy candidates와 numbered hint
5. syntax or lint validation
6. 실패 mutation rollback
7. bounded correction feedback

whole-file rewrite는 explicit profile capability와 permission이 있을 때만
마지막 수단으로 허용한다. fuzzy fallback은 unique target을 증명하지 못하면
수정하지 않는다.

## 9. Repetition and completion

ActionFingerprint 입력:

- tool name
- normalized argument digest
- target artifact revision
- preceding failure code
- active plan step

동일 fingerprint가 반복되더라도 target revision이 변하면 새로운 action으로
볼 수 있다. 단순 whitespace나 JSON key order 차이는 같은 action으로 본다.

CompletionContract는 다음을 분리한다.

- model claim
- Definition of Done
- observed tool outcomes
- verification evidence
- evidence policy

model이 done이라고 말해도 contract가 충족되지 않으면 verified가 아니다.

## 10. Durable session design

### 10.1 Identity

- DurableSessionID: conversation and recovery identity
- ActiveRunID: live cancellation and concurrency identity
- Revision: atomic append sequence
- ForkID: new DurableSessionID with parent ID and parent revision

ID는 client path나 title을 포함하지 않는 opaque random value다. workspace
directory는 canonical path의 digest로 결정하고 original path는 metadata로만
저장한다.

### 10.2 Persistence

권장 persisted records:

    Session metadata
    Append-only turn log
    Checkpoint snapshot
    Durable blobs
    Linked receipts
    Recovery markers

atomic write는 temp file과 verified replace를 사용한다. append 충돌은
expected revision mismatch로 반환하고 마지막 writer가 조용히 덮어쓰지
않는다.

### 10.3 Resume and fork

- resume은 마지막 committed revision에서 시작한다.
- interrupted tool의 side effect가 불명확하면 재실행하지 않고 reconciliation
  상태를 먼저 만든다.
- fork는 parent log를 수정하지 않는다.
- blob은 immutable digest로 공유할 수 있지만 retention reference count가
  필요하다.

## 11. Protocol adapter boundary

kernel canonical types는 provider wire type이 아니다.

IngressAdapter responsibilities:

- parse request
- validate protocol version
- map session and approval operations
- convert to canonical request
- convert canonical events to client protocol

ProviderAdapter responsibilities:

- convert canonical provider call to upstream wire request
- convert upstream stream frames to canonical events
- preserve usage and response headers

필수 adapter:

- Anthropic Messages
- OpenAI Chat Completions
- OpenAI Responses
- Gemini upstream
- ACP

parallel tool-call ID와 content block index는 golden fixture로 검증한다.
Gemini history는 function call과 function response를 다음 request까지
보존해야 한다.

## 12. CapabilityProfiler

### 12.1 Probe isolation

profiler는 사용자 workspace를 사용하지 않는다. versioned fixture를
disposable sandbox에 복제하고 outbound target은 선택한 provider로 제한한다.

### 12.2 Probe scoring

각 probe는 binary pass 하나가 아니라 다음을 기록한다.

- attempts and successes
- malformed rate
- retry count
- latency
- context size
- tool catalog size
- artifact digest
- false-done
- recovery path

sample이 적거나 분산이 크면 confidence를 낮춘다. profile candidate는 별도
holdout fixture를 통과해야 active가 된다.

### 12.3 Quarantine

다음 경우 candidate를 quarantine한다.

- holdout regression
- profile schema version mismatch
- model digest or serving parameters change
- probe trace missing
- confidence below threshold
- safety probe failure

quarantine 시 conservative fallback을 사용하며 기존 verified profile을
조용히 덮어쓰지 않는다.

## 13. Evidence and receipt

terminal states:

- verified
- partially-verified
- unverified
- blocked

receipt 권장 필드:

    Provider and model
    Harness profile ID and version
    Capability profile ID and confidence
    Durable session and run IDs
    Context budget summary
    Tool catalog digest
    Parser and recovery path
    Permission, approval, sandbox outcome
    Edited artifact digests
    Verification command and exit status
    Terminal state and failure code
    Provenance references

저장 금지:

- raw API key
- Authorization header
- password or token
- private key
- full prompt
- unbounded tool output

redaction은 serialization 이전에 수행하고 test corpus로 검증한다.

## 14. Clean-room provenance

capability마다 다음 record를 남긴다.

    Capability ID
    Behavior inspiration URL
    Observation date
    Behavior contract summary
    Non-copied constraints
    Independent Corelay Code design owner
    Implementation files
    Tests and fixtures
    Reviewer and verification date

source inspirations:

- Open Interpreter:
  https://github.com/openinterpreter/openinterpreter
- SmallCode:
  https://github.com/Doorman11991/smallcode

금지:

- source file copy
- line-by-line translation
- fork, vendor, embed
- original internal identifiers의 무근거 재사용
- upstream test fixture의 저작권 있는 본문 복제

허용:

- 공개 behavior와 protocol을 독립 test로 표현
- 표준 wire schema를 공식 사양에 따라 구현
- failure scenario를 자체 최소 fixture로 재작성
- provenance link와 비교 결과 기록

## 15. Test architecture

| Suite | 목적 |
|---|---|
| harness resolution table | precedence와 immutable compile |
| protocol golden | request, stream, tool ID, usage conversion |
| parser corpus | native, Hermes, Liquid, fenced, bare, malformed |
| context fixtures | 16K, 32K, 128K, 1M 예산과 compaction |
| tool safety regression | serial, parallel, deny, ask, timeout, sandbox |
| edit waterfall | patch, exact, fuzzy, lint fail, rollback |
| durable session crash | traversal, collision, interrupted write, resume, fork |
| profiler fake provider | deterministic probes, confidence, quarantine |
| evidence corpus | terminal states, secret redaction, artifact digest |
| lifecycle table | every terminal path and exactly-once hooks |

외부 service가 필요한 integration test와 offline deterministic test를
분리한다. capability 완료 gate는 offline suite 없이 외부 model 결과에만
의존하지 않는다.

## 16. Migration strategy

1. P0 fixes를 기존 behavior와 분리된 regression으로 먼저 착륙한다.
2. default behavior를 재현하는 CompiledHarness를 도입한다.
3. loop 내부 하드코딩을 하나씩 policy 호출로 이동한다.
4. protocol adapter를 기존 Anthropic route 앞에 적용해 golden parity를
   확인한다.
5. durable session을 opt-in migration으로 읽고 새 format으로 atomic write한다.
6. new parser와 router를 profile별로 활성화한다.
7. profiler는 manual trigger로 검증 후 automatic onboarding에 연결한다.
8. 모든 capability가 Verified가 되면 legacy profile and session path를
   migration policy에 따라 제거한다.

이 순서는 기능을 축소하는 단계가 아니다. 최종 target 전체를 유지하면서
각 중간 commit이 검증 가능한 상태가 되도록 의존성을 정렬한 것이다.
