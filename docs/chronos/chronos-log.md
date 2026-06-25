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
