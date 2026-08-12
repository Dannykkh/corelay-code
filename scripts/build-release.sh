#!/bin/bash
# Build Corelay Code binaries for all platforms

VERSION=${1:-"dev"}
OUTPUT_DIR="dist"
BUILD_FAILED=0
echo "Building Corelay Code $VERSION..."
mkdir -p $OUTPUT_DIR

# Build frontend first
echo "Building frontend..."
cd web && npm run build && cd ..
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
    echo "  Building $name for $os/$arch → $output"

    ldflags="-s -w"
    if [ "$name" = "corelaycode-acp" ]; then
      ldflags="$ldflags -X main.version=$VERSION"
    fi
    if GOOS=$os GOARCH=$arch go build -ldflags "$ldflags" -o "$output" "$package"; then
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
cd $OUTPUT_DIR
sha256sum corelaycode-* > checksums.txt 2>/dev/null || shasum -a 256 corelaycode-* > checksums.txt
cd ..

echo ""
echo "Done! Binaries in $OUTPUT_DIR/"
ls -lh $OUTPUT_DIR/
