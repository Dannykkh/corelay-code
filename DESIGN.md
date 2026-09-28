# Corelay Code Design System (`DESIGN.md`)

- **상태**: Canonical Design Tokens & Guidelines (rev 1)
- **적용 범위**: `corelay-code/web` 프런트엔드 전체
- **기준 토큰 파일**: `web/src/index.css`
- **디자인 방향**: Technical Cockpit & Modern Agentic Workspace (차분한 고밀도 엔지니어링 콘솔)

---

## 1. 디자인 철학 및 원칙

1. **Explicit & Evidence-First**:
   - 시스템의 상태(권한 모드, 검증 패스/실패, 도구 실행, 체크포인트 복원 가능 여부)를 모호한 애니메이션이나 장식으로 가리지 않고 명확한 텍스트 배지와 상태 톤으로 전달합니다.
2. **High-Density, Low-Noise**:
   - 마케팅 대시보드식 넓은 여백을 지양하고, 코드와 로그, 파일 트리, Diff를 빠르게 훑을 수 있는 고밀도 콤팩트 레이아웃을 유지합니다.
3. **No Raw Emojis**:
   - UI 전반에서 유니코드 원시 이모지 사용을 엄격히 금지합니다. 모든 아이콘은 `lucide-react` SVG 아이콘을 사용하며 일관된 크기(14px/16px/18px)와 선 굵기(1.5px)를 준수합니다.
4. **Honest Contrast**:
   - 다크 모드와 라이트 모드 모두에서 WCAG AA(최소 4.5:1) 이상의 텍스트 대비를 보장하며, 비활성/보조 텍스트도 3:1 이상의 식별 가능한 대비를 유지합니다.

---

## 2. 디자인 토큰 체계 (Token Reconciliation)

현재 코드베이스는 구 UI에서 쓰던 직관적 시맨틱 토큰(`--color-*`)과 이식된 설정 컴포넌트에서 쓰던 스케일 토큰(`surface-N`, `brand-N`)이 공존합니다. 신규 구현은 아래의 **통합 시맨틱 매핑**을 따릅니다.

### 2.1 색상 토큰 (Colors)

| 시맨틱 역할 | 다크 테마 값 (`[data-theme="dark"]`) | 라이트 테마 값 (`[data-theme="light"]`) | CSS 변수 | Tailwind 유틸리티 대응 |
|---|---|---|---|---|
| **App Background** | `#0f1117` | `#f8f9fb` | `var(--color-bg)` | `bg-surface-950` / `bg-[var(--color-bg)]` |
| **Card / Surface** | `#1a1d27` | `#ffffff` | `var(--color-surface)` | `bg-surface-900` / `bg-[var(--color-surface)]` |
| **Elevated Surface** | `#232733` | `#f0f1f4` | `var(--color-surface2)` | `bg-surface-800` / `bg-[var(--color-surface2)]` |
| **Default Border** | `#2e3343` | `#e2e4e9` | `var(--color-border)` | `border-surface-800` / `border-[var(--color-border)]` |
| **Primary Text** | `#e4e6ed` | `#1a1d27` | `var(--color-text)` | `text-surface-100` / `text-[var(--color-text)]` |
| **Secondary Text** | `#8b8fa3` | `#6b7080` | `var(--color-text2)` | `text-surface-400` / `text-[var(--color-text2)]` |
| **Muted Text / Hint** | `#6b7285` | `#8b8fa3` | - | `text-surface-500` |
| **Accent / Brand** | `#6c8cff` | `#4a6adf` | `var(--color-accent)` | `text-brand-500` / `bg-brand-500` |
| **Accent Hover** | `#4a6adf` | `#3454c5` | `var(--color-accent2)` | `text-brand-600` / `bg-brand-600` |
| **Success / Passed** | `#34d399` | `#059669` | `var(--color-green)` | `text-emerald-400` / `text-[var(--color-green)]` |
| **Warning / Attention**| `#fbbf24` | `#92400e` | `var(--color-warning)` / `var(--color-yellow)` | `text-[var(--color-warning)]` (라이트 값은 흰 배경·`surface2`에서 6:1 이상) |
| **Error / Blocked** | `#f87171` | `#dc2626` | `var(--color-red)` | `text-rose-400` / `text-[var(--color-red)]` |
| **Info / Orange** | `#fb923c` | `#ea580c` | `var(--color-orange)` | `text-orange-400` / `text-[var(--color-orange)]` |

### 2.2 타이포그래피 (Typography)

- **UI 글꼴**: `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif`
- **코드 및 모노스페이스**: `"JetBrains Mono", "Fira Code", Menlo, Monaco, Consolas, monospace`
- **계층 구조**:
  - `Header Title`: `18px` (`text-lg`), `font-semibold`
  - `Section / Card Title`: `14px` (`text-sm`), `font-medium`
  - `Body / Message`: `13px`~`14px` (`text-sm`), `leading-relaxed`
  - `Small / Badge / Meta`: `11px`~`12px` (`text-xs`), `tabular-nums`
  - `Micro Label (Uppercase)`: `10px`, `uppercase`, `tracking-wider`, `font-semibold`

