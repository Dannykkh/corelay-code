#!/usr/bin/env bash
set -euo pipefail

# Exercise build-release.sh's orchestration without downloading dependencies or
# compiling product code. The real cross-build is covered by release package
# smoke; this fixture keeps the artifact-count and fail-fast contract local.
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/corelay-build-release.XXXXXX")
trap 'rm -rf "$fixture"' EXIT

mkdir -p "$fixture/bin" "$fixture/web/dist" "$fixture/internal/server/webdist/assets"
printf '<!doctype html>\n' > "$fixture/web/dist/index.html"
cp "$repo_root/scripts/build-release.sh" "$fixture/build-release.sh"

cat > "$fixture/bin/npm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${FAKE_NPM_FAIL:-0}" == "1" ]]; then
  exit 7
fi
EOF

cat > "$fixture/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
output=""
while (($#)); do
  if [[ "$1" == "-o" ]]; then
    output=$2
    shift 2
  else
    shift
  fi
done
if [[ -z "$output" ]]; then
  echo "fake go: missing output" >&2
  exit 1
fi
printf 'fixture\n' > "$output"
EOF
chmod +x "$fixture/bin/npm" "$fixture/bin/go" "$fixture/build-release.sh"

run_fixture() {
  (
    cd "$fixture"
    PATH="$fixture/bin:$PATH" CORELAY_BUILD_COMMIT=fixture \
      bash ./build-release.sh fixture
  )
}

run_fixture
artifact_count=0
for artifact in "$fixture"/dist/corelaycode-*; do
  if [[ -f "$artifact" ]]; then
    artifact_count=$((artifact_count + 1))
  fi
done
if [[ "$artifact_count" -ne 15 ]]; then
  echo "build-release fixture artifact count = $artifact_count, want 15" >&2
  exit 1
fi
if [[ "$(wc -l < "$fixture/dist/checksums.txt")" -ne 15 ]]; then
  echo "build-release fixture checksum count is not 15" >&2
  exit 1
fi

# A stale binary must not be silently published as one of the expected files.
touch "$fixture/dist/corelaycode-stale"
if run_fixture > "$fixture/stale.log" 2>&1; then
  echo "build-release fixture accepted a stale artifact" >&2
  cat "$fixture/stale.log" >&2
  exit 1
fi

# Frontend failure must stop before any package is emitted.
rm -f "$fixture/dist/corelaycode-stale"
if FAKE_NPM_FAIL=1 run_fixture > "$fixture/frontend.log" 2>&1; then
  echo "build-release fixture ignored frontend failure" >&2
  cat "$fixture/frontend.log" >&2
  exit 1
fi

echo "build-release fixture: 15 artifacts, 15 checksums, stale/frontend failures rejected"
