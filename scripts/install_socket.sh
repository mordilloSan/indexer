#!/bin/bash
# Installation script for indexer daemon + systemd units

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY_PATH="$PROJECT_ROOT/indexer"
SERVICE_FILE="$PROJECT_ROOT/systemd/indexer.service"

cd "$PROJECT_ROOT"

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

# Step 1: Build the binary
echo -e "${YELLOW}[1/5]${NC} Building indexer binary..."
if [ -f "$BINARY_PATH" ]; then
    echo "Binary already exists at $BINARY_PATH"
else
    (cd "$PROJECT_ROOT" && make build) || (cd "$PROJECT_ROOT" && go build -o "$BINARY_PATH")
fi

if [ ! -f "$BINARY_PATH" ]; then
    echo -e "${RED}Error: Failed to build binary${NC}"
    exit 1
fi

# Step 2: Install binary
echo -e "${YELLOW}[2/5]${NC} Installing binary to /usr/local/bin..."
cp "$BINARY_PATH" /usr/local/bin/indexer
chmod +x /usr/local/bin/indexer
echo -e "${GREEN}✓${NC} Binary installed"

# Step 3: Install systemd files
echo -e "${YELLOW}[3/5]${NC} Installing systemd files..."
cp "$SERVICE_FILE" /etc/systemd/system/
echo -e "${GREEN}✓${NC} Systemd files installed"

# Optional: seed /etc/default/indexer if missing
if [ ! -f /etc/default/indexer ]; then
    echo -e "${YELLOW}Creating /etc/default/indexer with defaults...${NC}"
    cat >/etc/default/indexer <<'EOF'
# Environment for indexer.service
INDEXER_PATH=/
INDEXER_NAME=root
INDEXER_INCLUDE_HIDDEN=false
INDEXER_SOCKET=/var/run/indexer.sock
INDEXER_DB_PATH=/var/run/indexer.db
INDEXER_LISTEN=
# Set to duration (e.g., 6h) to enable in-process interval scans; 0 disables.
INDEXER_INTERVAL=0
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
