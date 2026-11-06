#!/usr/bin/env bash
# Builds the BridgeDesk hub for Windows (.exe) and the current host.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p ../dist

echo "[windows/amd64] -> dist/BridgeDesk.exe"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-H windowsgui -s -w" -o ../dist/BridgeDesk.exe .

echo "[windows/arm64] -> dist/BridgeDesk-arm64.exe"
GOOS=windows GOARCH=arm64 go build -trimpath -ldflags="-H windowsgui -s -w" -o ../dist/BridgeDesk-arm64.exe .

echo "[linux/amd64]   -> dist/bridgedesk-linux"
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ../dist/bridgedesk-linux .

echo "[darwin/arm64]  -> dist/bridgedesk-macos"
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o ../dist/bridgedesk-macos . || echo "  (macOS screen/input are stubbed; shell+files+terminal work)"

ls -la ../dist
