# Chronos Log
Started: 2026-06-25T21:55:07+09:00
Engine: direct
Scope: WebSearch/WebResearch, TeamPlan/worker harness, local-model loop engineering, observability/regression replay, and related web UI surfaces.
Verification Gate: targeted tests for each cycle; go test ./...; go vet ./...; go build ./cmd/proxy; npm run lint/build when web changes; git diff --check.
Completion Promise: Local-model loop engineering has bounded TeamPlan execution, durable receipts, traceable/replayable regressions, web research support, and verified tests/builds.

-- Cycle 1 ------------------------------------------------
Issue: WebFetch did not extract page-level published/updated dates from fetched HTML, so freshness evidence stopped at search snippets.
Fix: Added PublishedAt/UpdatedAt to webFetchResult, extracted dates from meta tags, JSON-LD, and time elements, propagated frame dates, and printed dates in WebFetch/WebResearch output.
Verify: go test ./internal/agent -count=1 -> PASS
-----------------------------------------------------------

-- Cycle 2 ------------------------------------------------
Issue: Multi-provider dedupe could merge a later provider's better snippet/date into an existing result without recomputing relevance/freshness/authority scores.
Fix: Recomputed all ranking score components after provider fusion, before final sorting.
Verify: go test ./internal/agent -count=1 -> PASS; go vet ./internal/agent -> PASS
-----------------------------------------------------------

Cycles 1-2 Gate (Web Research):
- go test ./... -> PASS
- go build ./cmd/proxy -> PASS
- git diff --check -> PASS

Cycles 1-2 Requirement Status:
- Multi-provider search -> proved
- Latest/freshness ranking -> proved
- Body fetch extraction with iframe/mobile support -> proved
- Fetched-page date extraction -> proved
- Tests/build verification -> proved

-- Cycle 3 ------------------------------------------------
Issue: Daedalus-style PM/Worker harness was still a concept; the Go runtime did not expose TeamPlan, AgentTask, local-model capacity limits, or a worker CLI entry point.
Fix: Added provider-neutral TeamPlan/AgentTask/AgentSpec models, a CapacityScheduler with conservative local defaults, optional team capacity on the server API, and `aniclew worker run` for single-task provider/Ollama execution. Updated README and operating-model docs.
Verify: go test ./internal/agent -count=1 -> PASS; go vet ./internal/agent -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS; go run ./cmd/proxy worker run --help -> PASS
-----------------------------------------------------------

-- Cycle 4 ------------------------------------------------
Issue: TeamPlan could be modeled but not run as a full task graph from the CLI.
Fix: Added `aniclew team run --plan team-plan.json` and `aniclew team run --objective ...`, loading plan/provider/model defaults and executing TeamPlan tasks through the existing Team runner with configured capacity and verification.
Verify: go test ./cmd/proxy -count=1 -> PASS; go build ./cmd/proxy -> PASS; go run ./cmd/proxy team run --help -> PASS
-----------------------------------------------------------

-- Cycle 5 ------------------------------------------------
Issue: Verification surfaced a nondeterministic WebSearch test failure because fused provider order depended on Go map iteration.
Fix: Added stable provider ordering before fusing search results, preserving deterministic provider arrays such as duckduckgo,google.
Verify: go test ./internal/agent -run TestFuseSearchResultsDeduplicatesProviders -count=20 -> PASS; go test ./internal/agent -count=1 -> PASS; go vet ./internal/agent -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS; worker/team help commands -> PASS
-----------------------------------------------------------

-- Cycle 6 ------------------------------------------------
Issue: The new worker/team CLI paths needed an integration smoke test to prove they actually traverse provider streaming, RunLoop, Team wave execution, and shutdown.
Fix: Tested both paths against a temporary fake Ollama/OpenAI-compatible SSE server without requiring a real local model.
Verify: fake SSE smoke -> worker run completed 1/1 task with exit 0; team run completed 5/5 generated Daedalus tasks with exit 0.
-----------------------------------------------------------

