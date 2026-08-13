# Reference Capability Absorption v2

## Decision

Continue capability absorption through the existing Agent Kernel. The next
wave starts with reproducible comparison evidence, then adds tool-call and
repository-context capabilities only when the comparison contract can measure
their contribution.

The selected order is:

1. content-free harness ablation comparison;
2. deterministic Continue codeblock and goose tokenized recovery;
3. optional interpreter Tool Shim, only after deterministic formats are measured;
4. repository map context;
5. LSP diagnostics and navigation;
6. container-backed sandboxing.

## Why comparison comes first

`CapabilityProfiler` already owns isolated repeated probes, holdouts, safety
checks, immutable profiles, and target binding. Creating a second benchmark
runner would duplicate those guarantees. Instead, a `HarnessVariant` becomes
part of the immutable `ProbePlan` and every `ProbeExecution`.

The first two variants are:

- `minimal`: the same Agent Kernel and safety pipeline with adaptive response
  recovery, two-stage routing, fuzzy edit fallback, and PlanAnchor disabled;
- `corelay`: the current capability-adaptive policy.

Both variants retain approval, sandboxing, target binding, durable terminal
semantics, and the same deterministic fixtures. This makes the comparison an
ablation of assistance, not an unsafe raw execution mode.

## Comparison contract

Two immutable profiles are comparable only when they have:

- the same exact target digest;
- the same plan version;
- different declared variants;
- the same case, stage, category, attempt, and safety shape.

The report contains profile IDs, target, plan, and fixture digests, bounded aggregate and
per-category deltas, and a deterministic verdict. It never contains prompts,
responses, endpoints, credentials, trace bodies, or artifact contents.

An explicit measurement-only CLI flag may treat successful immutable
publication as process success even when a profile is quarantined. It does not
alter verification, quarantine, or automatic selection.

Any safety regression makes the candidate verdict `unsafe-regression` even if
other metrics improve. Incompatible profiles fail closed.

## Compatibility

Profiles written before variants existed remain valid. An omitted provenance
variant on the original `corelay-capability-probes-v1` plan is interpreted as
legacy `corelay`; new profiles always persist an explicit variant.

## Deterministic format absorption

The existing ToolRecover cascade already handled provider-native calls, legacy
XML, Hermes, Liquid, fenced JSON, bare JSON, and bounded suffix repair. Adding
another XML path would duplicate behavior. The observed reference gap was
instead two bounded, directly decodable formats:

- Continue's final `tool` codeblock with `TOOL_NAME`, `BEGIN_ARG`, and
  `END_ARG` fields;
- goose Tool Shim's tokenized section and call markers before its optional
  interpreter fallback.

Both parsers are pure stages in the existing cascade. They require complete
final envelopes, enforce the existing input/call/depth/argument budgets, and
revalidate exact tool names and canonical JSON objects against the live tool
catalog. No parser directly executes a tool. Conflicting interpretations still
fail closed.

The formats are now fixed profiler categories shared by both ablation variants.
This measures whether a target emits them reliably before any interpreter model
is considered. The optional model-backed Tool Shim remains deferred because it
would add a second provider call, latency, and a new trust boundary.

Clean-room behavior references were observed at pinned revisions:

- Continue tool codeblock:
  `continuedev/continue@5522c6f44ca0ac3528b37244818fbfa39b5af470`;
- goose direct Tool Shim parser:
  `aaif-goose/goose@849b6f2ae84c2f8c0a8d90df3b29fafb1728d759`.

## Bounded repository map

Continue exposes an on-demand repository map that gives the model a compact
view of paths and signatures before it opens individual files. Corelay Code
absorbs that capability as the read-only `RepoMap` tool in the existing tool
catalog rather than as a second context provider or retrieval loop.

The local contract is intentionally narrower than the reference:

- only recognized source-file paths are listed;
- optional declarations contain names and type shapes, never bodies or values;
- hidden, generated, dependency, recovery, and runtime-state trees are skipped;
- symbolic links are not followed;
- paths remain inside the canonical workspace;
- inputs, candidates, files, symbols, signatures, and output bytes are bounded;
- cancellation and malformed or escaping paths fail closed;
- every call still crosses the common catalog, permission, routing, and
  dispatch pipeline.

The fixed profiler plan includes a repository-map calibration case. It writes
a declaration marker into an isolated source fixture, requires one real
`RepoMap` execution, and accepts the observation only when the final answer
contains that marker. Both harness variants use the same fixture and tool.

Clean-room behavior references were observed at pinned Continue revision
`continuedev/continue@5522c6f44ca0ac3528b37244818fbfa39b5af470`:

- `core/util/generateRepoMap.ts` for the bounded path/signature concept;
- `core/tools/definitions/viewRepoMap.ts` and
  `core/tools/implementations/viewRepoMap.ts` for the on-demand tool boundary;
- `core/context/providers/RepoMapContextProvider.ts` for the optional context
  surface that Corelay Code deliberately does not duplicate.

## Verification

- plan digests differ by variant while fixture shapes remain identical;
- fixture digests remain equal across variants and appear in comparisons;
- tool-codeblock and tokenized calls parse only as complete final envelopes;
- repository maps remain workspace-scoped, deterministic, content-free, and
  byte bounded;
- the profiler observes one real repository-map execution and declaration
  marker recovery;
- catalog, schema, ambiguity, and parser bounds still fail closed;
- executor receives the immutable variant and builds the expected harness;
- comparison rejects target, plan, shape, and same-variant mismatches;
- comparison detects safety regression before declaring improvement;
- CLI `dry-run`, `run`, and `compare` remain content-free and fail closed;
- existing profile, Agent Kernel, and repository tests remain green.
