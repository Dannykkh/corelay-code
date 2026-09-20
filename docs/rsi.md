# RSI: bounded lesson learning and measured adoption

Corelay supports a bounded lesson-learning cycle as well as measured profile
refresh. It reuses the profiler, disposable workspaces, fixed calibration and
holdout cases, immutable profile store, and automatic run selection. Lessons
are code-owned workflow reminders selected from observed failures, not arbitrary
model-written instructions. It does not synthesize source patches or train a model.

## Run

Use the same provider, model, endpoint, variant, and store for both commands:

```sh
corelaycode-profile dry-run --provider ollama --model <model>
corelaycode-profile run --provider ollama --model <model> --confirm
corelaycode-profile improve --provider ollama --model <model> --baseline <profile-id> --confirm
# Generate lessons, execute incumbent and candidate, then evaluate adoption:
corelaycode-profile improve --provider ollama --model <model> --baseline <profile-id> --learn --confirm
```

`run` creates the initial measurement using the existing publication semantics.
`improve` runs one fresh measurement and publishes its recommendations only if
the comparison accepts it. It requires a target-bound provider and the same
filesystem/network isolation as `run`. There is no unbounded background loop.
The normal timeout and per-probe timeout flags remain available. These commands
can call a paid model; installation or server startup does not run them.

`--learn` derives at most one combined candidate from the baseline's calibration
observations. It freezes that candidate before executing two full isolated
plans: a fresh control with incumbent lessons, followed by a candidate with the
proposed lessons. Each attempt starts from a disposable fixture workspace.
Holdout results do not generate or revise the candidate. There is no automatic
retry-until-pass search. If there is no new eligible lesson, no model calls are
made and the command returns a rejected decision. The total timeout covers
both plans; the maximum attempt count is twice the dry-run manifest count.

| Repeated calibration signal (at least two observations) | Candidate reminder |
|---|---|
| Malformed tool calls | Check the live tool name, required arguments and types |
| False completion | Run available verification and inspect the actual result |
| Unsuccessful edit probes | Read before editing and reread after mismatch |
| Unsuccessful plan/context probes | Recheck the latest objective and acceptance criteria |

Transport failures do not generate behavioral lessons. A single failure and
holdout-only failures do not generate candidates. Existing adopted lessons are
retained. Raw trace text, model-written commands, and secrets are never copied
into these instructions. The four reminders do not change tool permissions.

Exit codes: 0 means accepted and published; 3 means evaluated but rejected;
1 means execution, compatibility, isolation, or persistence failed; 2 means
invalid arguments. `--measurement-only` is not allowed with `improve`.

## Adoption contract

Baseline and candidate must match exact target identity, plan/fixture digest,
harness variant, profiler version, scoring policy, and observation shape.
Plain refresh also requires identical lesson provenance. Learning comparisons
permit only lesson policy/digest to differ. Every lesson-evaluation observation
must attest the evaluated policy digest; executors that ignore it are rejected.
The digest binds the actual instruction templates, so old evidence cannot
silently authorize changed template text.
Manual overrides are not eligible. If a selectable profile already exists,
the baseline must be that current profile, not an older one.

The candidate must be verified, newer than the baseline, current, and unexpired.
All existing holdout and safety checks must pass. False completion and safety
failures must be zero. A previously clean attempt cannot become unsuccessful;
per-attempt retries, malformed results, and transport errors cannot regress.
The existing comparison must report a measurable improvement; ties preserve
the baseline. Lower latency alone cannot authorize adoption.

Accepted and rejected candidates are retained under `<store>/rsi-trials/` with
content-free comparison decisions named `<baseline-id>-<candidate-id>.json`.
Only accepted candidates are also saved in the normal profile store. Subsequent
runs that use the exact-target automatic-selection integration can consume the
new recommendations and lesson instructions. Learning also retains the fresh
control profile, and the decision includes `sourceProfileId`, `control`,
`candidateLessons`, and `candidateLessonDigest`. Its comparison baseline is the
fresh control, not the historical source used for proposal generation.
Custom stores are not implicitly selected by the server;
use its configured capability-profile store for runtime adoption.

Decisions are written before publication. A decision marked `accepted` is an
evaluation result, not proof that publication finished: check the normal store
or the command's `published` field. A crash can leave a trial without a decision
or an accepted decision without publication; neither activates that trial.
The prior immutable profile is retained. An explicit rollback command is not
implemented by this change.

`run` and `improve` take an exclusive per-target lock. A killed process leaves
`<store>/.profiling-<target-digest>.lock`; after confirming no matching profiler
is running, the operator can remove that exact file. Locks are never stolen by
elapsed time. Direct library callers of Store.Save must coordinate publication;
the lock protects the profiler CLI/Runner, not arbitrary filesystem writers.

## What remains

Plain refresh derives recommendations from new measurements. Learning executes
different lesson instructions in control/candidate runs, but this sequential,
fixed-suite comparison does not establish statistically reliable live-model
improvement. Random variation and provider drift remain possible. Repeatedly
selecting on the same holdout also risks overfitting. The existing production
failed-trace regression collection and automatic skill-writing hook remain
separate paths; this command learns from profiler evidence only.

The next stages are:

1. Convert sanitized failures into reproducible, isolated fixtures with
   executable acceptance checks; do not replay arbitrary trace commands.
2. Extend the implemented bounded lesson generator to richer policy/skill
   candidates while retaining identity binding and evaluation separation.
3. Add unseen evaluation tasks, randomized/repeated control comparisons, explicit
   token/cost budgets, and production drift monitoring to the bounded A/B path.
4. Version deployment and rollback decisions, then add an opt-in scheduler.

These remaining stages are not marked implemented. General recursive code modification,
automatic skill deployment, and model-weight training remain outside this
first implementation.

## Verification (2026-09-20)

Local deterministic tests cover accepted publication, rejected trial retention,
selection after reopening the store, next-run recommendation binding, ties,
false completion, holdout/safety failures, per-attempt regression, invalid time,
changed plan, stale baseline, cancellation, and shared target locking.
CLI tests check required flags and rejection of incompatible options.
The capabilityprofile, profiler CLI, and server package suites passed; build,
vet for the changed packages, and diff whitespace checks passed.
Live model improvement and paid-provider evaluation were NOT RUN. Fixtures
verify the adoption mechanism, not real-world learning effectiveness.

The lesson extension also tests calibration-only proposal generation, frozen
control/candidate execution, ignored-injection rejection, template/evidence
tampering, accepted/rejected persistence, and target-scoped next-run selection.
A deterministic provider runs through the production Agent Kernel: without the
lesson it answers without reading; with the lesson it dispatches Read and
consumes its result. A separate production run receives lessons through
AutomaticSelection, including normal tool-category routing. This establishes
actual injection and execution, not the intelligence of a live model.

Full Go suite passed after lesson injection (agent 158.019s, server 27.343s).
Subsequent executor-input and cancellation guards were checked again with the
complete capabilityprofile/profiler package suites, build, and vet.