-- Cycle 7 ------------------------------------------------
Issue: TeamPlan exposed task-level provider/model fields, but the Team runner still executed every task with the team default runtime.
Fix: Added task provider/model metadata to TeamTask, ProviderFactory support in TeamConfig, runtime resolution per task, CLI/server provider-factory wiring, and tests for provider/model overrides.
Verify: go test ./internal/agent -count=1 -> PASS; go vet ./internal/agent -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS; fake SSE worker/team smoke -> PASS
-----------------------------------------------------------

-- Cycle 8 ------------------------------------------------
Issue: TeamPlan could be generated and run, but malformed plans were not rejected before workers started.
Fix: Added TeamPlan normalization, SaveTeamPlan, validation errors for duplicate task IDs, missing dependencies, dependency cycles, broken stage references, missing objective, empty tasks, and missing write-task file scopes. Added `aniclew team plan --objective ... --out ...` and `aniclew team validate --plan ...`; `team run` now validates before execution.
Verify: go test ./internal/agent -count=1 -> PASS; go vet ./internal/agent -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; team plan -> validate -> fake SSE run smoke -> PASS
-----------------------------------------------------------

-- Cycle 9 ------------------------------------------------
Issue: TeamPlan execution produced task output files but no structured run-level receipt, so unattended loops could not resume, audit, or regress against a durable Team run state.
Fix: Added TeamRunReceipt/TeamTaskReceipt, outputPath tracking on TeamTask, CLI worker/team receipt writes, server `/api/team` receipt events, and receipt unit coverage. Team receipts are compact JSON files under the normal receipts workspace namespace with `team-` filename prefixes.
Verify: go test ./internal/agent -count=1 -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; fake SSE `team run --objective` smoke wrote a completed 5-task receipt; fake SSE `worker run --prompt` smoke wrote a completed 1-task receipt.
-----------------------------------------------------------

-- Cycle 10 -----------------------------------------------
Issue: CLI TeamPlan runs validated malformed plans before spawning workers, but the server `/api/team` path still built tasks directly and could let duplicate IDs or broken dependency graphs reach execution.
Fix: Built a server-side TeamPlan first, normalized legacy empty file scopes to explicit `**`, compacted empty file/dependency entries, validated the plan before creating workers, and then executed the validated plan task contract.
Verify: go test ./internal/server -count=1 -> PASS
-----------------------------------------------------------

-- Cycle 11 -----------------------------------------------
Issue: Team file ownership enforcement used global worker state, so parallel workers in the same wave could overwrite the active worker ID and weaken cross-file ownership checks.
Fix: Added ToolExecutionOptions and RunOptions worker ownership fields, routed Team workers through RunLoopWithOptions with explicit worker ID/checker, and kept the old globals only as backward-compatible fallback.
Verify: go test ./internal/agent -count=1 -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; go vet ./... -> PASS; git diff --check -> PASS; fake SSE `worker run --prompt` smoke -> PASS
-----------------------------------------------------------

-- Cycle 12 -----------------------------------------------
Issue: TeamPlan exposed task resource reservations, but Team wave execution only capped model worker count and did not honor explicit model/tool/web/test slot reservations or same-wave file-scope conflicts before spawning workers.
Fix: Added resources to TeamTask and TeamTaskReceipt, propagated AgentTask resources into Team tasks, accepted resources on the server Team API, and split each topological wave into capacity-aware batches using model/tool/web/test slots plus file-scope lock checks.
Verify: go test ./internal/agent -count=1 -> PASS; go test ./internal/server -count=1 -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; go vet ./... -> PASS; git diff --check -> PASS; fake SSE `team run --objective` smoke -> PASS
-----------------------------------------------------------

-- Cycle 13 -----------------------------------------------
Issue: The web Team page still posted the legacy reduced task shape, so users could not exercise the newer TeamPlan contract for objective, verification command, task kind/role, provider/model overrides, read-only mode, capacity, or resource reservations from the UI.
Fix: Reworked `web/src/pages/Team.tsx` to submit the full server `/api/team` contract, typed gateway/audit data, added HTTP/SSE error handling, preserved local-safe capacity defaults, and synced the Vite build into `internal/server/webdist`.
Verify: npm run build -> PASS; npx eslint src/pages/Team.tsx -> PASS; go test ./internal/agent -count=1 -> PASS; go test ./internal/server -count=1 -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; go vet ./... -> PASS; git diff --check -> PASS. `npm run lint` is still red due pre-existing errors in App/SidePanel/ActivityBar/Kairos/Memory/Routes/Settings and is promoted to the next cycle.
-----------------------------------------------------------

