# Handoff: 저장소 독립 분리 + 로컬 모델 실측 기반 하드닝

## Session Metadata
- Created: 2026-07-26 23:41 KST
- Project: `D:\git\aniclew` (독립 분리 완료 — 구 경로 `D:\git\claudecode\proxy-go`)
- Branch: main / 오늘 15커밋, 워킹트리 클린, **미푸시 43**
- 관련 저장소: `D:\git\claudecode` 1커밋 (`6bef41f`), upstream 없음

## Current State Summary

제품 정체성 검토(/athena)로 시작해 **법적 기반 문제를 발견**하고 저장소를 독립 분리했다. 이후 로컬 모델(Ollama)로 실제 작업을 태워 **AniClew 자신의 "조용한 실패" 4건**을 찾아 고쳤다. 코드를 읽어서는 안 보이고 돌려봐야 나오는 종류였다. 마지막으로 비용 추적 제거와 잔재 정리를 수행했다. 모든 게이트(go build/vet/test 15패키지, web build) 통과.

## Work Completed

### 1. 저장소 독립 분리
- [x] 아테나 검토에서 `LICENSE`가 `UNLICENSED — leaked proprietary source (Anthropic)`임을 확인. 2026-03-31 npm 소스맵 사고로 유출된 트리이고 DMCA 집행 전례 있음
- [x] `proxy-go/`는 MIT·본인 저작이며 코드 결합 0 — 이동만으로 분리 가능했음
- [x] Git Bash `mv` 실패(`Device or resource busy`, 셸이 cwd 점유) → PowerShell `Move-Item`으로 성공

### 2. IP 검증
- [x] Go 문자열 1,455개를 유출 트리(2,432파일/34.4MB)와 대조 → 4건 일치, 전부 재작성. 재검사 0건
- [x] 결과와 남는 한계를 `docs/ip-provenance.md`에 기록

### 3. Settings UI 이식
- [x] 백엔드에 `runtimeplane`이 있는데 **조작 UI가 0건**이었음 (`grep runtime|quota|account|scheduler` → 0)
- [x] kkh 저작 14개 컴포넌트 + 훅 3개 = 4,822줄 이식. nirholas 저작 인프라(store/SettingRow/utils/constants)는 재작성
- [x] Next.js 특유 구문 0건이라 본문 무수정, import만 치환
- [x] `index.css`에 surface-100..950 / brand-* 스케일을 **라이트·다크 양쪽**에 정의(308개 클래스 호환)

### 4. 로컬 모델 실측 → 조용한 실패 4건 수정
| 결함 | 증상 | 커밋 |
|------|------|------|
| Test 도구 | 러너 못 찾고 `isError=false` | `47c2f4b` |
| `python3` 별칭 | Windows Store 별칭 실행, 빈 출력 후 exit 0 | `3d250a1` |
| `closestLinesHint` | 필요한 순간에만 빈 문자열 반환 | `6bce6f8` |
| 비용 계산 | 3중 $5/M 폴백, 예산까지 오집행 | `eb41cf1` |

### 5. 정리
- [x] `~/.claude-proxy/` → `~/.aniclew/` (11곳 하드코딩을 `config.BaseDir()`로 중앙화, 레거시 폴백 유지)
- [x] 비용 추적 제거, 요청/토큰 수는 유지 (`UsageEntry`, `GET /api/usage`)
- [x] 죽은 V1 도구 구현 166줄 삭제 → 미참조 함수 0건
- [x] 빌드 산출물 111MB 정리 (`kairos.exe` 등 4월 잔재, `dist/`)
- [x] README 수치 5곳 실측 대조 정정

### Files Modified (주요)
| File | Changes |
|------|---------|
| `internal/config/config.go` | `BaseDir()`/`IsBaseDirPath()` 신설, 레거시 폴백 |
| `internal/agent/tools_advanced.go` | Test 도구 에러 보고, 인터프리터 프로브 |
| `internal/agent/fuzzy.go` | 토큰 겹침 기반 힌트 폴백 |
| `internal/agent/project.go` | 확장자 기반 프로젝트 타입 폴백 |
| `internal/agent/tools.go` | V1 구현 6개 삭제 (381→220줄) |
| `internal/api/cost.go` | 삭제 |
| `internal/router/`, `observability/`, `gateway/` | 비용 필드 제거 |
| `web/src/components/settings/*` | 14개 이식 |
| `README.md` | 3회 재작성 (아래 참조) |

