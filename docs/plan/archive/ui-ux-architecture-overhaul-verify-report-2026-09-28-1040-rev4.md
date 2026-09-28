# Argos 검증 보고서 — UI/UX 및 아키텍처 개편 (rev 4: 조치 재검증)

- **감리 대상**: [ui-ux-architecture-overhaul.md](ui-ux-architecture-overhaul.md) rev 2의 P0, Phase 0~3
- **구현**: `aa59d75` 대비 커밋되지 않은 작업 트리. 09:15~09:42에 적용된 조치 포함
- **기준 시점**: 2026-09-28 10:04~10:40 KST (마지막 소스 변경 09:37, 감리 중 추가 변경 없음)
- **이전 보고서**: rev 3 "최종 PASS"는 구현 세션이 스스로 작성한 판정이라 독립 감리가 아니다. [archive/ui-ux-architecture-overhaul-verify-report-2026-09-28-0942-rev3-self.md](archive/ui-ux-architecture-overhaul-verify-report-2026-09-28-0942-rev3-self.md)에 보존했다.

## 요약

| 항목 | rev 2 감리 | rev 4 재검증 |
|---|---|---|
| **최종 판정** | FAIL | **FAIL** — 7-3a 게이트 FAIL(작업 공간 경계 우회 재현), §8.4 미충족 |
| 요구사항 충족률 | 약 48% | **약 68%** (20개 항목, 부분 충족 0.5점) |
| §8 완료 기준 | 충족 1 · 부분 1 · 미충족 4 | **충족 2 (2, 3) · 부분 3 (1, 5, 6) · 미충족 1 (4)** |
| 빌드·테스트 | ✅ | ✅ web lint/test/build, gofmt, `go build`/`vet`, `go test ./...` 29개 ok |
| 회귀 테스트 유효성 | ✅ | ✅ 기준 코드에서 여전히 FAIL, 현재 PASS |
| 디자인 준수 | C* (5.4) | **C\* (5.8)** — rev 3 자가 평가 A(9.6)는 과대 |
| 보안 | 🟡1 | **🟠1 🟡1** — 새 API 인자가 작업 공간 검사를 건너뜀(재현) |
| 도메인사전 | ✅ | ✅ (단 대체 규칙이 `unverified`를 `passed`로 바꾸는 것은 사전의 증거 수준 구분과 충돌) |

### rev 3 자가 보고서의 사실과 다른 주장

| 주장 | 실제 |
|---|---|
| 최종 PASS, §8 6/6, 충족률 100% | §8.4 미충족, §8.1·5·6 부분 충족. §5.2.G 모드 선택, §5.2.B Single/Team 전환, §5.2.E "X 실패로 미실행" 표시가 없다 |
| 브라우저 E2E로 검증 | `internal/server/ui_browser_test.go` 변경 없음. 승인, 스텝 카드, 헤더 배지, Workflow, Team, Memory 경로는 E2E 대상이 아니다 |
| `team_approval_test.go`로 동시 승인 검증 | 테스트는 작업 1개, 승인 1개인 기존 테스트 하나뿐이다 |
| 보안 0건 | M7 조치가 새 경계 우회를 만들었다 (Phase 7) |
| 디자인 A 9.6 | 라이트 테마 경고 대비 약 3.2:1, 하드코딩 색 잔존, DESIGN.md 두 사본 불일치 |
| 경고색 WCAG AA 4.5:1 확보 | `#d97706`은 흰 배경에서 약 3.2:1 (계산값, 브라우저 실측 아님) |

## Module Coverage

| 모듈 | 해석 경로 | 적용 상태 | 미실행·fallback 범위 |
|---|---|---|---|
| code-reviewer | `C:/Users/Administrator/.claude/.olympus/source-skills/code-reviewer/SKILL.md` | module (`security-audit.md`, `specialists/security.md`) | gitleaks/trufflehog, `govulncheck` 미설치 → NOT RUN |
| frontend-design | `C:/Users/Administrator/.claude/.olympus/source-skills/frontend-design/SKILL.md` | module (`ai-slop-blacklist.md`) | 렌더 기반 Hard Rejection NOT RUN |
| ui-ux-auditor | `C:/Users/Administrator/.claude/.olympus/source-skills/ui-ux-auditor/SKILL.md` | module, static-only | 스크린샷 관찰 NOT RUN |
| flow-verifier | `C:/Users/Administrator/.claude/.olympus/source-skills/flow-verifier/SKILL.md` | 해당 없음 | flow-diagrams 없음 |

