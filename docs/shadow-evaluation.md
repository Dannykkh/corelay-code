# Shadow judgment recording and offline evaluation

Corelay can record harness-injected shadow judgments and compare recorded answers
with reviewed labels. The explicit `shadow-judge` command can call a model for
offline comparison; the default agent path still has no shadow judge and no
policy is published.

## Recording in the existing trace

`RunOptions.ShadowJudgment` remains the trusted injection seam. The server's
`compositeRunRecorder` forwards its records to `observabilityRunRecorder` before
terminal finalization. Each accepted record becomes an `agent.shadow_judgment`
span; `data.record` contains JSON with enums, digests, evidence IDs, and counters.
The containing trace supplies the final run status and, when available, receipt
verification and terminal state. Its ID may differ from the kernel run ID;
`metadata.shadowKernelRunId` explicitly connects them.

The recorder rejects malformed records, different kernel run IDs in one recorder,
and late records. It deduplicates decision sequences and stores at most three.
No evidence excerpts, model error strings, or raw commands enter these spans.

The observation records subsequent tool rounds (`laterRounds`), rounds containing
an error (`laterErrorRounds`), and rounds where the same failed tool/result digest
recurs (`laterRepeatedErrorRounds`). These are observations, not proof that an
alternative action would have worked. `laterTruncatedRounds` exposes rounds with
more than sixteen tool results; error counters can undercount those rounds.
Only later dispatched tool rounds are counted, not text-only turns or other runs.

Run records use the existing daily JSONL and retention path. `RecordRunChecked`
reports write/sync/close failure; the compatible `RecordRun` API keeps the in-memory
diagnostic marked `metadata.persistence=failed`. This failure does not change the
agent terminal outcome. A failed write is not durable evaluation evidence.

## Run the offline comparison

From the product repository:

```powershell
go run ./cmd/corelaycode-profile shadow-eval --corpus cmd/corelaycode-profile/testdata/shadow/corpus.json --responses cmd/corelaycode-profile/testdata/shadow/responses.json --split development
```

The bundled six cases and their golden responses are synthetic protocol fixtures.
The responses were constructed from fixture labels; matching them demonstrates
plumbing only, not independent accuracy or a model improvement. They include
repeated failures, normal progress, and uncertainty in distinct task groups.
`selection` and `evaluation` are also available splits.

This command calls no provider, loads no model credentials, and writes no profile.
Without `--responses`, it reports request digests and missing answers (exit 3).
The explicit model adapter sends only the frozen evidence state derived from
`agent.BuildShadowEvaluationRequest`, never labels or subsequent outcomes.

Exit codes: 0 means all selected answers are validly evaluated, not that they are
correct or a policy is accepted; 3 means missing or unusable answers; 2 means input
or split validation failed; 1 means cancellation or output failure. `go run` wraps
a nonzero program exit in its own exit status; the built executable returns the
codes directly.

## Corpus and response contracts

The [example corpus](../cmd/corelaycode-profile/testdata/shadow/corpus.json) is the
schema example. Files are bounded to 2 MiB and 1,000 cases/responses; unknown fields
and duplicate JSON keys are rejected.

| Field | Meaning |
|---|---|
| `schemaVersion` | Currently 1 |
| `cases[].id` | Unique bounded case identifier |
| `group` | Repository/task/failure family grouping assigned by the curator |
| `split` | `development`, `selection`, or `evaluation` |
| `origin` | `synthetic` or `reviewed`; reviewed cases require `traceId` |
| `traceId`, `point.runId`, `point.sequence` | Trace and kernel decision references |
| `point` | Frozen identities, session revision, trigger state, preceding/current round references |
| `evidence` | Explicitly curated objective, acceptance criterion and excerpts keyed by evidence ID |
| `labels` | Independent `yes`/`no`/`unknown` labels for the three questions |
| `expectedAdvisory` | Curator's expected bounded action recommendation |
| `outcome.task` | `passed`, `failed`, or `unknown` from acceptance evidence |
| `outcome.intervention` | `none`, `direction`, `requirement`, `authority`, or `unknown` |
| `outcome.laterRepeatedErrorRounds` | Optional observed count; omit when not known |

