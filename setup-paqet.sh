#!/bin/bash
set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
INSTALL_DIR="/opt/paqet"
BIN_NAME="paqet"
SERVICE_NAME="paqet"
CONFIG_FILE="$INSTALL_DIR/config.yaml"

# Check for existing installation
SKIP_CONFIG=false

if [ -f "/etc/systemd/system/$SERVICE_NAME.service" ] || [ -f "$INSTALL_DIR/$BIN_NAME" ]; then
    echo -e "${BLUE}=== Existing Installation Detected ===${NC}"
    echo "1) Update & Optimize (Safe Migration) - Keeps ports/IPs, updates core"
    echo "2) Fresh Install (Remove all settings and re-configure)"
    read -r -p "Select option [1/2]: " INSTALL_OPT < /dev/tty
    
    if [ "$INSTALL_OPT" == "1" ]; then
        echo -e "${BLUE}Updating binary and migrating config...${NC}"
        systemctl stop "$SERVICE_NAME" || true
        SKIP_CONFIG=true
        
        # Migration Logic
        if [ -f "$INSTALL_DIR/config.yaml" ]; then
            echo "Backing up config to config.yaml.bak"
            cp "$INSTALL_DIR/config.yaml" "$INSTALL_DIR/config.yaml.bak"
            
            # ============================================================
            # paqet2 -> paqet Migration + Optimization
            # ============================================================
            
            # --- Step 1: Remap removed KCP modes (paqet2 had turbo/eco/stable) ---
            # Must happen BEFORE binary loads the config, otherwise validation fails
            if grep -q 'mode: "turbo"' "$INSTALL_DIR/config.yaml" 2>/dev/null; then
                sed -i 's/mode: "turbo"/mode: "fast2"/' "$INSTALL_DIR/config.yaml"
                echo "Migrated KCP mode: turbo -> fast2"
            fi
            if grep -q 'mode: "eco"' "$INSTALL_DIR/config.yaml" 2>/dev/null; then
                sed -i 's/mode: "eco"/mode: "normal"/' "$INSTALL_DIR/config.yaml"
                echo "Migrated KCP mode: eco -> normal"
            fi
            if grep -q 'mode: "stable"' "$INSTALL_DIR/config.yaml" 2>/dev/null; then
                sed -i 's/mode: "stable"/mode: "normal"/' "$INSTALL_DIR/config.yaml"
                echo "Migrated KCP mode: stable -> normal"
            fi
            
            # --- Step 2: Fix top-level pcap (paqet2 configs have pcap at root level) ---
            # New binary expects pcap nested under network:
            if grep -q "^pcap:" "$INSTALL_DIR/config.yaml" 2>/dev/null; then
                echo "Migrating top-level pcap section into network..."
                
                # Extract buffer value if buffer_mb exists (old format)
                OLD_BUF_MB=$(grep "buffer_mb:" "$INSTALL_DIR/config.yaml" | head -n1 | sed 's/[^0-9]//g')
                
                # Remove top-level pcap block (pcap: and all indented children)
                sed -i '/^pcap:/,/^[^ ]/{/^pcap:/d;/^  /d}' "$INSTALL_DIR/config.yaml"
                # Clean any leftover empty lines from removal
                sed -i '/^$/N;/^\n$/d' "$INSTALL_DIR/config.yaml"
                
                # Insert pcap under network with sockbuf in bytes
                if [ -n "$OLD_BUF_MB" ] && [ "$OLD_BUF_MB" -gt 0 ] 2>/dev/null; then
                    SOCKBUF_BYTES=$((OLD_BUF_MB * 1024 * 1024))
                else
                    SOCKBUF_BYTES=16777216  # Default 16MB
                fi
                
                if ! grep -q "sockbuf:" "$INSTALL_DIR/config.yaml"; then
                    sed -i '/^network:/a \  pcap:\n    sockbuf: '"$SOCKBUF_BYTES" "$INSTALL_DIR/config.yaml"
                fi
                echo "Migrated pcap: buffer_mb -> network.pcap.sockbuf ($SOCKBUF_BYTES bytes)"
            fi
            
            # Ensure pcap section exists under network (for configs that had neither)
            if ! grep -q "sockbuf:" "$INSTALL_DIR/config.yaml"; then
                sed -i '/^network:/a \  pcap:\n    sockbuf: 16777216' "$INSTALL_DIR/config.yaml"
            fi
            
            # --- Step 3: Remove orphaned paqet2-only config keys ---
            # These fields existed in paqet2 but were removed in paqet
            # Go YAML parser ignores them, but cleaning keeps config tidy
            sed -i '/^\s*promisc:/d' "$INSTALL_DIR/config.yaml"
            sed -i '/^\s*snaplen:/d' "$INSTALL_DIR/config.yaml"
            sed -i '/^\s*dscp:/d' "$INSTALL_DIR/config.yaml"
            sed -i '/^\s*tos:/d' "$INSTALL_DIR/config.yaml"
            
            # --- Step 4: Ensure transport section has conn ---
            if grep -q "protocol: \"kcp\"" "$INSTALL_DIR/config.yaml" && ! grep -q "conn:" "$INSTALL_DIR/config.yaml"; then
                 sed -i '/protocol: "kcp"/a \  conn: 8' "$INSTALL_DIR/config.yaml"
            fi

            # --- Step 5: Apply standard optimization patches ---
            sed -i 's/mode: .*/mode: "fast"/' "$INSTALL_DIR/config.yaml"
            sed -i 's/conn: .*/conn: 4/' "$INSTALL_DIR/config.yaml"
            sed -i 's/block: .*/block: "xor"/' "$INSTALL_DIR/config.yaml"
            sed -i 's/mtu: .*/mtu: 1200/' "$INSTALL_DIR/config.yaml"
            sed -i 's/sockbuf: .*/sockbuf: 16777216/' "$INSTALL_DIR/config.yaml"
            
            echo -e "${GREEN}Configuration migrated and optimized (fast mode, 16 conns, XOR, 16MB buffer).${NC}"
        fi
    else
        echo -e "${BLUE}Stopping and removing existing service...${NC}"
        systemctl stop "$SERVICE_NAME" || true
        systemctl disable "$SERVICE_NAME" || true
        rm -f "/etc/systemd/system/$SERVICE_NAME.service"
        systemctl daemon-reload
        
        if [ -f "$INSTALL_DIR/config.yaml" ]; then
            echo -e "${BLUE}Backing up existing config to config.yaml.bak${NC}"
            mv "$INSTALL_DIR/config.yaml" "$INSTALL_DIR/config.yaml.bak"
        fi
        
        if [ -f "$INSTALL_DIR/$BIN_NAME" ]; then
            rm -f "$INSTALL_DIR/$BIN_NAME"
        fi
    fi
