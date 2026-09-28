# Corelay Code UI/UX 및 아키텍처 개편 계획

- **문서 상태**: rev 3 — P0, Phase 0~3 구현 (감리: [verify-report](ui-ux-architecture-overhaul-verify-report.md)), Phase 4와 백엔드 트랙 대부분은 미착수
- **기준 시점**: 2026-09-28, product `main` `aa59d75`
- **대상**: `web/` 프런트엔드와 이를 제공하는 `internal/server`; 필요한 백엔드 선행 작업은 별도 트랙으로 분리
- **관련 기록**: [S10 검증 기록](corelay-reliability-roadmap/validation.md) (S10.A context strip, S10.C 누적 diff·복원, S10.E 브라우저 E2E), [README](../../README.md)

### 변경 이력

- rev 1: 전면 개편 명세 초안. 1단계에서 헤더·승인 바·누적 diff를 제거하고 3~4단계에서 대체 기능을 만드는 순서였다.
- rev 2: 현재 코드와 대조해 진단을 정정했다. 서버가 요구하는 동작의 UI는 삭제가 아니라 이동 대상으로 바꾸고, "만들고 → 옮기고 → 지운다" 순서로 재구성했다. 백엔드가 없는 기능은 별도 트랙으로 분리하고, 검증 단계와 정보 구조 이전표를 추가했다.
- rev 3 (2026-09-28): P0와 Phase 0~3을 구현했다. §9의 1·3·4번을 결정했다. 구현하면서 백엔드 트랙 일부를 앞당겼다: 승인 이벤트의 `toolCallId`(H1 연결용), 영수증 API의 `terminalState`(B1 일부), `/api/kairos/git`의 작업 공간 검증, wave 시간 초과의 `deadline_exceeded` 원인 전달. 헤더 모델 목록 교체는 B3 이후로 미룬다.

---

## 1. 목표와 비목표

### 목표

1. 첫 화면에서 엔진 내부 번호(revision, CAS)와 구현 용어를 걷어내고 바로 작업을 시작할 수 있게 한다.
2. Corelay의 핵심 안전망인 **검증 영수증, 파일 복원, 승인, 중단 복구**를 숨기지 않고 더 잘 보이게 한다.
3. 흩어진 화면(탑레벨 8개, 설정 15탭)을 사용자 과업 기준으로 재배치한다.
4. 다중 모델 활용(모델 전환, 팀 실행, 이후 동시 비교)을 백엔드 계약이 준비된 만큼만 UI에 올린다.

### 비목표

- 안전 기능을 줄여 평범한 챗봇으로 만드는 것.
- 명령 실행 부작용까지 되돌린다고 약속하는 것. 복원은 AI가 Write/Edit로 바꾼 파일에 한정된다.
- 백엔드 계약 없이 UI만으로 동시 비교(Arena)나 의도 기반 라우팅을 흉내 내는 것.

---

## 2. 현재 상태 진단 (aa59d75 기준 검증)

rev 1의 진단을 소스와 대조한 결과다. 줄 번호는 기준 커밋 시점 값이다.