-- Cycle 14 -----------------------------------------------
Issue: The frontend lint gate was red, so UI changes could not be verified by the project-level `npm run lint` command.
Fix: Removed the duplicate ActivityBar route case, replaced remaining UI `any` usages with typed API/SSE/browser contracts, moved Toast global dispatch into `web/src/lib/toast.ts`, adjusted initial effect loaders to satisfy React lint rules, and kept the rebuilt web bundle synced into `internal/server/webdist`.
Verify: npm run lint -> PASS; npm run build -> PASS; go test ./internal/agent -count=1 -> PASS; go test ./internal/server -count=1 -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; go vet ./... -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 15 -----------------------------------------------
Issue: Playwright render verification showed the new Team page was not reachable from the active ActivityBar navigation, leaving Team/Memory/Routes/Kairos pages hidden even though `App.tsx` could render them.
Fix: Added Team, Memory, Routes, and Kairos entries to `web/src/components/ActivityBar.tsx`, using the existing icon cases and preserving the compact rail layout.
Verify: Playwright `http://127.0.0.1:5300` -> Team button visible and Team page rendered; screenshot saved as `aniclew-team-page-cycle15.png`; npm run lint -> PASS; npm run build -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; go vet ./... -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 16 -----------------------------------------------
Issue: Playwright console verification showed TeamPage initial data loading still surfaced `Error: HTTP 502` stack traces when the backend API was unavailable, because gateway users/audit fetches were not independently caught.
Fix: Made TeamPage bootstrap fetches for gateway users, audit, and providers fail closed to empty arrays so the page remains renderable without unhandled load exceptions.
Verify: npx eslint src/pages/Team.tsx -> PASS; npm run lint -> PASS; npm run build -> PASS; go test ./... -> PASS; go build ./cmd/proxy -> PASS; go vet ./... -> PASS; git diff --check -> PASS; Playwright Team navigation -> no TeamPage `Error: HTTP 502` stack remains, only expected resource 502 entries while backend is not running.
-----------------------------------------------------------

-- Cycle 17 -----------------------------------------------
Issue: Server Team runs wrote durable receipts but were not first-class observable runs: `/api/team` had no trace id, no `/api/run-traces` record, and no workstream timeline events.
Fix: Added Team API `workstreamId` handling, SSE session/workstream trace metadata, Team run trace recording, and workstream start/completion/failure timeline updates tied to the saved Team receipt.
Verify: go test ./internal/server -run TestHandleTeamExecuteRecordsTraceAndWorkstream -count=1 -> PASS; go test ./internal/server -count=1 -> PASS; go test ./... -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS; npm run lint -> PASS; npm run build -> PASS
-----------------------------------------------------------

-- Cycle 18 -----------------------------------------------
Issue: The Team API accepted `workstreamId` after Cycle 17, but the Team web page had no workstream selector and still could not attach a Team run to a workstream from the UI.
Fix: Loaded workstreams on `TeamPage`, added a Workstream select control, sent `workstreamId` in the `/api/team` payload, and surfaced session trace/workstream SSE metadata in the Team log.
Verify: npx eslint src/pages/Team.tsx -> PASS; npm run lint -> PASS; npm run build -> PASS; copied Vite assets into `internal/server/webdist`; go test ./... -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS; Playwright Team render -> Workstream combobox visible, only expected backend-offline 502 resource errors.
-----------------------------------------------------------

