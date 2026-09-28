# Argos 검증 보고서 — UI/UX 및 아키텍처 개편 (rev 5: 직접 조치 후 재검증)

- **감리 대상**: [ui-ux-architecture-overhaul.md](ui-ux-architecture-overhaul.md) rev 3의 P0, Phase 0~3
- **구현**: `aa59d75` 대비 커밋되지 않은 작업 트리. rev 4 남은 작업 전체를 이 세션에서 직접 조치
- **기준 시점**: 2026-09-28 13:00경 KST
- **독립성 고지**: 이번 조치는 감리자(이 세션)가 직접 구현했다. 판정은 실행 증거(빌드·단위·브라우저 E2E·경계 재현 테스트)에 기대고, 별도의 읽기 전용 검토자가 변경을 한 번 더 검토했다(발견 14건 중 조치 결과는 아래 표).
- **이전 보고서**: [rev 4](archive/ui-ux-architecture-overhaul-verify-report-2026-09-28-1040-rev4.md) (FAIL), [rev 3 자가 보고서](archive/ui-ux-architecture-overhaul-verify-report-2026-09-28-0942-rev3-self.md)

## 요약

| 항목 | rev 4 | rev 5 |
|---|---|---|
| **최종 판정** | FAIL | **CONDITIONAL** — 필수 기능·보안 게이트 통과, 경고 2건(§8.1 E2E 범위, §8.6 렌더 미관찰) |
| 요구사항 충족률 | 약 68% | **약 93%** (20개 항목, 부분 충족 0.5점) |
| §8 완료 기준 | 충족 2 · 부분 3 · 미충족 1 | **충족 4 (2, 3, 4, 5) · 부분 2 (1, 6)** |
| 빌드·테스트 | ✅ | ✅ lint, 타입 검사, `npm test` 5/5, gofmt, vet, `go test ./...` 29개 ok, `git diff --check` |
| 브라우저 E2E | ✅ (새 화면 미검증) | ✅ 기존 S10 갱신 + 새 승인 흐름 E2E |
| 7-3a API 게이트 | FAIL (경계 우회 재현) | **PASS** (미등록 403·본문에 branch 없음) |
| 디자인 준수 | C* (5.8) | C* (6.9) — 정적 기준, 렌더 관찰 NOT RUN |
| 보안 | 🟠1 🟡1 | 🔴0 🟠0 🟡0 · scanner 미설치 영역 NOT RUN |

## Module Coverage

| 모듈 | 해석 경로 | 적용 상태 | 미실행 범위 |
|---|---|---|---|
| code-reviewer | `C:/Users/Administrator/.claude/.olympus/source-skills/code-reviewer/SKILL.md` | module | gitleaks/trufflehog, `govulncheck` 미설치 → NOT RUN |
| frontend-design | `C:/Users/Administrator/.claude/.olympus/source-skills/frontend-design/SKILL.md` | module | 렌더 기반 Hard Rejection NOT RUN |
| ui-ux-auditor | `C:/Users/Administrator/.claude/.olympus/source-skills/ui-ux-auditor/SKILL.md` | module, static-only | 스크린샷 관찰 NOT RUN |
| flow-verifier | `C:/Users/Administrator/.claude/.olympus/source-skills/flow-verifier/SKILL.md` | 해당 없음 | flow-diagrams 없음 |

## Phase 1: rev 4 남은 작업 조치 결과

