# Measurement: failed-edit hint on a local model

> 2026-07-26. Model `gemma4:12b-it-qat` via Ollama.

## What was measured

Whether the token-overlap fallback added to `closestLinesHint` (commit `6bce6f8`)
changes how an agent run behaves when `Edit` cannot find its `old_string`.

Before that commit the hint only fired when the model's first line appeared
verbatim in the file. That is the case where the edit would usually have
succeeded anyway, so the hint tended to be empty exactly when it was needed.

## Method

Two binaries differing only in `internal/agent/fuzzy.go`:

| Condition | Build |
|-----------|-------|
| `hint`    | `6bce6f8` — verbatim containment, then token-overlap fallback |
| `nohint`  | `3d250a1` — verbatim containment only |

The `nohint` binary was built from a `git worktree` at `HEAD~1` so nothing else
could drift between the two.

- Fixture: three Python files, two bugs across two of them, four failing tests.
  The first bug masks the second, so a run has to diagnose twice.
- Prompt: `"test_report.py is failing. Make all four tests pass. Do not modify test_report.py."`
  No file named, no order given.
- Fresh fixture per run; proxy restarted per run; tool budget left at the default.
- 5 runs per condition, alternating between conditions each round so model
  warm-up and machine load do not accumulate on one side.

## Results

| Condition | Tool calls | Mean | Edit failures | Mean |
|-----------|------------|-----:|---------------|-----:|
| `hint`    | 8, 13, 13, 14, 15 | 12.6 | 0, 0, 0, 1, 1 | 0.4 |
| `nohint`  | 7, 17, 17, 19, 22 | 16.4 | 0, 0, 1, 2, 2 | 1.0 |

Both conditions passed all four tests in all five runs (5/5 each).

Hint coverage, counting which code path produced the message:

| Condition | Edit failures | verbatim path | token path | **no hint at all** |
|-----------|--------------:|--------------:|-----------:|-------------------:|
| `hint`    | 2 | 0 | 2 | **0** |
| `nohint`  | 5 | 3 | 0 | **2** |

## Conclusions

**Confirmed — the fallback closes the gap it was meant to close.** In `nohint`,
2 of 5 edit failures returned no hint at all; the model had to retry with no
indication of where the text actually was. In `hint`, every failure carried a
hint, and all of them came from the new token path — cases the verbatim check
does not reach. This is a code-path property, so run-to-run variance does not
affect it.

**Not established — the effect on run length.** Mean tool calls dropped 16.4 to
12.6 and mean edit failures 1.0 to 0.4, both in the expected direction, but the
distributions overlap: the single best run in the whole set (7 calls) was a
`nohint` run. Mann-Whitney U = 5 against a critical value of U ≤ 2 for n = 5, 5
at α = 0.05 two-tailed, so the null is not rejected.

Do not cite the 23% figure as an effect. Separating it from noise needs roughly
15-20 runs per condition.

## Correction to earlier commit messages

`3d250a1` records a before/after of 13 vs 8 tool calls on `qwen3-coder:30b`.
That was a **single run per side**. The parts of it that are structural hold —
Test output went from 21 characters to 2003, and Bash fallbacks went to zero
because the tool started returning usable output — but the call-count delta
carries the same variance shown here and should not be read as a measured
speedup.

## Notes for re-running

- `scratchpad/ab_test.py` in the session scratchpad drove this; it alternates
  conditions, rebuilds the fixture per run, and writes `ab_results.csv`.
- An earlier version of that script counted any hint text as a hit, which made
  `nohint` appear to emit hints. Count `"Closest lines"` (token path) separately
  from `"Similar lines"` (verbatim path) — both builds contain the latter.
- Remove the comparison worktree when finished: `git worktree remove <path>`.
