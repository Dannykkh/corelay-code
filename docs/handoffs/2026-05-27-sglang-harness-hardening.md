# Handoff: SGLang provider + model-agnostic 하네스 보강

## Session Metadata
- Created: 2026-05-27
- Project: `D:\git\aniclew` (Go module `github.com/aniclew/aniclew`) — 세션 당시 경로는 `D:\git\claudecode\proxy-go`
- Branch: `main`
- 저장소: 2026-07-25 독립 분리 완료. (분리 전 상위 워크스페이스는 `D:\git\claudecode` — 출처와 검증 기록은 `docs/ip-provenance.md` 참조)

## Current State Summary
proxy-go를 "어떤 오픈 모델이든 SGLang 등으로 서빙해서, Claude Code 수준으로 활용하는
폐쇄망용 AI 코딩 에이전트 서비스"로 키우는 작업. 이번 세션에서 (1) SGLang provider를
추가하고 (2) 약한 모델을 보완하는 4중 안전망(fuzzy edit + lint 게이트 + reflection +
가드)을 구현·테스트했다. 순수 소스로 할 수 있는 핵심 하네스 보강은 완결. 다음은 실제
모델 실행 검증과 폐쇄망 배포 패키지(둘 다 GPU/실행 환경 필요).

## 핵심 아키텍처 (재확인용)
- **2층 구조**: 서빙층(SGLang/vLLM/Ollama, 모델을 GPU에 로드) + 서비스층(proxy-go).
  proxy-go는 모델을 "호출"할 뿐 "로드"하지 않는다.
- proxy-go 데이터 흐름: `providers/`(연결) → `translate/`(Anthropic↔OpenAI 변환)
  → `agent/loop.go RunLoop`(에이전트 루프: LLM→도구실행→반복).
- 모든 OpenAI 호환 백엔드(Ollama/SGLang/OpenAI...)는 `OpenAICompat` 하나를 공유 →
  코드 경로 동일, BaseURL/모델명만 다름. **Ollama로 검증하면 SGLang도 동일하게 동작.**

## Work Completed (커밋 4개, 모두 proxy-go 레포 main)
| Commit | 내용 |
|--------|------|
| `c1db905` | feat: SGLang provider(`NewSGLang`) + lint 게이트(Edit/Write 롤백) |
| `8190ac8` | feat: fuzzy(공백 무시) 매칭 — Edit 입력 관용 |
| `5be2324` | test: executeEditV2 통합 테스트 |
| `36ba83d` | feat: reflection 가드(연속 실패 중단) |

### Files Modified / Added (전부 내 작업)
| File | Changes |
|------|---------|
| `internal/providers/registry.go` | `NewSGLang()` + ProviderOrder/Create 등록 |
| `internal/agent/lint.go` (신규) | `lintFile` — go/py/js/json 문법 체크, 모르는 언어는 통과 |
| `internal/agent/fuzzy.go` (신규) | `fuzzyReplace`(공백 무시 줄 매칭) + `closestLinesHint` |
| `internal/agent/tools_improved.go` | `executeEditV2`/`executeWriteV2`에 fuzzy + lint 게이트 |
| `internal/agent/loop.go` | reflection 가드(`allToolsErrored`, maxErrorRounds=3) |
| `*_test.go` 4개 | lint/fuzzy/통합/가드 테스트 — 전부 통과 |

### 4중 안전망 (모델 격차 메꾸기 — 완성)
```
입력 관용(fuzzy) → 출력 검증(lint 게이트+롤백) → reflection(기존, 에러 피드백) → 가드(연속실패 중단)
```
Aider(입력 관용) + SWE-agent ACI(lint 게이트)에서 도출. reflection은 RunLoop이
tool_result를 user 메시지로 되돌리는 구조라 이미 존재했음 → lint 게이트만 넣어 자동 연결됨.