| rev 4 항목 | 판정 | 조치와 근거 |
|---|---|---|
| 🟠 `/api/kairos/git` 경계 | ✅ | `requestProjectWorkspace` 경유(`internal/server/server.go` `handleKairosGitStatus`). 회귀: `TestKairosGitStatusRejectsUnregisteredWorkspace` |
| H3 영수증 정직성 | ✅ | 판정 단일화 `web/src/lib/receipts.ts` `describeReceipt` + `components/ReceiptBadge.tsx`를 Chat·Inspector·History가 공유. terminal state에서 passed를 만들던 대체 규칙 제거. 영수증 API에 `terminalState` 추가(`evidence_api.go`). `max_cycles` 영수증도 blocked(`receipt.go` `receiptTerminalState` + 단위 테스트) |
| M8 숨은 Chat의 승인 | ✅ | ChatPage·TeamPage를 한 번만 마운트해 숨김 유지, ActivityBar 배지(`data-approval-badge`)와 모드 전환 배지. 배지를 누르면 결정할 곳(Team 승인이면 Team)으로 이동 |
| H1 승인-카드 연결 | ✅ | `approval_required`에 `toolCallId`(`tool_dispatch.go`), 도구 결과·승인을 호출 ID로 연결(`Chat.tsx` `findOpenToolMessage`). 처리된 승인은 경고로 다시 뜨지 않음, 실제 오류 문구 표시, 결정 전 카드 강제 펼침 |
| M10 브라우저 E2E | ⚠️ | 승인·배지·Memory·영수증·Files·테마 경로 E2E 추가. Plan 승인·revision 연결·단계 복구 경로는 E2E 없음(정적 확인만) |
| M5 wave 시간 초과 원인 | ✅ / ⚠️ | `context.WithCancelCause(DeadlineExceeded)`로 worker가 `deadline_exceeded` 기록(`team.go`, `loop.go`). 승인 하나에 wave 전체 시계가 멈추는 설계는 유지 |
| M4 권한 설명·§5.2.G 선택 | ✅ | Settings › 안전·권한에 모드 선택(라디오), full은 명시적 확인 후에만 선택, 요청마다 `executionPolicy.mode` 전송(Chat·Team). 설명은 `permission.go`·`tool_dispatch.go` 동작 그대로 |
| 라이트 경고 대비 | ✅ (계산) | `--color-warning`·`--color-yellow` 라이트 `#92400e` — 흰 배경 약 7.1:1, `#f0f1f4` 약 6.3:1 |
| DESIGN.md 사본 | ✅ | `web/DESIGN.md`는 정본 안내 한 줄로 교체 |
| M9 History 탭 | ✅ | 인스펙터 링크는 Receipts 탭으로, ActivityBar 진입은 첫 탭으로 |
| H6 Team 만료·404/409 | ✅ | 만료 타이머, 404/409는 "더 이상 처리할 수 없음"으로 제거, 그 외 오류는 카드에 표시 |
| Low: reduced motion | ✅ | `index.css` 전역 `prefers-reduced-motion` |
| Low: 안 쓰는 코드 | ✅ | `Routes.tsx` 삭제, `SettingsNav`는 타입만, `nav.*` 키 제거 |
| Low: 마케팅 문구 | ✅ | Team·History·Permissions 설명 문구로 교체 |
| Low: CLI `team run` | ✅ (문서화) | 시작 시 stderr 안내 + README 한 줄. 대화형 승인은 만들지 않음 |
| 정보 구조(§7) | ✅ | ActivityBar 3개(작업 공간·기록·설정), Team은 입력창의 "대화 | 팀" 모드, 설정 4그룹 한글·하네스 기본 접힘 |

### 독립 검토 발견과 조치