### 2.3 간격 및 라운딩 (Spacing & Radius)

- **기본 그리드**: 4px 베이스 (`gap-1`=4px, `gap-2`=8px, `gap-3`=12px, `gap-4`=16px)
- **컨테이너 패딩**:
  - 뷰포트/페이지 레벨: `p-4` ~ `p-6`
  - 패널/인스펙터 헤더: `px-4 py-2.5`
  - 카드 내부: `p-3` ~ `p-4`
- **Border Radius**:
  - 버튼 / 뱃지: `rounded-md` (6px)
  - 카드 / 인스펙터 섹션: `rounded-lg` (8px) 또는 `rounded-xl` (12px)
  - 플로팅 프롬프트 독: `rounded-2xl` (16px)

### 2.4 아이콘 체계 (Icons via `lucide-react`)

- 네비게이션: `size={18}`, `strokeWidth={1.5}`
- 툴바 및 인라인 액션: `size={15}` 또는 `size={16}`, `strokeWidth={1.5}`
- 상태 뱃지 및 메타 인디케이터: `size={12}` 또는 `size={14}`
- 대표 아이콘 매핑:
  - 채팅: `MessageSquare`
  - 설정: `Settings`
  - 파일 / 코드: `FileCode`, `Folder`
  - 실행 / 터미널: `Terminal`
  - 복원 / 실행취소: `Undo2`, `RotateCcw`
  - 승인 / 권한: `Shield`, `ShieldAlert`, `KeyRound`
  - 검증 / 성공: `CheckCircle2`, `CircleCheck`
  - 오류 / 실패: `AlertCircle`, `CircleX`, `X`
  - 로딩 / 스트리밍: `LoaderCircle` (`animate-spin`)
  - 복사: `Copy`, `Check`

---

## 3. 핵심 UI 상태 표현 규약

1. **검증 영수증 (Verification Receipt)** — 판정은 `web/src/lib/receipts.ts`의 `describeReceipt` 하나만 사용합니다 (`ReceiptBadge` 컴포넌트).
   - 입력은 두 신호입니다: 검증 명령 결과 `verificationStatus`(passed / failed / not-run)와 증거 게이트의 `terminalState`(verified / partially-verified / unverified / blocked).
   - 우선순위: `blocked`·`completion_blocked`·`max_cycles` → **Blocked**(적색) > 검증 `failed` → **Failed**(적색) > `verified` → **Verified**(녹색) > `partially-verified` → **Partially verified**(경고색) > `unverified` → **Unverified**(중립) > 검증 `passed` → **Passed**(녹색) > 나머지 → **Not run**(중립).
   - 검증 명령이 통과해도 terminal state가 blocked이면 녹색을 쓰지 않습니다. terminal state만으로 passed를 만들지 않습니다.
   - 색은 토큰만 씁니다: `text-[var(--color-green|red|warning)]` + 같은 색 10% 배경, 중립은 `text-[var(--color-text2)] bg-[var(--color-surface2)]`.
2. **도구 실행 스텝 카드**:
   - 실행 중: 회전하는 스피너 + 도구 이름 + 타겟 파라미터 한 줄 요약
   - 실행 완료: 접힌 상태로 소요 시간(예: `0.4s`)과 함께 한 줄 표시, 클릭 시 입출력 드로어 열림
3. **파일 복원 (Checkpoint Restore)**:
   - 복원 가능한 파일(`restorable`): 복원 버튼 활성화, 명확한 파일 경로 표기
   - 복원 불가/충돌 파일: 복원 버튼 없이 사유(예: `conflict`, `stale`)를 경고 톤으로 표기
   - 안내 문구 필수 포함: *"AI가 편집한 파일만 복원합니다. 명령 부작용은 되돌리지 않습니다."*
4. **승인 요청**:
   - 승인은 `approval_required.toolCallId`로 해당 스텝 카드에 연결하고, 결정이 필요한 동안 카드는 펼친 채로 둡니다. 연결할 카드가 없을 때만 대체 패널을 띄웁니다.
   - 처리된 승인(허용·거부·만료)은 `role="alert"`로 다시 알리지 않고 카드 안의 조용한 상태 표시로 남깁니다.
   - 채팅·팀 화면이 숨겨져 있어도 대기 중인 승인은 ActivityBar 배지(경고색 점)로 보입니다.
   - 허용·거부 버튼은 색 배경 위 흰 글씨 대신 색 테두리 + 본문 글자색을 씁니다 (대비 확보).
5. **모션**: 스피너·펄스·바운스는 `index.css`의 `prefers-reduced-motion` 전역 규칙으로 멈춥니다. 새 애니메이션도 이 규칙에 기대며 별도 예외를 두지 않습니다.
6. **강조 버튼 글자색**: accent 배경 위 글자는 `text-[var(--color-bg)]`를 씁니다 (흰 글씨는 accent 위 대비가 약 3:1로 부족).
