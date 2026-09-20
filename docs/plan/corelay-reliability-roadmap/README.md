# Corelay Code 신뢰성 개선 실행 계획

작성: 2026-09-13 / source: codex
대상: corelay-code / 기준 HEAD: 82d033bf4490033d782c97566877fed02536799e
상태: S01~S12 PASS, S13 IN_PROGRESS. 이 문서 묶음은 구현과 검증 증거를
섹션별로 기록하지만, 외부 CI·실제 release publish·유료 provider 검증을
완료했다는 뜻은 아니다. 현재 정본 상태는 [plan.md](plan.md)와
[validation.md](validation.md)에서 확인한다.

2026-09-20 Argos 후속: S13.A 실제 acceptance와 bootstrap을 수정하고 압축 중 최신 사용자 요청이 누락되던 결함도 해결했다. 로컬 회귀 PASS, 전체 감리 CONDITIONAL이며 ACP workflow 실행과 미검증 범위는 남아 있다. [감리 보고서](verify-report.md)가 최신 판정이다.

## 읽는 순서

1. [루나 실행 지침](luna-start.md)
2. [범위와 설계 계약](spec.md)
3. [결함·요구 추적표](findings.md)
4. [순서와 상태 장부](plan.md)
5. 해당 [섹션 작업서](sections/index.md)
6. [API·저장 계약](contracts.md), [검증 기준](validation.md)
7. [독립 검토와 정정](review.md)

이번 계획은 기존 제품의 보수·연결 작업이다. 기존 Agent Kernel, dispatcher, approval broker, process supervisor, durable session, Workstream, TeamPlan, capability profiler를 재사용한다. 별도의 에이전트 엔진이나 작업 관리 서버를 만들지 않는다. 완전한 새 제품 설계 파이프라인의 완료를 주장하지 않는다.

## 현재 작업 보존

제품 명령은 D:\git\claudecode\corelay-code 안에서 실행한다.
공유 기억·대화·핸드오프 루트는 부모 D:\git\claudecode 이다. 두 Git 이력을 합치지 않는다.

시작 시 git status --short와 diff를 다시 확인한다. 계획 작성 시 기존 미커밋 변경:
README.md, go.mod, go.sum, internal/agent/tool_web.go, tool_web_test.go, tools.go, tools_extended.go,
새 tool_web_browser.go, tool_web_browser_test.go, tool_web_enrichment_test.go, tool_web_markdown.go, docs/web-fetch.md.
이 변경은 승인된 로컬 Chromium 웹수집 구현이다. reset/clean/stash로 제거하지 않는다.
부모의 docs/INDEX.md, docs/llms.txt, scripts/workspace-doc-union.json 및 .claude/settings.json도 이번 구현 소유가 아니다.

## 계획의 완료 기준

모든 기존 지적을 findings.md에 연결하고, 구현 섹션마다 파일·의존성·동작 계약·회귀 시나리오·중단 조건을 제공한다.
실제 구현 완료는 각 섹션의 증거가 작성된 뒤에만 판정한다. 계획 문서 생성은 구현 완료가 아니다.