| rev 1 진단 | 실제 | 판정 |
|---|---|---|
| 상단에 `Project › Work › Session › Stage › Permission` 5단계 경로 노출 | 표시 전용 context strip (`web/src/pages/Chat.tsx:1264-1277`). revision 숫자 `r{n}`이 4곳에 보인다 | 맞음 |
| `CAS Revision 승인`, `stateRevision`, `r1 -> r2` 노출 | 버튼 이름은 `Plan 승인`이고 CAS는 툴팁에만 있다. `stateRevision`은 요청 본문에만 쓰이고 `r1 -> r2`는 없다 | 과장 |
| 설정 탭 15개 | Runtime 6 / Harness 6 / Data 3 (`web/src/pages/RuntimeSettings.tsx:28-59`) | 맞음 |
| 탑레벨 아이콘 8개 | 위 4개(chat, files, team, memory) + 아래 4개(activity, routes, kairos, settings), 인라인 SVG (`web/src/components/ActivityBar.tsx:10-19`) | 맞음 |
| 모델 사용에 `ANTHROPIC_BASE_URL` 수동 주입 필요 | 외부 CLI(claude, codex)가 루프를 소유할 때만 해당한다. 네이티브 `/api/agent`는 서버의 provider 또는 세션의 provider/model을 쓴다 | 틀림 |
| `Routes.tsx` 16행 수동 매핑 | 백엔드 16개 역할(`internal/router/types.go:6-21`)을 그대로 편집하는 화면이다. 이 규칙은 relay 경로(`/v1/*`)에만 쓰이고 `/api/agent`는 쓰지 않는다. `PUT /api/routes`는 메모리만 바꾸므로 재시작하면 사라진다 | 부분: 문제는 UI가 아니라 기능 자체 |
| 원시 이모지 사용 | 도달 가능한 화면에 35개 (Routes 17, Quick Start 8, SidePanel 6, Chat 4). `lucide-react`는 이미 의존성이다 | 맞음 |
| raw diff·stdout 덤프 | 도구 카드는 항상 펼쳐진 채 JSON `<pre>`를 보여 준다 (`Chat.tsx:1487-1533`). diff는 `+`/`-` 줄 색칠뿐이다 | 맞음 |
| 모델을 바꾸려면 설정 화면으로 가야 함 | 하단 상태바에 모델 선택이 있지만 목록이 하드코딩이고, `send()`가 모델을 보내지 않는다 (`web/src/App.tsx:258-293`) | 부분 |

rev 1이 놓친 현재 기능은 다음과 같다.

- **Plan 승인은 서버가 강제하는 실행 전제다.** Plan에 연결된 세션은 `ApprovedRevision == Revision`이어야 실행된다 (`internal/server/session_workflow_binding.go:32-35`). 이를 호출하는 UI는 `Plan 승인` 버튼 하나다 (`Chat.tsx:1338-1346`, 호출처 `approveSelectedPlan` 하나).
- **`현재 revision 연결`과 `중단 단계 복구`도 UI 경로가 하나뿐이다.** running 상태로 남은 단계는 모든 실행을 막는다 (`session_workflow_binding.go:39`).
- **체크포인트 복원은 이미 있다** (S10.C). 누적 diff 최대 200개, 체크포인트 후보, `restorable`만 복원 가능. 위치는 `Chat.tsx:1385-1429`이다.
- **실행 권한 모드 표시는 의도된 설계다** (S10.A). 서버가 검증한 정책만 표시하고, 불분명하면 unknown으로 닫는다.
- **브라우저 E2E가 이 화면들을 검증한다.** `TestS10BrowserUIProjectFilesAndStaleStreams`(`internal/server/ui_browser_test.go`)는 context strip, Changes/Restore, 테마 전환을 검사한다.
- **Team 실행 중 승인 요청이 처리되지 않는다.** `web/src/pages/Team.tsx`는 `approval_required` 이벤트를 무시한다. 승인 대기 기본 5분(`internal/approval/broker.go:19`)과 wave 기본 5분(`internal/agent/team.go:238`)이 같아서, 승인이 필요한 작업은 실패로 끝날 가능성이 높다 (정적 추적, 실행 재현 전). CLI `team run`은 승인 처리기가 없어 해당 도구가 거부된다.
- **안 쓰는 파일이 있다.** `components/Sidebar.tsx`, `pages/Workspace.tsx`, `pages/Explorer.tsx`는 import되지 않는다.
- **디자인 정본(`DESIGN.md`)이 없다.** 토큰은 `web/src/index.css`에 두 체계(`--color-*`, `surface-N`/`brand-N`)로 공존하며, 푸른빛 다크(`#0f1117`, accent `#6c8cff`)와 라이트 테마가 있다.

---

## 3. 원칙

1. **만들고 → 옮기고 → 지운다.** 대체 UI가 동작하고 테스트가 통과한 뒤에만 기존 UI를 제거한다.
2. **서버가 요구하는 동작의 UI는 삭제 대상이 아니다.** 승인, revision 연결, 단계 복구, reconciliation은 위치만 옮긴다.
3. **내부 번호는 숨기되 안전 상태는 숨기지 않는다.** revision 숫자는 상세 영역으로 보낸다. 실제 권한 모드, 승인 필요, 복구 필요 상태는 항상 보인다.
4. **백엔드가 없는 기능은 UI로 흉내 내지 않는다.** 저장되지 않는 라우팅 규칙이나 검증하지 않는 API 키 "정상" 뱃지 같은 것이다.
5. **실제 범위만 말한다.** 복원 문구, 권한 모드 이름, 검증 미실행(`not-run`) 표시가 여기에 해당한다.

