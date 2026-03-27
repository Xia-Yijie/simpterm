#!/bin/sh
set -e

REPO="Xia-Yijie/simpterm"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

# Detect OS
OS="$(uname -s)"
case "$OS" in
    Linux)  OS="linux" ;;
    Darwin) OS="darwin" ;;
    *)      echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Detect arch
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)             echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Get latest release tag
TAG="$(curl -sL "https://api.github.com/repos/$REPO/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)"
if [ -z "$TAG" ]; then
    echo "Failed to get latest release"
    exit 1
fi

URL="https://github.com/$REPO/releases/download/$TAG/simpterm-${OS}-${ARCH}"
echo "Installing simpterm $TAG (${OS}/${ARCH})..."

mkdir -p "$INSTALL_DIR"
curl -sL "$URL" -o "$INSTALL_DIR/simpterm"
chmod +x "$INSTALL_DIR/simpterm"

echo "Installed to $INSTALL_DIR/simpterm"

# Check if in PATH
case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) echo "Add $INSTALL_DIR to your PATH:"; echo "  export PATH=\"\$PATH:$INSTALL_DIR\"" ;;
esac
