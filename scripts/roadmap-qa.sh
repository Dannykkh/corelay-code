#!/usr/bin/env bash
set -euo pipefail

# The roadmap scenarios intentionally share the production package suites.
# This is a CI entry point, not a second test server or a score collector.
# Q01-Q10: config/auth, permissions, sessions, change recovery, workflow,
#           context, skills, CLI, images, and UI contracts in ./...
# Q11:     image transport and browser acceptance in internal/{agent,server}.
# Q12:     direct file/diff and browser file viewer contracts.
# Q13:     Rod bounded fetch and cancellation contracts.
# Q14:     stdio/remote MCP lifecycle and schema refresh contracts.
# Q15:     installed gopls semantic fixture plus structural fallback.
# Q16:     server/CLI lifecycle, migration fixtures, and cross-platform build.

printf 'roadmap QA host: %s/%s\n' "$(uname -s)" "$(uname -m)"
go version
if command -v gopls >/dev/null 2>&1; then
  gopls version
else
  printf 'gopls: unavailable (semantic fixture is an explicit configured-environment check)\n'
fi

go test ./... -count=1
# Keep the legacy-session migration contract visible as a named CI signal;
# the complete suite above remains authoritative for all migration fixtures.
go test ./internal/agent ./internal/server -run 'Migration|Legacy' -count=1
go test ./internal/sandbox ./internal/processsupervisor -count=3
go vet ./...
bash scripts/build-release-test.sh
