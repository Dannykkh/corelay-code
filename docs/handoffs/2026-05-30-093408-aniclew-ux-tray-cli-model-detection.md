# Handoff: AniClew UX/속도 개선 — tray, native CLI, 경로/세션/바인딩 버그, 모델 갱신, 하트비트, Ollama 자동감지

## Session Metadata
- Created: 2026-05-30 09:34
- Project: `D:\git\aniclew` (Go module `github.com/aniclew/aniclew`, 앱 이름 "AniClew") — 세션 당시 경로는 `D:\git\claudecode\proxy-go`
- Branch: `main` (origin보다 앞섬, 미푸시)
- 저장소: 2026-07-25 독립 분리 완료. 커밋은 `git -C D:\git\aniclew`. (분리 전에는 상위 워크스페이스 `D:\git\claudecode`에서 `.gitignore`로 제외된 중첩 레포였음)
- 이전 핸드오프: `docs/handoffs/2026-05-27-sglang-harness-hardening.md` (Session 1~3: SGLang provider, 4중 안전망, 스킬 억제 버그, 에어갭).

## Current State Summary
AniClew를 "Claude Code CLI/앱을 롤모델로, 일반(로컬) 모델로 하네스를 구현"하는 방향으로 다듬는 작업.
이번 세션은 **실사용 중 드러난 버그 연쇄 수정 + 약한 모델 체감 응답성(UX) 개선 + 데스크톱 통합(tray/CLI)**에
집중. 사용자가 지정한 (a)체감응답성 → (b)빠른모델 → (c)배포패키지 중 **(a)·(b) 완료, (c) 보류**
(SGLang 설치 필요 — 사용자 결정). 모든 변경 빌드·실측 검증 완료. 17개 커밋 `baa8084`..`fc15759`.

## 핵심 아키텍처 (재확인)
- **2경로**: (1) CLI 패스스루 `POST /v1/messages` = 순수 프록시(도구는 CLI 소유). (2) 내장 에이전트
  `POST /api/agent` → `RunLoop`(도구 실행). **폐쇄망 타깃은 (2)** — claude/codex CLI는 폐쇄망 설치/인증 불가.
- 모든 OpenAI 호환 백엔드는 `OpenAICompat` 하나 공유 → `translate.ToOpenAI`가 유일한 요청 변환 길목.
- **의존성 0, 단일 정적 바이너리, 순수 표준 라이브러리** (go.mod에 require 없음) — 에어갭의 핵심 자산.

## Work Completed (커밋 17개, 전부 proxy-go main)
| Commit | 내용 |
|--------|------|
| `baa8084` | fix: 스킬 카탈로그가 로컬 모델 도구호출 억제 → 한 줄 포인터로 (근본 버그) |
| `9589cb5` | feat: 에어갭 모드 `ANICLEW_OFFLINE=1` (WebSearch/WebFetch/HTTPRequest 차단) |
| `76c31d7` | chore: `.gitignore` 미앵커 `proxy`가 `cmd/proxy/` 소스 무시 → 진입점 추적 복구 |
| `0075a37` | feat: 툴 예산 `ANICLEW_MAX_TOOLS=N` (도구 과부하 가지치기, MCP 디모트) |
| `a32d8f3` | feat: 내장 터미널 클라이언트 `aniclew chat` (폐쇄망용, 외부의존 0) |
| `79b8007` | fix: `CheckPath`가 상대경로를 server cwd로 해석 → workDir 기준으로 (Write 차단 버그) |
| `e95d5ea` | feat: 순수 syscall Windows 트레이 아이콘 (CGO 0, build tag로 격리) |
| `6f75387` | fix: `:port`(all interfaces) → 기본 `127.0.0.1`, `ANICLEW_BIND`로 opt-in |
| `7c8d4c2` | fix: `fetchJSON`가 4xx를 성공처리 / Up 버튼 `D:`드라이브 / Add 버튼 노출 |
| `742a1fe` | feat: 커스텀 트레이 .ico 임베드 (gen.go 생성기, 의존성 0) |
| `7420c3f` | fix: 빈 세션목록 `null` → `[]` (새 프로젝트 추가 시 빈 화면 크래시) |
| `5b6342d` | feat: Anthropic 모델 갱신 (Opus 4.7) + Quick Start 라벨 |
| `79c44af` | feat: Claude Opus 4.8 + GPT-5.5로 모델 bump |
| `77f236f` | feat: 라이브 하트비트 (경과+출력량) — 느린 모델 "살아있음" 신호 |
| `8f6f743` | fix: 하트비트를 `StreamMessage` **이전**으로 (모델 로드 dead-air 커버) |
| `152ceda` | feat: Stop 버튼 + Esc 인터럽트 (서버측 ollama 취소 전파) |
| `fc15759` | feat: Ollama 설치 모델 자동 감지 `GET /api/ollama/models` |

### 신규 파일
- `cmd/proxy/chat.go` — `aniclew chat` 터미널 클라이언트 (SSE 렌더, 스피너)
- `internal/agent/airgap.go`(+test) — egress 게이팅
- `internal/translate/toolbudget.go`(+test) — 도구 예산 가지치기
- `internal/tray/tray_windows.go` / `tray_other.go` / `assets/gen.go` / `assets/aniclew.ico` — 트레이
- `internal/server/ollama_models.go` — Ollama 설치 모델 조회 핸들러
- `internal/agent/skill_index_test.go` / `permission_test.go` — 테스트

