# Handoff: AniClew 로컬 모델 에이전트 하드닝 + Claude Code급 UX

## Session Metadata
- Created: 2026-06-19 10:17 (KST)
- Project: `D:\git\aniclew` (Go module `github.com/aniclew/aniclew`) — 세션 당시 경로는 `D:\git\claudecode\proxy-go`(중첩 저장소), 2026-07-25 독립 분리
- Branch: main
- Commits this session: 18 (`0bbec98` … `5651d9d`), 17 files, ~1,127 insertions

## Current State Summary
"Ollama로 이 프로그램(AniClew = Anthropic↔OpenAI↔Ollama 프록시 + 에이전트 루프 CLI) CLI 쓸 수 있나?"에서 출발해, 실제로 Ollama(qwen3-coder:30b / gemma4:12b)로 돌려보며 "불안정하다"는 진단을 내린 뒤, **로컬 모델 에이전트 경로의 "조용한 실패"를 제거하고 Claude Code급 UX를 이식**하는 작업을 18커밋에 걸쳐 진행했다. 거의 모든 변경은 `internal/agent/loop.go`(RunLoop)와 그 주변 신규 파일들에 있다. 핵심 골격(프로바이더 추상화, 에이전트 루프, MCP, 세션, plan/planmode 파일 등)은 원래 탄탄했고, 비어 있던 건 "로컬 모델이 약하다는 전제로 환경을 깎아주는 엔지니어링"이었다.

## Work Completed

### 1) 로컬 모델 안정화 (8)
- [x] `0bbec98` 툴 프루닝(로컬 기본 16개) + temperature=0 + 읽기전용 과탐색 가드
- [x] `1e3eab3` capability 경고 (`/api/show`로 tools 미지원 모델 감지 → devstral 함정)
- [x] `8fe01bf` 컨텍스트 고갈 경고 (stop=max_tokens + 출력≈0 → "컨텍스트 늘려라")
- [x] `30156e3` 모델별 프로파일 (coder/devstral/reasoning/size → budget·temp)
- [x] `b54ed41` 탐색 가중치 (Read/Grep=1.0, LS/Glob=0.5)
- [x] `29b3bc3` 읽기전용 답변 언어 유지 (collapse 끝에 한국어 리마인더)
- [x] `cdae9a0` 첫 생성 알림 (MEMORY.md / .claude/settings.json)
- [x] `4281d78` 자동 검증 루프 (편집 후 test 자동 실행 → 실패 시 되먹임, 2회 제한)

### 2) Claude Code급 UX (4)
- [x] `1c496b7`+`430bbb1` 편집 Diff 표시 (백엔드 diff 이벤트 + CLI 색상 + 웹 카드)
- [x] `99586bd`+`83cae9b` Plan 모드(`/plan`) + 웹 "승인 & 실행" 버튼
- [x] `4281d78` 자동 검증 루프(위 1)도 Claude Code "edit→test→fix"

### 3) 폴리시 (5)
- [x] `a713798` `@파일` 멘션 (탐색 없이 파일 선주입)
- [x] `1da0612` 체크포인트 + `/undo` (편집 전 백업, 되돌리기)
- [x] `41bad93` 완료 요약 ("[요약] 바꾼 파일 N개 · 테스트 통과 · N회 반복")
- [x] `fb2317b` `/help`에 `/plan`·`/undo` 문서화
- [x] `7dda808` 툴 호출 누출 복구 (`<function=…>` 텍스트를 실제 툴 호출로 복구)
- [x] `5651d9d` (버그수정) plan 모드 읽기전용 하드 강제 (권한 계층에서 변형 툴 차단)