### Decisions Made
| Decision | Rationale |
|----------|-----------|
| 저장소 분리 (제품화 아님) | 법적 절연이 목적. 아테나 판정은 CONDITIONAL GO이고 수요 데이터는 여전히 0건 |
| Claude Code를 제품 서사에서 제거 | Claude Code 사용자는 Claude를 쓰려고 씀. Ollama를 원하면 자체 에이전트를 쓰면 됨 — 성립하지 않는 시나리오였음 |
| 두 경로를 README에 명시 | `/api/agent`(하드닝 전부) vs `/v1/messages`(요청 변조·라우팅만). 섞여 있어서 혼란의 근원이었음 |
| 비용 제거, 사용량 유지 | 토큰·요청은 관측된 값, 달러는 외부 가격표 추측 |
| `:12b` 프로파일 추가 안 함 | 도구 예산 10 실험에서 오히려 악화(16→18회). 근거 없는 추가 회피 |
| gateway 예산 기능 제거 | 가짜 비용으로 요청을 차단하고 있었음 |

## Pending Work

### Immediate Next Steps
1. **미푸시 43커밋** — `Dannykkh/Ani-Claw`. 외부 공개 행위라 미실행
2. **프록시 경로 미검증** — 오늘 실측은 전부 `/api/agent`였음. `/v1/messages` 경유는 한 번도 안 돌려봄
3. **힌트 효과 통계 확정** — n=5로는 미확정(Mann-Whitney U=5, 임계 U≤2). 조건당 15~20회 필요

### Blockers/Open Questions
- [ ] `AppearanceSettings` 이식 — 244줄 중 19곳이 터미널 스킨 설정인데 AniClew에 터미널 UI가 없음. 이식하면 무동작 화면
- [ ] 프록시 경로(클라우드 계정 풀) 유지 여부 — 접으면 `runtimeplane` 1,351줄도 정리 대상
- [ ] `docs/athena/aniclew.md`가 claudecode 쪽에 있고 `docs/*` gitignore라 버전 관리 밖

## Context for Resuming

### Important Context
- **두 경로 구분이 핵심이다.** `RunLoop` 호출처는 `/api/agent`와 bridge뿐 — 프록시 경유 요청은 에이전트 하드닝을 타지 않는다. README 상단 표가 이 구분을 명시한다
- 아테나 산출물(`claudecode/docs/athena/aniclew.md`)에 **정정 섹션**이 붙어 있다. Phase 2의 "쿼터 스케줄링이 유일한 해자" 판정은 오류였고, 경쟁자는 claude-code-router가 아니라 Aider/Cline/Continue다
- `internal/agent` 16,030줄(48%) vs `runtimeplane` 1,351줄(4%) — 무게중심은 로컬 모델 에이전트에 있다
- opencodex는 Claude Code에 shim이 아니라 **래퍼 명령**(env 조립 후 spawn)을 쓴다. shim은 codex 전용이며 config.toml 주입이 필요해서다

### Potential Gotchas
- **`exec.LookPath` 성공 ≠ 실행 가능.** Windows `python3.exe`는 Store 앱 별칭이라 PATH에 있고 exit 0인데 아무것도 안 한다. 인터프리터는 `--version` 프로브로 검증한다
- **테스트가 버그를 보증할 수 있다.** `TestCalculateCost_Unknown`이 $5 폴백을 기대값으로 고정하고 있었다
- 실측 지표 중 **호출 횟수는 변동이 크다**(같은 조건에서 7~22회). 단일 실행 비교로 효과를 주장하면 안 된다. 코드 경로 지표(힌트 표시 여부 등)는 결정적이다
- `git status`는 무시된 파일을 안 보여준다. 오늘 111MB 잔재는 `ls`로만 보였다
- 로컬 모델 실측은 `scratchpad/ab_test.py` 패턴 참고 — 조건 교대, fixture 매회 재생성, 프록시 재시작
