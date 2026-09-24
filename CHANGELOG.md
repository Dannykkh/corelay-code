# Changelog

All notable changes to Corelay Code are documented here.

## [2.0.1] - 2026-09-24

### Bug Fixes

- Resolve macOS workspace paths to their stored filesystem casing before comparing paths and loading ancestor instructions.
- Keep polling for a matching durable receipt when the session read briefly fails during a server restart.
- Make cross-platform integration tests use an explicit execution policy and filesystem case detection; use an HTTP fixture for browser stream checks and disable Git fixture auto-maintenance on hosted runners.

## [2.0.0] - 2026-09-24

### Breaking Changes

- The Go module path changed from `github.com/aniclew/aniclew` to `github.com/Dannykkh/corelay-code`. Update module imports and build references.
- The primary executable changed from `aniclew` to `corelaycode`. Update launch scripts and commands.

### Release Highlights

- A shared native agent kernel now serves CLI, TUI, Web, HTTP, ACP, and team workflows.
- Durable request receipts prevent duplicate model/tool dispatch for an identical committed turn; CLI/TUI recover a dropped SSE response from the saved result.
- Bounded lesson learning and opt-in shadow self-questioning can be evaluated without activating model advice in normal runs.
- CI tests Linux, macOS, and Windows; tagged releases build native binaries and publish a container image.

### Features

