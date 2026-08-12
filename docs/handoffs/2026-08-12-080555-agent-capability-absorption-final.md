# Handoff: Agent capability absorption final

## Session Metadata

- Created: 2026-08-12 08:05:55 +09:00
- Project: D:\git\aniclew
- Branch: main
- Base commit: 5b1cf79
- Supersedes operationally: `2026-08-12-064456-agent-capability-absorption-single-kernel.md` (preserved as history)

## Current State Summary

AniClew now records 43 of 45 target capabilities as Verified. Every production ingress uses `RunLoopWithOptions`; Chronos and SubAgent contribute phase-only `RunMode` controllers, while Team, Bridge, and ACP consume the same typed terminal contract. HTTP and ACP durable sessions also share a synchronous write-ahead execution journal: authorized calls persist a content-free marker before the start event and executor, successful terminals atomically commit transcript and terminal while clearing the exact marker, and failure or hard exit blocks fresh resume until explicit reconciliation. CAP-029 and CAP-045 remain Connected for evidence limits rather than missing runtime composition.

## Work Completed

- [x] Converged HTTP, profiler, Chronos, SubAgent, Team, Bridge, and ACP on one agent kernel and one idempotent terminal finalizer.
- [x] Closed advertised provider-wire conformance and repetition audit propagation through terminal, summary, and receipt evidence.
- [x] Connected SessionStore CRUD/CAS and fork lineage across HTTP, Web, and stable ACP surfaces without adding another persistence layer.
- [x] Added HTTP and ACP pre-execution journals with exact marker CAS, schedule-independent multi-tool aggregation, atomic successful-terminal clear, and runtime quarantine on persistence ambiguity.
- [x] Proved fresh-process hard-exit recovery: load/resume blocks before provider execution, explicit reconciliation enables continuation, and the prior side effect is not replayed.
- [x] Added independent semantic review for all 45 capabilities and retained the prior-access limitation for CAP-045.
- [x] Configured Linux, macOS, and Windows CI jobs for full Go and repeated platform sandbox/process contracts.

### Files Modified

| File or area | Changes |
|---|---|
| `internal/agent/run_options.go`, `tool_dispatch.go`, `sessions.go` | synchronous journal seam; first marker, aggregate update, exact successful commit |
| `internal/server/agent_session.go`, `server.go` | HTTP durable-run journal and terminal composition |
| `internal/acpbridge/backend.go` | ACP prompt journal, aggregate enrichment, exact atomic commit, runtime quarantine |
| `internal/protocol`, `cmd/aniclew-acp`, adapter tests | advertised provider and ACP surface conformance |
| `.github/workflows/ci.yml` | Ubuntu, macOS, and Windows test/vet matrix plus repeated platform contracts |
| `docs/plan/agent-capability-absorption` | 43 Verified / 2 Connected matrix, ledger, semantic review, and synchronized flows |
| `MEMORY.md`, `memory/architecture.md` | durable write-ahead architecture decision index and summary |

### Decisions Made

| Decision | Rationale |
|---|---|
| Keep one `RunLoopWithOptions` kernel | Context, catalog, safety, evidence, hooks, and terminal meaning must not drift by ingress. |
| Journal after authorization, identity, and proof binding but before event or execution | A proposed call must not create an interruption marker, while an executed side effect must never precede durable replay protection. |
| Aggregate every same-run tool start with exact marker and revision CAS | A first-marker-only scheme quarantines the run but loses multi-tool reconciliation identity. |
| Clear an interruption only in the successful transcript-and-terminal CAS | Cancel, failure, persistence ambiguity, and process death must preserve reconciliation state. |
| Keep CAP-029 Connected | Three-OS CI is configured, but a repository-verifiable completed hosted run is still absent. |
| Keep CAP-045 Connected | Independent semantic review exists, but documented prior access and an unavailable historical comparison prevent legal clean-room certification. |

## Verification Evidence

- Agent journal, aggregate, and atomic CAS focused tests passed repeatedly; full `internal/agent`, vet, and proxy build passed.
- Server journal, graceful enrichment, resume/no-replay, and actual child-process hard-exit tests passed repeatedly; full `internal/server` and vet passed.
- `go test ./internal/acpbridge -run '^TestACPExecutionJournal' -count=10`, full ACP bridge tests, command conformance, and ACP vet passed.
- Independent semantic review reports 43 pass, 2 pass-with-limitations, and 0 fail after resolving the CAP-033/CAP-038 hard-crash findings.
- `go test ./internal/provenance -count=10` passed with 45 aligned records, 43 Verified, and 2 Connected.
- Mermaid structural validation passed for four linked diagrams: 20-node kernel, 23-node tool safety, 8-participant durable session, and 19-node profiler.
- Final handoff/MEMORY size checks and scoped whitespace/diff checks passed.
- The shared-tree global Go gate remains root-owned so concurrent workers do not duplicate it.

## Pending Work

### Immediate Next Steps

1. Capture successful hosted Ubuntu, macOS, and Windows workflow runs, then reassess CAP-029.
2. Treat CAP-045 as an explicit legal-evidence limitation; do not promote it from repository functional tests alone.
3. Run the single root-owned global test, vet, build, Web, provenance, and diff gate after all shared-tree edits settle.

### Blockers/Open Questions

- [ ] CAP-029 lacks completed external three-OS CI run evidence.
- [ ] CAP-045 cannot claim legal clean-room independence after disclosed prior access without evidence outside this repository.

## Context for Resuming

### Important Context

- Final capability count is 43 Verified and 2 Connected; only CAP-029 and CAP-045 remain Connected.
- `SessionStore` remains the only durable session layer. HTTP and ACP call the same `MarkInterrupted`, `UpdateInterruptedRun`, and `CommitInterruptedRun` APIs.
- The journal stores only tool-call ID, tool name, input digest, and run ID. It excludes raw input, credentials, and storage paths.
- The first marker quarantines the run. Each later same-run tool updates a schedule-independent `multiple_tools` aggregate under exact CAS.
- Stable ACP intentionally omits a fork method because the protocol does not define one; resumed fork children still preserve lineage and isolation.

### Potential Gotchas

- Moving the journal after `tool_execution_start` or the executor reopens the hard-crash replay window.
- Treating same-run callbacks as idempotent without rereading the exact persisted marker can adopt stale state.
- A successful event is insufficient to clear reconciliation; only the exact atomic terminal commit may clear the marker.
- Guarded ACP outcomes with a non-committing durable policy must fail closed instead of reporting success.
- Matrix, ledger, and semantic-review status counts must move together or provenance tests will reject the change.

## Memory Distillation

- New gotcha/learned observation delta for this handoff: 0.
- Existing raw observation files were not read or rewritten.
