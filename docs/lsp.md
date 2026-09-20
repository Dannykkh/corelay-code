# Semantic Go navigation

The built-in `LSP` tool provides read-only Go definition, references, and
diagnostics through a configured `gopls` executable. It sends the target file
as a full `textDocument/didOpen` document, uses `file://` URIs that preserve
spaces and Windows drive letters, bounds JSON frames and results, and stops the
server after the request. A canceled run or request deadline stops the child
through the process supervisor.

Set `lspExecutable` in `~/.corelay/config.json`, or use
`CORELAY_LSP_EXECUTABLE`. If neither is set, the executable name `gopls` is
resolved from `PATH`; Corelay Code never installs it. `ANICLEW_LSP_EXECUTABLE`
is accepted as a compatibility environment name.

Only `.go` files are currently supported. If the executable is missing, fails
to initialize, or crashes, the tool reports that semantic capability is
unavailable and appends a structural `RepoMap` fallback. The fallback is not
reported as a definition, reference, or diagnostic. The LSP boundary does not
send workspace edits or execute server commands.

Workspace mode keeps the target inside the selected project. An explicitly
selected full-mode run may target an external Go file; in that case the server
is rooted at the external file's directory so its `go.mod` can be discovered.
The supervised child receives a deny-by-default environment plus an isolated
temporary `GOCACHE`; Corelay does not inherit user credentials or install
language-server dependencies.

Example tool input:

```json
{
  "operation": "definition",
  "file_path": "internal/agent/loop.go",
  "line": 1045,
  "column": 18
}
```
