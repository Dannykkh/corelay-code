# Domain Dictionary

> 생성일: 2026-08-11
> 도메인: Corelay Code agent capability absorption
> 버전: v1.0
> 대상 청중: 설계자, 구현자, 검증자

이 문서는 agent capability absorption 계획의 규범 용어를 정의한다.
영문 식별자와 한글 문서 표현은 아래 표를 따른다.

## 핵심 용어

| 규범 용어 | 한글 표기 | 영문 식별자 | 정의와 경계 |
|---|---|---|---|
| Agent Kernel | 에이전트 커널 | AgentKernel | own-agent의 plan, provider call, tool, verification 상태 전이를 조정하는 유일한 loop. provider별 loop를 뜻하지 않는다. |
| Harness Profile | 하네스 프로파일 | HarnessProfile | 실행 정책을 선언하는 저장 가능한 정의. 실측 결과 자체가 아니다. |
| Compiled Harness | 컴파일된 하네스 | CompiledHarness | 한 run 시작 시 resolve되어 끝까지 고정되는 immutable policy snapshot. |
| Harness Resolver | 하네스 해석기 | HarnessResolver | override, empirical profile, configured profile, fallback 순으로 CompiledHarness를 선택하는 구성요소. |
| Capability | 능력 | Capability | model 또는 runtime이 특정 행동 계약을 안정적으로 수행하는 성질. 기능 파일 존재와 다르다. |
| Capability Probe | 능력 탐침 | CapabilityProbe | 한 capability를 isolated fixture에서 측정하는 재현 가능한 실험. |
| Capability Profile | 능력 프로파일 | CapabilityProfile | probe 결과, confidence, provenance를 담은 model 실측 기록. HarnessProfile과 구분한다. |
| Capability Profiler | 능력 프로파일러 | CapabilityProfiler | probe를 실행, 채점, holdout 검증하고 CapabilityProfile을 저장하는 runtime. |
| Provider | 프로바이더 | Provider | model service로 canonical call을 전달하는 transport 대상. Harness와 동의어가 아니다. |
| Protocol Adapter | 프로토콜 어댑터 | ProtocolAdapter | 외부 wire request와 event를 canonical form으로 양방향 변환하는 경계. |
| Canonical Request | 표준 내부 요청 | CanonicalRequest | protocol과 provider에 독립적인 kernel 입력. Anthropic request type 자체를 뜻하지 않는다. |
| Tool Catalog | 도구 카탈로그 | ToolCatalog | 한 run에서 schema가 노출되고 실제 실행 가능한 tool의 immutable snapshot. |
| Tool Router | 도구 라우터 | ToolRouter | task, context, capability에 따라 ToolCatalog의 노출 subset을 결정하는 policy. |
| Tool-call Parser | 도구 호출 파서 | ToolCallParser | native 또는 text 형식 응답에서 schema 검증 전 tool-call 후보를 추출하는 구성요소. 실행기는 아니다. |
| Prepared Tool Call | 준비된 도구 호출 | PreparedToolCall | catalog, schema, permission, approval, validator, scope, sandbox 준비를 통과한 실행 후보. |
| Common Tool Safety Pipeline | 공통 도구 안전 파이프라인 | ToolSafetyPipeline | serial과 parallel tool이 공유하는 실행 전후 trust boundary. |
| Permission Decision | 권한 결정 | PermissionDecision | allow, deny, ask 중 하나인 policy 결과. ask는 approval과 다르다. |
| Approval | 사용자 승인 | Approval | ask에 대해 사용자가 명시적으로 반환한 approve 또는 deny 결과. |
| Sandbox | 샌드박스 | Sandbox | tool process와 filesystem, environment, network 범위를 OS 수준에서 제한하는 실행 경계. |
| Context Plan | 컨텍스트 계획 | ContextPlan | context window, output reserve, protocol overhead, 각 입력 항목 budget을 계산한 결과. |
| Compaction Snapshot | 압축 스냅샷 | CompactionSnapshot | objective, plan, tool pair, file revision, verification을 구조적으로 보존한 압축 상태. |
| Durable Session | 영속 세션 | DurableSession | create, append, resume, fork, interrupt, recover가 가능한 대화와 실행 상태 단위. |
| Active Run | 활성 실행 | ActiveRun | 현재 진행 중이며 cancel과 concurrency cap의 대상인 실행. DurableSession과 별도 ID를 가진다. |
| Session Revision | 세션 리비전 | SessionRevision | atomic append의 순서를 나타내는 monotonic version. |
| Session Fork | 세션 분기 | SessionFork | parent session의 특정 revision에서 새 DurableSession을 만드는 작업. |
| Recovery Policy | 복구 정책 | RecoveryPolicy | parser repair, edit fallback, retry, repetition stop의 순서와 budget을 정의한다. |
| Completion Contract | 완료 계약 | CompletionContract | Definition of Done과 필요한 verification evidence를 구조화한 완료 조건. |
| Evidence Gate | 증거 게이트 | EvidenceGate | observed evidence로 terminal state를 verified, partially-verified, unverified, blocked 중 하나로 판정한다. |
| Receipt | 실행 영수증 | Receipt | redacted metadata, artifact digest, verification outcome, provenance를 담은 compact proof. |
| Provenance Record | 출처 기록 | ProvenanceRecord | behavior inspiration과 독립 설계, 구현, test의 연결 기록. source code copy 기록이 아니다. |