| # | 발견 | 조치 |
|---|---|---|
| M1 | Team 승인 배지가 Chat으로 이동시킴 | 배지 클릭 시 Team으로 이동, 모드 전환 배지 |
| M2 | 인스펙터 증거 조회가 기본 프로젝트만 봄 | `scope=all` |
| M3 | Team이 권한 모드·프로젝트를 안 보냄 | `workDir`·`executionPolicy` 전송(서버가 둘 다 검증) |
| M4 | `max_cycles` 영수증이 History에서 Verified | 영수증 terminal state blocked + 단위 테스트 |
| M5 | read-only·plan 모드에서 막힌 호출이 화면에 없음 | 카드 없는 도구 결과를 새 카드로 표시 |
| L | 재로드 후 이름 대체 연결 오류 | ID가 있으면 ID로만 연결 |
| L | `completionBlocked`는 숫자 | 숫자·불리언 모두 처리 |
| L | 카드 없는 만료 승인 무음 | `role="status"` 안내 |
| L | 결과 없는 카드 스피너 계속 | run 종료 후 "No result recorded" 표시, 초록 체크 대신 중립 아이콘 |
| L | History가 마지막 탭 기억 | ActivityBar 진입 시 첫 탭 |
| L | 실패한 Team run이 중립 | `failed` 수가 있으면 Failed |
| L | 하네스 접기 버튼 무반응 | 접기 상태를 사용자 조작 기준으로 |
| L | 특수문자 작업 ID 파싱 실패, 실행 중 편집 | ID 문자 제한 검증, 실행 중 편집 잠금 |
| L | read-only 설명 과장 | 읽기 전용 명령·웹 조회 허용 사실 반영 |
| L | 모드 전환·탭 키보드 | 모드 전환 화살표 키, 탭 `aria-controls`·`tabpanel` 연결 (탭 화살표 키는 미구현) |
| 계획 | §5.2.G가 full 모드 MCP 승인을 유지한다고 적음 | 코드 동작에 맞춰 계획 정정 |

### §8 완료 기준

| 기준 | 판정 | 근거 |
|---|---|---|
| 1. 기능 보존 | ⚠️ | 모든 경로 도달 가능. E2E는 승인·복원·Memory·Files·영수증 경로를 확인. Plan 승인·revision 연결·단계 복구는 정적 확인만 |
| 2. 첫 화면 | ✅ | revision 숫자 없음, 권한 모드·승인 배지 표시 |
| 3. 복원 정직성 | ✅ | 문구·버튼 조건 유지 |
| 4. 영수증 정직성 | ✅ | E2E: blocked + 검증 passed → Blocked(적색), 검증 없는 실제 run → success 톤 아님. 단위: `max_cycles` 영수증 blocked |
| 5. 정보 구조 | ✅ | §7 이전표의 목적지 모두 동작 (Routes 페이지는 §9-4 결정대로 제거) |
| 6. 시각 일관성 | ⚠️ | 이모지 0, 토큰 사용, 대비 계산값 충족. 실제 렌더에서의 라이트·다크 대비 확인은 NOT RUN |

## Phase 2: 런타임 검증

| 검사 | 결과 |
|---|---|
| `npm --prefix web run lint` / `npx tsc -b` / `npm test` / `run build` | ✅ (SSE 테스트 5/5, 기존 500 kB 청크 경고) |
| webdist 동기화 | ✅ `web/dist`와 embed 번들 동일 |
| `gofmt -l` (변경·신규 Go 파일 전체) | ✅ 출력 없음 |
| `go -C corelay-code vet ./...` | ✅ |
| `TestKairosGitStatusRejectsUnregisteredWorkspace` | ✅ |
| `TestS10BrowserUIProjectFilesAndStaleStreams` (갱신) | ✅ 4.37s |
| `TestOverhaulBrowserApprovalFlowAndDestinations` (신규) | ✅ 1.99s — 실제 서버가 낸 승인 요청으로 카드·배지·거부·재경고 없음·영수증 톤·Memory 확인 |
| `TestTeamWave_*` 3종 | ✅ (기존 테스트는 기준 `aa59d75`에서 FAIL 재현 확인됨) |
| `TestReceiptTerminalStateBlocksFailedRunTerminals` | ✅ |
| `go test ./... -count=1` | ✅ 29개 패키지 ok |
| `git diff --check` | ✅ |

## Phase 6: 디자인 준수 (정적, `*`)

