#!/bin/bash
# paqet-ctl: Control Panel for Paqet
set -e

CONFIG_FILE="/opt/paqet/config.yaml"
SERVICE_NAME="paqet"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

check_root() {
    if [ "$EUID" -ne 0 ]; then 
        echo -e "${RED}Please run as root (sudo paqet-ctl)${NC}"
        exit 1
    fi
}

show_header() {
    clear
    echo -e "${BLUE}=== Paqet Control Panel ===${NC}"
    systemctl is-active --quiet $SERVICE_NAME && echo -e "Status: ${GREEN}Running${NC}" || echo -e "Status: ${RED}Stopped${NC}"
    # Show installed version
    if [ -x "/opt/paqet/paqet" ]; then
        INSTALLED_VER=$(/opt/paqet/paqet version 2>/dev/null | grep -oP 'Version:\s+\K\S+' || echo "unknown")
        echo -e "Version: ${CYAN}${INSTALLED_VER}${NC}"
    fi
    echo "---------------------------"
}

view_logs() {
    echo -e "${BLUE}Showing last 50 log lines (Ctrl+C to exit)...${NC}"
    journalctl -u $SERVICE_NAME -n 50 -f | \
        grep --line-buffered -E --color=auto 'ERROR|WARN|INFO|DEBUG|panic|$' 
}

view_stats() {
    echo -e "${CYAN}═══════════════════════════════════════${NC}"
    echo -e "${CYAN}       📊 Live Performance Stats       ${NC}"
    echo -e "${CYAN}═══════════════════════════════════════${NC}"
    echo -e "${BLUE}Showing only [STATS] lines (Ctrl+C to exit)...${NC}\n"
    journalctl -u $SERVICE_NAME -n 200 -f | \
        grep --line-buffered '\[STATS\]'
}

view_alerts() {
    echo -e "${RED}═══════════════════════════════════════${NC}"
    echo -e "${RED}       ⚠️  Bottleneck Alerts            ${NC}"
    echo -e "${RED}═══════════════════════════════════════${NC}"
    
    # Show recent alerts (last 500 lines)
    RECENT_ALERTS=$(journalctl -u $SERVICE_NAME -n 500 --no-pager 2>/dev/null | grep '\[ALERT\]')
    
    if [ -z "$RECENT_ALERTS" ]; then
        echo -e "\n${GREEN}✓ No bottleneck alerts in recent logs. System healthy.${NC}"
    else
        ALERT_COUNT=$(echo "$RECENT_ALERTS" | wc -l)
        echo -e "\n${YELLOW}Found $ALERT_COUNT alert(s) in recent logs:${NC}\n"
        echo "$RECENT_ALERTS" | tail -20
        
        # Summary by type
        echo -e "\n${YELLOW}── Alert Summary ──${NC}"
        echo "$RECENT_ALERTS" | grep -oP 'BOTTLENECK: \K[a-z_]+' | sort | uniq -c | sort -rn | while read COUNT TYPE; do
            case $TYPE in
                packet_loss)        ICON="📡" ;;
                pcap_drops)         ICON="💧" ;;
                send_saturated)     ICON="📤" ;;
                recv_saturated)     ICON="📥" ;;
                read_errors)        ICON="❌" ;;
                throughput_collapse) ICON="🐌" ;;
                stream_overload)    ICON="🔥" ;;
                goroutine_leak)     ICON="🧵" ;;
                *)                  ICON="⚠️"  ;;
            esac
            echo -e "  $ICON $TYPE: ${RED}${COUNT}x${NC}"
        done
    fi
    
    echo -e "\n${BLUE}Press Enter to return, or 'f' to follow live alerts...${NC}"
    read -r -p "" CHOICE < /dev/tty
    if [[ "$CHOICE" =~ ^[Ff]$ ]]; then
        echo -e "${RED}Following live alerts (Ctrl+C to exit)...${NC}\n"
        journalctl -u $SERVICE_NAME -n 200 -f | \
            grep --line-buffered '\[ALERT\]'
    fi
}

