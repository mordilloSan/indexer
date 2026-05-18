#!/bin/bash
# Installation script for indexer daemon + systemd units

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}Installing Indexer Daemon${NC}\n"

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    echo -e "${RED}Error: This script must be run as root${NC}"
    echo "Please run: sudo $0"
    exit 1
fi

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

if systemctl list-unit-files | grep -q '^indexer-index.timer'; then
    echo -e "${YELLOW}Stopping existing indexer-index.timer (if running)...${NC}"
    systemctl stop indexer-index.timer 2>/dev/null || true
    echo -e "${YELLOW}Disabling existing indexer-index.timer...${NC}"
    systemctl disable indexer-index.timer 2>/dev/null || true
fi

if systemctl list-unit-files | grep -q '^indexer-index.service'; then
    echo -e "${YELLOW}Stopping existing indexer-index.service (if running)...${NC}"
    systemctl stop indexer-index.service 2>/dev/null || true
fi

# Step 1: Verify the prebuilt binary exists
echo -e "${YELLOW}[1/5]${NC} Verifying prebuilt binary..."
if [ ! -x "./indexer" ]; then
    echo -e "${RED}Error: ./indexer not found or not executable.${NC}"
    echo "Run 'make build-only' before 'make localinstall'."
    exit 1
fi
echo -e "${GREEN}✓${NC} Prebuilt binary found"

# Step 2: Install binary
echo -e "${YELLOW}[2/5]${NC} Installing binary to /usr/local/bin..."
cp indexer /usr/local/bin/indexer
chmod +x /usr/local/bin/indexer
echo -e "${GREEN}✓${NC} Binary installed"

# Step 3: Install systemd files
echo -e "${YELLOW}[3/5]${NC} Installing systemd files..."
cp systemd/indexer.service /etc/systemd/system/
cp systemd/indexer.socket /etc/systemd/system/
cp systemd/indexer-index.service /etc/systemd/system/
cp systemd/indexer-index.timer /etc/systemd/system/
echo -e "${GREEN}✓${NC} Systemd files installed"

# Ensure data and config directories exist
mkdir -p /var/lib/indexer
chmod 0755 /var/lib/indexer
mkdir -p /etc/indexer
chmod 0755 /etc/indexer

# Optional: seed /etc/indexer/config.json if missing
if [ ! -f /etc/indexer/config.json ]; then
    echo -e "${YELLOW}Creating /etc/indexer/config.json with defaults...${NC}"
    cat >/etc/indexer/config.json <<'EOF'
{
  "index_path": "/",
  "index_name": "root",
  "include_hidden": true,
  "include_network_mounts": false,
  "fresh_index": true,
  "keep_indexes": 0,
  "db_path": "/tmp/indexer.db",
  "db_busy_timeout": "5s",
  "db_journal_mode": "WAL",
  "db_synchronous": "OFF",
  "db_auto_vacuum": "INCREMENTAL",
  "db_max_open_conns": 5,
  "db_max_idle_conns": 2,
  "db_conn_max_idle_time": "5m0s",
  "socket_path": "/var/run/indexer.sock",
  "listen_addr": "",
  "interval": "1h0m0s"
}
EOF
    echo -e "${GREEN}✓${NC} /etc/indexer/config.json created (edit to suit your system)"
fi

# Step 4: Reload systemd and enable socket + timer
echo -e "${YELLOW}[4/5]${NC} Enabling systemd socket and timer..."
systemctl daemon-reload
systemctl enable indexer.socket
systemctl enable indexer-index.timer
systemctl start indexer.socket
systemctl start indexer-index.timer
echo -e "${GREEN}✓${NC} Socket and timer started${NC}"

# Step 5: Verify installation
echo -e "${YELLOW}[5/5]${NC} Verifying installation..."
sleep 1

if systemctl is-active --quiet indexer.socket; then
    echo -e "${GREEN}✓${NC} Socket is active"
else
    echo -e "${RED}✗${NC} Socket is not active"
    exit 1
fi

if systemctl is-active --quiet indexer-index.timer; then
    echo -e "${GREEN}✓${NC} Index timer is active"
else
    echo -e "${RED}✗${NC} Index timer is not active"
    exit 1
fi

echo -e "\n${GREEN}Installation completed successfully!${NC}\n"

# Display usage instructions
echo -e "${YELLOW}Usage:${NC}"
echo "  Edit configuration:"
echo "    sudo nano /etc/indexer/config.json"
echo ""
echo "  Apply configuration:"
echo "    sudo indexer config set --interval 6h"
echo ""
echo "  Check status:"
echo "    sudo systemctl status indexer.socket"
echo "    sudo systemctl status indexer-index.timer"
echo "    sudo systemctl status indexer.service"
echo ""
echo "  View logs:"
echo "    sudo journalctl -u indexer.service -f"
echo "    sudo journalctl -u indexer-index.service -f"
echo ""
echo "  Manual index (via HTTP API):"
echo "    sudo curl -X POST --unix-socket /var/run/indexer.sock http://localhost/index"
echo ""
echo "  Auto-index interval:"
echo "    sudo indexer config set --interval 6h"
echo "    Set to 0 to disable the systemd timer"
echo ""
echo "  Socket location: /var/run/indexer.sock (managed by systemd)"
echo ""