## 관계도

    flowchart LR
        HR[Harness Resolver] --> CH[Compiled Harness]
        CP[Capability Profile] --> HR
        HP[Harness Profile] --> HR
        CH --> AK[Agent Kernel]
        AK --> TC[Tool Catalog]
        AK --> CT[Context Plan]
        AK --> DS[Durable Session]
        AK --> EG[Evidence Gate]
        PA[Protocol Adapter] --> AK
        TC --> TS[Tool Safety Pipeline]
        EG --> RC[Receipt]

## 구분이 중요한 용어

| 혼동 후보 | 구분 |
|---|---|
| HarnessProfile / CapabilityProfile | 전자는 실행 policy, 후자는 model 실측 evidence다. |
| DurableSession / ActiveRun | 전자는 복구 가능한 영속 상태, 후자는 취소 가능한 현재 실행이다. |
| PermissionDecision ask / Approval | ask는 질문 필요 상태, Approval은 실제 사용자 응답이다. |
| ToolRouter / Provider router | ToolRouter는 노출 tool을 고르고 provider router는 model service를 고른다. |
| Parser recovery / Tool execution | parser는 후보를 만들 뿐 permission과 sandbox 전에 실행하지 않는다. |
| Compaction / truncation | compaction은 필수 상태를 보존하고, truncation은 bounded content 축소다. |
| Receipt / raw trace | receipt는 redacted compact proof이며 전체 prompt와 tool output을 담지 않는다. |

## 금지 표현

| 금지 또는 지양 | 대신 사용 | 이유 |
|---|---|---|
| agent loop들, provider loop | Agent Kernel | own-agent loop는 하나다. |
| model profile | HarnessProfile 또는 CapabilityProfile | policy와 evidence가 모호해진다. |
| session | DurableSession 또는 ActiveRun | persistence와 live execution이 혼동된다. |
| auto allow | explicit Approval | ask를 승인으로 오해하게 만든다. |
| safe tool | concurrency-safe tool 또는 approved tool | concurrency와 trust를 혼동한다. |
| memory compression | CompactionSnapshot | durable memory와 transient context를 혼동한다. |
| done | verified, partially-verified, unverified, blocked | 완료 증거 수준이 사라진다. |
| upstream implementation 흡수 | clean-room behavior absorption | source 복사를 암시한다. |

## 동사 규칙

| 동사 | 의미 |
|---|---|
| resolve | 입력과 precedence로 immutable 실행 policy를 선택한다. |
| compile | 선언형 profile을 한 run의 검증된 snapshot으로 만든다. |
| probe | isolated fixture로 capability를 측정한다. |
| compact | 필수 상태를 보존하며 context 사용량을 줄인다. |
| resume | 동일 DurableSession의 committed revision에서 계속한다. |
| fork | parent revision에서 별도 DurableSession을 만든다. |
| verify | 외부 signal로 contract 충족을 입증한다. |
| absorb | behavior contract를 독립 구현하고 kernel에 연결해 검증한다. |

## 변경 이력

| 날짜 | 변경 | 이유 |
|---|---|---|
| 2026-08-11 | v1.0 초안, 핵심 용어 30개 확정 | single-kernel clean-room capability absorption 설계 |