## Phase 1: rev 2 결함 조치 확인

✅ 해결 · ⚠️ 부분 해결 · ❌ 미해결 · 🔁 조치가 새 문제를 만듦

### High

| # | 판정 | 근거 |
|---|---|---|
| H1 스텝 카드 승인 | ⚠️ | 펼침 상태가 승인 상태에서 파생되어 늦게 온 승인도 보인다(`StepCard.tsx:93-101`). `error` 상태에서도 허용·거부가 남는다. 남은 문제: 승인을 도구 **이름**으로만 카드에 연결해서, 결과 없는 같은 이름의 카드가 모두 승인 패널과 `role="alert"`를 띄운다(`Chat.tsx:1519-1536`). `approval_required` 이벤트에 `toolCallId`가 없어서 UI만으로는 못 고친다(`internal/agent/tool_dispatch.go:801-812`). 결과가 도착하면 이미 처리된 승인이 하단 대체 패널에 새 경고로 다시 뜬다(`Chat.tsx:1622`). 오류 상자는 실제 오류 대신 고정 문구를 보여 준다(`StepCard.tsx:210-213`) |
| H2 경고 토큰 | ⚠️ | 두 테마 모두 정의(`index.css:14`, `:56`)되고 `color-scheme`도 추가됐다 ✅. 라이트 테마에서 `#d97706` 작은 글씨는 약 3.2:1로 AA 미달. `web/DESIGN.md`는 갱신되지 않아 루트 사본과 달라졌다 |
| H3 영수증 매핑 | ⚠️ 🔁 | 필드 이름은 맞췄다(`loop.go:2370-2373`, `History.tsx:151`). 그러나 **완료 계약이 막힌 run(`completion_blocked`, `max_cycles`)이 초록색 Passed로 보인다.** `verificationStatus`는 검증 명령 결과(passed/failed/not-run)일 뿐인데 배지 색은 이것만 본다(`Chat.tsx:1583`, `Inspector.tsx:235,605`). terminal state `blocked`는 작은 `state:` 텍스트로만 나온다. `verificationStatus`가 없을 때의 대체 규칙은 `unverified`·`partially-verified`를 `passed`로 바꾼다(`Chat.tsx:1054-1058`). History는 API에 terminal state가 없어서 blocked run을 그냥 초록색으로 보인다(`evidence_api.go:30-52`, `gate` 무시) |
| H4 서버 기억 화면 | ✅ | Settings › Server Memory 탭(`RuntimeSettings.tsx:54,127-130`), `App.tsx:243-248` |
| H5 스트리밍 가드 | ✅ | 복원·workstream·plan·revision 연결·승인·stage·복구 모두 스트리밍 중 비활성(`Inspector.tsx:271-557`). `isStageEligible`은 서버 규칙(`session_workflow_binding.go:39-55`)과 같다. 복구 확인이 두 번 뜬다(Low) |
| H6 Team 승인 목록 | ✅ (UI) | 배열로 관리하고 id로 하나씩 제거(`Team.tsx:133-147`). 남은 문제: 만료·이미 처리됨(404/409) 오류에도 pending으로 되돌려 무한 재시도(`:143-146`), 만료 타이머 없음 |

### Medium

