# S12 의미 기반 코드 탐색

상태: PASS
연결: F29
선행: S02,S03

## 수정 시작점과 소유 범위

internal/agent/repo_map.go; internal/agent/lsp.go; tool definitions/dispatcher; internal/config/config.go; cmd/proxy/diagnostics.go; 기존 reference-capability-absorption-v2/plan.md; processsupervisor

경로는 시작점이다. 실제 production caller와 기존 테스트를 rg로 추적한다. 공통 파일은 plan.md 순차 소유 규칙을 따른다.

## 하위 단계

- A. [PASS] 기존 v2 계획과 현재 production caller를 대조했다. RepoMap은 bounded 구조 요약으로 유지하고, semantic 결과는 별도 `LSP` tool로만 표시한다. 별도 검색 엔진이나 regex 결과 승격은 추가하지 않았다.

- B. [PASS] `LSP` 카탈로그가 Go `definition`/`references`/`diagnostics`를 제공한다. `lspExecutable`, `CORELAY_LSP_EXECUTABLE`, PATH의 `gopls` 순서로 executable을 선택하며 자동 설치하지 않는다. JSON-RPC Content-Length framing, bounded payload/result, semantic location/diagnostic rendering을 구현했다.

- C. [PASS] workspace 경계와 full-mode 외부 경로 정책, 공백·Windows drive URI, full-document `didOpen`, request timeout/cancel, supervisor child 종료를 구현했다. LSP는 도구 카탈로그·dispatcher·plan mode에서 read-only로만 허용되며 server command/edit는 실행하지 않는다.

- D. [PASS] executable 부재·초기화 실패·요청 실패 시 `LSP unavailable`과 `RepoMap fallback (structural only; not semantic)`을 분리해 반환한다. 비 Go 파일도 semantic 결과를 만들지 않고 fallback한다. doctor는 configured gopls availability와 fallback을 표시한다.

## 완료 조건

Q15. 작은 Go fixture에서 여러 파일 참조와 type error를 실제 gopls로 검증. Windows URI/공백/외부 경로 처리. 서버 부재에서도 일반 coding 기능 사용 가능.

## 회귀·제약

regex 결과를 semantic 결과로 표시 금지. LSP가 파일을 바꾸는 command를 read-only navigation으로 허용 금지.

## 검증 기록

- 기준: 이번 변경의 production 소유 파일은 `internal/agent/lsp.go`, `internal/agent/lsp_test.go`, `internal/agent/tools_extended.go`, `internal/agent/path_safety.go`, `internal/agent/context.go`, `internal/agent/loop.go`, `internal/agent/permission.go`, `internal/agent/planmode.go`, `internal/agent/command_effect.go`, `internal/agent/concurrency.go`, `internal/agent/agenttypes.go`, `internal/config/config.go`, `cmd/proxy/diagnostics.go`이며 문서는 `docs/lsp.md`, 이 섹션, README다.
- PASS: `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run 'LSP|ToolSchema|PathSafety|ContextScope|ToolCatalog' -count=1` (exit 0), `go test ./cmd/proxy -run 'Doctor|Diagnostics' -count=1` (exit 0), `go vet ./internal/agent ./internal/config ./cmd/proxy` (exit 0), `git diff --check` (exit 0).
- PASS: local LSP fixture가 실제 `processsupervisor` child와 JSON-RPC framing, initialize/initialized, didOpen, definition/references/diagnostics, cancellation, shutdown, URI의 공백 처리를 검증한다.
- PASS: 임시 검증용 `gopls v0.23.0`을 제품 PATH나 설정에 설치하지 않고 별도 `GOBIN`에 준비한 뒤 `$env:GOTOOLCHAIN='go1.26.1'; go test ./internal/agent -run '^TestInstalledGoplsSemanticFixture$' -count=1 -v` (exit 0)로 여러 파일의 definition/references/type-error를 실제 서버에서 확인했다. Windows 긴 경로 정규화와 공백 포함 URI, full-mode 외부 프로젝트 루트 선택도 같은 fixture에서 통과했다. 검증 후 임시 executable은 제거한다.
- HISTORICAL / RESOLVED IN S13: 당시 `go test ./internal/agent -count=1`에서 LSP 경로와 무관한 Chronos full-mode fixture가 16K context overflow로 실패했으며, S13에서 정책 시스템 프롬프트를 수용하는 32K fixture harness로 조정해 전체 suite가 통과했다. 고정된 gopls 버전이 아닌 사용자별 설치 버전의 pull/push diagnostics 차이는 계속 capability limitation으로 남긴다.