---

## 4. 항목별 판정 (rev 1 Kill List 재판정)

| 대상 | 판정 | 새 위치 / 처리 | 근거 |
|---|---|---|---|
| `Plan 승인`, `현재 revision 연결`, `중단 단계 복구`, Workstream 선택·New·Handoff | **이동 (필수)** | 인스펙터 Workflow 탭 | 서버가 강제하는 동작의 유일한 UI 경로 |
| 5단계 context strip | **숨김 (삭제 아님)** | 헤더에 권한 모드 배지와 "승인 필요"/"복구 필요" 배지만 남긴다. project·workstream·stage·revision은 Workflow 탭 상세로 옮긴다 | S10.A 안전 표시 유지 |
| 누적 Diff 접기 + Restore | **유지 후 격상** | 인스펙터 Changes 탭 | rev 1이 4단계에서 새로 만들려던 기능의 현재 구현 |
| 항상 펼쳐진 도구 카드 (raw JSON `<pre>`) | **교체** | 접힌 스텝 카드 (5.C) | 정보는 유지하고 기본 표시만 줄인다 |
| `Team.tsx` 슬롯·토큰 수동 폼 | **교체 (대체물 먼저)** | 계획 편집기(용량 설정은 고급으로 접기) + 작업 진행 패널 | 대체 화면과 승인 처리가 먼저 필요 |
| Team Gateway (사용자·예산·감사) | **이동** | Settings › 안전·권한 | Team 실행과 무관한 접근 제어 |
| `Routes.tsx` 매핑 테이블 | **숨김** | Settings › 모델·제공자 › 라우팅. "relay 경로 전용, 재시작 시 초기화"를 명시하거나 B7 전까지 숨긴다 | `/api/agent`에 영향 없고 저장되지 않음 |
| 탑레벨 8개 | **통합** | Workspace / History / Settings 3개 (7장 이전표) | 모든 기존 화면의 목적지를 먼저 정한다 |
| 설정 15탭 | **재그룹** | 4그룹, 하네스는 기본 접힘 (7장) | Harness 6탭과 영수증 목록의 목적지가 rev 1에 없었다 |
| 원시 이모지 35개 | **폐기** | `lucide-react` 아이콘 | 이미 의존성에 있음 |
| `Sidebar.tsx`, `Workspace.tsx`, `Explorer.tsx` | **폐기** | 삭제 (i18n 키 사용처 확인 후) | import 없음 |
| Zinc-950 + Indigo 토큰 일괄 교체 | **보류** | `DESIGN.md`에서 방향을 정한 뒤 적용 | 근거 없는 기본 팔레트 도입 방지 |

---

## 5. 목표 화면 구조

### 5.1 레이아웃

```
┌────────────────────────────────────────────────────────────────────────────────────────┐
│ Corelay  [claudecode · main]   [권한: 작업 폴더]  [승인 필요 1]   [모델 ▾]   [인스펙터] │
├──────────────┬────────────────────────────────────────────────────┬────────────────────┤
│ SESSIONS     │ WORKSPACE                                          │ INSPECTOR          │
│              │                                                    │ [Changes|Workflow| │
│ • Fix auth   │  User: 로그인 세션 만료 로직 개선해줘.             │  Receipt]          │
│ • Refactor   │                                                    │                    │
│              │  ✓ Read src/lib/auth.ts                  0.4s  ›   │ Changes            │
│              │  ✓ Edit src/lib/auth.ts                  0.2s  ›   │  auth.ts  +12 −4   │
│              │  ✓ Bash go test ./...                    8.1s  ›   │  [파일 복원]       │
│              │                                                    │  AI가 편집한 파일만│
│              │  ┌ 검증 영수증 ───────────────────────────────┐    │  복원합니다. 명령  │
│              │  │ 검증: passed · go test ./... · exit 0      │    │  부작용은 되돌리지 │
│              │  │ 완료 상태: completed · 증거 2건            │    │  않습니다.         │
│              │  └────────────────────────────────────────────┘    │                    │
│              │ ┌────────────────────────────────────────────────┐ │                    │
│              │ │ 무엇을 할까요?                                 │ │                    │
│              │ │ [이미지] [음성]       [Single | Team ▾] [전송] │ │                    │
│              │ └────────────────────────────────────────────────┘ │                    │
└──────────────┴────────────────────────────────────────────────────┴────────────────────┘
```

