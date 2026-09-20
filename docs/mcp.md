# MCP transports

Corelay loads MCP definitions into a run-owned catalog. A run never reads the
process-global MCP client registry, and a borrowed session runtime cannot be
merged with another workspace authority.

Configuration sources are merged from low to high precedence:

1. `~/.claude/settings.json`
2. `mcpConfigPaths` from Corelay's application config, in list order
3. `<workspace>/.claude/settings.json`
4. `<workspace>/mcp.json`
5. `<workspace>/.mcp.json`

The later definition replaces a server with the same name. Supplemental paths
may be absolute or relative to the workspace. Each explicit path must exist and
be valid; at most 64 paths and 256 KiB per file are accepted.

Stdio remains the default:

```json
{
  "mcpServers": {
    "local-tools": {
      "command": "my-mcp-server",
      "args": [],
      "env": {}
    }
  }
}
```

Streamable HTTP is explicit and uses a run-owned client. Custom headers are the
place for an operator-provided bearer token or API key; credentials are never
written to transcripts, receipts, or tool definitions.

```json
{
  "mcpServers": {
    "remote-tools": {
      "type": "http",
      "url": "https://mcp.example.test/mcp",
      "headers": {
        "Authorization": "Bearer <operator-provided-token>"
      }
    }
  }
}
```

The HTTP client uses MCP protocol version `2025-06-18`, accepts JSON or
`text/event-stream` response frames, honors request cancellation, and keeps the
server-issued `Mcp-Session-Id`. A 404/410 session loss performs one fresh
initialize and tool-list refresh before retrying; generic network and 5xx
failures are not retried because a remote tool may already have run. A
failure during that initialization or refresh ends the recovery attempt rather
than starting a nested reconnect. Waiting for another reconnect honors the
request's cancellation and deadline. A
`notifications/tools/list_changed` response marks the catalog stale and causes
the next tool call to refresh its schema. Redirects, embedded URL credentials,
reserved headers, unauthorized responses, and oversized frames are rejected.

Legacy SSE is not advertised by the ACP bridge. OAuth issuer discovery and
token refresh are outside the current local configuration contract; provide a
ready-to-use authorization header through an approved secret configuration.