### Decisions Made
| Decision | Rationale |
|----------|-----------|
| 비용 추적(cost.go/router.go)에 SGLang 추가 안 함 | self-hosted는 비용 0, 추적 무의미 (한번 넣었다 되돌림) |
| SGLang provider는 OpenAICompat 재사용 | Ollama와 동일 구조, 코드 최소 |
| lint "신규 에러" 판정 단순화 | 편집 전 정상 → 편집 후 에러면 롤백 (SWE-agent의 라인시프트 비교는 미구현) |
| proxy-go main에 직접 커밋 | 레포 관행이 main 직선 개발 |

## Pending Work
### Immediate Next Steps
1. **실제 모델 검증** — Ollama(Windows 네이티브)로 `proxy-go → 모델 → 도구실행 → 응답`
   전체 파이프라인 1회 검증. SGLang은 그 다음(주소만 교체).
2. **폐쇄망 배포 패키지** — docker-compose로 SGLang(GPU) + proxy-go 묶기 + install 스크립트.
3. **SGLang 실기동** — WSL2에 SGLang + 16GB GPU에 맞는 양자화 모델(Qwen2.5-Coder-7B AWQ 등),
   `--tool-call-parser` / `--reasoning-parser` 설정.

### Open Questions
- [ ] `registry.go`의 `NewSGLang` ModelList ID는 예시. 실제 띄울 `--model-path`와 일치시켜야 함.
- [ ] 폐쇄망 패키지를 proxy-go 레포에 둘지, 별도 deploy 레포로 둘지.

## Context for Resuming
### 사용자 환경
- Windows 11 + NVIDIA RTX 3080 16GB (노트북 GPU일 가능성).
- SGLang은 Windows 네이티브 불가 → **WSL2**(GPU passthrough) 또는 원격 GPU 서버.
- 16GB VRAM → 7~14B 모델 **양자화(AWQ)** 가 적정. 32B는 안 들어감.
- 최종 목표: **폐쇄망(에어갭)** 배포. 인터넷 없이 동작해야 함.

### Potential Gotchas
- **AniClew는 독립 git 레포다** — 커밋은 `git -C D:\git\aniclew ...`.
  (2026-07-25 이전에는 상위 `claudecode` 레포에서 `.gitignore`로 제외된 중첩 레포였다.)
- **사용자의 미커밋 작업이 working tree에 있다**: `internal/server/server.go`(M),
  `internal/agent/loop_registry.go`, `loop_registry_test.go`(??). **건드리지 말고
  선별 커밋할 것** (내 파일만 `git add` 지정).
- 폐쇄망 반입 시 SGLang은 Claude Code 포크의 phone-home(텔레메트리/OAuth/Sentry 등)과
  무관하지만, proxy-go 자체에 외부 통신이 있는지 별도 점검 필요(에어갭 모드).
- lint 게이트의 `.py` 검사는 `python3` 필요. Windows에 없으면 자동 skip(best-effort).
- SGLang chat-template(.jinja)이 필요한 모델(DeepSeek-V3, llama4 pythonic)은 오프라인 반입 대상.

### 참고 (이번 세션에서 조사한 외부 자료)
- model-agnostic 하네스 레퍼런스: OpenHands(LiteLLM), Aider(edit format 적응), SWE-agent(ACI/linter 게이트), Cline.
- SGLang `--tool-call-parser`: 모델 패밀리별(llama3/qwen/qwen3_coder/deepseekv3/glm/...). 일부는 chat-template 필요.
- SGLang `--reasoning-parser`: deepseek-r1/qwen3/kimi_k2/gpt-oss 등 → `reasoning_content` 분리.
- RadixAttention 캐시 극대화: `--mem-fraction-static` 높게 + `--schedule-policy lpm`,
  프롬프트는 불변(시스템+도구) 앞 / 가변(rag/메모리) 뒤. loop.go:211 sysPrompt 순서 재구성이 향후 과제.

---

## Session 2 (continuation) — 실제 모델 검증 + 차단 버그 수정 (2026-05-27 오후)

### 한 일
핸드오프 1순위(Ollama 실제 검증)를 수행하다 **하네스 차단 버그**를 발견·수정·검증 완료.
- 환경: Ollama 0.24.0, `qwen3.6:latest`(23GB, thinking 모델), RTX 4060 Ti 16GB.
- 경로: `POST /api/agent` → `RunLoop` → `OpenAICompat.StreamMessage` → Ollama.