Arena 모드는 백엔드 B3·B6가 끝난 뒤(Phase 4)에만 모드 선택에 나타난다.

### 5.2 컴포넌트 명세

#### A. 헤더

- **표시:** 워크스페이스 이름과 Git 브랜치, 실제 권한 모드 배지(unknown이면 unknown), 승인·복구 필요 배지(클릭하면 해당 인스펙터 탭으로 이동), 모델 표시, 인스펙터 토글, 테마 전환.
- **제거:** 5단계 경로 텍스트와 revision 숫자. 둘 다 Workflow 탭 상세로 옮긴다.
- **모델 표시:** 지금은 현재 모델만 보여 준다. 대화 중 전환은 B3 계약이 확정된 뒤 퀵 스위처로 연다. 하드코딩 목록은 `GET /api/ollama/models`와 등록된 provider 목록으로 대체한다.

#### B. 프롬프트 독

- 하단 일체형 입력: 자동 확장 텍스트 영역, 전송·중단 버튼(생성 중에는 중단).
- **첨부:** 기존 이미지 첨부·붙여넣기 기능을 칩 UI로 옮긴다(썸네일, 파일명, 크기, 삭제). 일반 파일 첨부는 `/api/agent` 입력 계약에 없으므로 이번 범위 밖이다 [확인 필요].
- **음성:** 기존 음성 입력과 읽어 주기(TTS)를 아이콘 버튼으로 유지한다.
- **실행 모드:** `Single` / `Team`. Team은 5.2.E 진행 패널과 연결된다.

#### C. 실행 스텝 카드

- 도구 호출마다 한 줄 요약(상태 아이콘, 도구 이름과 대상, 소요 시간)을 기본으로 접어 둔다.
- 펼치면 입력과 출력, diff를 보여 준다. 현재 일반 `tool_input`은 원본 입력을 표시하고 가림 처리는 승인 이벤트(`redactedInput`)에만 있으므로, 스텝 카드 입력에 같은 가림 기준을 적용할지 Phase 2에서 정한다 [확인 필요]. diff는 기존 `+`/`-` 포맷을 쓰고, 전용 diff 뷰어 도입은 Phase 2 안에서 판단한다.
- 승인 대기 도구는 카드 안에 기존 허용·거부 패널을 띄운다. 거부가 기본이고 만료되면 거부된다.

#### D. 검증 영수증 카드

- 턴이 끝날 때 `done` 이벤트의 `terminalState`, 완료 메타데이터, 영수증 경로로 카드를 만든다.
- 검증 상태를 있는 그대로 표시한다: `passed` / `failed` / `not-run`. `not-run`을 초록색으로 보이게 하지 않는다.
- 지난 턴의 영수증 조회는 B1 이후에 가능하다. 그 전에는 History의 최근 영수증 목록(`GET /api/evidence/recent`)으로 연결한다.

#### E. Team 실행

- **계획 편집기:** objective, 작업, 의존성, 파일 범위, verify command. 용량 슬롯은 "고급"으로 접는다.
- **진행 패널:** wave별 작업 박스(상태, 담당 worker, 의존 작업), 지금 도는 작업, 막힌 작업, 검증 결과.
  - Phase 3 초기에는 기존 `status` 문자열로 최선의 표시만 한다. 구조화된 작업 이벤트(B4)가 들어오면 정식 패널로 전환한다.
- **승인:** `approval_required`를 받아 5.2.C와 같은 패널로 처리한다. 이 부분은 즉시 수정 항목이다(6장 P0).
- **실패:** 실패로 run이 중단되면 남은 작업을 "X 실패로 실행되지 않음"으로 표시한다(B4의 blocked 상태 전까지는 UI가 추론해서 표시).