fi


echo -e "${BLUE}=== Paqet Installer & Configurator ===${NC}"

# 1. Check Root
if [ "$EUID" -ne 0 ]; then 
  echo -e "${RED}Please run as root (sudo)${NC}"
  exit 1
fi

# 2. Install Dependencies
echo -e "${BLUE}Installing dependencies...${NC}"
apt-get update -qq
apt-get install -y libpcap-dev iproute2 curl jq

# 3. Detect Architecture & Download
# Note: In a real scenario, we would fetch the latest release tag from GitHub.
# For this script, we'll try to determine the download URL or ask the user.
ARCH=$(uname -m)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')

if [ "$ARCH" == "x86_64" ]; then
    GOARCH="amd64"
    FILENAME="amd64"
elif [ "$ARCH" == "aarch64" ]; then
    GOARCH="arm64"
    FILENAME="arm64"
elif [ "$ARCH" == "armv7l" ]; then
    GOARCH="arm"
    FILENAME="arm32"
else
    echo -e "${RED}Unsupported architecture: $ARCH${NC}"
    exit 1
fi

echo -e "${GREEN}Detected System: $OS/$GOARCH${NC}"

mkdir -p "$INSTALL_DIR"

# Try to find the latest release (including pre-releases)
echo -e "${BLUE}Attempting to fetch latest release version...${NC}"
LATEST_TAG=$(curl -s "https://api.github.com/repos/LivingG0D/paqet/releases?per_page=1" | jq -r '.[0].tag_name')

