# S04 변경·커밋·복구

상태: PASS
연결: F11,F12,F13
선행: S02,S03

## 수정 시작점과 소유 범위

internal/agent/tools_advanced.go(executeGitCommit),checkpoint.go,sessions.go; internal/server/agent_session.go; cmd/proxy/tui_commands.go

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] staged B + 지정 A의 선택 커밋은 `git commit --only`로 실행하며 B의 staged 상태를 보존한다. `files` 생략은 기본 거부하고 `scope=staged` 명시 및 workspace==Git root 조건에서만 staged 전체 커밋을 허용한다. 선택 경로 partial staging은 `staged_conflict`로 거부한다. commit 전 독점 index lock과 index snapshot을 사용하고, hook 실행 뒤 HEAD·candidate index의 변경 범위·live index를 검사한다. amend·rename/delete·unborn HEAD·실패/취소·동시 staging·hook 범위 변경을 회귀 테스트로 다뤘다.

- B. [PASS] checkpoint를 run/turn·generation에 바인딩하고 SHA-256 preimage/postimage를 저장한다. 성공한 mutation 배치에서 최종 파일 postimage만 게시하며, worker는 같은 `CheckpointScope`를 공유한다. 실패 배치는 실제 preimage 복구가 확인된 파일만 정산한다.

- C. [PASS] restore preflight에서 현재 파일의 존재 여부/digest와 postimage를 비교한다. 충돌 파일은 덮어쓰지 않으며 안정 ID 목록과 선택 복구를 제공한다. 손상된 backup 한 건이 목록의 다른 안전 항목을 가리지 않는다. manifest v4는 기존 permission bits를 저장하고 복원하며 v1–v3은 거부한다.

- D. [PASS] reconcile은 durable journal에 raw input을 추가하지 않고 파일 digest/receipt 증거로 상태를 판정한다. preview token은 세션/run/checkpoint/현재 파일 증거에 결합하며 저장 직전 다시 검사한다. shell 등 파일 digest로 판단할 수 없는 side effect는 unknown으로 분리하고 명시적 수동 확인을 요구한다.

## 완료 조건

Q05/Q06. stage와 사용자 수정 보존, 삭제/새파일/부분실패/취소/재시작, symlink 교체. 복원이 실패하면 성공 표시나 reconcile 해제 없음.

## 회귀·제약

git reset --hard/clean으로 복구 금지. 이미 staged인 사용자 변경을 stash/pop으로 임의 이동 금지.

## 검증 기록

### A — PASS

- 기준 commit: `82d033bf4490033d782c97566877fed02536799e`.
- 변경 파일: `internal/agent/tools_advanced.go`, `internal/agent/tool_process.go`, 신규 `internal/agent/git_commit_index.go`, `internal/agent/git_commit_index_replace_unix.go`, `internal/agent/git_commit_index_replace_windows.go`, `internal/agent/git_commit_hooks.go`, `internal/agent/git_commit_test.go`.
- 재현→수정: 기존 동작은 선택 경로를 실제 index에 추가하고 전체 index를 커밋해 대상 밖 staged B까지 포함할 수 있었다. 선택 경로 `--only` 커밋, 명시적 `scope=staged`, partial-stage 차단, hook/HEAD/index 검증 및 실패 복구 보호를 추가했다.
- `go test ./internal/agent -run '^(TestGitCommitHookGuardIsCreatedInGitMetadataDirectory|TestGitCommitSpecifiedPathPreservesOtherStagedChanges|TestGitCommitRejectsPreCommitHookAddingOutsidePathToCandidateIndex|TestGitCommitSelectedPathFromNestedWorkspaceUsesRepositoryRelativeHookScope)$' -count=1`: PASS (exit 0).
- `go test ./... -count=1`: PASS (exit 0; 최신 변경 기준 전체 패키지).
- `GOOS=linux GOARCH=amd64 go build ./...`: PASS (exit 0).
- `git diff --check`: PASS (exit 0).
- 독립 구현 검토: PASS. Windows Git for Windows에서 실제 `.exe` hook 실행은 NOT RUN (경로 선택 검증만 수행). 실패 rollback 시 외부 작업이 도구의 임시 stage와 의미상 완전히 같은 상태를 stage한 좁은 동시성 경계는 raw diff만으로 구분할 수 있어 외부 stage 의도가 해제될 수 있다. 이 범위의 완전한 동시 staging 보존은 미해결 위험으로 기록한다.
- 당시 다음 단계: B — checkpoint run/session/generation binding과 성공 배치 postimage 기록.

### B — PASS