#### F. 인스펙터

- **Changes:** 기존 누적 diff, 변경 파일 목록, 체크포인트 후보, 파일별 복원(`/undo --select`)을 옮긴다. 충돌·불가 항목은 복원 버튼 없이 사유를 표시한다. 문구는 "AI가 편집한 파일만 복원합니다. 명령 부작용은 되돌리지 않습니다."로 한다.
- **Workflow:** workstream 선택·생성·handoff, Plan 상태와 revision 상세, `Plan 승인`, `현재 revision 연결`, `중단 단계 복구`(확인 대화 유지). 세션 저장 충돌과 reconciliation 필요 안내도 여기에 모은다.
- **Receipt:** 현재 턴 영수증 상세와 증거 목록.

#### G. 권한 모드 선택

현재 백엔드의 세 모드를 실제 의미 그대로 노출한다.

| 모드 | 표시 이름 | 설명 문구 |
|---|---|---|
| `read-only` | 읽기 전용 | 파일 수정과 명령 실행을 하지 않는다 |
| `workspace` (기본) | 작업 폴더 | 작업 폴더 안에서 동작한다. 읽기·검색과 보통 위험도 도구는 자동으로 실행하고, 위험한 명령은 승인을 받는다 |
| `full` | 전체 권한 | 현재 OS 사용자 권한으로 호스트에서 실행하고 작업 폴더 밖 경로도 허용한다. 내장 도구는 확인 없이 실행되고, MCP·플러그인·호스트 상호작용 도구는 사용자에게 묻지 않고 그 호출에만 묶인 승인이 자동 발급된다. 금지 규칙과 명시적 거부 규칙만 남는다 (rev 3에서 `permission.go`·`tool_dispatch.go` 동작에 맞춰 정정) |

- rev 1의 "완전 자율: 모든 도구 자율 실행"은 금지·거부 규칙이 남으므로 쓰지 않는다. 대신 무엇이 확인 없이 실행되는지를 그대로 적는다.
- rev 1의 "매번 승인" 모드는 백엔드에 없다. 자동 승인 기준이 고정돼 있어서 필요하면 B9로 추가한다.
- 클라이언트는 `executionPolicy: {mode}`만 보낸다. 사용자 선택 근거(provenance)와 revision은 서버가 붙인다(`internal/server/server.go` `resolveServerExecutionPolicy`). 요청의 모드는 그 run에만 적용되고 세션에 저장되지 않으므로, 웹은 Settings에서 고른 모드를 매 요청마다 보낸다(`web/src/lib/executionMode.ts`). full은 화면에서 명시적 확인을 받은 뒤에만 고를 수 있다.

---

## 6. 로드맵

```
P0 즉시 수정 ─┐
Phase 0 기반 ─┼─► Phase 1 인스펙터·이전 ─► Phase 2 스텝·영수증 ─► Phase 3 정보 구조·Team
              │                                                          │
백엔드 트랙 B1~B10 (각각 별도 계획) ─────────────────────────────────────┴─► Phase 4 (조건부) Arena·모델 허브 완성
```

### P0. 즉시 수정 (개편과 무관한 버그)

- Team 화면에서 `approval_required`를 처리하고, 승인 대기가 wave 시간 제한을 소진하지 않도록 조정한다(방식은 구현 계획에서 결정).
- 먼저 실패를 재현하는 테스트를 만든다.

### Phase 0. 기반 (위험 낮음)

- **`DESIGN.md` 작성:** 현재 `index.css` 토큰에서 출발해 색·타이포·간격·아이콘 규칙과 라이트/다크 대비 기준을 정한다. 두 토큰 체계(`--color-*`, `surface-N`/`brand-N`)를 하나로 합치는 방향도 포함한다. 팔레트 변경은 근거와 함께 여기서 결정한다.
- 도달 가능한 화면의 이모지 35개를 lucide 아이콘으로 교체한다.
- 안 쓰는 파일 3개와 전용 i18n 키를 정리한다.

