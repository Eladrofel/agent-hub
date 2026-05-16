#!/usr/bin/env bash
# -----------------------------------------------------------------------------
# install.sh — install agentctl on this host.
#
# Invoked by the concept-workflow plugin's /setup-agent-events skill via
# SSH-dispatch to each agent VM (and locally on the operator's Mac).
#
# Usage:
#   ./install.sh [--binary-dir <dir>] [--source-binary <path>]
#
# What it does:
#   1. Validate target dir is on PATH (default /usr/local/bin; sudo required)
#   2. Copy the agentctl binary from the source path (default: ./bin/agentctl-<os>-<arch>)
#   3. chmod 755
#   4. Verify with `agentctl --version`
#
# The binary itself is cross-compiled from the gateway/cmd/agentctl source
# tree on the operator's Mac:
#   cd gateway
#   GOOS=darwin GOARCH=arm64 go build -o ../bin/agentctl-darwin-arm64 ./cmd/agentctl
#   GOOS=linux  GOARCH=amd64 go build -o ../bin/agentctl-linux-amd64  ./cmd/agentctl
#
# v0.1.0-dev status: this script is the install path; the binary build steps
# above need go.sum (run `go mod tidy` first) before they'll succeed. Tracked
# in CHANGELOG.md.
# -----------------------------------------------------------------------------

set -euo pipefail

BINARY_DIR="/usr/local/bin"
SOURCE_BINARY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary-dir)
      BINARY_DIR="$2"
      shift 2
      ;;
    --source-binary)
      SOURCE_BINARY="$2"
      shift 2
      ;;
    *)
      echo "Unknown arg: $1" >&2
      exit 2
      ;;
  esac
done

# Auto-detect source binary if not specified.
if [[ -z "$SOURCE_BINARY" ]]; then
  OS=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in
    arm64|aarch64) ARCH="arm64" ;;
    x86_64) ARCH="amd64" ;;
    *) echo "Unsupported arch: $(uname -m)" >&2; exit 2 ;;
  esac
  SOURCE_BINARY="$(dirname "$0")/../bin/agentctl-${OS}-${ARCH}"
fi

if [[ ! -f "$SOURCE_BINARY" ]]; then
  echo "Binary not found: $SOURCE_BINARY" >&2
  echo "Build it first: cd ../gateway && GOOS=<os> GOARCH=<arch> go build -o ../bin/agentctl-<os>-<arch> ./cmd/agentctl" >&2
  exit 1
fi

echo "Installing $SOURCE_BINARY → $BINARY_DIR/agentctl"
sudo install -m 0755 "$SOURCE_BINARY" "$BINARY_DIR/agentctl"

echo "Verifying..."
agentctl --version

echo "Done."
