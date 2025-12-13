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

# Ensure Go is on PATH (sudo often drops user PATH)
PATH="/usr/local/go/bin:$PATH"
GO_BIN="${GO_BIN:-$(command -v go || true)}"
if [ -z "$GO_BIN" ] && [ -x "/usr/local/go/bin/go" ]; then
    GO_BIN="/usr/local/go/bin/go"
fi
if [ -z "$GO_BIN" ]; then
    echo -e "${RED}Error: Go toolchain not found. Install Go or set GO_BIN to the go binary.${NC}"
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

# Step 1: Build the binary
echo -e "${YELLOW}[1/5]${NC} Building indexer binary..."
if command -v make &>/dev/null; then
    GO_BIN="$GO_BIN" make build
else
    "$GO_BIN" build -o indexer .
fi

# Step 2: Install binary
echo -e "${YELLOW}[2/5]${NC} Installing binary to /usr/local/bin..."
cp indexer /usr/local/bin/indexer
chmod +x /usr/local/bin/indexer
echo -e "${GREEN}✓${NC} Binary installed"

# Step 3: Install systemd files
echo -e "${YELLOW}[3/5]${NC} Installing systemd files..."
cp systemd/indexer.service /etc/systemd/system/
cp systemd/indexer.socket /etc/systemd/system/
echo -e "${GREEN}✓${NC} Systemd files installed"

# Ensure data directory exists for persistent DB
mkdir -p /var/lib/indexer
chmod 0755 /var/lib/indexer

# Optional: seed /etc/default/indexer if missing
if [ ! -f /etc/default/indexer ]; then
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

# Step 4: Reload systemd and enable socket + service
echo -e "${YELLOW}[4/5]${NC} Enabling systemd socket and service..."
systemctl daemon-reload
systemctl enable indexer.socket
systemctl enable indexer.service
systemctl start indexer.socket
systemctl start indexer.service
echo -e "${GREEN}✓${NC} Socket and service started${NC}"

# Step 5: Verify installation
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

# Display usage instructions
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
echo "  Auto-index interval:"
echo "    Edit INDEXER_INTERVAL in /etc/default/indexer (e.g., 1h, 6h, 30m)"
echo "    Set to 0 to disable automatic indexing"
echo ""
echo "  Socket location: /var/run/indexer.sock (managed by systemd)"
echo ""
