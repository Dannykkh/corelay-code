# Handoff: AniClew Full-Screen Terminal UI

## Session Metadata
- Created: 2026-08-12 10:25:25 KST
- Project: `D:\git\aniclew`
- Branch: `main`

## Current State Summary
AniClew의 기존 `/api/agent` HTTP/SSE 경계를 재사용하는 풀스크린 Agent Workbench TUI를 완성했다. 실제 TTY의 `aniclew chat`은 TUI로 진입하고 `aniclew tui`도 같은 화면을 연다. non-TTY, `TERM=dumb`, `-p`, `-plain`은 bounded plain mode를 유지한다. 별도 agent kernel은 만들지 않았으며 durable session CAS, approval, cancellation, typed terminal, reconciliation을 기존 서버 계약에 결합했다.

## Work Completed
- [x] Bubble Tea 기반 반응형 transcript, composer, command palette, 상태 rail, approval/reconcile modal 구현
- [x] durable session save-before-run, revision update, load/fork/rename/close/delete/reconcile 연결
- [x] active runtime ID와 durable session ID 분리 및 cancel exactly-once 구현
- [x] bounded SSE/HTTP error, UTF-8 및 terminal control sanitization, token redaction 구현
- [x] 작은 화면 safety lock, approval expiry, run-generation stale result 차단 구현
- [x] 서버 전역 provider/model/router 변경과 loop registration 사이 원자 active-run gate 구현
- [x] README 사용법 및 기존 단일 `cmd/proxy` 배포 경로 반영

### Files Modified
| File | Changes |
|------|---------|
| `cmd/proxy/tui.go` | TUI reducer, event ordering, approval/cancel/session state |
| `cmd/proxy/tui_commands.go` | command palette 및 durable session actions |
| `cmd/proxy/tui_view.go` | 반응형 Agent Workbench 렌더링과 safety lock |
| `cmd/proxy/chat_transport.go` | UI-neutral bounded HTTP/SSE transport |
| `cmd/proxy/chat.go` | TTY TUI 진입, secure plain fallback |
| `cmd/proxy/main.go` | `tui` alias와 root help |
| `internal/agent/loop_registry.go` | config mutation registration barrier |
| `internal/server/server.go` | active-run 중 global config mutation 원자 차단 |
| `go.mod`, `go.sum` | Go 1.22 호환 Charm TUI dependencies |
| `README.md` | TUI 실행법, 키맵, 보안·session semantics |

### Decisions Made
| Decision | Rationale |
|----------|-----------|
| 기존 HTTP/SSE transport를 composition point로 사용 | UI가 agent kernel, approval broker, session store를 중복 구성하지 않게 한다. |
| Bubble Tea v1.3.4/Bubbles v0.20.0/Lip Gloss v1.1.0 | 프로젝트의 Go 1.22 지원을 유지하면서 필요한 viewport/input/alt-screen 기능을 제공한다. |
| `done`만 plain success terminal로 인정 | EOF와 `stream_end`의 부분 응답을 성공으로 오인하지 않는다. |
| global config mutation을 LoopRegistry barrier 안에서 수행 | active-run 조회와 model 변경 사이 TOCTOU를 제거한다. |

## Pending Work
### Immediate Next Steps
1. 실제 Windows Terminal PTY에서 140x40, 100x30, 72x24, 50x15 크기의 최종 시각 스냅샷을 선택적으로 캡처한다.
2. CGO와 C compiler가 있는 CI lane에서 `go test -race ./cmd/proxy ./internal/agent ./internal/server`를 실행한다.

### Blockers/Open Questions
- [ ] 현재 환경은 `CGO_ENABLED=0`이고 gcc가 없어 race detector를 실행하지 못했다.

## Context for Resuming
### Important Context
전체 `go test ./...`, `go vet ./...`, `go build ./...`, `go mod verify`, `git diff --check`와 Windows amd64/Linux amd64/macOS arm64 CGO-disabled cross-build가 통과했다. 독립 blocker-only 리뷰도 GO 판정이다. 저장소에는 이 작업 전부터 다수의 capability-absorption 변경이 공존하므로 TUI 파일만 분리 검토하되 기존 변경을 reset하지 않는다.

### Potential Gotchas
- TUI의 async result에는 `runGeneration`과 expected state가 함께 있어야 하며 stale result는 context를 취소하고 버린다.
- approval/confirmation 내용이 화면에 모두 보이지 않으면 긍정 action을 허용하지 않는다.
- provider/model/router는 server-wide 설정이므로 client-side loop 조회만으로 보호하지 말고 서버 barrier를 유지한다.
- `stream_end`는 delimiter이며 typed success terminal이 아니다.
