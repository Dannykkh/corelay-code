#!/bin/bash
set -euo pipefail

# Build Corelay Code binaries for all platforms

VERSION=${1:-"dev"}
OUTPUT_DIR="dist"
BUILD_FAILED=0
expected_artifacts=()
echo "Building Corelay Code $VERSION..."
mkdir -p "$OUTPUT_DIR"

# Build frontend first
echo "Building frontend..."
cd web
npm run build
cd ..
rm -f internal/server/webdist/assets/index-*.js internal/server/webdist/assets/index-*.css
cp -r web/dist/* internal/server/webdist/

# Build for each platform
platforms=(
  "windows/amd64:.exe"
  "darwin/amd64:"
  "darwin/arm64:"
  "linux/amd64:"
  "linux/arm64:"
)

for platform in "${platforms[@]}"; do
  IFS=':' read -r os_arch ext <<< "$platform"
  IFS='/' read -r os arch <<< "$os_arch"

  for target in \
    "corelaycode:./cmd/proxy" \
    "corelaycode-acp:./cmd/corelaycode-acp" \
    "corelaycode-profile:./cmd/corelaycode-profile"
  do
    IFS=':' read -r name package <<< "$target"
    output="$OUTPUT_DIR/${name}-${VERSION}-${os}-${arch}${ext}"
    expected_artifacts+=("${output##*/}")
    echo "  Building $name for $os/$arch → $output"

    ldflags="-s -w"
    ldflags="$ldflags -X github.com/Dannykkh/corelay-code/internal/buildinfo.Version=$VERSION -X github.com/Dannykkh/corelay-code/internal/buildinfo.Commit=${CORELAY_BUILD_COMMIT:-unknown}"
    if CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags "$ldflags" -o "$output" "$package"; then
      echo "    built $(du -h "$output" | cut -f1)"
    else
      echo "    failed"
      BUILD_FAILED=1
    fi
  done
done

if [ "$BUILD_FAILED" -ne 0 ]; then
  echo "One or more release builds failed." >&2
  exit 1
fi

# Create checksums
echo "Creating checksums..."
cd "$OUTPUT_DIR"
artifacts=(corelaycode-*)
if [ "${#artifacts[@]}" -ne "${#expected_artifacts[@]}" ]; then
  echo "Expected ${#expected_artifacts[@]} release artifacts, found ${#artifacts[@]}." >&2
  exit 1
fi
for artifact in "${artifacts[@]}"; do
  matched=0
  for expected in "${expected_artifacts[@]}"; do
    if [ "$artifact" = "$expected" ]; then
      matched=1
      break
    fi
  done
  if [ "$matched" -ne 1 ]; then
    echo "Unexpected release artifact: $artifact" >&2
    exit 1
  fi
done
sha256sum "${artifacts[@]}" > checksums.txt 2>/dev/null || shasum -a 256 "${artifacts[@]}" > checksums.txt
test "$(wc -l < checksums.txt)" -eq "${#expected_artifacts[@]}"
cd ..

echo ""
echo "Done! Binaries in $OUTPUT_DIR/"
ls -lh "$OUTPUT_DIR/"
