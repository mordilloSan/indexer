#!/usr/bin/env bash
# Install script for indexer daemon + systemd units (downloads assets from GitHub Releases)

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

usage() {
  cat <<'EOF'
Usage:
  sudo ./global_install.sh [version]

Environment variables:
  INDEXER_REPO           GitHub repo in owner/name form (default: mordilloSan/indexer)
  INDEXER_VERSION        Release tag to install (default: latest, unless release-pinned)
  INDEXER_INSTALL_DIR    Binary install dir (default: /usr/local/bin)
  INDEXER_SYSTEMD_DIR    systemd unit dir (default: /etc/systemd/system)
  INDEXER_BINARY_NAME    Installed binary name (default: indexer)

Examples:
  sudo ./global_install.sh v1.1.2
  curl -fsSL https://github.com/mordilloSan/indexer/releases/latest/download/indexer-install.sh | sudo bash
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

echo -e "${GREEN}Installing Indexer Daemon (from GitHub Releases)${NC}\n"

if [[ "${EUID}" -ne 0 ]]; then
  echo -e "${RED}Error: This script must be run as root${NC}"
  echo "Please run: sudo $0"
  exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
  echo -e "${RED}Error: systemctl not found. This installer targets systemd-based Linux systems.${NC}"
  exit 1
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
if [[ "$os" != "linux" ]]; then
  echo -e "${RED}Error: unsupported OS '$os'. This installer currently supports Linux only.${NC}"
  exit 1
fi

arch_raw="$(uname -m)"
case "$arch_raw" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *)
    echo -e "${RED}Error: unsupported architecture '$arch_raw'. Supported: x86_64/amd64, aarch64/arm64.${NC}"
    exit 1
    ;;
esac

repo="${INDEXER_REPO:-mordilloSan/indexer}"
default_version="__VERSION__"
if [[ "$default_version" == "__VERSION__" ]]; then
  default_version="latest"
fi
version="${1:-${INDEXER_VERSION:-$default_version}}"

install_dir="${INDEXER_INSTALL_DIR:-/usr/local/bin}"
systemd_dir="${INDEXER_SYSTEMD_DIR:-/etc/systemd/system}"
binary_name="${INDEXER_BINARY_NAME:-indexer}"

asset_bin="${binary_name}-${os}-${arch}"
asset_service="indexer.service"
asset_socket="indexer.socket"

download() {
  local url="$1"
  local out="$2"

  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$out"
    return 0
  fi
  if command -v wget >/dev/null 2>&1; then
    wget -qO "$out" "$url"
    return 0
  fi

  echo -e "${RED}Error: neither curl nor wget is available for downloads.${NC}"
  exit 1
}

release_url() {
  local asset="$1"
  if [[ "$version" == "latest" ]]; then
    printf 'https://github.com/%s/releases/latest/download/%s' "$repo" "$asset"
  else
    printf 'https://github.com/%s/releases/download/%s/%s' "$repo" "$version" "$asset"
  fi
}

tmpdir="$(mktemp -d)"
cleanup() { rm -rf "$tmpdir"; }
trap cleanup EXIT

# Stop/disable any existing services and socket to avoid conflicts during install
if systemctl list-unit-files | grep -q '^indexer.service'; then
  echo -e "${YELLOW}Stopping existing indexer.service (if running)...${NC}"
  systemctl stop indexer.service 2>/dev/null || true
  echo -e "${YELLOW}Disabling existing indexer.service...${NC}"
  systemctl disable indexer.service 2>/dev/null || true
fi

if systemctl list-unit-files | grep -q '^indexer.socket'; then
  echo -e "${YELLOW}Stopping existing indexer.socket (if running)...${NC}"
  systemctl stop indexer.socket 2>/dev/null || true
  echo -e "${YELLOW}Disabling existing indexer.socket...${NC}"
  systemctl disable indexer.socket 2>/dev/null || true
fi

echo -e "${YELLOW}[1/5]${NC} Downloading ${asset_bin} (${version})..."
bin_path="${tmpdir}/${asset_bin}"
download "$(release_url "$asset_bin")" "$bin_path"
chmod +x "$bin_path"

echo -e "${YELLOW}[2/5]${NC} Installing binary to ${install_dir}/${binary_name}..."
install -m 0755 "$bin_path" "${install_dir}/${binary_name}"
echo -e "${GREEN}✓${NC} Binary installed"

echo -e "${YELLOW}[3/5]${NC} Installing systemd files to ${systemd_dir}..."
svc_path="${tmpdir}/${asset_service}"
sock_path="${tmpdir}/${asset_socket}"
download "$(release_url "$asset_service")" "$svc_path"
download "$(release_url "$asset_socket")" "$sock_path"
install -m 0644 "$svc_path" "${systemd_dir}/indexer.service"
install -m 0644 "$sock_path" "${systemd_dir}/indexer.socket"
echo -e "${GREEN}✓${NC} Systemd files installed"

mkdir -p /var/lib/indexer
chmod 0755 /var/lib/indexer

if [[ ! -f /etc/default/indexer ]]; then
  echo -e "${YELLOW}Creating /etc/default/indexer with defaults...${NC}"
  cat >/etc/default/indexer <<'EOF'
# Environment for indexer.service
INDEXER_PATH=/
INDEXER_NAME=root
INDEXER_INCLUDE_HIDDEN=true
INDEXER_SOCKET=/var/run/indexer.sock
INDEXER_DB_PATH=/tmp/indexer.db
INDEXER_INTERVAL=1h
INDEXER_LISTEN_FLAG=
EOF
  echo -e "${GREEN}✓${NC} /etc/default/indexer created (edit to suit your system)"
fi

echo -e "${YELLOW}[4/5]${NC} Enabling systemd socket and service..."
systemctl daemon-reload
systemctl enable indexer.socket
systemctl enable indexer.service
systemctl start indexer.socket
systemctl start indexer.service
echo -e "${GREEN}✓${NC} Socket and service started${NC}"

echo -e "${YELLOW}[5/5]${NC} Verifying installation..."
sleep 1

if systemctl is-active --quiet indexer.socket; then
  echo -e "${GREEN}✓${NC} Socket is active"
else
  echo -e "${RED}✗${NC} Socket is not active"
  exit 1
fi

if systemctl is-active --quiet indexer.service; then
  echo -e "${GREEN}✓${NC} Service is active"
else
  echo -e "${RED}✗${NC} Service is not active"
  exit 1
fi

echo -e "\n${GREEN}Installation completed successfully!${NC}\n"

echo -e "${YELLOW}Usage:${NC}"
echo "  Edit configuration:"
echo "    sudo nano /etc/default/indexer   # set INDEXER_PATH, INDEXER_INTERVAL, etc."
echo ""
echo "  Restart daemon with new settings:"
echo "    sudo systemctl restart indexer.service"
echo ""
echo "  Check status:"
echo "    sudo systemctl status indexer.socket"
echo "    sudo systemctl status indexer.service"
echo ""
echo "  View logs:"
echo "    sudo journalctl -u indexer.service -f"
echo ""
echo "  Manual index (via HTTP API):"
echo "    curl -X POST --unix-socket /var/run/indexer.sock http://localhost/index"
echo ""