### 발견한 버그 (커밋 `baa8084`)
`loop.go`가 시스템 프롬프트에 스킬을 인라인 → 로컬 모델이 도구를 호출하지 않고 코드만 텍스트로
출력 후 "파일 생성했다" 환각. 2단계 원인:
1. 전체 SKILL.md 인라인 → 100개 ≈ **720KB(~180K토큰)** → 컨텍스트 초과.
2. 컴팩트 인덱스(100줄)조차 qwen3이 "스킬 메뉴"로 인식 → 도구 호출 억제.

### 진단 방법 (재현 가능)
1. 캡처 프록시(11435)로 proxy가 Ollama로 보내는 **실제 요청 본문** 캡처 → tools 30개 정상 전달 확인(translate 무고).
2. 캡처 본문을 Ollama에 **직접 재생**하며 변형: 시스템 프롬프트 제거(T1)→도구 호출 O,
   스킬 인덱스만 제거(T4)→O, 한 줄 포인터(T6)→O. **스킬 열거가 범인** 확정.

### 수정 내용
- `loop.go`: 스킬 전체/열거 → **한 줄 포인터**. 스킬은 슬래시 커맨드로 정상 동작.
- `loop.go`: baseSystemPrompt 강화("행동은 도구 호출로만, 코드 출력은 파일 생성 아님").
- `context.go`: `SkillDescription` 헬퍼(+테스트). 시스템 프롬프트 720,801→약 9K자.

### 최종 검증 (PASS)
`POST /api/agent`로 "hello.go 만들어라" → qwen3.6이 **LS→Write→Bash** 호출 →
`hello.go`(76B) 디스크 생성 → `go run` → `Hello, AniClew!`. **오픈 모델 + 실제 도구 실행 입증.**
OpenAICompat 공유 경로이므로 SGLang도 동일 동작.

### 남은 작업 (다음 세션)
1. ~~에어갭 하드닝~~ → **완료** (커밋 `9589cb5`). 아래 Session 3 참조.
2. 폐쇄망 배포 패키지 (docker-compose: SGLang GPU + proxy-go + install).
   - `ANICLEW_OFFLINE=1` 환경변수를 compose에 설정하면 egress 도구 자동 차단됨.
3. SGLang 실기동 (WSL2 GPU). 주소만 교체하면 됨(Ollama로 이미 동등 검증).

### Gotcha 기록
`memory/gotchas.md` → `skill-catalog-suppresses-tool-calls` 항목 추가.

---

## Session 3 — 에어갭 하드닝 + 진입점 추적 복구 (2026-05-27 오후)

### 에어갭 모드 (커밋 `9589cb5`)
`ANICLEW_OFFLINE=1` 설정 시 인터넷 egress 도구를 차단. 2중 방어:
- `AllToolDefs`가 offline일 때 `WebSearch`/`WebFetch`/`HTTPRequest`를 toolset에서 제외(모델이 못 봄).
- `ExecuteTool`이 해당 도구를 `[OFFLINE]` 에러로 거부(강제/캐시 호출 방어).
- Bash는 비게이팅(정상 로컬 용도). 에어갭 호스트는 네트워크 경로 부재에 의존.
- 파일: `internal/agent/airgap.go`(+test), `tools.go` 연결, `main.go` 기동 배너.

### 진입점 추적 복구 (커밋 `76c31d7`)
`.gitignore`의 미앵커 패턴 `proxy`가 바이너리뿐 아니라 `cmd/proxy/` 소스 디렉터리까지 무시 →
**진입점 `cmd/proxy/main.go`가 그동안 버전 관리에서 빠져 있었음**. `/aniclew`,`/proxy`로 앵커
수정(`/memory/`와 동일 방식). 루트 바이너리·*.exe는 계속 무시, cmd/proxy/는 추적 시작.

### 여전히 사용자 미커밋 (건드리지 않음)
`internal/server/server.go`(M), `internal/agent/loop_registry.go`,`loop_registry_test.go`(??).