### Phase 1. 인스펙터 구축과 기능 이전

1. 인스펙터(Changes, Workflow, Receipt 틀)를 만들고 기존 기능을 옮긴다. 옮기는 동안 기존 위치의 기능은 유지한다.
2. 헤더에 권한 모드·승인·복구 배지를 넣는다.
3. 이전이 확인된 뒤 context strip과 헤더의 승인 바를 제거한다.
4. S10.A 결정을 부분 대체한다: 권한 표시는 유지하고 revision 표시만 옮긴다. 대체 사실을 로드맵 기록에 남긴다.

### Phase 2. 실행 스텝 카드와 영수증 카드

- 도구 카드를 접힌 스텝 카드로 교체하고, 승인 패널을 카드 안으로 옮긴다.
- `done` 이벤트 기반 영수증 카드를 만든다.
- 전용 diff 뷰어(Monaco, CodeMirror 등) 도입 여부는 번들 크기와 함께 판단한다. 현재 빌드에는 이미 500 kB 이상 청크 경고가 있다.

### Phase 3. 정보 구조 통합과 Team 교체

- 7장 이전표대로 탑레벨 3개와 설정 4그룹을 적용한다.
- Team 슬롯 폼을 계획 편집기와 진행 패널로 교체한다(5.2.E).

### Phase 4. 조건부 (백엔드 선행 후)

- **모델 허브 완성:** 연결 확인 뱃지(B2), 대화 중 모델 전환(B3).
- **Arena:** B3과 B6가 끝난 뒤 좌우 비교, 원클릭 채택.
- **여러 턴 체크포인트 타임라인:** B5 이후.

### 백엔드 트랙 (각 항목은 별도 계획과 검증을 가진다)

| ID | 내용 | 현재 상태 | 이 계획에서 막히는 것 |
|---|---|---|---|
| B1 | 영수증에 session/run 식별자를 넣고 세션·run별 조회 API 추가 | `AgentReceipt`에 식별자 없음. `/api/evidence/recent`는 workDir 기준 최대 50개 | 지난 턴 영수증 카드 |
| B2 | provider 연결·API 키 검증 API | `POST /api/providers/register`는 검증 없이 저장. `Validate()`는 사실상 no-op | "정상 연결 N ms" 뱃지 |
| B3 | 대화 중 모델 전환 계약 | `/api/agent` 요청에 모델 필드 없음. 세션에는 provider/model 필드가 있으나 중간 변경 허용 여부는 [확인 필요] | 퀵 스위처, Arena |
| B4 | Team 구조화 작업 이벤트와 blocked 상태 | 작업 상태는 `status` 문자열에만 있음. 상태는 pending/running/completed/failed 4종 | 정식 Team 진행 패널 |
| B5 | 여러 턴 체크포인트 | 최근 한 턴, Write/Edit만, durable 세션 필요 (`internal/agent/checkpoint.go`) | 타임라인 복원 |
| B6 | worktree 격리 세션과 채택(merge) API | worktree 헬퍼는 있으나 운영 경로에서 쓰이지 않음. 작업 폴더 잠금 없음 | Arena |
| B7 | 라우팅 규칙 저장, `/api/agent` 적용 여부 결정 | 규칙은 메모리에만 있고 relay 경로 전용 | 라우팅 화면 노출, 의도 기반 라우팅 |
| B8 | KAIROS 알림 종류(결정 요청 vs 정보) 정의와 webhook 설정 UI | `type` 필드를 웹이 버림. webhook은 API로만 설정 가능 | History 알림 구분 |
| B9 | 자동 승인 기준 설정 (선택) | 기준이 moderate로 고정, 설정 API 없음 | "매번 승인" 모드 |
| B10 | 실시간 세션 구독 (선택) | SSE는 요청 단위. TUI와 웹은 durable 세션만 공유 | TUI·웹 실시간 동기화 |

---

## 7. 정보 구조 이전표

### 탑레벨

