# API·저장·상태 계약

아래는 목표 계약이다. 기존 endpoint와 struct를 확장하는 방향으로 구현한다.
새 이름은 제안이며 동일 책임의 기존 타입이 있으면 재사용한다. JSON schema/DB를 새로 도입할 필요는 없다.

## Session / Run

Session 추가 필드:
- workspace: 기존 정규화 경로 유지
- workstreamId?: 같은 workspace의 Workstream만 참조
- planId?, planRevision?, stageId?: 같은 Workstream의 정확한 Plan definition revision과 소속 Stage를 참조; stage 단독 지정 금지. Workstream 또는 Plan을 바꿀 때 이전 Plan revision/Stage를 묵시적으로 이어받지 않는다.
- executionPolicy?: {mode: read-only|workspace|full, revision: uint64}
- provider/model: 기존 필드가 실행의 실제 대상이 되도록 연결

Workstream은 workspace별 프로젝트 목표와 진행 상태의 canonical owner다. revisioned Plan/Stage는 정확히 하나의 Workstream에 속하며, Session은 그 ID를 참조한다. TeamPlan은 검증된 plan을 실행에 투영하는 adapter/value이며 별도의 mutable approval/workflow owner가 되지 않는다. PlanAnchor는 canonical plan state의 prompt projection이다.

Run 내부 snapshot:
sessionId, expectedRevision, workspaceKey, workstreamId, planRevision,
provider/model, effectivePolicy, capabilitySnapshot, runId.
snapshot은 server/CLI/ACP/team/daemon 진입점에서 동일 resolver를 거친다.
현재 ACP는 Workstream/Plan/Stage 결합 세션의 lifecycle을 지원하지 않아 load/prompt 전에 명시적으로 거절한다. 승인·revision·stage evidence 통합 전까지 Web/API workflow 지원을 ACP 지원으로 간주하지 않는다.

정책 상향은 사용자 설정 API 또는 CLI 명시 옵션만 허용한다.
모델 tool input, skill text, recalled memory, MCP 응답은 정책 상향 근거가 아니다.
child는 부모 권한과 요청 권한 중 더 좁은 범위; parent full일 때도 child read-only는 그대로다.

## 요청 처리 순서

인증/Origin → 제한된 body decode → session revision 확인 → workspace binding 확인
→ workstream/plan 소속 확인 → 권한/모델 snapshot → active run 등록
→ 도구 실행 → 결과/receipt 저장 → session revision commit → terminal event.
workspace/model/policy 검증과 등록 사이 경쟁은 기존 registration barrier/CAS로 막는다.
오류 시 tool dispatch 이전에 종료한다. 중복 terminal/durable commit을 만들지 않는다.

기존 durableSessionId + expectedRevision을 유지한다.
workDir 생략 시 durable session의 workspace를 사용한다. 명시 workDir이 다르면 409.
legacy 비영속 호출만 서버 default workspace fallback을 허용하고 deprecation 정보를 문서화한다.
한 세션의 활성 run 중 정책/작업 소속 변경은 409. 다른 세션 실행은 방해하지 않는다.
임의 workspace field는 다른 사용자의 세션 접근 권한을 부여하지 않는다.

## 오류 계약

400 invalid_request/invalid_policy, 401 unauthenticated,
403 origin_denied/policy_denied, 404 workstream_not_found,
409 session_workspace_conflict/session_revision_conflict/active_run_conflict/restore_conflict,
413 request_too_large, 500 config_persist_failed, 503 config_unavailable.
기존 오류 코드가 같은 뜻이면 기존 코드를 유지한다.
오류 본문에 token, raw prompt, 전체 filesystem 경로를 불필요하게 노출하지 않는다.

## 인증 bootstrap

