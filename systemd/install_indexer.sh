#!/usr/bin/env bash
set -euo pipefail

# Simple installer for the Indexer binary, systemd service, and timer.
# Intended to be run as root (or via sudo) from the project root or from a
# GitHub release archive containing these files.
#
# Usage:
#   sudo ./install_indexer.sh [/path/to/indexer-binary]
#
# If no binary path is provided, ./indexer is used by default.

BINARY_SOURCE="${1:-./indexer}"
PROJECT_DIR="$(cd "$(dirname "${BINARY_SOURCE}")/.." && pwd 2>/dev/null || pwd)"

SERVICE_SRC="${PROJECT_DIR}/systemd/indexer.service"
TIMER_SRC="${PROJECT_DIR}/systemd/indexer.timer"

# Destination paths
INSTALL_PREFIX="/usr/local"
BIN_DEST_DIR="${INSTALL_PREFIX}/bin"
BIN_DEST="${BIN_DEST_DIR}/indexer"
SERVICE_DEST="/etc/systemd/system/indexer.service"
TIMER_DEST="/etc/systemd/system/indexer.timer"

echo ">>> Installing indexer binary from ${BINARY_SOURCE} to ${BIN_DEST}"
mkdir -p "${BIN_DEST_DIR}"
install -m 0755 "${BINARY_SOURCE}" "${BIN_DEST}"

echo ">>> Installing systemd service to ${SERVICE_DEST}"
install -m 0644 "${SERVICE_SRC}" "${SERVICE_DEST}"

echo ">>> Installing systemd timer to ${TIMER_DEST}"
install -m 0644 "${TIMER_SRC}" "${TIMER_DEST}"

echo ">>> Reloading systemd units"
systemctl daemon-reload

echo ">>> Enabling and starting indexer.timer"
systemctl enable --now indexer.timer

echo "Installation complete."