if [ "$LATEST_TAG" == "null" ] || [ -z "$LATEST_TAG" ]; then
    echo -e "${RED}Could not fetch latest release. Please enter the version manually (e.g., v1.0.0):${NC}"
    read -r VERSION < /dev/tty
else


    # Set VERSION to LATEST_TAG for checking
    VERSION="$LATEST_TAG"

    # Verify if asset exists in the release
    # Note: Using GOARCH (amd64) not ARCH (x86_64)
    ASSET_NAME="paqet-linux-${FILENAME}-${VERSION}.tar.gz"
    BINARY_NAME="paqet_linux_${FILENAME}"

    ASSET_CHECK=$(curl -s "https://api.github.com/repos/LivingG0D/paqet/releases/tags/${LATEST_TAG}" | jq -r ".assets[] | select(.name == \"${ASSET_NAME}\") | .name")
    
    if [ -z "$ASSET_CHECK" ]; then
        echo -e "${RED}Release $LATEST_TAG found, but asset '${ASSET_NAME}' is not yet uploaded.${NC}"
        echo -e "${BLUE}The build pipeline might still be running. Please wait a few minutes and try again.${NC}"
        echo -e "${BLUE}Or enter a previous working version manually:${NC}"
        read -r VERSION < /dev/tty
    else
        VERSION="$LATEST_TAG"
    fi
fi

if [ -z "$VERSION" ]; then
    VERSION=$LATEST_TAG
fi

# Re-construct asset name if version changed manually
ASSET_NAME="paqet-linux-${FILENAME}-${VERSION}.tar.gz"
BINARY_NAME="paqet_linux_${FILENAME}"
DOWNLOAD_URL="https://github.com/LivingG0D/paqet/releases/download/${VERSION}/${ASSET_NAME}"

echo -e "${BLUE}Downloading from: $DOWNLOAD_URL${NC}"

# Download binary
if curl -fL -o "/tmp/${ASSET_NAME}" "$DOWNLOAD_URL"; then
    echo "Download successful."
    
    # Extract the binary
    echo "Extracting..."
    # Note: build.yml uses "paqet_linux_amd64" (underscore) inside the tarball, 
    # but the tarball itself uses dashes.
    tar -xzf "/tmp/${ASSET_NAME}" -C /tmp
    
    # Move binary to installation path
    if [ -f "/tmp/${BINARY_NAME}" ]; then
        mv "/tmp/${BINARY_NAME}" "$INSTALL_DIR/$BIN_NAME"
        chmod +x "$INSTALL_DIR/$BIN_NAME"
        rm "/tmp/${ASSET_NAME}"
        echo -e "${GREEN}Installed to $INSTALL_DIR/$BIN_NAME${NC}"
    else
        echo -e "${RED}Error: Extracted binary '${BINARY_NAME}' not found in archive.${NC}"
        ls -l /tmp/paqet*
        exit 1
    fi
else
    echo -e "${RED}Error: Failed to download release asset.${NC}"
    echo "URL: $DOWNLOAD_URL"
    exit 1
fi

# 4. Configuration Wizard
if [ "$SKIP_CONFIG" == "true" ]; then
    echo -e "\n${GREEN}Skipping configuration (Update mode selected).${NC}"
else
    echo -e "\n${BLUE}=== Configuration ===${NC}"
    echo "Is this server the Exit Node (Server) or the Entry Node (Client)?"
