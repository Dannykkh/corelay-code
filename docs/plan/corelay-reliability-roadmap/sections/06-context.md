# S06 탐색·문맥 보존

상태: PASS
연결: F16,F17
선행: S05

## 수정 시작점과 소유 범위

internal/agent/loop.go,compact.go,context_plan.go,plan_anchor.go,session_memory.go; 관련 *_test.go

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] read-only weighted budget 종료의 실제 prompt/tool payload를 fake provider로 재현한다.

- B. [PASS] budget을 무한히 늘리는 대신 bounded continue/partial answer/사용자 stop 이유를 명시한다. 중요한 남은 read가 있으면 추가 탐색할 수 있는 정책을 둔다.

- C. [PASS] CompactionSnapshot에 최신 objective/정정/constraints/plan revision/decisions/evidence를 실제 caller에서 전달한다. source 선택은 최근성과 중요도 중심, tool pair 무결성 보존.

- D. [PASS] UTF-8 경계·토큰 예산·대형 tool blob 참조를 검증한다. 압축 전후 동일 지시 보존을 비교한다.

## 완료 조건

Q08. 중간 사용자 정정이 오래된 지시보다 우선; 5회 초과 탐색 과제가 필요한 근거까지 도달; context window 초과/무한 loop 없음.

## 회귀·제약

가짜 provider 테스트만으로 모델 추론 품질 향상을 주장하지 않는다. 압축 실패 때 유효 history를 파괴하지 않는다.

## 검증 기록

### A — PASS

- 기준: S05가 선행 PASS. 실제 경로는 `internal/agent/loop.go`의 `iterationWeight` → `exploreScore` 경계 → history collapse → 다음 `MessagesRequest`다. payload를 저장하는 기존 `completionLoopProvider`를 재사용했다.
- 구현: `internal/agent/loop_readonly_budget_test.go`에 fake-provider run loop 회귀를 추가했다. config budget=2에서 LS(0.5) → Read(1) → LS(0.5) 뒤 네 번째 요청을 캡처하고, 단일 user 메시지에 최신 질문·모든 LS/Read 결과·sentinel 내용이 들어가며 이전 도구 payload가 history 형식으로 남지 않는 것을 확인한다. S06.B에서 이 요청은 제한된 read-only 도구를 유지하도록 확장했다.
- 초기 관찰 결함: history가 접힐 때 마지막 사용자 질문만 넣어 그 전 user turn의 명시적 제약("Keep the API authorization boundary unchanged")은 실제 요청에서 빠졌다. S06.C에서 실제 loop payload 및 다중 압축 회귀를 추가해 보존 기대치로 전환했다. 현재 가중치는 결과 크기·성공 여부가 아닌 도구 이름만 사용한다.
- `go test ./internal/agent -run '^TestRunLoopReadOnlyWeightedBudgetSendsCollapsedPayload$' -count=1 -v`: PASS (exit 0).
- 다음: B — 종료 이유·부분 응답·제한된 추가 탐색 정책을 정하고 실행 경로에 연결한다.

### B — PASS

