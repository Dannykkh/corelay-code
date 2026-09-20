# 독립 검토와 반영

## 최신 감리 정정 — 2026-09-20

[Argos 보고서](verify-report.md): CONDITIONAL. A01–A10 로컬 결함을 수정하고 실제 lifecycle·bootstrap·압축 회귀를 검증했다. ACP workflow 결합 세션은 lifecycle 지원 전까지 명시 거절한다. 아래 과거 G1/G3 PASS는 해당 실행 범위의 기록이며 전수 API·플랫폼·보안 검증 완료를 의미하지 않는다.

2026-09-13 / source: codex
검토자: 기존 safety_audit 서브에이전트. 코드 읽기 전용, 런타임 재현 없음.
별도 Luna 검토 인스턴스 생성은 thread limit으로 실행되지 않았다. Luna가 검토했다고 주장하지 않는다.

| 검토 내용 | 반영 |
|---|---|
| atomic rename만으로 lost update 해결 불가 | S01/contracts에 cross-process transaction 명시 |
| CORS 헤더만 제거하면 실제 POST 차단 아님 | 인증/Origin/Host 계약과 Q02 보강 |
| 실제 기본 AutoApprove는 moderate | findings 정정 |
| plugin/desktop per-call proof가 별도 | full 정책으로 broker 증명 발급, identity 검증 유지 |
| Git partial staging 의미 불명확 | working tree 의미, partial staging conflict, 명시 staged scope |
| Undo postimage+generation+전체 preflight | 외부 파일 포함 contracts에 명시 |

## 남은 검토

G1: auth bootstrap/full-policy proof 구현 독립 검토.
G2: multi-project 실제 CLI/Web 흐름 검토.
G3: migration/package smoke/현실 과제 평가 검토.
계획 작성 시 이 구현 검토들은 NOT RUN이다.

## S13 게이트 기록 — 2026-09-20

- G1: PASS. `go test ./internal/approval -count=1`, `go test ./internal/acpbridge ./internal/server -run 'Approval|ExecutionJournal|SessionAPI.*(Revision|Lifecycle|HardCrash)|RootReportsBuildMetadata' -count=1`, 관련 `internal/agent` execution-policy/approval/context/path 회귀와 전체 `go test ./... -count=1`가 통과했다. Chronos full-mode fixture는 16K context에서 정책 시스템 프롬프트가 overflow되던 harness를 32K로 조정해 실제 Write dispatch까지 도달하도록 했다.
- G2: PASS 범위와 미실행 범위를 분리했다. `go test ./cmd/proxy -run 'Chat(Session|OneShot|RejectsExplicitWorkspaceMismatch|ReloadsSession|JSONL|MachineOutput)|TUI|ManagedChatConcurrentCLIProcessesShareOneChild' -count=1`가 통과했다. 프로젝트 전환·durable session·TUI/JSON output 회귀는 근거가 있으나, 사람이 직접 수행하는 interactive terminal 사용자성 평가는 이 세션에서 실행하지 않았다.
- G3: PASS 범위와 미실행 범위를 분리했다. `go test ./internal/capabilityprofile ./cmd/corelaycode-profile ./internal/updater -count=1`, `go test ./cmd/proxy -run 'Version|Doctor|Update' -count=1`, 전체 `go test ./... -count=1`, Git Bash `scripts/roadmap-qa.sh`가 통과했고, Windows self-update/rollback integration fixture도 통과했다. Q01~Q16 전체 OS hosted run과 실제 release artifact publish/download는 NOT RUN이다.