| 영역 | rev 4 | rev 5 | 비고 |
|---|---|---|---|
| 다크/라이트 | 6 | 7 | 경고 대비 조정, 흰 글씨 버튼 → 토큰 글자색. 손대지 않은 설정 컴포넌트의 `surface-N`/`brand-N` 체계는 그대로 |
| 반응형 | 6 | 6 | 정적 확인만 |
| 접근성 | 5 | 7 | 탭 role·연결, `aria-expanded`, 아이콘 버튼 라벨, 중복 경고 제거. 탭 화살표 키 미구현 |
| 로딩 상태 | 6 | 7 | History 오류·재시도, 증거 로딩 표시 |
| 폼 UX | 5 | 7 | Team 검증·ID 규칙·실행 중 잠금 |
| 네비게이션 | 6 | 8 | 3개 목적지, 배지, History 탭 |
| 타이포/간격 | 6 | 6 | 한글 웹폰트 없음(기존) |
| 애니메이션 | 5 | 7 | 전역 reduced-motion |
| AI Slop | 8 | 8 | 로고 보라 그라데이션 제거, 마케팅 문구 제거 |

**총점 6.9/10 (C\*)** → CONDITIONAL 요인. 렌더 관찰 뒤 재채점 필요.

## Phase 7: 보안 검증

- **7-3a API 직접 호출**: 변경된 API는 `/api/kairos/git`(작업 공간 인자)와 `/api/team` 요청 본문(웹이 `workDir`·`executionPolicy`를 새로 보냄).
  - `/api/kairos/git`: 미등록 존재 경로 → 403 `workspace_not_registered`, 본문에 branch 없음; 없는 경로 → 400; 기본값·등록 프로젝트 → 200 (`TestKairosGitStatusRejectsUnregisteredWorkspace`). **PASS**
  - `/api/team`: 서버가 기존대로 `requestProjectWorkspace(r, body.WorkDir)`와 `resolveServerExecutionPolicy`로 검증(`server.go` handleTeamExecute). 웹 변경은 기존 계약 안의 값만 보냄. 직접 호출 재실행은 NOT RUN(서버 코드 변경 없음).
- XSS 싱크 없음, 비밀값 형태 문자열 없음, 의존성 변경 없음(`npm audit` 기존 0건).
- 권한 설명 오류(rev 4 🟡)는 코드 동작과 일치하도록 정정.

### STRIDE

| 위협 | 상태 |
|---|---|
| Spoofing | ✅ 인증 경로 변경 없음 |
| Tampering | ✅ 승인·모드 판정은 서버 |
| Repudiation | ✅ wave 시간 초과가 `deadline_exceeded`로 기록 |
| Information Disclosure | ✅ 작업 공간 밖 저장소 정보 노출 차단 |
| Denial of Service | ⚠️ 승인 하나로 wave 전체 시계 정지(설계 유지) |
| Elevation of Privilege | ✅ full 모드는 명시적 확인 후 선택, 설명 정확 |

### Coverage Gaps

- 비밀값 이력 스캔, `govulncheck`: 도구 미설치 → NOT RUN

## Phase 8: 도메인사전

- 영수증 판정이 사전의 증거 수준(verified / partially-verified / unverified / blocked)을 그대로 쓴다 ✅. 금지 표현 없음 ✅.

## 남은 항목 (CONDITIONAL 사유와 범위 밖)

1. **§8.1**: Plan 승인·revision 연결·단계 복구 경로의 브라우저 E2E 없음.
2. **§8.6**: 실제 렌더에서의 라이트·다크 대비·반응형 관찰 NOT RUN (디자인 점수 `*`).
3. 서브에이전트의 승인 요청은 서버에서 삼켜져 UI에 도달하지 않음 (`internal/agent/subagent.go`, 기존 동작).
4. 세션 저장이 `toolCallId`를 보존하지 않음 — 재로드 후 오래된 카드는 연결되지 않고 "No result recorded"로 표시 (잘못 연결되지는 않음).
5. 승인 하나에 wave 전체 시계가 멈추는 설계(M5 잔여), 헤더 모델 목록 하드코딩(B3 이후), 탭 화살표 키, 두 토큰 체계 통합, 한글 웹폰트.
6. 백엔드 트랙 B2, B3, B5~B10 미착수 (B1은 terminalState 추가만).