-- Cycle 19 -----------------------------------------------
Issue: Team runs could be attached to a workstream and recorded in timeline/trace state, but the worker RunLoop did not receive the rendered workstream context, so workers lacked the durable objective/background during execution.
Fix: Added `WorkstreamContext` to `TeamConfig`, passed it into each worker `RunLoopWithOptions`, and wired `/api/team` to render the selected workstream context before creating the Team runner.
Verify: go test ./internal/server -run TestHandleTeamExecuteRecordsTraceAndWorkstream -count=1 -> PASS and asserts worker system prompt contains `## Workstream Context`; go test ./... -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 20 -----------------------------------------------
Issue: `/api/team` closed its SSE response after `done` without the explicit `stream_end` frame used by the other long-running endpoints, making harness-side stream completion less consistent.
Fix: Added a final `stream_end` SSE frame to the Team API response loop and updated the Team API test to require it.
Verify: go test ./internal/server -run TestHandleTeamExecuteRecordsTraceAndWorkstream -count=1 -> PASS; go test ./... -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 21 -----------------------------------------------
Issue: Failed Team run traces could be promoted into regression cases, but they were not replayable even though Team receipts contain enough durable state to reconstruct the task graph.
Fix: Preserved task descriptions in Team receipts, marked Team regression cases with receipt paths as replayable, and added `/api/regressions/{id}/run` support that rebuilds a TeamPlan from the recorded receipt and replays it through the Team runner.
Verify: go test ./internal/observability -run TestTrackerCreateRegressionCaseFromFailedTeamRun -count=1 -> PASS; go test ./internal/server -run TestRunRegressionTeamAPI -count=1 -> PASS; go test ./... -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 22 -----------------------------------------------
Issue: The Costs/Activity UI still disabled replay for replayable Team regression cases even after the backend gained Team regression replay support.
Fix: Allowed replay for `team` regression cases, kept unsupported kinds disabled, and changed the regression case label/title helpers so Team cases show objective/team metadata instead of falling back to a Chronos-only task field.
Verify: npx eslint src/pages/Costs.tsx -> PASS; npm run lint -> PASS; npm run build -> PASS; copied Vite assets into `internal/server/webdist`; go test ./... -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS; Playwright Activity render -> PASS with only expected backend-offline `/api/*` resource 502s plus the promoted Cycle 23 `/api/costs` stack signal.
-----------------------------------------------------------

-- Cycle 23 -----------------------------------------------
Issue: Activity page bootstrap handled optional metrics/traces/regression fetch failures, but `/api/costs` still lacked a fallback and produced repeated `Error: HTTP 502` console stacks when the backend API was offline.
Fix: Added a zero-cost fallback for `/api/costs` so the Activity page can render in the offline/dev-server-only state without unhandled load errors.
Verify: npx eslint src/pages/Costs.tsx -> PASS; npm run lint -> PASS; npm run build -> PASS; copied Vite assets into `internal/server/webdist`; go test ./... -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS; Playwright Activity render screenshot `aniclew-activity-cycle23.png` -> PASS, console now only shows expected backend-offline `/api/*` resource 502s and no `Error: HTTP 502` stack.
-----------------------------------------------------------

-- Cycle 24 -----------------------------------------------
Issue: `CapacityConfig.fileScopeLock` was exposed in the TeamPlan/API contract, but JSON `false` was indistinguishable from an omitted field, so normalization always restored the safe default and callers could not deliberately disable file-scope locking.
Fix: Tracked whether `fileScopeLock` was explicitly present during JSON unmarshal and emitted explicit false on marshal, preserving safe defaults when omitted while honoring deliberate `fileScopeLock:false`.
Verify: go test ./internal/agent -run TestCapacityConfigPreservesExplicitFileScopeLockFalse -count=1 -> PASS; go test ./internal/agent -count=1 -> PASS; go test ./... -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 25 -----------------------------------------------
Issue: The Chronos log header still described only the initial web research scope and used a stale `Final Gate` section even though later cycles expanded into TeamPlan, local-model loop engineering, observability, and UI regression replay.
Fix: Updated the log header to match the current expanded scope and relabeled the early web research final gate/status as the Cycles 1-2 gate instead of a whole-session final gate.
Verify: git diff --check -> PASS; go test ./... -count=1 -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS
-----------------------------------------------------------