- 기준 commit: `82d033bf4490033d782c97566877fed02536799e`.
- 변경 파일: `internal/agent/checkpoint.go`, 신규 `internal/agent/checkpoint_scope.go`, `internal/agent/run_options.go`, `internal/agent/loop.go`, `internal/agent/subagent.go`, `internal/agent/team.go`, `internal/agent/tool_dispatch.go`, `internal/agent/checkpoint_runloop_test.go`, `internal/agent/tool_dispatch_test.go`, `internal/agent/execution_policy_runloop_test.go`, `docs/plan/corelay-reliability-roadmap/contracts.md`.
- 재현→수정: run/session/generation이 manifest에 결합되지 않고 반복 경로·병렬 worker의 복구 항목이 분리되거나 덮일 수 있었다. manifest identity key 검증, 공유 scope 직렬화, 최종 postimage 기록, 실패 배치 복구 검증 및 외부 파일 undo의 현재 Full 정책 확인을 추가했다. 부재 digest와 파일 bytes가 우연히 같아도 존재 여부까지 비교하며, 캡처 preimage가 없는 실패 경로는 정산 오류로 거부한다.
- 회귀: `go test ./internal/agent -run '^(TestDispatchCheckpointFailureMarksUnrevertedMutationAsError|TestCheckpointRejectsExternalRepositoryControlTargetAboveNestedWorkspace|TestCheckpointScopeSerializesConcurrentWorkerCaptures|TestFailedMutationBatchSettlesCheckpointAndPreservesPriorEntries|TestFailedMutationBatchDoesNotSettleAbsentSentinelContentFile|TestCheckpointSettlementRejectsUncapturedMutationPath|TestRunLoopCheckpointStoresSessionRunAndCommittedDigests|TestRunLoopEphemeralCheckpointsAreIsolatedByLiveSession|TestCheckpointPostimageUsesFinalRevisionForRepeatedPathBatch|TestRunLoopFullModeWritesExternalTempFileWithoutPerCallApproval|TestUndoCheckpointRequiresCurrentFullModeForExternalTarget|TestSubAgentWorkersShareOneCheckpointGeneration|TestTeamWorkersShareOneCheckpointGeneration)$' -count=1`: PASS (exit 0).
- `go test ./... -count=1`: PASS (exit 0; 최종 실행은 모든 패키지 통과).
- `GOOS=linux GOARCH=amd64 go build ./...`: PASS (exit 0).
- `git diff --check`: PASS (exit 0).
- 독립 재감리: PASS. sentinel digest 충돌, 누락 preimage 정산, callback/rollback 순서, 이전 캡처 보존, worker scope 직렬화, Full 외부 undo gate, manifest identity binding에서 남은 B blocker 없음.
- `go test -race`: NOT RUN. Windows 환경에서 `CGO_ENABLED=0`이며 사용 가능한 `gcc`가 없어 Go race detector가 요구하는 cgo 빌드를 사용할 수 없다.
- 다음: C — 현재 대상 존재 여부/digest가 postimage와 일치할 때만 복구하고, 충돌 목록·선택 복구 및 legacy checkpoint 거부를 구현한다.

### C — PASS

- 변경 파일: `internal/agent/checkpoint.go`, 신규 `internal/agent/checkpoint_undo.go`, `internal/agent/checkpoint_security_test.go`, 신규 `internal/agent/checkpoint_undo_test.go`, `internal/agent/loop.go`, `docs/plan/corelay-reliability-roadmap/contracts.md`.
- 재현→수정: undo가 postimage 충돌을 무시하거나 충돌 파일을 포함해 쓸 수 있었고, 복원 mode를 0644로 고정했으며 backup 손상 하나가 안전 항목의 목록/선택 복구를 막을 수 있었다. manifest v4에 preimage permission bits를 저장하고 검증한 뒤 복원하며, v1–v3은 거부한다. 기본 undo는 전체 선택 항목을 먼저 검사하고, `/undo --list`는 안정 ID·상태를 표시하며 한 항목의 backup 오류는 그 항목만 복원 불가로 분리한다. `/undo --select`는 선택 집합만 원자 preflight하고 비선택 충돌 항목을 보존한다.
- 경쟁 조건 완화: undo와 Corelay 파일 mutation batch를 직렬화한다. restore temp file을 만든 후 canonical target, parent directory identity, 존재 여부와 digest를 다시 확인하고 rename/remove 직전에 재검증한다. 외부 프로세스가 최종 확인 뒤 syscall 전 변경하는 경로 기반 TOCTOU는 Go 1.22 공통 API의 원자적 digest-CAS 부재로 남으며 `contracts.md`에 범위를 기록했다.
- 회귀: 권한 0600/0751 보존, v1/v2/v3 거부, preflight 후 파일·부모 교체 거부, backup digest 손상 분리, 충돌 집합 전체 차단, 안전 ID 선택 복구 및 비선택 항목 보존.
- `go test ./internal/agent -run '^(TestUndoCheckpointPreservesPreimagePermissions|TestUndoCheckpointRejectsInvalidPreimageModes|TestUndoCheckpointRechecksPostimageAfterInitialCheck|TestUndoCheckpointRejectsParentReplacedAfterPreflight|TestUndoCheckpointListKeepsSafeEntriesVisibleWhenBackupIsCorrupt|TestUndoCheckpointSecureRestoresValidManifest|TestUndoCheckpointDetectsCurrentPostimageConflict|TestUndoCheckpointSelectedRestorePreservesUnselectedConflict|TestUndoCheckpointRejectsBackupThatDoesNotMatchPreimageDigest|TestUndoCheckpointRejectsVersionWorkspaceAndControlState)$' -count=1`: PASS (exit 0).
- `go test ./... -count=1`: PASS (exit 0; 모든 패키지).
- `GOOS=linux GOARCH=amd64 go build ./...`: PASS (exit 0).
- `git diff --check`: PASS (exit 0).
- 독립 재검토: v4 권한 보존, backup 손상 목록 분리, 선택 복구는 통과. 최종 filesystem syscall 직전의 비협조 외부 프로세스 경합은 완전 제거되지 않았고 위 계약 제한으로 기록했다.
- 다음: D — reconcile은 raw input 없이 digest/receipt 증거로 판정하고 shell side effect의 수동 확인 상태를 분리한다.

