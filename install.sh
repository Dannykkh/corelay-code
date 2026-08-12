#!/bin/bash
# Corelay Code installer — downloads the latest release for your platform
set -e

REPO="Dannykkh/corelay-code"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

echo "Corelay Code Installer"
echo "======================"

# Detect OS and arch
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$OS" in
  mingw*|msys*|cygwin*) OS="windows" ;;
esac

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)             echo "Unsupported arch: $ARCH"; exit 1 ;;
esac

# Release assets include the tag so each executable can be selected exactly.
RELEASE_JSON=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" || true)
TAG=$(printf '%s\n' "$RELEASE_JSON" | grep '"tag_name"' | head -1 | cut -d '"' -f 4 || true)
EXT=""
if [ "$OS" = "windows" ]; then
  EXT=".exe"
fi
COMMANDS=("corelaycode" "corelaycode-acp" "corelaycode-profile")

echo "Platform: $OS/$ARCH"

# Resolve all public executables before downloading any of them.
echo "Fetching latest release..."
DOWNLOAD_URLS=()
for command in "${COMMANDS[@]}"; do
  asset="${command}-${TAG}-${OS}-${ARCH}${EXT}"
  url=$(printf '%s\n' "$RELEASE_JSON" | grep 'browser_download_url' | grep -F "$asset" | head -1 | cut -d '"' -f 4 || true)
  if [ -z "$url" ]; then
    DOWNLOAD_URLS=()
    break
  fi
  echo "Binary: $asset"
  DOWNLOAD_URLS+=("$url")
done

if [ "${#DOWNLOAD_URLS[@]}" -ne "${#COMMANDS[@]}" ]; then
  echo ""
  echo "No pre-built binary found. Building from source..."
  echo ""
  echo "  git clone https://github.com/$REPO.git && cd corelay-code"
  echo "  cd web && npm install && npm run build && cd .."
  echo "  cp -r web/dist/* internal/server/webdist/"
  echo "  make go"
  echo ""
  exit 1
fi

STAGING_DIR=$(mktemp -d)
trap 'rm -rf "$STAGING_DIR"' EXIT
for index in "${!COMMANDS[@]}"; do
  command="${COMMANDS[$index]}"
  local_name="${command}${EXT}"
  url="${DOWNLOAD_URLS[$index]}"
  echo "Downloading: $url"
  curl -fsSL "$url" -o "$STAGING_DIR/$local_name"
  chmod +x "$STAGING_DIR/$local_name"
done

for command in "${COMMANDS[@]}"; do
  local_name="${command}${EXT}"
  if [ -w "$INSTALL_DIR" ]; then
    mv "$STAGING_DIR/$local_name" "$INSTALL_DIR/$local_name"
  else
    sudo mv "$STAGING_DIR/$local_name" "$INSTALL_DIR/$local_name"
  fi
done

echo ""
echo "Installed:"
for command in "${COMMANDS[@]}"; do
  echo "  $INSTALL_DIR/${command}${EXT}"
done
echo ""
echo "Quick start:"
echo "  corelaycode                              # interactive provider select"
echo "  corelaycode -provider ollama -model qwen3:14b  # direct start"
echo ""
echo "Web UI: http://localhost:4000/app"
echo ""
echo "Connect CLI tools:"
echo "  ANTHROPIC_BASE_URL=http://localhost:4000 claude"
echo "  OPENAI_BASE_URL=http://localhost:4000 codex"