echo "1) Server (Exit Node - The destination)"
echo "2) Client (Entry Node - The machine you connect to)"
read -r -p "Select role [1/2]: " ROLE_OPT < /dev/tty

# Helper to get Gateway MAC
get_gateway_mac() {
    DEFAULT_IFACE=$(ip route | grep default | awk '{print $5}' | head -n1)
    GATEWAY_IP=$(ip route | grep default | awk '{print $3}' | head -n1)
    
    # Try ip neigh first
    MAC=$(ip neigh show "$GATEWAY_IP" | grep "$DEFAULT_IFACE" | awk '{print $5}')
    
    if [ -z "$MAC" ]; then
        # Try pinging gateway to populate ARP table
        ping -c 1 -W 1 "$GATEWAY_IP" >/dev/null 2>&1
        MAC=$(ip neigh show "$GATEWAY_IP" | grep "$DEFAULT_IFACE" | awk '{print $5}')
    fi
    
    echo "$MAC"
}

# Helper to get Interface IP
get_iface_ip() {
    local iface=$1
    ip -4 addr show "$iface" | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | head -n1
}

# Auto-detect Network Details
AUTO_IFACE=$(ip route | grep default | awk '{print $5}' | head -n1)
AUTO_MAC=$(get_gateway_mac)
AUTO_IP=$(get_iface_ip "$AUTO_IFACE")

echo -e "\n${GREEN}Network Detection:${NC}"
echo "Interface: $AUTO_IFACE"
echo "Local IP:  $AUTO_IP"
echo "Gateway MAC: $AUTO_MAC"
echo "-----------------------------------"

read -r -p "Use these detected settings? [Y/n] " USE_DETECTED < /dev/tty
if [[ "$USE_DETECTED" =~ ^[Nn]$ ]]; then
    read -r -p "Enter Interface Name: " CFG_IFACE < /dev/tty
    read -r -p "Enter Local IP: " CFG_IP < /dev/tty
    read -r -p "Enter Gateway MAC: " CFG_MAC < /dev/tty
else
    CFG_IFACE="$AUTO_IFACE"
    CFG_IP="$AUTO_IP"
    CFG_MAC="$AUTO_MAC"
fi

# Secret Key Handling
echo -e "\n${BLUE}Security Setup${NC}"
read -r -p "Enter a shared secret key (or press Enter to generate one): " CFG_KEY < /dev/tty
if [ -z "$CFG_KEY" ]; then
    CFG_KEY=$("$INSTALL_DIR/$BIN_NAME" secret)
    echo -e "${GREEN}Generated Key: $CFG_KEY${NC}"
    echo -e "${RED}SAVE THIS KEY! You need it for the other side.${NC}"
fi

# Performance Tuning
echo -e "\n${BLUE}Performance Tuning${NC}"
echo "KCP Mode determines aggressiveness. 'fast' is standard, 'normal' is better for unstable/lossy networks."
read -r -p "KCP Mode [fast/normal/fast2/fast3/manual] (default fast): " KCP_MODE < /dev/tty
KCP_MODE=${KCP_MODE:-"fast"}

read -r -p "MTU Size (default 1200, try 1200 if unstable): " KCP_MTU < /dev/tty
KCP_MTU=${KCP_MTU:-1200}

if [ "$ROLE_OPT" == "1" ]; then
    # --- SERVER CONFIGURATION ---
    ROLE="server"
    read -r -p "Listen Port (default 9999): " PORT < /dev/tty
    PORT=${PORT:-9999}
    
    cat > "$CONFIG_FILE" <<EOF
role: "server"
log:
  level: "info"
listen:
  addr: ":$PORT"
network:
  interface: "$CFG_IFACE"
  ipv4:
    addr: "$CFG_IP:$PORT"
    router_mac: "$CFG_MAC"
  pcap:
    sockbuf: 16777216
