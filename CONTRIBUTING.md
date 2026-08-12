# Contributing to Corelay Code

Thank you for your interest in contributing!

## Quick Start

```bash
# Clone
git clone https://github.com/Dannykkh/corelay-code.git
cd corelay-code

# Backend (Go; builds corelaycode and both helper executables)
make go
./corelaycode --provider ollama --model qwen3:14b

# Frontend (React)
cd web
bun install
bun dev  # → http://localhost:5173 (proxies API to :4000)
```

## Project Structure

```
cmd/proxy/          → Entry point (main.go)
internal/
  agent/            → Agent loop, tools, permissions, MCP, sub-agents
  server/           → HTTP server, API endpoints, embedded React UI
  providers/        → LLM provider adapters (7 providers)
  router/           → Smart routing (16 roles)
  kairos/           → Background daemon, memory, A/B testing
  gateway/          → Team auth, budgets, audit
  config/           → Config persistence
  types/            → Shared type definitions
  translate/        → Anthropic ↔ OpenAI format translation
  stream/           → SSE read/write
web/                → React frontend (Vite + TypeScript + Tailwind)
```

## Adding a New Tool

1. Define the tool in `internal/agent/tools_*.go`:
   ```go
   {
     Name: "MyTool",
     Description: "What it does",
     InputSchema: json.RawMessage(`{...}`),
   }
   ```

2. Implement the executor:
   ```go
   func executeMyTool(input json.RawMessage, workDir string) (string, bool) {
     // ... return result, isError
   }
   ```

3. Register in the appropriate `Execute*Tool` switch.

## Adding a New Provider

1. Create `internal/providers/myprovider.go`
2. If OpenAI-compatible, extend `OpenAICompat`
3. Add to `registry.go` Create/ProviderOrder

## Adding a New Page

1. Create `web/src/pages/MyPage.tsx`
2. Add to `App.tsx` routing
3. Add nav item in `components/Sidebar.tsx`
4. Add i18n keys in `lib/i18n.ts`

## Guidelines

- Go code: `go vet` must pass
- React: TypeScript strict mode
- No external Go dependencies (stdlib only)
- Keep binary size under 15MB
- Test with Ollama locally before pushing