- 구현: 초기 weighted budget에서 transcript를 증거 digest로 한 번 접고, completion control을 포함한 read-only tool catalog만 유지한다. 사용자가 특정한 미해결 근거를 더 확인할 수 있게 기본 2 weighted follow-up rounds를 허용한다. 두 번째 한도를 넘으면 초기 checkpoint prompt와 추가 tool-result pair를 보존해 한 번 더 접고 탐색 도구를 제거한다. 강제 종료 prompt는 가장 나은 부분 답변·미확인 사항·budget 종료를 명시하고, status event에도 stop reason을 보낸다. 최대 반복 횟수는 기존 harness limit가 계속 적용된다.
- 결함과 수정: route state의 원본 catalog가 남아 있으면 성공한 후속 read 뒤 `observeDispatch`가 전체 catalog를 다시 열 수 있었다. checkpoint에서 route state의 현재 catalog뿐 아니라 widening 기준인 `base`도 read-only 도구로 영구 제한하고, 이후 provider request와 immutable dispatch allow-list가 같은 제한 catalog에서 만들어지게 했다.
- 회귀: 가중 한도·2회 follow-up·부분 답변·종료 이유를 확인하고, two-stage routing의 checkpoint 뒤 후속 read 이후에도 provider catalog가 read-only인지 확인한다. 모델이 숨겨진 `Write` 호출을 보내더라도 immutable dispatch allow-list가 이를 거부해 파일을 만들지 않는 것도 검증했다. deterministic/two-stage route-state 양쪽에서 이후 widening 한계가 read-only인지 확인했다.
- `go test ./internal/agent -run 'TestToolRoutingReadOnlyRestrictionLimitsFutureWidening|TestRunLoopReadOnlyCheckpointKeepsRoutedCatalogAndDispatchReadOnly|TestRunLoopReadOnlyWeightedBudgetSendsCollapsedPayload|TestRunLoopReadOnlyContinuationIsBoundedAndReportsPartialStop' -count=1 -v`: PASS (exit 0).
- 이 테스트는 요청 payload와 도구 경계를 검증하며 모델 답변 품질 향상을 증명하지 않는다.
- 독립 검토: `/root/s06b_review`가 route base/active 제한, `observeDispatch` 이후 provider catalog와 dispatch allow-list 정렬, 숨겨진 Write 차단을 확인했다. 검토자는 수정하지 않았다.
- 독립 검토 명령 `go test ./internal/agent -run 'TestToolRoutingReadOnlyRestrictionLimitsFutureWidening|TestRunLoopReadOnlyCheckpointKeepsRoutedCatalogAndDispatchReadOnly' -count=1 -v`: PASS (exit 0).
- 다음: C — 실제 compaction caller에서 최신 사용자 목적·정정·제약과 Workstream Plan revision/결정/증거를 연결하고, history source/도구 쌍 보존을 검증한다.

### C — PASS

- 기준 commit: `82d033b` (작업 트리에는 기존 사용자 변경이 있어 커밋·정리하지 않음).
- 실제 caller: Agent RunLoop가 최근 user 지시, run-local evidence, PlanAnchor, typed Workstream/Plan 상태를 공통 `CompactionState`로 전달한다. Team worker는 Plan 실행 소유권 binding 없이 typed context를 받는다. Agent·Team·Chronos의 서버 경로는 정본 Workstream/Plan에서 typed context를 구성한다.
- 구현: 스냅샷은 현재 질문과 canonical Plan objective를 분리하고, Workstream objective/constraints/decisions/verification, Plan ID·revision·state revision·stage, 최신 stage attempt 상태와 안전한 receipt/criteria digest, run evidence digest를 담는다. 최근 narrative source를 우선하며 tool pair grouping은 유지한다. 이전 structured snapshot의 검증된 user instruction 목록도 다음 압축에 이어 복원한다.
- 결함과 수정: 반복 압축에서 이전 스냅샷의 user 제약·정정이 새 지시 목록에서 사라지는 회귀를 재현하고 복원 경로를 추가했다. 실제 Plan 저장소의 bare 64-hex acceptance digest가 prefix-only sanitizer에서 사라지는 결함을 독립 리뷰로 찾아 bare/prefixed 입력을 `sha256:` 표기로 정규화했다. 실패한 최신 stage attempt가 있을 때 과거 성공 attempt의 acceptance evidence를 현재 증거처럼 내보내지 않도록 했다.
- 실제 경로 회귀: `TestRunLoopCompactionCallerPreservesUserAndWorkstreamState`는 가짜 provider·실제 RunLoop로 압축 뒤 다음 provider payload에서 이전 constraint, 최신 correction, 질문, Workstream/Plan 상태와 run-local verification을 확인한다. `TestDeterministicCompactionPreservesLatestInstructionsAndDurableWorkflowState`는 두 번째 압축 뒤에도 지시가 이어지는지 확인한다. 서버 projection 테스트는 stored bare digest의 bounded snapshot 보존, 선택 단계 유지, 최신 실패 attempt에 과거 증거가 섞이지 않는 점을 검증한다.
- 테스트 `go test ./internal/agent -run 'TestDeterministicCompactionPreservesLatestInstructionsAndDurableWorkflowState|TestCompactionNarrativeSourcePrefersRecentHistory|TestRunLoopCompactionCallerPreservesUserAndWorkstreamState' -count=1`: PASS (exit 0).
- 테스트 `go test ./internal/server -run '^TestWorkstreamCompactionContextProjectsCanonicalPlanState$' -count=1 -v`: PASS (exit 0).
- 영향 패키지 `go test ./internal/agent ./internal/server -count=1`: PASS (exit 0, agent 115.068s; server 9.952s).
- 독립 검토 `/root/s06c_trace`: bare-hash 정규화, Agent/Team/Chronos wiring, stage tail, 반복 압축 지시 복원을 확인했고 blocker 없음. 검토자는 파일을 수정하지 않았다. 검토 중 발견된 digest 불일치는 반영 후 focused 테스트와 패키지 suite에서 재검증했다.
- `git diff --check`: PASS (exit 0).