transport:
  protocol: "kcp"
  conn: 4
  kcp:
    mode: "$KCP_MODE"
    mtu: $KCP_MTU
    key: "$CFG_KEY"
    block: "xor"
EOF

    echo -e "${GREEN}Config created at $CONFIG_FILE${NC}"
    
    # Apply System Optimizations (Buffers)
    if [ ! -f "/etc/sysctl.d/99-paqet.conf" ]; then
        echo -e "\n${BLUE}Applying System Network Optimizations...${NC}"
        cat > /etc/sysctl.d/99-paqet.conf <<EOF
net.core.rmem_max=33554432
net.core.wmem_max=33554432
net.core.rmem_default=33554432
net.core.wmem_default=33554432
net.core.netdev_max_backlog=5000
EOF
        sysctl -p /etc/sysctl.d/99-paqet.conf
    fi

    # Configure Firewall (iptables)
    echo -e "\n${BLUE}Applying Firewall Rules (Anti-Probe/RST Prevention)...${NC}"
    
    # Check if rules exist to avoid duplicates
    if ! iptables -t raw -C PREROUTING -p tcp --dport "$PORT" -j NOTRACK 2>/dev/null; then
        iptables -t raw -A PREROUTING -p tcp --dport "$PORT" -j NOTRACK
        echo "Applied: raw PREROUTING NOTRACK"
    fi
    
    if ! iptables -t raw -C OUTPUT -p tcp --sport "$PORT" -j NOTRACK 2>/dev/null; then
        iptables -t raw -A OUTPUT -p tcp --sport "$PORT" -j NOTRACK
        echo "Applied: raw OUTPUT NOTRACK"
    fi
    
    # Two methods for RST prevention (trying mangle first)
    if ! iptables -t mangle -C OUTPUT -p tcp --sport "$PORT" --tcp-flags RST RST -j DROP 2>/dev/null; then
        iptables -t mangle -A OUTPUT -p tcp --sport "$PORT" --tcp-flags RST RST -j DROP
        echo "Applied: mangle OUTPUT DROP RST"
    fi
    
    # Persist iptables
    echo -e "${BLUE}Persisting iptables rules...${NC}"
    apt-get install -y iptables-persistent
    netfilter-persistent save

else
    # --- CLIENT CONFIGURATION ---
    ROLE="client"
    read -r -p "Enter Remote Server IP: " SERVER_IP < /dev/tty
    read -r -p "Enter Remote Server Port (default 9999): " SERVER_PORT < /dev/tty
    SERVER_PORT=${SERVER_PORT:-9999}
    
    echo -e "\n${BLUE}Client Mode:${NC}"
    echo "1) SOCKS5 Proxy (Dynamic forwarding)"
    echo "2) Port Forwarding (Map remote port to local)"
    read -r -p "Select mode [1/2]: " CLIENT_MODE < /dev/tty
    
    cat > "$CONFIG_FILE" <<EOF
role: "client"
log:
  level: "info"
network:
  interface: "$CFG_IFACE"
  ipv4:
    addr: "$CFG_IP:0"
    router_mac: "$CFG_MAC"
  pcap:
    sockbuf: 16777216
server:
  addr: "$SERVER_IP:$SERVER_PORT"
transport:
  protocol: "kcp"
  conn: 4
  kcp:
    mode: "$KCP_MODE"
    mtu: $KCP_MTU
    key: "$CFG_KEY"
    block: "xor"
EOF

    if [ "$CLIENT_MODE" == "1" ]; then
        read -r -p "SOCKS5 Listen Address (default 0.0.0.0:1080): " SOCKS_ADDR < /dev/tty
        SOCKS_ADDR=${SOCKS_ADDR:-"0.0.0.0:1080"}
        
        cat >> "$CONFIG_FILE" <<EOF
socks5:
  - listen: "$SOCKS_ADDR"