- **agent**: replay durable turns and recover dropped CLI streams ([57de150](https://github.com/Dannykkh/corelay-code/commit/57de150))
- **agent**: add shadow self-questioning evaluation ([76e13a0](https://github.com/Dannykkh/corelay-code/commit/76e13a0))
- harden agent execution and add bounded lesson learning ([d0f8820](https://github.com/Dannykkh/corelay-code/commit/d0f8820))
- absorb measurable reference capabilities ([82d033b](https://github.com/Dannykkh/corelay-code/commit/82d033b))
- complete agent kernel and rename to Corelay Code ([2d545e8](https://github.com/Dannykkh/corelay-code/commit/2d545e8))
- **launch**: set CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST in the launch hint ([692b3b9](https://github.com/Dannykkh/corelay-code/commit/692b3b9))
- **web**: port the runtime and harness settings surface ([db09a00](https://github.com/Dannykkh/corelay-code/commit/db09a00))
- **server**: wire runtime plane routing, evidence gate, and stream telemetry ([80743de](https://github.com/Dannykkh/corelay-code/commit/80743de))
- **runtimeplane**: add quota-aware account scheduler package ([3ba697d](https://github.com/Dannykkh/corelay-code/commit/3ba697d))
- **agent**: load custom agent types ([67079c9](https://github.com/Dannykkh/corelay-code/commit/67079c9))
- **web**: expose team orchestration controls ([016eb19](https://github.com/Dannykkh/corelay-code/commit/016eb19))
- **observability**: replay team regressions from receipts ([5255096](https://github.com/Dannykkh/corelay-code/commit/5255096))
- **team**: add TeamPlan worker harness ([ed322e1](https://github.com/Dannykkh/corelay-code/commit/ed322e1))
- **agent**: complete web freshness extraction ([be4528b](https://github.com/Dannykkh/corelay-code/commit/be4528b))
- **agent**: add multi-provider web search ranking ([d32a769](https://github.com/Dannykkh/corelay-code/commit/d32a769))
- **agent**: improve iframe web fetch extraction ([d523b63](https://github.com/Dannykkh/corelay-code/commit/d523b63))
- **agent**: add provider-backed web research ([2daa9a9](https://github.com/Dannykkh/corelay-code/commit/2daa9a9))
- **web**: surface run regression controls ([d64d49e](https://github.com/Dannykkh/corelay-code/commit/d64d49e))
- **observability**: replay chronos regressions ([137de32](https://github.com/Dannykkh/corelay-code/commit/137de32))
- **observability**: generate regressions from failed runs ([ef69c2e](https://github.com/Dannykkh/corelay-code/commit/ef69c2e))
- **observability**: trace agentic runs ([33f6725](https://github.com/Dannykkh/corelay-code/commit/33f6725))
- **workstream**: attach kairos tasks ([2687b3c](https://github.com/Dannykkh/corelay-code/commit/2687b3c))
- **workstream**: attach chronos runs ([4539997](https://github.com/Dannykkh/corelay-code/commit/4539997))
- **web**: add workstream controls to chat ([7d57bd9](https://github.com/Dannykkh/corelay-code/commit/7d57bd9))
- **workstream**: add durable workstream runtime ([5d75acc](https://github.com/Dannykkh/corelay-code/commit/5d75acc))
- **agent**: add loop registry and completion receipts ([3a13dc0](https://github.com/Dannykkh/corelay-code/commit/3a13dc0))
- **agent**: recover tool calls a model leaked as plain text ([7dda808](https://github.com/Dannykkh/corelay-code/commit/7dda808))
- **agent**: document /plan and /undo in /help ([fb2317b](https://github.com/Dannykkh/corelay-code/commit/fb2317b))
- **agent**: completion summary after edit sessions ([41bad93](https://github.com/Dannykkh/corelay-code/commit/41bad93))
- **agent**: checkpoint + /undo to revert the agent's edits ([1da0612](https://github.com/Dannykkh/corelay-code/commit/1da0612))
- **agent**: @file mentions — pull referenced files into context ([a713798](https://github.com/Dannykkh/corelay-code/commit/a713798))
- **web**: plan-approval UI — "Approve & Run" after a /plan ([83cae9b](https://github.com/Dannykkh/corelay-code/commit/83cae9b))
- **agent**: plan mode — /plan explores read-only and proposes a plan ([99586bd](https://github.com/Dannykkh/corelay-code/commit/99586bd))
- **web,cli**: render edit diffs in the dashboard and terminal client ([430bbb1](https://github.com/Dannykkh/corelay-code/commit/430bbb1))
- **agent**: show a diff for each edit (Claude-Code-style preview) ([1c496b7](https://github.com/Dannykkh/corelay-code/commit/1c496b7))
- **agent**: auto-verify after edits (edit -> test -> fix loop) ([4281d78](https://github.com/Dannykkh/corelay-code/commit/4281d78))
- **agent**: announce agent-managed files on first creation ([cdae9a0](https://github.com/Dannykkh/corelay-code/commit/cdae9a0))
- **agent**: weight read-only exploration by content vs navigation ([b54ed41](https://github.com/Dannykkh/corelay-code/commit/b54ed41))
- **agent**: per-model tuning profiles for local models ([30156e3](https://github.com/Dannykkh/corelay-code/commit/30156e3))
- **agent**: warn on suspected context-window exhaustion ([8fe01bf](https://github.com/Dannykkh/corelay-code/commit/8fe01bf))
- **agent**: warn when an Ollama model lacks tool-calling capability ([1e3eab3](https://github.com/Dannykkh/corelay-code/commit/1e3eab3))
- **agent**: make local-model agent loop reliable for tool use ([0bbec98](https://github.com/Dannykkh/corelay-code/commit/0bbec98))
- port Fable 5 lessons — memory trust boundary, auto-skill creation, breadth routing ([7050012](https://github.com/Dannykkh/corelay-code/commit/7050012))
- **models**: detect installed Ollama models instead of hardcoding ([fc15759](https://github.com/Dannykkh/corelay-code/commit/fc15759))
- **ux**: Stop button + Esc to interrupt a slow generation (Claude Code parity) ([152ceda](https://github.com/Dannykkh/corelay-code/commit/152ceda))
- **ux**: live heartbeat (elapsed + output size) so slow local models feel alive ([77f236f](https://github.com/Dannykkh/corelay-code/commit/77f236f))
- **models**: bump to Claude Opus 4.8 and GPT-5.5 (newest as of 2026-05-28) ([79c44af](https://github.com/Dannykkh/corelay-code/commit/79c44af))
- **models**: refresh Anthropic list (add Opus 4.7) + Quick Start labels ([5b6342d](https://github.com/Dannykkh/corelay-code/commit/5b6342d))
- **tray**: custom AniClew tray icon (embedded, zero deps) ([742a1fe](https://github.com/Dannykkh/corelay-code/commit/742a1fe))
- **tray**: pure-syscall Windows system tray icon (zero new deps) ([e95d5ea](https://github.com/Dannykkh/corelay-code/commit/e95d5ea))
- **cli**: add built-in `aniclew chat` terminal client for air-gapped use ([a32d8f3](https://github.com/Dannykkh/corelay-code/commit/a32d8f3))
- **translate**: optional tool budget to prune oversized tool lists for weak models ([0075a37](https://github.com/Dannykkh/corelay-code/commit/0075a37))
- **agent**: add air-gap mode to block internet-egress tools ([9589cb5](https://github.com/Dannykkh/corelay-code/commit/9589cb5))
- add reflection guard to the agent loop ([36ba83d](https://github.com/Dannykkh/corelay-code/commit/36ba83d))
- add fuzzy (whitespace-insensitive) matching to the Edit tool ([8190ac8](https://github.com/Dannykkh/corelay-code/commit/8190ac8))
- add SGLang provider and lint gate for edit tools ([c1db905](https://github.com/Dannykkh/corelay-code/commit/c1db905))
- **memory**: long-term memory with extract + autoDream ([fe1abd7](https://github.com/Dannykkh/corelay-code/commit/fe1abd7))
- distribution — install script, Docker, CI, README ([fa26902](https://github.com/Dannykkh/corelay-code/commit/fa26902))

### Bug Fixes

- **profile**: retain Jev probabilities and synthetic evaluation evidence ([9173abd](https://github.com/Dannykkh/corelay-code/commit/9173abd))
- close cross-platform CI races ([0dbd03a](https://github.com/Dannykkh/corelay-code/commit/0dbd03a))
- **edit**: make the failed-edit hint work when the quote was wrong ([6bce6f8](https://github.com/Dannykkh/corelay-code/commit/6bce6f8))
- **agent**: probe the interpreter instead of trusting LookPath ([3d250a1](https://github.com/Dannykkh/corelay-code/commit/3d250a1))
- **agent**: stop the Test tool from reporting a no-op as success ([47c2f4b](https://github.com/Dannykkh/corelay-code/commit/47c2f4b))
- **rag**: strip backticks when extracting retrieval keywords ([383d444](https://github.com/Dannykkh/corelay-code/commit/383d444))
- **agent**: preserve mixed-role tool results ([10ad4ab](https://github.com/Dannykkh/corelay-code/commit/10ad4ab))
- **agent**: hoist misplaced tool results ([b9e0f8b](https://github.com/Dannykkh/corelay-code/commit/b9e0f8b))
- **observability**: preserve team trace workdir ([2de422d](https://github.com/Dannykkh/corelay-code/commit/2de422d))
- **web-search**: stabilize fused provider ordering ([84c5d62](https://github.com/Dannykkh/corelay-code/commit/84c5d62))
- **agent**: keep web defaults provider independent ([a3ae313](https://github.com/Dannykkh/corelay-code/commit/a3ae313))
- **hooks**: prefer git bash on windows ([03293a3](https://github.com/Dannykkh/corelay-code/commit/03293a3))
- **agent**: hard-enforce read-only in plan mode ([5651d9d](https://github.com/Dannykkh/corelay-code/commit/5651d9d))
- **agent**: keep read-only answers in the requested language ([29b3bc3](https://github.com/Dannykkh/corelay-code/commit/29b3bc3))
- **ux**: start heartbeat BEFORE StreamMessage to cover model-load dead air ([8f6f743](https://github.com/Dannykkh/corelay-code/commit/8f6f743))
- **sessions**: always return [] instead of null for empty sessions list ([7420c3f](https://github.com/Dannykkh/corelay-code/commit/7420c3f))
- **web**: surface API errors + fix Up button drive-root + always show Add ([7c8d4c2](https://github.com/Dannykkh/corelay-code/commit/7c8d4c2))
- **server**: default to loopback bind instead of all interfaces ([6f75387](https://github.com/Dannykkh/corelay-code/commit/6f75387))
- **agent**: resolve relative tool paths against workDir in the permission check ([79b8007](https://github.com/Dannykkh/corelay-code/commit/79b8007))
- **agent**: stop the skill catalog from suppressing tool calls on local models ([baa8084](https://github.com/Dannykkh/corelay-code/commit/baa8084))
- **ui**: chat fills main pane + activity bar status-bar offset ([2bd219d](https://github.com/Dannykkh/corelay-code/commit/2bd219d))

### Documentation

- explain current self-questioning workflow ([2466bef](https://github.com/Dannykkh/corelay-code/commit/2466bef))
- redefine Corelay Code and add visual guide ([8f20360](https://github.com/Dannykkh/corelay-code/commit/8f20360))
- **readme**: say what this is, why it was built, and what it produced ([5b1cf79](https://github.com/Dannykkh/corelay-code/commit/5b1cf79))
- **readme**: group the API table and cover the routes people actually hit ([a0d6e09](https://github.com/Dannykkh/corelay-code/commit/a0d6e09))
- **handoff**: record the repo split and the local-model hardening session ([797921c](https://github.com/Dannykkh/corelay-code/commit/797921c))
- **readme**: correct the counts to what the tree actually contains ([154d5f9](https://github.com/Dannykkh/corelay-code/commit/154d5f9))
- **readme**: stand on its own instead of on Claude Code ([bac7a46](https://github.com/Dannykkh/corelay-code/commit/bac7a46))
- record the edit-hint A/B and correct the earlier call-count claim ([6181cc9](https://github.com/Dannykkh/corelay-code/commit/6181cc9))
- **readme**: lead with the local-model runtime, not the quota scheduler ([20ab55e](https://github.com/Dannykkh/corelay-code/commit/20ab55e))
- **handoffs**: point session metadata at the split-out repo path ([2b07442](https://github.com/Dannykkh/corelay-code/commit/2b07442))
- **chronos**: record agent loop cycles ([b8375fa](https://github.com/Dannykkh/corelay-code/commit/b8375fa))
- **agent**: describe team and regression workflows ([af6609f](https://github.com/Dannykkh/corelay-code/commit/af6609f))
- session handoff — local-model agent hardening + Claude-Code UX (18 commits) ([1e194da](https://github.com/Dannykkh/corelay-code/commit/1e194da))
- session handoff — UX/tray/CLI/model-detection (2026-05-30) ([b2628d3](https://github.com/Dannykkh/corelay-code/commit/b2628d3))

### Refactoring

- **agent**: delete the superseded V1 tool implementations ([eba085f](https://github.com/Dannykkh/corelay-code/commit/eba085f))
- drop cost tracking, keep usage counts ([eb41cf1](https://github.com/Dannykkh/corelay-code/commit/eb41cf1))
- **config**: move state out of ~/.claude-proxy into ~/.aniclew ([e9b9d62](https://github.com/Dannykkh/corelay-code/commit/e9b9d62))

### Other Changes

- ignore workspace TermSnap state ([c4b46e8](https://github.com/Dannykkh/corelay-code/commit/c4b46e8))
- accept fail-closed sandbox setup timeout ([ee802e1](https://github.com/Dannykkh/corelay-code/commit/ee802e1))
- make cross-platform gates deterministic ([cbf9c17](https://github.com/Dannykkh/corelay-code/commit/cbf9c17))
- **ip**: drop verbatim strings shared with the leaked upstream tree ([86ff6ab](https://github.com/Dannykkh/corelay-code/commit/86ff6ab))
- **workstream**: cover agent loop recording ([83f6940](https://github.com/Dannykkh/corelay-code/commit/83f6940))
- **gitignore**: ignore local workstream state ([08ede66](https://github.com/Dannykkh/corelay-code/commit/08ede66))
- track cmd/proxy entry point (was ignored by unanchored .gitignore) ([76c31d7](https://github.com/Dannykkh/corelay-code/commit/76c31d7))
- add executeEditV2 integration tests for lint gate and fuzzy match ([5be2324](https://github.com/Dannykkh/corelay-code/commit/5be2324))
- beginner-friendly UI — simplified navigation + welcoming chat ([5f2f399](https://github.com/Dannykkh/corelay-code/commit/5f2f399))
- Chat welcome + page descriptions + clearer navigation ([bb353d3](https://github.com/Dannykkh/corelay-code/commit/bb353d3))
- Settings — simplified one-click start ([f00dcf9](https://github.com/Dannykkh/corelay-code/commit/f00dcf9))

[2.0.1]: https://github.com/Dannykkh/corelay-code/compare/v2.0.0...v2.0.1
[2.0.0]: https://github.com/Dannykkh/corelay-code/compare/v1.6.0...v2.0.0