### D — PASS

- 기준 commit: `82d033b` (기존 사용자 변경이 포함된 작업 트리는 보존하고 커밋하지 않음).
- UTF-8·지시 보존: 긴 한국어 정정의 시작과 끝을 byte cap 안에 함께 남기며 중간은 생략하고, 실제 RunLoop 다음 provider payload와 두 번째 deterministic compaction에서도 마지막 정정이 남는 것을 확인했다. 지시 항목은 최대 8개(최초 지시 + 최신 7개), 항목당 600바이트로 제한한다. 9개 이상이면 중간 지시가 빠질 수 있으므로 이 경계를 테스트로 고정했다. Workstream 제약·결정은 각각 최대 12개·300바이트다.
- tool result: 4096바이트는 inline으로 유지하고 4097바이트부터 old paired result를 bounded preview로 바꾼다. 저장소가 없거나 저장에 실패한 preview는 `reload=unavailable`로 표시하며 durable `tool-result://` 참조로 오인하지 않는다. 실제 `SessionMemory` 저장 경로는 planner의 result binding, compaction digest, `LoadToolResult` 원문 복구까지 연결해 확인했다.
- 예산: 실제 RunLoop에서 token estimator가 최종 provider request와 동일한 요청을 받는 것을 확인했다. oversized narrative 후보는 deterministic fallback이 정확히 window에 맞으면 대체하고, fallback이 1 token 초과하면 요약 provider를 호출하지 않고 차단한다.
- 회귀 명령: `go test ./internal/agent -run 'TestBoundHistoricalToolResults|TestCompactionPreservesUTF8InstructionTailAndSnapshotBounds|TestUserInstructionBoundKeepsOriginalAndSevenMostRecent|TestDeterministicCompactionPreservesLatestInstructionsAndDurableWorkflowState|TestRunLoopCompactionCallerPreservesUserAndWorkstreamState|TestPlanContextCompactionKeepsSessionMemoryBlobLoadableAtBoundary' -count=1 -v`: PASS.
- 예산 회귀: `go test ./internal/agent -run 'TestCompactPlannedContext(FallsBackWhenVerboseNarrativeIsOneTokenOver|BlocksWhenDeterministicFallbackIsOneTokenOver)' -count=1 -v`: PASS.
- 독립 검토 `/root/s06d_final_review`: UTF-8 byte bounds, 실제 durable reload 경로, over-budget fallback, no-provider block을 검증했고 추가 blocker 없음. 검토 중 확인한 지시 8개 상한과 중간 항목 손실 가능성을 이 절에 기록했다.
- 영향 패키지 `go test ./internal/agent ./internal/server -count=1`: PASS (agent 105.019s; server 7.236s).
- `git diff --check`: PASS (exit 0).
- 다음: S07 — `sections/07-skills.md`와 선행 S02/S03·공통 계약을 읽고 스킬 출처·로딩 경로의 첫 하위 TODO를 시작한다.