### D — PASS

- 기준 commit: `82d033bf4490033d782c97566877fed02536799e`.
- 변경 파일: `internal/agent/checkpoint_reconciliation.go`(신규), `internal/agent/checkpoint_reconciliation_test.go`(신규), `internal/agent/sessions.go`, `internal/agent/session_lifecycle_test.go`, `internal/acpbridge/execution_journal_test.go`, `internal/server/agent_session_journal_test.go`, `internal/server/server.go`, `internal/server/session_hard_crash_test.go`, `internal/server/session_lifecycle_api_test.go`, `internal/server/session_resume_continuation_test.go`, `cmd/proxy/chat_transport.go`, `cmd/proxy/chat_transport_test.go`, `cmd/proxy/tui.go`, `cmd/proxy/tui_commands.go`, `cmd/proxy/tui_test.go`, `docs/plan/corelay-reliability-roadmap/contracts.md`.
- 재현→수정: reconcile은 증거를 평가하지 않고 interruption guard를 지울 수 있었다. checkpoint identity·backup·현재 file digest 평가, preview evidence digest, mutation lock 안의 저장 직전 재검사, stale evidence 거부, 명시적 수동 확인, content-free receipt를 추가했다. 직접 `/interrupt` API도 caller가 보낸 run/tool/call ID, digest, state, summary를 저장하지 않고 고정된 unknown marker만 기록한다.
- 회귀: preimage/postimage/divergence, 기록 전 변경, 누락·불일치·손상 checkpoint, shell unknown/manual ack, 증거 변경, 오래된 preview 후 사용자 파일 수정 보존, raw caller metadata 비저장, UI preview/confirmation, transport roundtrip.
- `go test ./internal/agent -run '^(TestAssessSessionReconciliation|TestReconciliationEvidenceDigest|TestSessionStoreInterruptionRequiresExplicitReconciliation)$' -count=1`: PASS (exit 0).
- `go test ./internal/server ./internal/acpbridge ./cmd/proxy -run '^(TestSessionAPIInterruptResumeReconcileAndClose|TestSessionAPIInterruptRejectsUnknownFieldsAndDiscardsCallerMetadata|TestSessionAPILifecycleMutationsRequireRevision|TestTUIInterruptedSessionRequiresExplicitReconcile|TestAgentStreamTransportReconciliationEvidenceRoundTrip|TestACPExecutionJournalSurvivesHardProcessExit|TestDurableExecutionJournal|Test.*HardCrash|TestDurableSessionInterruptedRunReconcilesThenContinuesWithoutReplay)$' -count=1`: PASS (exit 0).
- `go test ./... -count=1`: PASS (exit 0; 모든 패키지, internal/agent 포함).
- `GOOS=linux GOARCH=amd64 go build ./...`: PASS (exit 0).
- `git diff --check`: PASS (exit 0).
- 독립 재검토: PASS. 첫 검토에서 direct `/interrupt`의 자유 형식 summary와 식별자 입력 저장 위험을 발견해 고정 summary 및 metadata 무시를 적용했다. 재검토에서 raw runId/toolName/toolCallId/digest/state/summary가 marker에 저장되지 않음을 확인했다.
- 잔여 위험: file-mutation lock은 Corelay 내부 변경을 직렬화하지만 외부 프로세스가 최종 digest 확인 직후 파일을 수정하는 경합은 막지 않는다. `go test -race`: NOT RUN (이 Windows 환경은 CGO_ENABLED=0이고 사용 가능한 gcc가 없어 race detector 빌드를 구성할 수 없음).
- 다음: S05.A — 기존 Workstream/TeamPlan/Plan production caller를 추적해 단일 소유 상태를 정한다.