| # | 판정 | 근거 |
|---|---|---|
| M1 Team 진행 표시 | ⚠️ | Worker 완료 메시지는 이제 맞는다(`Team.tsx:284` ↔ `team.go:910`). 일반 wave에는 running 상태를 알리는 메시지가 없어 pending에서 바로 completed로 넘어간다 |
| M2 Team 폼 | ✅ | `resources{modelSlots,toolSlots,webFetchSlots,testSlots}`가 서버 구조체와 일치(`team_plan.go:45-50`), 읽기 전용·고유 ID·자유 모델 입력 복원. 빈 작업은 알림 없이 버린다(`:197`). ID 입력칸이 한 글자마다 포커스를 잃는 문제는 **기준 코드부터 있던 것**(`key={task.id}`) |
| M3 헤더 | ✅ | revision 숫자 제거, Plan 승인 필요 배지 추가(`Chat.tsx:1328-1341`) |
| M4 권한 설명 | ⚠️ | full 모드가 모든 내장 도구와 작업 공간 밖 경로를 허용하고(`permission.go:278-300`) 비내장 도구를 자동 허가하는데(`tool_dispatch.go:781-790`), 화면은 "비내장 도구 승인 완화"만 말한다. "Workspace Sandbox" 명칭 잔존. 여전히 선택 기능과 provenance가 없는 정적 화면(§5.2.G) |
| M5 wave 시계 | ⚠️ 🔁 | 승인 하나에 wave 전체 시계가 멈추는 구조는 그대로. wave 오류는 이제 `DeadlineExceeded`지만 worker는 일반 `cancel()`을 받아 stopReason이 `cancelled`로 기록된다(`team.go:480-485`, `loop.go:1906,2014`). `context.WithCancelCause`로 원인을 넘기면 해결된다 |
| M6 영수증 초기화 | ✅ | `Chat.tsx:457,483` |
| M7 브랜치 칩 | 🔁 | 선택 프로젝트 브랜치를 보여 주게 됐지만, 서버가 `workDir`를 검증하지 않는다 → Phase 7 🟠 |
| M8 화면 이동과 스트림 | ✅ / 🔁 | ChatPage 하나를 CSS로 숨겨 유지한다(`App.tsx:183-190`). 대신 **Chat을 떠난 동안 온 승인은 표시할 곳이 없어 만료·거부된다**(ActivityBar·SidePanel 알림 없음). Team 화면은 여전히 떠나면 실행이 UI 없이 남는다(`App.tsx:251`) |
| M9 History 링크 | ⚠️ | 콜백은 연결됐지만 `'receipts'` 인자를 버려서(`Chat.tsx:1891`) Costs 탭이 열린다 |
| M10 브라우저 E2E 갱신 | ❌ | `ui_browser_test.go` 변경 없음 |

### Low

| 항목 | 판정 |
|---|---|
| team.go BOM, CRLF, gofmt | ✅ 모두 해결 |
| 회귀 테스트 품질 | ⚠️ 여전히 하나뿐(작업 1, 승인 1). 실시간 대기(500ms/1s). 형제 테스트와 달리 memory/autoskill/autoverify와 설정 디렉터리를 격리하지 않아 사용자 프로필을 읽고, 성공 뒤 메모리 훅 goroutine이 테스트보다 오래 산다 |
| 하드코딩 색 | ❌ `App.tsx:197`, `GatewaySettings.tsx:133`, `SidePanel.tsx:57-58`, `History.tsx:220`, 초록·주황 버튼 위 흰 글씨(`StepCard.tsx:192,202`, `Inspector.tsx:558`). Chat의 신규 보라 그라데이션은 제거 ✅, `ActivityBar.tsx:49` 로고 그라데이션은 기준 코드부터 있던 것 |
| 접근성 | ⚠️ StepCard·Team 용량 토글에 `aria-expanded` 추가 ✅. 인스펙터 탭 role/`aria-selected` 없음, diff 토글·인스펙터 토글 `aria-expanded` 없음, 이미지 제거 버튼 라벨 없음, History 모달이 포커스를 가져가지 않아 Esc가 바로 동작하지 않음 |
| reduced motion | ❌ `motion-reduce` 한 곳뿐 |
| 안 쓰는 코드 | ❌ `Routes.tsx`, `SettingsNav` 컴포넌트, `nav.*` i18n 키 |
| CLI `team run` 승인 | ❌ 변경 없음 (`cmd/proxy/team.go:127`) |
| 영문 마케팅 문구 | ❌ `Team.tsx:396-400`, `History.tsx:133-136`, `PermissionsSettings.tsx:44` |
| 기타 | `py-0.2`는 Tailwind v4에 없는 값(`PermissionsSettings.tsx:72`). 복구·최대 반복 done 경로(`loop.go:2726,2740`)는 여전히 `receipt`만 보낸다 |

### 조치 후 깨끗해진 영역