## (a) 약한 모델 체감 응답성 — 완료
- **근본 원인** (워크플로우로 4계층 매핑): 웹은 `/api/agent`→`RunLoop`이 provider 첫 토큰 대기 중
  자기 신호 0. watchdog 아님(그건 `/v1/messages`에만). 특히 **dead-air의 대부분은 `StreamMessage`
  내부** — Ollama가 23GB 모델 로드+프리필 끝날 때까지 HTTP 응답 블록.
- **해결**: `startHeartbeat()` 1초 틱 → `Event{type:"heartbeat", {elapsedMs, chars}}`. `StreamMessage`
  **이전**에 시작(로드 구간 커버), 모든 종료 경로에서 idempotent stop. 웹은 클럭+카운터, 터미널은 `\r` 스피너.
- **Stop 버튼/Esc**: AbortController → fetch abort → `r.Context()` 취소 → ollama 호출 취소(검증됨).

## (b) 빠른 모델 — 완료
- **gemma4(9.6GB)가 이 하드웨어(RTX 4060 Ti 16GB)의 정답**: 완전 적재 → 빠름 + **도구 호출 가능**(파일 생성 검증).
  qwen3.6(23GB)은 CPU 오프로드로 느림.
- **Ollama 모델 자동 감지** `fc15759`: `/api/ollama/models`가 실제 설치 모델만(임베딩 필터, 작은순 정렬) 반환.
  프론트 드롭다운/Quick Start가 하드코딩 대신 이걸 사용. "추천 모델 미설치" 마찰 근본 해결.

### Decisions Made
| Decision | Rationale |
|----------|-----------|
| 하트비트를 StreamMessage 이전에 시작 | 최장 dead-air가 모델 로드(StreamMessage 내부)라 그 이후 시작은 무의미 |
| 트레이는 순수 Windows syscall | CGO/외부의존 추가하면 단일 바이너리·에어갭 크로스컴파일 깨짐. build tag로 linux 무영향 |
| 서버 기본 바인딩 127.0.0.1 | 에이전트가 Bash/Write 보유 + 인증 옵션 → LAN 기본노출 위험. `ANICLEW_BIND=0.0.0.0` opt-in |
| Ollama 모델 동적 감지 | 하드코딩은 머신마다 다른 설치 상태와 불일치 |
| (c) 보류 | SGLang 설치 필요 — 사용자 결정. proxy-go-only docker는 대안으로 남김 |

## Pending Work
### Immediate Next Steps (모두 SGLang/GPU 불필요)
1. **(a) 남은 폴리시**: per-tool 결과 스트리밍(병렬 도구 즉시 표시), web first-byte 상태머신
   (connecting→waiting→generating), 정적 `~Xk tokens` 라벨 정리. (워크플로우 결과 #5·#9·#10)
2. **proxy-go-only docker 패키지**: SGLang 없이 호스트/외부 Ollama 가리키는 compose + `ANICLEW_OFFLINE`/`BIND` 반영.
3. **(c) GPU 서버 배포**: SGLang + 32B 코더 모델 환경 — GPU 서버 확보 시.

### Open Questions
- [ ] Claude Opus 4.8 / GPT-5.5 모델 ID는 명명 패턴 추정 (`gpt-5.5-codex` 등). 실제 벤더 ID 확인 필요.
- [ ] 사용자 미커밋 `server.go`(loops 동시성 캡 등 81줄), `loop_registry.go/_test.go`는 **사용자 작업** — 미완료.

## Context for Resuming
### 사용자 환경 / 선호
- Windows 11 + RTX 4060 Ti 16GB. Ollama 0.24.0 (gemma4, qwen3.6, bge-m3 설치).
- 응답은 **한국어 경어체, 이모지 금지**. 커밋 Co-Author: `Claude Opus 4.8 (1M context)`.
- AniClew는 **사용자(Dannykkh) 본인 저작물** (MIT, 커밋 70+개 kkh). Claude Code 롤모델.

### Potential Gotchas (이번 세션에서 겪음 — memory/gotchas.md 참조)
- **좀비 프로세스가 포트 4000 점유**: 이전 테스트 `aniclew-*.exe`가 안 죽으면 새 빌드가 bind 실패하고
  테스트가 구버전 서버에 붙어 "수정이 안 먹힌 것처럼" 보임. 재기동 전 `taskkill //F //IM aniclew*.exe` 필수.
- **server.go는 사용자 미커밋 보호 파일**: 라우트 추가 시 백업→`git checkout HEAD`→내 줄만 재적용→커밋→백업 복원.
- **webdist 동기화 필수**: web 빌드(`npm run build`)는 `web/dist`에 나오지만 Go는 `internal/server/webdist`를
  embed. `cp web/dist/* internal/server/webdist/` + Go 재빌드 안 하면 옛 UI가 서빙됨.
- 빌드 검증 루프: `cd web && npm run build` → webdist 동기화 → `go build -o aniclew.exe ./cmd/proxy` → 재기동.
- 느린 검증은 **gemma4**로 (qwen3.6은 분 단위). active 모델은 `PUT /api/config`로 전환.

### 빠른 명령
```
taskkill //F //IM aniclew.exe; (cd web && npm run build)
cp web/dist/assets/*.js web/dist/assets/*.css internal/server/webdist/assets/; cp web/dist/index.html internal/server/webdist/
go build -o aniclew.exe ./cmd/proxy
OLLAMA_BASE_URL=http://localhost:11434 ./aniclew.exe -provider ollama -model gemma4:latest -port 4000
```
