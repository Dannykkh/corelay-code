# ACP stable v1 interoperability evidence

Observed: 2026-08-12

2026-09-20 workflow boundary: ACP does not yet support Workstream/Plan/Stage
execution. Loading or prompting a session with any workflow binding returns an
explicit unsupported error before runtime registration, MCP, provider calls, or
durable mutation. Use the native Web/API workflow path. The regression tests in
`internal/acpbridge/workflow_binding_test.go` cover both entry points; ordinary
ACP sessions remain supported.

## Pins

- Protocol current-main snapshot: `af41b25f57a79c5629b3164e23fb4e8650badeeb`
- Current-main `schema/v1/schema.json` SHA-256:
  `dce90564fc0d87e16cd9645fa5faba1cb5fb7adac2608bb0b98f6fbda8e951f9`
- Current-main `schema/v1/meta.json` SHA-256:
  `e94998dd88acca9e53d5a0d7c89587b9b4e6fec4c1d925519da04e70917f797b`
- Latest released schema tag observed: `schema-v1.20.0`, peeled commit
  `5e89c71497fe07dd4ae633c181a17224f4a8956d`
- Python SDK smoke client: `082c8f09ccf1c7c235890db2da954aba0b25b4d2`

The current-main snapshot and the released schema are separate evidence lanes.
They must not be described as the same artifact. The current-main stable v1
schema includes methods added after the observed release tag.

## Official-suite availability

The pinned protocol repository has repository checks (`cargo test`, lint,
formatting, and schema generation), but no runner that launches an arbitrary
ACP agent and certifies it. The Rust SDK Testy utility exercises ACP clients
with a deterministic agent, which is the opposite direction from validating
Corelay Code as an agent. Therefore this project does not claim an official full
conformance certification.

## Repository gates

```powershell
go test ./internal/acp ./internal/acpbridge ./cmd/corelaycode-acp
go build -trimpath -o $env:TEMP\corelaycode-acp-conformance.exe ./cmd/corelaycode-acp
```

Both commands passed on 2026-08-12. The built Windows executable was
13,896,192 bytes.

## Official Python SDK smoke

The pinned official Python SDK `examples/client.py` launched the built Corelay Code
agent over stdio. The agent used the configured local Ollama provider.

```powershell
"Reply with exactly ACP_SMOKE_OK.`n" |
  $py examples/client.py $agent `
    --provider ollama `
    --model gemma4:12b-it-qat `
    --response-lang en `
    --shutdown-timeout 30s
```

Observed result: process exit `0`, final client output
`Agent: ACP_SMOKE_OK`. This verifies external SDK interoperability across
initialize, session/new, session/prompt, session/update streaming, and prompt
result decoding. It is a smoke test, not a full conformance suite.

## Advertised boundary

Supported and exercised:

- initialize
- session new, load, list, close, delete, and resume
- session configuration updates
- session mode selection (`read-only`, `workspace`, `full`) with durable policy revision and workspace default
- text prompts and cancellation
- redacted permission requests
- stable stdio MCP with session-owned process and catalog lifetime

Unsupported or deliberately unadvertised:

- authenticate and logout
- additional directories
- image, audio, and embedded-resource prompts
- HTTP and SSE MCP transports

Capability status remains `connected` until a deterministic advertised-surface
fixture and the relevant external SDK lane run in repeatable CI.

The execution-mode boundary has backend coverage in
`internal/acpbridge/execution_mode_test.go` and a built-command protocol fixture
in `cmd/corelaycode-acp/conformance_test.go`, including `session/set_mode`, mode
update notification, and full-mode persistence across load/resume. The pinned
external SDK smoke above predates this addition and does not certify mode
negotiation through that SDK end to end.
