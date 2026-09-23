# 자체질문 기반 실행 정책 실험

작성: 2026-09-22 / source: codex
상태: P1 관찰 기반과 P2 영속 기록/오프라인 평가 구현·로컬 검증 완료. 실제 모델 실험은 미완료.
읽는 순서: 이 계약 → [구현 순서](plan.md) → [검증 계약](validation.md).

## 목적과 첫 실험

사용자가 확인한 현재 코딩 성능을 출발점으로 유지한다. 실제 성능이 부족하다는 가정으로 커널을 교체하지 않는다.
검증할 가설은 “반복 실패 시 좁은 자체질문으로 개입 시점을 판단하면, 완료율을 유지하면서 사람의 방향 수정과 불필요한 재시도가 줄어든다”이다.
실사용 성능과 자기개선으로 얻는 추가 효과는 별도로 기록한다.

첫 실험은 반복 도구 실패의 관찰(shadow)에 한정한다. 추천을 프롬프트에 넣거나 도구 실행·재시도 한도·중단·권한을 바꾸지 않는다.
판단 모델은 코드 생성 모델과 분리한다. Jev는 후보 어댑터이며 필수 의존성이 아니다. 일반 소형 모델의 구조화 출력도 동일 계약으로 비교한다.
전체 대화마다 질문하지 않는다. 사용자 질문은 사용자만 정할 목표·우선순위·권한에 필요한 경우로 제한한다.

## 확인한 재사용 지점