-- Cycle 26 -----------------------------------------------
Issue: Regression replay attempts returned HTTP 500 without recording a regression run when no provider was configured, so failed replay attempts disappeared from observability instead of becoming durable unsupported runs.
Fix: Converted no-provider replay attempts for Chronos/Team cases into recorded `unsupported` regression runs, with defensive handling in both the shared regression runner and the Team-specific replay path.
Verify: go test ./internal/server -run TestRunRegressionRecordsUnsupportedWithoutProvider -count=1 -> PASS; go test ./internal/server -count=1 -> PASS; go test ./... -count=1 -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 27 -----------------------------------------------
Issue: The README/API table and operating-model document did not expose the new agentic run trace and regression replay surface, so the checker/replay layer existed in code and UI but was hard to discover from docs.
Fix: Documented Chronos/Team run traces, regression case promotion, regression replay attempts, and the `/api/run-traces`, `/api/regressions`, and `/api/regression-runs` endpoints.
Verify: git diff --check -> PASS; go test ./... -count=1 -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS
-----------------------------------------------------------

-- Cycle 28 -----------------------------------------------
Issue: Qwen/Ollama smoke testing proved the Team API run path, but the in-memory TeamRunReceipt did not preserve `workDir` until disk write time, so API Team run traces could lose the workspace that produced the receipt.
Fix: Populated TeamRunReceipt.WorkDir when building the receipt and extended receipt/server Team API tests to assert the workspace is preserved in run traces.
Verify: qwen3-coder:30b CLI worker write+verify -> PASS; qwen3-coder:30b API Team write+verify -> PASS; qwen3.6:27b CLI worker write+verify -> PASS; API smoke `TRACE_WORKDIR` matched the temp workspace -> PASS; go test ./internal/agent ./internal/server -run "TestWriteTeamRunReceiptToDir|TestHandleTeamExecuteRecordsTraceAndWorkstream" -count=1 -> PASS; go test ./... -count=1 -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS; commit 2de422d `fix(observability): preserve team trace workdir`.
-----------------------------------------------------------

-- Cycle 29 -----------------------------------------------
Issue: `/api/agent-types` exposed only builtin roles even though `LoadCustomAgentTypes` promised project `.claude/agents/*.md` support, so local/team orchestration could not discover project-specific agent roles.
Fix: Implemented a conservative markdown frontmatter parser for project custom agents, using the markdown body as the system prompt, and merged custom agents into the server `/api/agent-types` response.
Verify: go test ./internal/agent -run TestLoadCustomAgentTypes -count=1 -> PASS; go test ./internal/server -run TestHandleAgentTypesIncludesCustomAgents -count=1 -> PASS; go test ./... -count=1 -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 30 -----------------------------------------------
Issue: `HoistToolResults` was a no-op even though the normalization pipeline documented that assistant-role `tool_result` blocks should be moved to user-role messages before API calls.
Fix: Wired `HoistToolResults` into `NormalizeMessages` and split assistant content arrays so misplaced `tool_result` blocks become proper user messages while non-tool assistant blocks stay assistant-owned.
Verify: go test ./internal/agent -run "TestHoistToolResults|TestNormalizeMessages" -count=1 -> PASS; go test ./... -count=1 -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS
-----------------------------------------------------------

-- Cycle 31 -----------------------------------------------
Issue: After tool-result hoisting, `mergeConsecutiveSameRole` could still drop mixed same-role content such as a plain user message followed by a user-role `tool_result` array because it only knew how to merge text+text or array+array.
Fix: Changed same-role merging to preserve unmergeable mixed content as separate messages, leaving `ensureAlternatingRoles` to insert a filler role instead of losing tool output.
Verify: go test ./internal/agent -run "TestMergeConsecutiveSameRole|TestNormalizeMessages|TestHoistToolResults" -count=1 -> PASS; go test ./... -count=1 -> PASS; go vet ./... -> PASS; go build ./cmd/proxy -> PASS; git diff --check -> PASS
-----------------------------------------------------------

Current Completion Gate:
- go test ./... -count=1 -> PASS
- go vet ./... -> PASS
- go build ./cmd/proxy -> PASS
- npm run lint -> PASS
- npm run build -> PASS
- git diff --check -> PASS
- web/dist asset hashes match internal/server/webdist -> PASS
- Playwright Activity render -> PASS; backend-offline console shows only expected `/api/*` resource 502s

Parked/Environment-Limited Checks:
- go test -race ./internal/server -run TestRunRegressionTeamAPI -count=1 -> BLOCKED in this Windows environment because CGO needs gcc and gcc is not installed.