EOF
    else
        echo "forward:" >> "$CONFIG_FILE"
        
        while true; do
            echo -e "\n${BLUE}--- Add Forwarding Rule(s) ---${NC}"
            echo "Enter '0.0.0.0:8080' for specific binding,"
            echo "OR enter a list of ports '8080 8081' for bulk 1:1 mapping."
            read -r -p "Input: " INPUT_VAL < /dev/tty
            
            # Check for multiple ports (space detected)
            if [[ "$INPUT_VAL" =~ [[:space:]] ]]; then
                # Bulk Mode
                TGT_IP="127.0.0.1"
                read -r -p "Protocol [tcp/udp] (default tcp): " FWD_PROTO < /dev/tty
                FWD_PROTO=${FWD_PROTO:-"tcp"}
                
                for PORT in $INPUT_VAL; do
                    cat >> "$CONFIG_FILE" <<EOF
  - listen: "0.0.0.0:$PORT"
    target: "$TGT_IP:$PORT"
    protocol: "$FWD_PROTO"
EOF
                    echo "Added: 0.0.0.0:$PORT -> $TGT_IP:$PORT"
                done
                
            else
                # Single Entry Mode (Smart)
                # Check if input has colon (IP:Port)
                if [[ "$INPUT_VAL" == *":"* ]]; then
                    LISTEN_ADDR="$INPUT_VAL"
                    read -r -p "Target Destination (e.g., 127.0.0.1:80): " TARGET_ADDR < /dev/tty
                else
                    # Just a port number
                    LISTEN_ADDR="0.0.0.0:$INPUT_VAL"
                    read -r -p "Target IP for port $INPUT_VAL (e.g., 127.0.0.1): " TGT_IP < /dev/tty
                    TARGET_ADDR="$TGT_IP:$INPUT_VAL"
                fi
                
                read -r -p "Protocol [tcp/udp] (default tcp): " FWD_PROTO < /dev/tty
                FWD_PROTO=${FWD_PROTO:-"tcp"}
                
                cat >> "$CONFIG_FILE" <<EOF
  - listen: "$LISTEN_ADDR"
    target: "$TARGET_ADDR"
    protocol: "$FWD_PROTO"
EOF
                echo "Added: $LISTEN_ADDR -> $TARGET_ADDR"
            fi

            echo -e "${GREEN}Rule(s) added!${NC}"
            read -r -p "Add more rules? [y/N]: " ADD_MORE < /dev/tty
            if [[ ! "$ADD_MORE" =~ ^[Yy]$ ]]; then
                break
            fi
        done
    fi

    echo -e "${GREEN}Config created at $CONFIG_FILE${NC}"
fi
fi

# 5. Create Systemd Service
echo -e "\n${BLUE}Creating Systemd Service...${NC}"

# Install Control Panel Script
# cp "$0" /tmp/setup-script.sh # Self-copy logic is complex via curl, simpler to embed or download
# Actually, we should download the separate management script
curl -fsSL https://raw.githubusercontent.com/LivingG0D/paqet/master/manage-paqet.sh -o /usr/local/bin/paqet-ctl
chmod +x /usr/local/bin/paqet-ctl

cat > "/etc/systemd/system/$SERVICE_NAME.service" <<EOF
[Unit]
Description=Paqet Proxy Service ($ROLE)
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/$BIN_NAME run -c $CONFIG_FILE
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "$SERVICE_NAME"
systemctl start "$SERVICE_NAME"

echo -e "${GREEN}Service started!${NC}"
systemctl status "$SERVICE_NAME" --no-pager | head -n 10

echo -e "\n${GREEN}Installation Complete!${NC}"
if [ "$ROLE" == "server" ]; then
    echo "Ensure your cloud provider firewall allows TCP port $PORT."
fi

# Launch Control Panel
echo -e "\n${BLUE}Launching Control Panel...${NC}"
sleep 1
/usr/local/bin/paqet-ctl