| 기존 기능 | 근거 | 연결 방식 |
|---|---|---|
| 동일 행동·상태 반복 제한 | [run_guard.go:17](../../../internal/agent/run_guard.go#L17), Observe/ObserveResult | 기존 결정과 결과의 참조를 관찰 입력으로 사용. 별도 중복 카운터로 대체하지 않음 |
| 연속 실패 중단 | [loop.go:2690](../../../internal/agent/loop.go#L2690) | dispatch 결과 집계 후 판단 관찰. 기존 중단 조건 유지 |
| 실행 옵션·기록 인터페이스 | [run_options.go:88](../../../internal/agent/run_options.go#L88) | run-owned observer와 선택적 recorder 확장. nil은 현재 동작 |
| 도구 결과와 guard 진행 갱신 | [tool_dispatch.go:600](../../../internal/agent/tool_dispatch.go#L600) | 결과·revision·fingerprint를 이용하되 사전 실행 journal 순서 불변 |
| trace 및 span 저장 | [run_trace_recorder.go](../../../internal/server/run_trace_recorder.go), [trace.go](../../../internal/observability/trace.go) | 기존 recorder에 bounded 이벤트 연결. 새 서버·별도 실행 루프 불필요 |
| 회귀 실행 | [regression_runner.go](../../../internal/server/regression_runner.go) | 현재 Chronos/Team 실제 실행 경로. 무료 counterfactual replay로 간주하지 않음 |
| 격리 평가와 채택 | [agent_executor.go](../../../cmd/corelaycode-profile/agent_executor.go), [improvement.go](../../../internal/capabilityprofile/improvement.go) | 격리 실행·identity binding 패턴 재사용. 현재 lesson 게이트를 그대로 정책 평가에 적용하지 않음 |

현재 RunTrace/RunSpan은 진단 기록이다. 모든 분기의 workspace snapshot과 관측 가능한 행동 집합을 가진 Discovery Tree가 아니다.

## 질문과 결정 계약 v1

| ID | 질문 | yes / no / unknown |
|---|---|---|
| same_failure | 직전 실패와 현재 실패가 같은 관측 가능한 실패 유형인가? | 같은 유형 / 다른 유형 / 비교할 근거 부족 |
| new_evidence | 이번 시도에서 이전 실패 이후의 해결 판단에 관련된 새 근거가 생겼는가? | 관련 근거 추가 / 동일 근거 반복 / 관련성 판단 불가 |
| alternative_supported | 현재 증거가 다른 구체적 접근을 검토할 근거를 제공하는가? | 지지 근거 있음 / 근거 없음 / 정보 부족 |

질문은 원인 확정이나 해결 가능성을 묻지 않는다. 답마다 evidence ID를 요구하고 실제 입력에 없는 ID는 거절한다.
unknown은 근거 ID가 없어도 허용한다. yes/no는 근거가 필수이며, 입력에 이전·현재 round의 발췌가 각각 없으면 모델을 호출하지 않는다.
관측은 `runID`, monotonic sequence, decision-point ID, workspace/session revision, policy/question/schema digest, coder/judge target identity를 결합한다.
모델 입력은 현재 목적·완료 기준의 필요한 부분, 최근 두 관련 실패의 도구/상태, 제한된 진단 발췌, 변경·읽기 결과의 참조다.
digest만으로 의미 판단을 시키지 않는다. 적절한 발췌가 없으면 unknown 처리한다. 전체 transcript·환경변수·원시 인자를 무조건 전달하지 않는다.
snapshot에는 절단·누락 플래그를 둔다. 근거 부족, 잘못된 enum/ID, 응답 파싱 실패는 부정 답이 아니라 invalid/unknown이다.

응답 확률은 어댑터가 실제 제공할 때만 보관한다. 일반 LLM의 자가 confidence는 보정된 확률로 취급하지 않는다.
모델은 셸 명령·패치·권한 승격을 출력할 수 없다. 정책 코드가 검증된 답을 다음 advisory enum으로 조합한다.

| 조건(우선순위 순) | 추천 |
|---|---|
| invalid, timeout, budget 초과, 핵심 답 unknown | abstain |
| new_evidence=yes | continue |
| same_failure=yes, new_evidence=no, alternative_supported=yes | consider_alternative |
| same_failure=yes, new_evidence=no, alternative_supported=no | gather_evidence |
| 나머지 유효 조합 | continue |

추천의 정답은 후속 결과로 확인해야 한다. `consider_alternative`가 지금 접근의 실패를 증명하지 않는다.

## 관찰 실행·비용·저장

- 기본 off. 최초 연결은 시험 하네스에서 명시 주입; 아직 존재하지 않는 제품 CLI 플래그를 사용법으로 안내하지 않는다.
- 초기 trigger: 연속 all-tools-error 2회 이상 또는 RunGuard repeated-action 거절. 같은 decision-point의 두 신호는 한 번으로 합친다.
- 초기 실험값: run당 최대 3회, 동시 판단 1회, 대기 슬롯 1개, 요청당 timeout 2초, UTF-8 입력 8 KiB/출력 2 KiB 상한. 보장된 최적값이 아니라 측정 후 조정할 가설이다.
- 실행 스레드는 불변 snapshot을 비차단 제출한다. queue 포화는 dropped로 기록. run 취소/종료 시 판단 context도 취소하며 무제한 goroutine·종료 대기를 만들지 않는다.
- 종료 후 늦은 결과는 closed sink에서 폐기한다. 종료 시 미완료는 canceled/pending으로 집계한다. 실행 terminal 상태를 나중에 변경하지 않는다.
- 이벤트에는 enum·상태·지연·usage·digest·근거 ID만 저장. 원문은 자동 trace 저장 대상이 아니다. 별도 평가 corpus의 정제된 근거는 명시 수집 시에만 보존한다.
- 기존 span 배열을 직접 동시 수정하지 않고 recorder가 소유한 동기화 경계로 전달한다. 기록 실패는 기존 실행을 실패시키지 않되 evaluation에서 missing으로 센다.
- observer opt-in은 coder와 다른 외부 provider로 프로젝트 내용을 전송하는 허가가 아니다. judge provider와 전송 범위를 명시한다.
- 요청·토큰·시간 예산을 모두 기록하고, 요율이 없으면 금액은 unknown으로 표시한다. 요율이 있는 실험은 금액 상한도 설정한다.
- 같은 로컬 GPU의 경합도 성능 영향이다. 비동기라는 이유로 무영향을 주장하지 않고 off 대비 지연·처리량을 측정한다.

## 이후 단계와 변경 경계

P1 구현은 `RunOptions.ShadowJudgment`의 trusted Prepare/Judge 콜백 주입이다. Prepare는
run-owned 근거 ID를 해석하고 정제해야 하며, 원시 결과 자동 수집/정제 기능은 아직 없다.
`RunShadowJudgmentRecorder`는 run 종료 전에 최대 3개 bounded 레코드를 받는다.
기존 server recorder의 영속 저장 연결과 offline shadow-eval은 P2에 구현했다. [사용 계약](../../shadow-evaluation.md)을 따른다. 실제 호출 usage/요율/토큰 예산은 P3 어댑터에서
추가해야 하며 P1의 요청 수·직렬화 byte·timeout 상한을 토큰/금액 계측 완료로 표현하지 않는다.
두 콜백은 context 취소를 준수해야 한다. 관찰기는 종료 시 기다리지 않고 늦은 결과를 폐기하지만,
취소를 무시하는 외부 함수 자체를 Go에서 강제 종료하지는 못한다. 호출마다 goroutine을 추가하지 않고 단일 worker만 사용한다.

관찰 → 제한 개입 → 정책 후보 평가 → 기록 재생 순서다. 실제 분기 적용은 관찰 결과를 확인한 후 별도 구현한다.
개입 단계에서도 기존 permission/approval, journal, 검증·completion gate, 하드 예산은 판단 모델이 변경하지 못한다.
질문/임계값/trigger/행동 정책은 한 실험에서 한 종류씩 변경하고 후보를 평가 전에 고정한다.
기존 lesson 네 가지와 정책 후보는 서로 다른 provenance다. lesson digest에 정책을 끼워 넣거나 기존 acceptance를 완화하지 않는다.
향후 정책 적용은 immutable artifact, 정확한 대상 결합, 명시 activation과 이전 버전 복귀를 요구한다. 자동 background 학습·배포는 이번 범위 밖이다.

기록 재생은 그 시점에 공개된 근거만 입력으로 사용한다. unseen action은 unsupported로 처리하며 누락 비율도 보고한다.
새 질문을 저장된 근거에 묻는 것은 judge 비용이 드는 재평가다. 새 행동의 결과는 실제 실행 없이 추정해서 성공으로 채점하지 않는다.
사용자의 개입은 후보의 단서다. 그 뒤 성공했다는 이유만으로 인과적 정답으로 자동 채택하지 않는다.

## 참고

- [기존 RSI 계약](../../rsi.md)
- [Dream-RSI](https://dream-rsi.com/): 탐색 기록 재생과 실제 실행의 순환. Corelay 적용 효과는 미검증.
- [jev-code 스킬](https://github.com/FrancoisChastel/jev-code/blob/main/skills/jev/SKILL.md): 좁은 typed judgment의 참고. 설치·API 호출은 수행하지 않음.