══ Chronos Run: 독립 분리 마무리 (2026-07-25) ══════════
Engine: 직접 루프
Scope: D:\git\aniclew (문서·README), D:\git\claudecode (정리), Settings 이식
Gate: go build/vet/test + (cd web && npm run build)
Promise: R1~R5 전부 proved
Baseline: web build exit 0 (886 modules), go test 15 pkg ok

── Cycle 1: 구 경로/프로젝트명 참조 정리 ──────────
Issue: 저장소 이동 후 문서가 구 경로 D:\git\claudecode\proxy-go를 가리킴 (7곳/3파일) + MEMORY.md 프로젝트명이 proxy-go
Fix:   handoffs 3개 파일 메타데이터를 새 경로로 갱신 (당시 경로는 역사 표기로 보존),
       MEMORY.md 프로젝트명/목표/날짜 갱신, memory/gotchas.md 제품명 통일
       memory/learned/observations.jsonl은 append-only raw 로그이므로 미변경 (과거 시점 기록)
Verify: grep 재실행 → 잔존 3건 전부 "세션 당시 경로는 ..." 형태의 의도적 역사 표기, Project 필드는 신규 경로 → PASS
────────────────────────────────────────
── Cycle 2: README 초점 조정 + 사실 오류 수정 ──────────
Issue: 대표 문장이 claude-code-router 포지션과 동일. 유일한 차별 자산인 계정/쿼터 스케줄링이
       README에 아예 없음. Stats 수치가 낡음(17,871줄/214 tests/4 packages) + 근거 없는
       "95% technical fidelity" 표기
Fix:   상단을 "Keep coding past the rate limit"으로 재작성(5h/7d 윈도우·무변경 CLI 접속),
       Features 최상단에 Account & Quota Scheduling 섹션 신설(7항목, 실제 구현 기준),
       API 표에 /api/runtime* 4개 추가, Architecture에 Runtime Plane 계층 추가,
       Stats를 실측치로 교체(43,300줄/302 test funcs/18 packages) + fidelity 표기 제거
Verify: 소스 출처 언급 0건, 첫 문단 스케줄링 초점 확인, 스케줄러 섹션·다이어그램 반영 확인 → PASS
Note:  aniclew/README.md에는 원래부터 Claude Code 소스 언급이 0건이었음 —
       해당 조건은 상위 claudecode/README.md 대상이었음 (Cycle 3에서 처리)
────────────────────────────────────────
── Cycle 3: claudecode 저장소 정리 ──────────
Issue: 분리 후 claudecode 쪽이 사라진 proxy-go/를 계속 가리킴 — .gitignore 2줄,
       MEMORY.md 목표/인덱스, memory/gotchas.md의 "중첩 저장소" gotcha(이제 거짓),
       README 47-86의 AniClew 제품 서사 + Quick Start의 `cd proxy-go`
Fix:   .gitignore proxy-go 항목 제거(주석으로 이동 사실 명시), MEMORY.md 목표/인덱스 갱신,
       gotchas.md에 SUPERSEDED 표기 + 신규 항목(aniclew 독립 분리) 추가(CLAUDE.md Superseded 패턴),
       README 제품 서사 섹션을 포인터로 축약 + TOC 갱신 + proxy 실행 경로를 D:\git\aniclew로
Verify: README proxy-go 참조 0건, TOC/헤딩 일치, claudecode git status = .gitignore M만 → PASS
Note:  src/cli/runtime*.ts와 web/은 이 저장소에 남음 — AniClew 서버의 클라이언트 역할.
       두 저장소에 걸친 상태이며 향후 이관 여부는 미결(Cycle 4 이후 판단)
────────────────────────────────────────
── Cycle 4: Settings 이식 Tier 1 (스케줄러 계열) ──────────
Issue: aniclew 백엔드에 runtimeplane/api/runtime*가 있으나 조작 UI가 전무
       (grep runtime|quota|account|scheduler in web/src → 0건). 조작 UI는 claudecode/web에만 존재
