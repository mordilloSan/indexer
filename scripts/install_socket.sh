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

# Stop/disable any existing service to avoid conflicts during install
if systemctl list-unit-files | grep -q '^indexer.service'; then
    echo -e "${YELLOW}Stopping existing indexer.service (if running)...${NC}"
    systemctl stop indexer.service 2>/dev/null || true
    echo -e "${YELLOW}Disabling existing indexer.service...${NC}"
    systemctl disable indexer.service 2>/dev/null || true
fi

# Step 1: Build the binary
echo -e "${YELLOW}[1/5]${NC} Building indexer binary..."
GO_BIN="$GO_BIN" make build || "$GO_BIN" build -o indexer

# Step 2: Install binary
echo -e "${YELLOW}[2/5]${NC} Installing binary to /usr/local/bin..."
cp indexer /usr/local/bin/indexer
chmod +x /usr/local/bin/indexer
echo -e "${GREEN}✓${NC} Binary installed"

# Step 3: Install systemd files
echo -e "${YELLOW}[3/5]${NC} Installing systemd files..."
cp systemd/indexer.service /etc/systemd/system/
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
INDEXER_LISTEN_FLAG=
# Set to Go duration (e.g., 6h, 30m). Units are required; 0 disables.
INDEXER_INTERVAL=1h
EOF
    echo -e "${GREEN}✓${NC} /etc/default/indexer created (edit to suit your system)"
fi

# Step 4: Reload systemd and enable service
echo -e "${YELLOW}[4/5]${NC} Enabling systemd service..."
systemctl daemon-reload
systemctl enable indexer.service
systemctl start indexer.service
echo -e "${GREEN}✓${NC} Service started${NC}"

# Step 5: Verify installation
echo -e "${YELLOW}[5/5]${NC} Verifying installation..."
sleep 1

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
echo "    sudo nano /etc/default/indexer   # set INDEXER_PATH, INDEXER_NAME, etc."
echo ""
echo "  Restart with new settings:"
echo "    sudo systemctl restart indexer.service"
echo ""
echo "  Check service status:"
echo "    sudo systemctl status indexer.service"
echo ""
echo "  View logs:"
echo "    sudo journalctl -u indexer.service -f"
echo ""