최초 설치에서 localhost용 credential을 원자적으로 생성한다.
CLI는 사용자 전용 파일/안전한 credential resolver를 사용한다.
Web 최초 연결은 짧은 수명의 일회성 bootstrap 교환을 제공하고 장기 token을 URL에 남기지 않는다.
구현 프로토콜: `POST /api/bootstrap/challenge`에서 60초 유효한 난수 challenge를 받고, `POST /api/bootstrap`의 JSON `challenge` 필드로 한 번 교환한다. challenge는 Host/Origin에 묶이며 동시 요청에서도 한 번만 소비된다. 만료·재사용·잘못된 요청은 credential 저장 전에 거부한다. 브라우저는 동시 인증 재시도를 하나의 교환으로 합친다.
same-origin UI와 허용된 개발 origin을 allowlist로 검사한다.
Host도 허용된 bind/public host와 대조한다. 불허 Origin의 실제 POST는 CORS 헤더 유무와 무관하게 거절한다.
서비스 credential과 모델 provider Authorization은 별도 처리하여 proxy 공급자 키의 의미를 바꾸지 않는다.
Origin 없는 CLI 요청도 credential 필요. OPTIONS가 요청 자체를 인증한 것으로 취급되지 않는다.
외부 bind는 명시 설정과 credential을 요구한다.
config corruption은 기존 token 없는 최초 설치와 다르며 API 접근 허용으로 fallback하지 않는다.
실제 OS user isolation이 없는 로컬 악성 프로세스까지 방어한다고 주장하지 않는다.

## 설정 저장

LoadChecked / Update 형태의 단일 소유 저장소를 우선 설계한다.
mutex만으로 별도 CLI 프로세스 쓰기 충돌은 막을 수 없다. cross-process lock 또는 revision CAS를 구현한다.
전체 read-modify-write를 직렬화한다. 임시 파일 write/flush/close → OS에 맞는 atomic replace.
Windows replace 실패에서도 이전 파일 유지. 저장 실패 전 runtime state를 성공 상태로 바꾸지 않는다.
provider secret은 credential resolver에서만 사용. prompt에는 allowlist된 비민감 설정만 포함.
Unix permission 0600; Windows는 실제 사용자 ACL 동작 확인. chmod만으로 Windows 보호를 주장하지 않는다.

## 저장 migration

아래의 승인/변경 계약도 migration과 함께 적용한다.

### 승인 증명과 full 모드 연결

현재 plugin/desktop은 일반 AutoApprove 외에 per-call approval 증명을 요구한다.
full에서도 사용자에게 같은 승인을 반복 요구하지 않되 기존 identity/digest 검증은 제거하지 않는다.
명시적으로 선택된 full 정책 revision에 근거해 broker가 해당 run/tool/input에만 유효한 자동 승인 증명을 발급한다.
승인 출처는 user-selected-full로 기록하며, 임의 caller가 boolean 값으로 우회할 수 없게 한다.
지원하지 못하는 executor는 모드 적용 불가를 명시한다. full이라고 표시한 뒤 숨은 승인 요구를 남겨 완료 처리하지 않는다.

### GitCommit 대상 의미

files 지정은 해당 경로의 현재 working tree 변경을 커밋한다. 대상 밖 staged 변경은 보존한다.
대상 경로에 기존 partial staging이 있으면 초기 구현은 staged_conflict로 중단한다.
files 생략은 staged 전체를 암묵적으로 커밋하지 않는다. scope=staged를 명시한 경우만 허용한다.
amend는 입력의 명시 선택이 필요하며 workspace 승인 규칙을 따른다. full에서는 추가 승인 없이 실행하되 index 충돌 검사는 유지한다.
커밋 뒤 대상 index가 새 HEAD와 일치하고 대상 밖 staged diff가 보존되는지도 확인한다.

### Undo 원자성과 외부 파일