view_diagnostics() {
    echo -e "${CYAN}═══════════════════════════════════════${NC}"
    echo -e "${CYAN}       🔍 Diagnostics Snapshot          ${NC}"
    echo -e "${CYAN}═══════════════════════════════════════${NC}\n"
    
    # Latest stats
    echo -e "${YELLOW}── Latest Stats ──${NC}"
    journalctl -u $SERVICE_NAME -n 200 --no-pager 2>/dev/null | grep '\[STATS\]' | tail -4
    
    # Latest alerts
    echo -e "\n${YELLOW}── Recent Alerts ──${NC}"
    ALERTS=$(journalctl -u $SERVICE_NAME -n 200 --no-pager 2>/dev/null | grep '\[ALERT\]' | tail -5)
    if [ -z "$ALERTS" ]; then
        echo -e "${GREEN}✓ No alerts${NC}"
    else
        echo "$ALERTS"
    fi
    
    # Service status
    echo -e "\n${YELLOW}── Service ──${NC}"
    systemctl is-active --quiet $SERVICE_NAME && echo -e "Status: ${GREEN}Running${NC}" || echo -e "Status: ${RED}Stopped${NC}"
    UPTIME=$(systemctl show $SERVICE_NAME --property=ActiveEnterTimestamp --value 2>/dev/null)
    [ -n "$UPTIME" ] && echo "Since: $UPTIME"
    
    read -r -p "\nPress Enter to continue..." DUMMY < /dev/tty
}

edit_ports() {
    echo -e "\n${YELLOW}Port Manager${NC}"
    if command -v nano &> /dev/null; then
        echo "Opening config in nano..."
        nano "$CONFIG_FILE"
    else
        echo "nano not found, using vi..."
        vi "$CONFIG_FILE"
    fi
    
    read -r -p "Restart service to apply changes? [Y/n] " RESTART < /dev/tty
    if [[ ! "$RESTART" =~ ^[Nn]$ ]]; then
        systemctl restart $SERVICE_NAME
        echo -e "${GREEN}Service restarted.${NC}"
    fi
}

tune_performance() {
    echo -e "\n${YELLOW}Performance Tuning${NC}"
    echo "Current Settings:"
    grep -E "mode|mtu|sockbuf" "$CONFIG_FILE" || echo "Not set in config"
    echo "-------------------"
    
    read -r -p "Enter new KCP Mode [fast/normal/fast2/fast3/manual] (leave empty to keep): " NEW_MODE < /dev/tty
    read -r -p "Enter new MTU [1200-1500] (leave empty to keep): " NEW_MTU < /dev/tty
    read -r -p "Enter Number of Connections [e.g. 4] (leave empty to keep): " NEW_CONN < /dev/tty
    read -r -p "Enter PCAP Buffer Size (MB) [e.g. 16] (leave empty to keep): " NEW_BUF < /dev/tty
    read -r -p "Enter Encryption Method [aes/xor/none] (leave empty to keep): " NEW_CRYPT < /dev/tty

    # Sanitize inputs to remove hidden characters/newlines
    NEW_MODE=$(echo "$NEW_MODE" | tr -cd '[:alnum:]')
    NEW_MTU=$(echo "$NEW_MTU" | tr -cd '[:digit:]')
    NEW_CONN=$(echo "$NEW_CONN" | tr -cd '[:digit:]')
    NEW_BUF=$(echo "$NEW_BUF" | tr -cd '[:digit:]')
    NEW_CRYPT=$(echo "$NEW_CRYPT" | tr -cd '[:alnum:]')

    if [ ! -z "$NEW_MODE" ]; then
        if grep -q "mode:" "$CONFIG_FILE"; then
            sed -i "s/mode: \".*\"/mode: \"$NEW_MODE\"/" "$CONFIG_FILE"
        else
            # Insert with 2 spaces indentation
            sed -i "/kcp:/a \  mode: \"$NEW_MODE\"" "$CONFIG_FILE"
        fi
        echo "Updated Mode to $NEW_MODE"
    fi
    
    if [ ! -z "$NEW_MTU" ]; then
        if grep -q "mtu:" "$CONFIG_FILE"; then
            sed -i "s/mtu: [0-9]*/mtu: $NEW_MTU/" "$CONFIG_FILE"
        else
            sed -i "/kcp:/a \  mtu: $NEW_MTU" "$CONFIG_FILE"
        fi
        echo "Updated MTU to $NEW_MTU"
    fi

    if [ ! -z "$NEW_CONN" ]; then
        if grep -q "conn:" "$CONFIG_FILE"; then
            sed -i "s/conn: [0-9]*/conn: $NEW_CONN/" "$CONFIG_FILE"
        else
             # Assuming 'transport:' section exists, if not create it
             if ! grep -q "transport:" "$CONFIG_FILE"; then
                 echo "transport:" >> "$CONFIG_FILE"
                 echo "  conn: $NEW_CONN" >> "$CONFIG_FILE"
             else
                 sed -i "/transport:/a \  conn: $NEW_CONN" "$CONFIG_FILE"
             fi
        fi
        echo "Updated Connection Pool to $NEW_CONN"
    fi

    if [ ! -z "$NEW_BUF" ]; then
        BUF_BYTES=$((NEW_BUF * 1024 * 1024))
        if grep -q "sockbuf:" "$CONFIG_FILE"; then
            sed -i "s/sockbuf: [0-9]*/sockbuf: $BUF_BYTES/" "$CONFIG_FILE"
        else
             if ! grep -q "pcap:" "$CONFIG_FILE"; then
                 # Insert pcap section under network
                 sed -i '/^network:/a \  pcap:\n    sockbuf: '"$BUF_BYTES"'' "$CONFIG_FILE"
             else
                 sed -i "/pcap:/a \    sockbuf: $BUF_BYTES" "$CONFIG_FILE"
             fi
        fi
        echo "Updated PCAP Buffer to ${NEW_BUF}MB ($BUF_BYTES bytes)"
    fi

    if [ ! -z "$NEW_CRYPT" ]; then
        if grep -q "block:" "$CONFIG_FILE"; then
            sed -i "s/block: \".*\"/block: \"$NEW_CRYPT\"/" "$CONFIG_FILE"
        else
            sed -i "/kcp:/a \  block: \"$NEW_CRYPT\"" "$CONFIG_FILE"
        fi
        echo "Updated Encryption to $NEW_CRYPT"
    fi
    
    if [ ! -z "$NEW_MODE" ] || [ ! -z "$NEW_MTU" ] || [ ! -z "$NEW_BUF" ] || [ ! -z "$NEW_CRYPT" ]; then
        systemctl restart $SERVICE_NAME
        echo -e "${GREEN}Applied changes and restarted service.${NC}"
    else
        echo "No changes made."
    fi
    read -r -p "Press Enter to continue..." DUMMY < /dev/tty
}