User intervention is manually categorized; a message or a thumbs-up is not
automatically interpreted as a direction correction or a correct policy label.
Likewise, a successful run without verification is not automatically a passed
task. Outcome labels remain reviewer assertions, not cryptographically attested
ground truth; this command does not independently verify the source trace.

Task groups, kernel runs, identical evidence, and identical requests cannot cross
splits. This catches structural leakage, not semantic paraphrases or an incorrectly
assigned group. The curator must keep related tasks in the same split and reserve
fresh evaluation tasks when revising questions after seeing their results.

The [response file](../cmd/corelaycode-profile/testdata/shadow/responses.json) maps
each `caseId` to the exact `requestDigest` and a typed `response`. The digest binds
the current question contract, identities, and evidence, excluding labels and
outcomes. Responses from changed inputs are unusable. Missing, malformed, or
unbound answers receive no match credit, even when the expected action is abstain.
Missing provider usage is unknown, not zero; no cost-effectiveness claim is made.

## Run an explicit model comparison

`shadow-judge` sends only the frozen objective, acceptance criterion, preceding and
current tool metadata, and curated evidence excerpts. It never sends labels,
expected advisories, observed outcomes, or the full trace. Run it only with a
corpus whose excerpts are approved for the selected provider. The default remains
off; this command does not attach a judge to normal agent runs.

```powershell
go run ./cmd/corelaycode-profile shadow-judge --corpus cmd/corelaycode-profile/testdata/shadow/corpus.json --split development --provider ollama --model qwen3:0.6b --out responses.json
go run ./cmd/corelaycode-profile shadow-eval --corpus cmd/corelaycode-profile/testdata/shadow/corpus.json --split development --responses responses.json
```

Ollama must be running locally. `--endpoint` accepts only a loopback HTTP
`/api/chat` URL. The command requests structured JSON, disables thinking, and
limits context and generated tokens. A Jev call instead uses `--provider jev
--model jev-latest` and `TYPESAFE_API_KEY`; the endpoint is fixed to TypeSafe's
HTTPS API. The two adapters use the same evidence and three semantic questions.
Jev returns probabilities from three Nouls in one call; values at or above 0.8
become `yes`, at or below 0.2 become `no`, and middle values become `unknown`.
These thresholds are provisional and must be selected on reviewed data. Jev
does not supply source citations, so its `evidenceIds` identify the excerpts
provided to the call, not model-selected proof.
The response artifact preserves Jev's three original probabilities and the
resolved model version alongside its thresholded answers. The evaluator rejects
out-of-range or label-inconsistent probabilities. Older Jev artifacts without
probabilities remain readable, but cannot support threshold calibration.

The new response file is created with mode `0600` where supported, cannot
overwrite an existing file, and
contains only contract-valid answers or `null` for malformed output. Its optional
`usage` stores provider-reported token counts and locally measured end-to-end
latency. The offline report aggregates calls, tokens, p50/p95 latency, question
matches, advisory matches, and abstentions. `costStatus=unknown-no-price` means
no monetary price was measured. Local Ollama tokens and wall time do not measure
electricity or shared GPU contention. A failed or timed-out live run returns
nonzero without saving a partial response file.

## What the report measures

The baseline recommends `continue`. A declared fixed rule recommends
`gather_evidence` on two consecutive failed rounds or a guard denial, otherwise
`continue`. This is an offline reference recommendation, not a simulation of the
entire existing agent. Judge responses use the same enum/evidence validation and
advisory mapping as the online observer.

The report includes case counts, synthetic counts, missing/invalid/evaluated
answers, valid abstentions, advisory matches for each reference, and per-question
confusion counts (`label->answer`). Full-case counts remain the denominator;
coverage and accuracy among evaluated cases must both be considered. Observed
outcomes are shown beside recommendations, not attributed causally to them.
The report contains no source evidence text and does not deploy a winner.

Jev's live transport was exercised on the bundled synthetic development cases;
its responses are not evidence of real-world accuracy. Next: collect actual
reviewed cases and run the same approved corpus across the candidate models.
Only then decide whether model judgments are worth
testing as interventions. See the [implementation plan](plan/self-questioning-rsi/plan.md).
