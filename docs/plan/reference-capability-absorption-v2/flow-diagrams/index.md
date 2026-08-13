# Reference Capability Absorption v2 Flows

| Diagram | Purpose |
|---|---|
| [ablation-comparison.mmd](ablation-comparison.mmd) | Same-target, same-fixture harness ablation and fail-closed comparison |
| [tool-recovery-cascade.mmd](tool-recovery-cascade.mmd) | Native-first deterministic parsing, catalog validation, and correction boundary |
| [repository-map-tool.mmd](repository-map-tool.mmd) | Workspace-scoped, declaration-only repository context through the common tool pipeline |

The flow reuses `CapabilityProfiler`, `AgentKernel`, and the common tool safety
pipeline. It does not introduce a second provider, parser, dispatcher, or
completion loop.