| 현재 | 새 위치 |
|---|---|
| chat | Workspace |
| files | Workspace 좌측 패널(세션 목록과 탭 전환) |
| team | Workspace 실행 모드 `Team` + 진행 패널. Team Gateway는 Settings › 안전·권한 |
| memory (서버 기억) | **결정 필요:** Workspace 인스펙터 탭 또는 Settings › 일반·데이터 |
| activity (Costs: 지표, 실패 run, 회귀 케이스, 요청) | History |
| routes | Settings › 모델·제공자 › 라우팅 (4장 조건 적용) |
| kairos | 알림 목록은 History, webhook 설정은 Settings › 일반·데이터 (B8) |
| settings | Settings |
| 테마 전환 | 헤더 + Settings › 일반·데이터 |

### 설정 15탭 → 4그룹

| 새 그룹 | 포함 탭 |
|---|---|
| 모델·제공자 | Quick Start(첫 화면), Overview(설정 점검 카드로 상단 배치), Accounts, Providers(고급), Scheduler(고급), Routing |
| 안전·권한 | 권한 모드(5.2.G), Privacy, Team Gateway |
| 일반·데이터 | 응답 언어, 테마, Memory(브라우저 대화 내보내기·삭제), Advanced, KAIROS webhook |
| 하네스 (기본 접힘) | Agents, Commands, Skills, Loops, Verification(정책), Handoffs |

Verification 탭의 최근 영수증 목록은 History로 옮기고 정책 설정만 하네스에 남긴다.

---

## 8. 검증과 완료 기준

### 단계마다 실행할 검사

- `cd web && npm run lint && npm run build && npm test`
- `make deploy`로 embedded `webdist`를 동기화한다(웹 변경 후 필수).
- `go test ./internal/server -run '^TestS10BrowserUIProjectFilesAndStaleStreams$' -count=1 -v`
  - Phase 1부터는 context strip 검사를 헤더 배지와 Workflow 탭 검사로, Changes/Restore 검사를 인스펙터 위치로 갱신한다.
- `go test ./internal/server -count=1`, `git diff --check`

### 완료 기준

1. **기능 보존:** Plan 승인, revision 연결, 중단 단계 복구, 도구 승인 허용·거부, 파일 복원, Team 실행 중 승인이 모두 UI 경로를 가지며, 브라우저 E2E가 각 경로를 확인한다.
2. **첫 화면:** revision 숫자와 CAS 같은 내부 용어가 보이지 않는다. 단 실제 권한 모드와 승인·복구 필요 상태는 항상 보인다.
3. **복원 정직성:** 복원 UI는 Write/Edit로 바뀐 파일만 대상으로 한다고 표시하고, 충돌·불가 항목에는 복원 버튼이 없다.
4. **영수증 정직성:** 턴이 끝나면 검증 상태가 `passed` / `failed` / `not-run` 중 하나로 표시되고, `not-run`을 성공처럼 보이게 하지 않는다.
5. **정보 구조:** 7장 이전표의 모든 항목이 새 위치에서 동작하고, 목적지 없는 기존 기능이 없다.
6. **시각 일관성:** 도달 가능한 화면에 이모지가 없고 `DESIGN.md` 토큰만 쓴다. 라이트·다크 모두에서 본문·폼 대비를 확인한다.
7. **백엔드 의존 기준은 해당 트랙 완료 후 적용한다:** 연결 확인 1초 이내(B2), 대화 중 모델 전환(B3), 동시 비교(B6).

---

## 9. 열린 질문

1. ~~서버 기억(Memory) 화면 위치~~ → **결정(rev 3)**: Settings › 일반·데이터 › Server Memory. 브라우저 대화 저장소는 Browser Storage로 분리.
2. Arena를 이번 개편 범위에 넣을지, 별도 제품 계획으로 분리할지. (열림, B3·B6 선행)
3. ~~색 방향~~ → **결정(rev 3)**: 현재 푸른빛 다크를 유지하고 `DESIGN.md`로 고정. 라이트 경고색만 대비 기준(작은 글씨 4.5:1)에 맞춰 `#92400e`로 조정.
4. ~~라우팅 화면~~ → **결정(rev 3)**: 독립 Routes 페이지는 제거. 설정의 Routing 탭만 남기고 규칙 편집 화면은 B7 전까지 두지 않는다.
5. 일반 파일 첨부를 지원할지 (입력 계약 확인 필요). (열림)
