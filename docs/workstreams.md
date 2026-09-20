# Workstreams, Plans, and Resume

Workstream data belongs to one workspace under `.corelay/workstreams`. A Plan is a revisioned child of one Workstream. Chat sessions keep an exact `workstreamId`, `planId`, `planRevision`, and `stageId` reference so loading a session cannot silently switch it to another Plan revision.

## Plan API

- `GET /api/workstreams/{id}/plans` lists Plans for the selected workspace.
- `GET /api/workstreams/{id}/plans/{planId}` reads one Plan.
- `POST /api/workstreams/{id}/plans` creates a Plan from a validated TeamPlan definition.
- `PUT /api/workstreams/{id}/plans/{planId}` updates a draft or approved definition. Send `expectedRevision`, `expectedStateRevision`, and the full `definition`. The definition revision advances and any previous approval is revoked. A definition cannot be changed after an execution attempt begins.
- `POST /api/workstreams/{id}/plans/{planId}/approve` approves only the exact current definition/state revisions. Send `expectedRevision` and `expectedStateRevision` from the latest Plan response.

Plan-bound execution requires a current approval and an eligible sequential stage. Agent sessions bind the exact definition revision. `/api/team` and `/api/chronos` take `workstreamId`, `planId`, `planRevision`, and `stageId`; they use the stored stage tasks and stored verification command rather than caller-supplied overrides. A stage becomes completed only after a server-saved receipt is bound to that run and stage and its required verification and acceptance evidence pass. Model prose is not completion evidence. KAIROS Plan tasks are read-only observers and never advance Plan stages.

## Interrupted stage recovery

A stage left `running` after a process interruption remains incomplete. Same-process runs register their exact Workstream/Plan/run binding, and recovery requests are rejected while that run remains active. After restart, call:

```http
POST /api/workstreams/{id}/plans/{planId}/stages/{stageId}/reconcile
Content-Type: application/json
```

```json
{
  "workDir": "C:/path/to/project",
  "expectedRevision": 1,
  "expectedStateRevision": 3,
  "runId": "run_...",
  "manualAcknowledged": true
}
```

Before sending this request, confirm the old process has stopped and inspect any files it changed. Reconciliation closes the exact running attempt as `failed` with verification `not-run`, advances `stateRevision`, and stores no acceptance evidence. It does not change the Plan definition revision or approval. The failed stage may be retried after the current state is reloaded.

## Decisions and handoff

Workstream decisions are stored in `decisions`. Update them through `PATCH /api/workstreams/{id}` with a `decisions` array and `hasDecisions: true`; the flag permits intentionally clearing the list with `decisions: []`.

`POST /api/workstreams/{id}/handoff` accepts optional `planId` to select the exact Plan associated with the current session. If omitted, the handoff selects the most actionable Plan in this order: executing, failed, approved, draft, then completed; ties use most-recently-updated first. The generated Markdown includes Workstream goals and decisions, Plan definition/state revisions and approval, current and unfinished stages/tasks, latest attempt and acceptance evidence, verification, and explicit recovery instructions for a still-running stage. Other Plans are listed by revision and status.

The Agent, Team, Chronos, and KAIROS Plan-bound prompts use a 2,000-byte Workstream context that puts exact Workstream/Plan/revision/stage references and current recovery status before descriptive text. A running attempt is never rendered as complete. Loading a durable Chat session restores its persisted Plan binding; stale revisions are rejected and must be explicitly rebound in the UI.