apply_sysctl() {
    echo -e "\n${YELLOW}Applying System Network Optimizations${NC}"
    echo "This will increase kernel UDP buffer limits (rmem_max/wmem_max) to 16MB."
    read -r -p "Apply changes? [y/N]: " CONFIRM < /dev/tty
    
    if [[ "$CONFIRM" =~ ^[Yy]$ ]]; then
        cat >> /etc/sysctl.conf <<EOF
# Paqet Optimizations
net.core.rmem_max=33554432
net.core.wmem_max=33554432
net.core.rmem_default=33554432
net.core.wmem_default=33554432
net.core.netdev_max_backlog=5000
EOF
        sysctl -p
        echo -e "${GREEN}System optimizations applied!${NC}"
    else
        echo "Cancelled."
    fi
    read -r -p "Press Enter to continue..." DUMMY < /dev/tty
}

update_system() {
    echo -e "\n${CYAN}Updating Paqet System...${NC}"
    echo "This will download the latest setup script and run the installer/updater."
    read -r -p "Continue? [y/N]: " CONFIRM < /dev/tty
    if [[ "$CONFIRM" =~ ^[Yy]$ ]]; then
        # Use the master branch or specific hash logic if needed. 
        # For now, master is safe as we merge stable releases there.
        curl -fsSL https://raw.githubusercontent.com/LivingG0D/paqet/master/setup-paqet.sh | bash
        echo -e "${GREEN}Update process initiated. The installer should have completed.${NC}"
        read -r -p "Press Enter to return to menu..." DUMMY < /dev/tty
        # Reload self in case this script was updated
        exec "$0" "$@"
    else
        echo "Update cancelled."
        sleep 1
    fi
}

main_menu() {
    while true; do
        show_header
        echo "1) View Logs (all)"
        echo "2) 📊 View Stats (live performance)"
        echo "3) ⚠️  View Alerts (bottleneck detection)"
        echo "4) 🔍 Diagnostics Snapshot"
        echo "5) Edit Configuration (Ports/Rules)"
        echo "6) Tune Performance (Mode/MTU)"
        echo "7) Restart Service"
        echo "8) Stop Service"
        echo "9) Update System"
        echo "10) Apply System Optimization"
        echo "0) Exit"
        
        read -r -p "Select option: " OPT < /dev/tty
        
        case $OPT in
            1) view_logs ;;
            2) view_stats ;;
            3) view_alerts ;;
            4) view_diagnostics ;;
            5) edit_ports ;;
            6) tune_performance ;;
            7) systemctl restart $SERVICE_NAME; echo "Restarted."; sleep 1 ;;
            8) systemctl stop $SERVICE_NAME; echo "Stopped."; sleep 1 ;;
            9) update_system ;;
            10) apply_sysctl ;;
            0) exit 0 ;;
            *) echo "Invalid option" ;;
        esac
    done
}

check_root
main_menu