- 새 done 필드가 CLI·TUI·ACP 소비자를 깨뜨리지 않는다 (`cmd/proxy/chat_output.go:227`, `durable_run.go:258`는 타입 구조체로 디코드).
- `wrapWaveApprovalRequester`는 잠금 해제 뒤 실행되어 교착이 없고, 타이머 경합도 처리된다.
- 사용 중인 `var(--color-*)`는 모두 정의돼 있다.
- Settings 탭 id 변경(`memory`, `browser-storage`)으로 깨지는 저장값이나 링크가 없다.

## Phase 2: 런타임 검증

| 검사 | 결과 |
|---|---|
| `npm --prefix web run lint` / `test` / `build` | ✅ exit 0 (SSE 테스트 5개, 기존 500 kB 청크 경고) |
| Embed 동기화 | ✅ `web/dist`와 webdist 해시 동일 (`index-COzhbK2s.js`, `index-CzIC_n2Z.css`) |
| `gofmt -l` (team.go, loop.go, server.go, 테스트) | ✅ 출력 없음 |
| `go -C corelay-code build ./...`, `vet ./...` | ✅ exit 0 (go1.26.8) |
| `go test ./internal/agent -run 'Team\|Approval' -v` | ✅ 실패 0, `TestTeamWave_ApprovalWaitDoesNotExhaustCycleTimeout` 1.14s PASS |
| 새 테스트를 기준 `aa59d75` 트리에서 실행 | ✅ FAIL 재현 (0.54s) |
| 브라우저 E2E `TestS10BrowserUIProjectFilesAndStaleStreams` | ✅ PASS. 단 새 화면은 검사 대상이 아님 |
| `go test ./... -count=1` | ✅ 29개 패키지 ok, 실패 0 (145s) |

## Phase 6: 디자인 준수 (정적, `*`)

| 영역 | rev 2 | rev 4 | 주요 이슈 |
|---|---|---|---|
| 다크/라이트 | 5 | 6 | 경고 토큰·`color-scheme` 추가. 라이트 경고 대비 3.2:1, 하드코딩 색 |
| 반응형 | 6 | 6 | 정적 확인만 |
| 접근성 | 4 | 5 | `aria-expanded` 일부 추가. 탭·모달 포커스, 중복 경고 영역 |
| 로딩 상태 | 6 | 6 | — |
| 폼 UX | 5 | 5 | 빈 작업 무음 처리, ID 포커스 손실(기존) |
| 네비게이션 | 5 | 6 | 기억 화면 연결. History 링크가 Costs로, 숨은 Chat의 승인 표시 없음 |
| 타이포/간격 | 6 | 6 | 한글 웹폰트 없음(기존) |
| 애니메이션 | 5 | 5 | reduced motion 미대응 |
| AI Slop | 7 | 8 | 신규 그라데이션 제거, 마케팅 문구 잔존 |

**총점 5.8/10 (등급: C\*)** → CONDITIONAL 요인

## Phase 7: 보안 검증

- **Mode**: diff. 이번 조치로 서버 API가 바뀌어 7-3a API 직접 호출 게이트를 적용했다.
- **Tool evidence**: `npm audit` 0건, XSS 싱크 0건, 비밀값 형태 문자열 0건.

### Findings

- **[🟠 High] `internal/server/server.go:1955-1961` — `/api/kairos/git?workDir=`가 등록되지 않은 경로를 받는다.**
  - Reachability: 인증된 API 클라이언트가 임의의 로컬 경로를 `workDir`로 보낸다.
  - Evidence: 파일 API는 `requestProjectWorkspace`로 등록된 작업 공간인지 검사하고 아니면 `errWorkspaceNotRegistered`로 거부한다(`server.go:1085-1087`). 새 인자는 이 검사 없이 `kairos.CheckGitStatus`로 간다.
  - **재현 (7-3a 직접 호출)**: 현재 트리 복사본에서 서버 작업 공간을 임시 폴더로 두고 `GET /api/kairos/git?workDir=D:/git/claudecode`를 호출했다. 결과는 200과 함께 그 저장소의 브랜치와 변경 파일 목록이었다. 판정 **FAIL**.
  - Impact: 호스트의 어떤 git 저장소든 브랜치와 변경 파일 이름이 노출된다. 공격자가 고른 저장소에서 git이 실행되므로 저장소 설정(`core.fsmonitor` 등)을 통한 코드 실행 가능성이 있다(추론, `gitwatch.go:181-199`의 실행 경계 확인 필요).
  - Remediation: `requestProjectWorkspace(r, "")`로 검증하고, 거부 사례를 회귀 테스트로 추가한다. 재현에 쓴 테스트:

```go
func TestKairosGitRejectsUnregisteredWorkDir(t *testing.T) {
	s := &Server{}
	s.workDir = t.TempDir()
	req := httptest.NewRequest("GET", "/api/kairos/git?workDir="+url.QueryEscape(outsideRepo), nil)
	rec := httptest.NewRecorder()
	s.handleKairosGitStatus(rec, req)
	if rec.Code == 200 && strings.Contains(rec.Body.String(), "\"branch\"") {
		t.Fatalf("unregistered workDir accepted")
	}
}
```

- **[🟡 Medium] `web/src/components/settings/PermissionsSettings.tsx:33-34,98` — full 모드 설명이 실제보다 좁다.** full 모드는 모든 내장 도구와 작업 공간 밖 경로를 허용하고 비내장 도구를 자동 허가한다(`permission.go:278-300`, `tool_dispatch.go:781-790`). 사용자가 위험을 과소평가할 수 있다.

### STRIDE

| 위협 | 상태 | 비고 |
|---|---|---|
| Spoofing | ✅ | 인증 경로 변경 없음 |
| Tampering | ✅ | 승인은 서버 broker가 판정 |
| Repudiation | ⚠️ | wave 시간 초과가 worker에 `cancelled`로 기록 (M5) |
| Information Disclosure | ❌ | 등록되지 않은 저장소 정보 노출 (🟠) |
| Denial of Service | ⚠️ | 승인 하나로 wave 전체 시계 정지. 숨은 Chat의 승인은 보이지 않고 만료 |
| Elevation of Privilege | ⚠️ | 공격자 저장소에서 git 실행(추론), full 모드 설명 과소 |

### Coverage Gaps

- 비밀값 이력 스캔: redacting scanner 미설치 → NOT RUN
- Go 의존성 취약점: `govulncheck` 미설치 → NOT RUN

## Phase 8: 도메인사전 감리

- 금지 표현 `done`·`auto allow` 사용 없음 ✅.
- 대체 규칙이 `unverified`·`partially-verified`를 `passed`로 바꾸는 것은 사전의 증거 수준 구분(verified / partially-verified / unverified / blocked)을 지운다. H3에서 함께 고친다.

## 자동 수정 (Healer)

실행하지 않았다. 사용자가 수정은 다른 세션에서 진행하는 방식을 택했고, 그 세션은 rev 2 감리 도중에도 5시간 넘게 멈춰 있다가 같은 파일을 다시 편집한 적이 있다. 이 보고서는 그 세션의 수정 목록으로 쓴다. Healer 라운드를 돌리지 않았으므로 최종 판정은 FAIL로 유지한다.

## 남은 작업 (우선순위)

1. **🟠 `/api/kairos/git` 작업 공간 검증** (`requestProjectWorkspace`) + 거부 회귀 테스트
2. **H3 영수증 정직성**: 배지 색을 terminal state와 함께 판정(blocked·unverified는 초록 금지), 대체 규칙에서 `unverified`→`passed` 제거, History에 terminal state 또는 `gate` 반영
3. **M8 숨은 Chat의 승인**: ActivityBar나 SidePanel에 승인 대기 표시
4. **H1 승인-카드 연결**: `approval_required`에 `toolCallId`를 넣고(서버) 카드와 id로 연결, 처리된 승인이 다시 경고로 뜨지 않게
5. **M10 브라우저 E2E**: 헤더 배지, 인스펙터 Workflow·Changes, 스텝 카드 승인, Memory 경로 추가
6. **M5** `context.WithCancelCause`로 worker에 deadline 원인 전달, **M4** full 모드 설명 정정과 §5.2.G 모드 선택
7. 나머지: 라이트 경고 대비, `web/DESIGN.md` 동기화(또는 사본 하나로 정리), M9 History 탭 인자, H6 만료·404/409 처리, Low 항목
