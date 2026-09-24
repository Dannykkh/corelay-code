# 검증·실험 계약

상태: P1/P2 결정론적 회귀 및 Linux race PASS. 상세 실행 증거는 plan.md 참조.
아래 표는 전체 단계의 검증 목표다. P3 로컬 모델과 Jev의 합성 smoke는 실행했다. 실제 reviewed 사례 평가는 NOT RUN이며 표 전체 PASS가 아니다.
구현 계약은 [spec.md](spec.md), 작업 상태는 [plan.md](plan.md)가 정본이다.

## P1–P2 필수 회귀

| ID | 입력/조건 | 확인할 결과 |
|---|---|---|
| V01 | 동일 deterministic provider와 workspace, off 대 shadow | tool 순서·인자·파일 결과·terminal 동일; 시각·관찰 기록 차이는 허용 |
| V02 | 동일 실패 반복 / 같은 명령이나 revision·결과가 변경됨 | 정체와 관측된 진전 구분; guard 자체 동작 유지 |
| V03 | 일부 도구 성공인 혼합 round, 첫 실패, 비슷하지만 다른 실패 | trigger 범위 명시, 오탐 표본 보존 |
| V04 | guard 거절과 all-error trigger 동시 발생 | decision-point당 최대 1회, run 최대 3회 |
| V05 | timeout/파싱 실패/잘못된 enum/가짜 evidence ID/필수 답 누락 | invalid/abstain, 부정 답·성공으로 변환 안 함 |
| V06 | blocked judge, queue 포화, 종료와 응답 경합 | 실행 비차단, dropped/canceled 계측, 늦은 결과로 terminal 변경 안 함 |
| V07 | 판단 모델 취소/주 provider 취소/recorder 저장 오류 | 기존 취소 계약 보존, missing 관측을 성공으로 세지 않음 |
| V08 | 근거 속 명령 유도·민감값·너무 큰 발췌 | 허용 field/크기/정제 경계 확인, 원문 trace 유출·정책 변경 없음 |
| V09 | 같은 observer 재사용 시도·다른 run/session | 독립 상태/예산, 교차 evidence 참조 거절 |
| V10 | fixture tool 실행과 recorder를 포함한 integration | 이벤트가 정확한 run/sequence/policy/target과 결합 |

## 실제 모델 평가

P2는 fake-provider kernel→composite recorder→일일 trace 재개방을 검증했다.
저장 실패는 checked API의 오류와 메모리 진단의 persistence=failed로 구분한다.
shadow-eval은 명시 라벨·기록 응답만 비교하며, 실제 과제 재실행이나 성능 개선 증거를 생성하지 않는다.
합성 corpus 6개는 모든 split의 request digest와 검증 연결을 검사하는 fixture다.

- A: 현재 코딩 모델·하네스 그대로. B: 같은 실행에 deterministic 실패 규칙. C: 같은 실행에 judge 분류. P3까지 B/C는 추천 관찰만 하므로 완료율 개선의 인과적 증거로 사용하지 않는다.
- 질문별 라벨은 정제된 실제 증거로 사람이 작성한다. 애매한 사례는 unknown/불일치로 남긴다. judge 답으로 judge 정답을 만들지 않는다.
- 초기 10개 실패를 진단·질문 개발에 사용하되, 정상 작업과 개입 불필요 사례도 추가한다. 같은 실패의 여러 turn을 train/test 양쪽으로 나누지 않는다.
- task/repository/failure family 단위로 개발·선정·최종 평가를 분리한다. 후보를 본 뒤 최종 평가 과제에 맞춰 수정하면 새 평가셋이 필요하다.
- 질문별 confusion matrix, unknown/invalid/drop 비율, 근거 ID 유효성, 잘못된 개입 추천, 놓친 정체를 보고한다. 확률 제공 모델은 별도 calibration도 측정한다.
- judging 요청/토큰/금액, p50/p95 지연, coding 실행 지연과 공유 GPU 경합을 포함한다. 제공되지 않은 usage·금액은 0으로 기록하지 않는다.

## P4 효과 판단

과제별 동일 초기 commit·fixture·모델/설정·자원/예산을 고정한다. A/B/C 순서를 무작위화하고 초기 pilot은 과제·arm당 3회 반복한다. 3회만으로 통계적 확증을 주장하지 않는다.
사람 개입은 방향 수정/추가 요구/권한 결정으로 분류한다. 필수 권한 확인 감소를 좋은 성과로 계산하지 않는다.
완료율, executable acceptance, 거짓 완료, 방향 수정 횟수, 반복 실패, 시간, 전체 비용을 과제별 paired 결과와 불확실성으로 보고한다.
정상 완료 보존과 불필요한 개입을 함께 평가한다. shadow에서 좋았다는 이유로 배포하지 않는다.

안전·권한 위반과 거짓 완료는 관측 표본에서 0을 요구하되 일반적 부재 보장으로 표현하지 않는다.
첫 pilot의 보수적 gate는 기존에 성공한 평가 과제의 신규 실패가 없어야 하며, 전체 비용 상한 내에서 방향 수정 개입 또는 낭비가 감소해야 한다.
최종 반복 수·최소 유용 개선폭·비용 상한·완료율 허용 차이는 pilot으로 분산과 비용을 파악한 뒤 최종 평가 전에 고정한다. 미설정이면 자동 채택 불가다.
재시도 횟수만 늘었다고 유용한 해결을 자동 거절하지 않는다. 기존 lesson acceptance는 그대로 유지하며 정책 실험의 기준은 별도로 버전 관리한다.

## 중단·복귀 조건

모델 판단이 단순 규칙보다 낫지 않거나 지연/비용이 이득을 상쇄하면 해당 판단 경로는 off로 유지한다.
관찰 유실·증거 누출·guard 변화가 발견되면 효과 실험보다 계약 수정이 먼저다.
과거 기록에서 unsupported 비율이 높으면 replay 최적화보다 실제 격리 실행 자료 확보를 우선한다.

## 실행할 검사

P1 변경 후: 관련 agent 단위·integration 검사와 `go test -race` (지원 환경), 변경 패키지 vet.
P2 변경 후: agent/server/profiler 관련 suite, `go build ./...`, 문서 링크와 `git diff --check`.
명령·exit code·실제 범위를 각 단계 완료 시 plan.md에 기록한다. 새 코드가 없는 P0에서 Go 검사를 실행한 것처럼 표시하지 않는다.