Fix:   [기반] lucide-react+clsx+tailwind-merge 설치, vite/tsconfig에 @/ alias,
       index.css에 surface-100..950 / brand-* 스케일을 라이트·다크 양쪽에 정의(308개 클래스 호환),
       lib/utils(cn), lib/constants(MODELS), lib/settings-store(useSyncExternalStore 기반 최소 스토어),
       components/settings/SettingRow(5개 프리미티브 자체 구현), SettingsNav(신규), hooks/useRuntimeStatus(kkh 저작 이식)
       [이식] Scheduler/Accounts/Overview/Providers/Routing 5개 = 2,271줄, sed로 import만 치환
       (Next.js 특유 구문 0건이라 본문 무수정), pages/RuntimeSettings 쉘로 App에 연결
Verify: 1차 빌드에서 타입 갭 6종 노출 → SectionAction 타입, resetSettings(scope), conversations,
        permissions.autoApprove, SettingsSection union 보강 후 재빌드
        web build exit 0 (JS 1,085→1,186kB), go build/vet OK, go test 15 pkg ok / 0 fail → PASS
Note:  conversations는 sessions 계층 미연결 상태로 빈 배열 반환 — 카운터가 틀린 수를 보이지 않도록
       의도적으로 0 처리, 연결 지점은 주석으로 명시
────────────────────────────────────────
── Cycle 5: Settings 이식 Tier 2/3 (9개) ──────────
Issue: Tier 1 외 나머지 섹션 미이식 — aniclew에 harness/data 계열 설정 UI 부재
Fix:   kkh 저작 훅 2개 이식(useHarnessStatus 156, useEvidenceStatus 175),
       Agents/Commands/Loops/Handoffs/Skills/Verification/Memory/Privacy/Advanced 9개 = 2,022줄 이식,
       ConversationSummary를 실제 형태(Conversation: id/title/messages/createdAt/updatedAt/isPinned)로 교정,
       deleteConversation 추가(sessions 계층 미연결이므로 no-op + 사유 주석),
       RuntimeSettings 쉘을 Runtime/Harness/Data 3그룹 15탭으로 확장
Verify: 빌드 오류가 MemorySettings 1개 파일에만 집중 → 타입 교정 후 통과
        web build exit 0 (JS 1,186→1,246kB, CSS 47.6kB) → PASS
────────────────────────────────────────
Parked: AppearanceSettings 이식 — 사유: 제품 결정 — 진행 상태: 의존성 분석 완료.
        ThemeProvider(nirholas 92줄)/lib/types(nirholas 223줄) 의존이며 재작성 자체는 가능하나,
        244줄 중 19곳이 TerminalTheme(tokyo-night/dracula/monokai 등)·TerminalEffects(scanlines/glow)
        설정이고 AniClew에는 대응하는 터미널 UI가 없음. 이식 시 무동작 설정 화면이 됨.
        테마(light/dark)는 이미 ActivityBar 토글로 존재.
Parked: 원격 push (11 unpushed commits) — 사유: 권한 경계 — 진행 상태: 커밋은 로컬 완료,
        외부 공개 행위이므로 사용자 확인 필요.
── Cycle 6: README 포지셔닝 정정 (쿼터 → 로컬 모델) ──────────
Issue: Cycle 2에서 대표 문장을 "Keep coding past the rate limit"으로 잡았으나, 코드 무게중심은
       internal/agent 16,030줄(48%)인 반면 runtimeplane은 1,351줄(4%). 최근 작업을 핵심 자산으로
       착각한 판단 오류 — 4개월 개발 이력(SGLang, 로컬 모델 하드닝, Codex-maxxing)도 로컬 모델 방향
Fix:   대표 문장을 "Run Claude Code on your own models"로 교체 + 로컬 모델 실패 양상(잘못된 tool call
       shape, 중단, 컨텍스트 초과)을 도입부에 명시, Features 최상단에 Local Model Runtime 섹션 신설
       (프로토콜 변환/thinking 파싱/auto-compaction/capacity/failure absorption),
       Account & Quota Scheduling을 Coding Agent 뒤로 이동, Architecture에 Translate 계층 추가,
       Coding Agent·Teams의 중복 항목 3건 제거
       docs/athena/aniclew.md에 정정 섹션 추가 (Phase 2 판정 오류 + 경쟁 지형 재정의)
Verify: 중복 grep 0건, 섹션 순서 Local Model Runtime→Proxy→Coding Agent→Quota 확인 → PASS
────────────────────────────────────────