### Files Modified
| File | Changes |
|------|---------|
| internal/agent/loop.go | RunLoop 대부분의 로직 (+515줄): 프루닝·temp·프로파일·가드·자동검증·diff·체크포인트·요약·plan·@멘션·누출복구·plan차단 |
| internal/agent/{capabilities,verify,diff,checkpoint,mentions,profiles,toolrecover}.go | 신규 헬퍼 파일 |
| internal/agent/{commands,memory_hook}.go | /help 갱신, MemoryHeadsUp |
| internal/config/config.go | localToolBudget, agentTemperature, readOnlyExploreRounds |
| cmd/proxy/chat.go | CLI diff 렌더 |
| web/src/pages/Chat.tsx | diff 카드, plan 승인 버튼, send 오버라이드 |
| internal/server/webdist/* | 재빌드된 웹 번들 (vite build → webdist 동기화 필요) |

### Decisions Made
| Decision | Rationale |
|----------|-----------|
| temperature=0 (로컬 에이전트) | 기본 샘플링에서 모델이 툴 대신 산문으로 드리프트 → 0이 툴 호출 안정화 (실측) |
| 사용자 주도 `/plan` (모델 EnterPlanMode 아님) | 로컬 모델이 plan 진입 툴을 신뢰성 있게 못 부름. Claude Code도 사용자 토글 |
| 읽기전용/plan 강제는 "툴 제거"가 아니라 **실행 계층 차단** | Ollama가 tools 목록과 무관하게 `<tool_call>`을 파싱 → 목록에서 빼도 실행됨. 권한 계층에서 막아야 진짜 강제됨 (`5651d9d`) |
| 백업/메모리는 워크스페이스 밖(`~/.claude-proxy/`) | 사용자 프로젝트 오염 방지 (단 MEMORY.md/memory/는 설계상 프로젝트 루트) |
| 프로파일 숫자(coder 제외)는 휴리스틱 | coder 16/0만 실측. 나머지는 config로 덮어쓰기 가능 |

## Pending Work

### Immediate Next Steps
1. **실사용 검증** — 합성 테스트로는 한계. 실제 프로젝트(복사본)에서 다양한 모델로 돌려 프로파일 숫자·동작 튜닝.
2. (선택) 결정-계층 폴리시는 **더 안 함** — 스킬(zephermine/chronos 등)이 자체 오케스트레이션을 가져 겹침. 실행-계층(누출복구 등)만 가치.
3. (선택) 누출 복구 e2e — 누출이 비결정적이라 이번엔 unit test로만 검증. 실사용에서 발동 관찰 필요.

### Blockers/Open Questions
- [ ] `server.go`(+81줄)와 `loop_registry.go`/`loop_registry_test.go`는 **이번 세션 이전부터 있던 미커밋 owner WIP** — 손대지 않음. 커밋/정리는 owner 판단.
- [ ] 웹 plan "승인 & 실행" 클릭 시 실제 편집 실행은 미클릭 검증(artwport 읽기전용 합의) — 복사본에서 클릭 e2e 권장.

## Context for Resuming

### 빌드·실행
- 빌드: `cd proxy-go && go build -o /tmp/aniclew.exe ./cmd/proxy`
- 웹 변경 시: `cd web && bun run build` → `rm -rf internal/server/webdist && cp -r web/dist internal/server/webdist` → `go build`(재임베드, `//go:embed all:webdist`)
- 실행: `aniclew -provider ollama -model qwen3-coder:30b -port 4137` → 대시보드 `http://localhost:4137/app`
- claude CLI 연결: `ANTHROPIC_BASE_URL=http://localhost:4137 claude`
- 테스트: `go test ./internal/agent/`

### 설치된 모델 / 하드웨어
- GPU: RTX 4060 Ti **16GB** VRAM, RAM 64GB
- qwen3-coder:30b(18GB, MoE A3B, tools) = 에이전트 주력 / gemma4:12b(7GB, 멀티모달 vision+audio+tools) / qwen3.6:27b / bge-m3(임베딩)

### Potential Gotchas (다음 세션 필독)
- **Ollama 컨텍스트 기본 8k** (앱 설정 슬라이더). 에이전트 프롬프트가 이를 넘기면 `finish_reason: "length"` + 1토큰 출력 = "조용한 실패". 사용자가 16k로 올려둠. `/v1/chat/completions`는 `num_ctx` 무시(서버 기본/`OLLAMA_CONTEXT_LENGTH`만 적용).
- **Ollama는 tools 목록과 무관하게 `<tool_call>`을 파싱** → "툴 제거"로는 행동을 못 막음. 실행/권한 계층에서 차단해야 함.
- **모델 변동성**: temp=0이어도 Ollama엔 약간의 비결정성. 모호한 지시("고쳐줘")엔 편집을 생략하기도 → 명시적 지시("Edit 도구로 직접 수정")가 안정적. e2e 테스트가 가끔 다르게 나오는 이유.
- **qwen 툴 호출 누출**: 가끔 `<function=Read>...` 텍스트로 흘림 → `7dda808`이 복구.
- **devstral**: Ollama 일반 API로 tool calling 거부(OpenHands 스캐폴드 전제) → 삭제됨. capability 경고로 사전 안내.
- **경로 함정**: Git Bash `/tmp` ≠ Windows Python `/tmp`(C:\tmp). 테스트 파일은 cwd나 절대 Windows 경로로.
- **콘솔 cp949**: 한글이 깨져 보여도 실제 UTF-8 데이터는 정상. 검증은 파일로 출력 후 Read.

`#tags: handoff, aniclew, 로컬모델, ollama, 에이전트루프, claude-code급, 18commits, plan모드, 누출복구`