기본 Undo는 선택 turn 전체를 preflight한다. 각 대상의 현재 존재 여부/digest가 postimage와 일치해야 복원 가능하며, 하나라도 충돌하면 어떤 파일도 변경하지 않고 충돌 경로를 보고한다. preimage와 일치하는 항목은 이미 복원된 상태로 취급한다. 저장된 backup도 preimage digest로 검증한다.
checkpoint manifest v4은 기존 파일의 permission bits를 preimage와 함께 저장하고 Undo가 그 값을 복원한다. v1–v3 manifest는 이전 세대 형식으로 거부한다. Undo는 Corelay 파일 mutation 배치와 직렬화하며 임시 파일을 먼저 준비한 뒤 target의 canonical path, parent identity, 존재 여부와 digest를 rename/remove 직전에 재검증한다. 외부 프로세스가 최종 검사와 파일시스템 연산 사이에 변경하는 아주 좁은 경합은 Go 1.22의 이식 가능한 경로 API에서 원자적 digest-CAS를 제공하지 않아 완전히 제거되지 않는다.
`/undo --list`는 generation에 바인딩된 안정 ID와 복원 가능/충돌/이미 복원/Full 필요/항목 손상 상태를 표시한다. 한 항목의 손상된 backup 때문에 다른 안전 항목을 숨기지 않는다. `/undo --select <id> [id ...]`는 선택 항목 전체에 같은 원자적 preflight를 적용한다. 선택하지 않은 항목은 보존하고, 선택된 성공 항목만 manifest에서 제거한다. stale ID는 거부한다.
`checkpointKey`는 canonical workspace + session digest + run ID + generation의 SHA-256 결합이다. 물리 저장 디렉터리는 세션별 최신 turn Undo를 유지하기 위해 workspace/session bucket으로 분리하고, manifest의 run/generation binding과 `checkpointKey`를 읽기·쓰기마다 검증한다. worker들은 한 `CheckpointScope`와 owner를 공유하며 preimage capture/postimage publication을 직렬화한다. 따라서 같은 workspace의 서로 다른 세션은 저장 경로를 공유하지 않고, 현재 generation과 다른 owner를 가진 mutation/postimage writer는 manifest 갱신을 거부한다. durable root run은 한 세션에 하나만 활성화된다.
반복 Edit는 마지막 성공 postimage를 사용하고 기존 file mutation batch의 PostRevision을 재사용한다.
full 외부 Read/Write/Edit도 해당 run이 실제 수정한 경로에 한해 pre/postimage manifest로 추적한다.
외부 복원은 현재 policy가 허용해야 하며 저장 당시 full 권한을 몰래 복원하지 않는다.
임의 shell/network side effect는 Undo 가능하다고 표시하지 않고 receipt에 복구 범위 밖임을 표시한다.

### Interrupted-run reconciliation

실행 시작 journal은 run/tool/call 식별자, SHA-256 input digest, 제한된 side-effect 상태만 기록하며 raw tool input/output은 저장하지 않는다. 재조정 미리보기는 세션 revision과 run에 묶인 checkpoint manifest, 현재 파일의 존재 여부·digest를 비교해 `matches_preimage`, `matches_postimage`, `diverged`, `postimage_unrecorded`, `unavailable` 상태를 반환한다. checkpoint identity 불일치·누락·손상은 `unavailable`로 닫힌다.
미리보기는 파일을 변경하지 않는다. 확인 후 reconcile은 같은 in-process file-mutation lock 안에서 평가 digest와 요약을 다시 확인하고, 미리보기 이후 파일이 바뀌었으면 guard를 해제하지 않는다. 이 lock은 외부 프로세스의 파일 변경을 직렬화하지 않으므로 외부 파일시스템 경합까지 원자적으로 막는다고 주장하지 않는다.
파일 digest가 아닌 Bash·MCP·외부 실행은 side effect를 `unknown`으로 판정하고 사용자의 명시적 수동 확인 전까지 세션을 막는다. 수동 `/interrupt` API가 받는 호환 필드는 실행 증거가 아니므로 저장하지 않으며, 고정된 식별자와 unknown 상태만 기록한다. 해제 시에는 raw content 없이 run/evidence digest, bounded file-state counts, 수동 확인 상태를 담은 session receipt를 저장한다.

### 버전 호환

기존 세션 필드 없으면 workspace 보존, workstream 미지정, full 아님으로 읽는다.
최초 유효 변경 시 새 버전으로 원자 저장. 알 수 없는 미래 버전은 쓰기 거절.
legacy checkpoint는 postimage를 추정하지 않는다. 안전한 복원 판단이 불가능하면 conflict/manual recovery.
기존 staged index와 worktree는 commit 실패/취소에도 보존한다.
blob/receipt는 기존 소유권 검사를 유지하며 project/workstream 연결이 권한 우회가 되지 않는다.

## 작업·단계 상태

Workstream active/blocked/completed/archived는 그대로 사용.
Plan은 Workstream 하위 `plans/{planId}.json`에 저장하고 definition `revision`과 lifecycle `stateRevision`을 분리한다. Session `planRevision`은 definition revision에 고정되며 status 변경은 승인 결정을 바꾸지 않는다. 수정된 승인 계획은 revision이 바뀌고 이전 승인을 재사용하지 않는다.
Task pending→running→completed 또는 failed. interrupted 실행은 재개 전에 reconcile.
completed는 acceptance evidence와 검증 결과에 연결. 모델의 “완료” 문장만으로 승격하지 않는다.
재시작 후 running을 자동 성공 처리하지 않는다. 서로 다른 session의 단계 갱신은 revision 충돌 검사를 거친다.
