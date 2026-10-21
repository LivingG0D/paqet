#!/usr/bin/env bash
# paqet-plus installer — downloads a release, writes the config, and starts the systemd service.
#
# Run as root on the server:
#   bash <(curl -fsSL https://raw.githubusercontent.com/LivingG0D/paqet-plus/master/setup-paqet-plus.sh)
# The wizard asks for the role (server or client) and the transport settings; both sides must match on
# the key, mode, MTU, block, and FEC. It ends by installing and opening the `paqet-plus-ctl` control panel.
set -uo pipefail

REPO=LivingG0D/paqet-plus
BRANCH="${PAQET_PLUS_BRANCH:-${PAQET_BRANCH:-master}}"
INSTALL_DIR=/opt/paqet-plus
BIN_NAME=paqet-plus
SERVICE_NAME=paqet-plus
CONFIG_FILE="$INSTALL_DIR/config.yaml"
SELF=/usr/local/bin/paqet-plus-ctl
ROLE=''
UPDATE=0

if [ -t 1 ]; then Y=$'\e[1;33m' G=$'\e[32m' R=$'\e[31m' D=$'\e[2m' N=$'\e[0m'; else Y='' G='' R='' D='' N=''; fi

say()  { printf '%s\n' "$*"; }
good() { printf '%s✓%s %s\n' "$G" "$N" "$*"; }
bad()  { printf '%s✗%s %s\n' "$R" "$N" "$*"; }

# Read one answer into REPLY (default when empty). /dev/tty, so a `curl | bash` install can still ask.
ask() { # prompt [default]
  read -r -p "$1" REPLY < /dev/tty || { echo; exit 1; }
  REPLY="${REPLY:-${2:-}}"
}
yes_no() { # prompt default(y|n)
  ask "$1" "$2"
  [[ $REPLY == [yY]* ]]
}

[ "$(id -u)" -eq 0 ] || { bad "Run as root: sudo bash setup-paqet-plus.sh"; exit 1; }

# paqet2 -> paqet: retired modes, pcap move, removed keys, and the standard tuning of a fresh install.
migrate_config() {
  local f="$CONFIG_FILE" old sockbuf=16777216
  [ -f "$f" ] || return 0
  say "Backing up the config to config.yaml.bak"
  cp "$f" "$f.bak"
  # Retired KCP modes: they must go before the new binary reads the config, or validation fails.
  if grep -qE 'mode: "(turbo|eco|stable)"' "$f"; then
    sed -i 's/mode: "turbo"/mode: "fast2"/; s/mode: "eco"/mode: "normal"/; s/mode: "stable"/mode: "normal"/' "$f"
    say "  KCP modes remapped (turbo -> fast2, eco/stable -> normal)"
  fi
  # paqet2 kept pcap at the top level with buffer_mb; paqet wants it under network as sockbuf (bytes).
  if grep -q '^pcap:' "$f"; then
    old=$(grep 'buffer_mb:' "$f" | head -n1 | tr -cd '0-9')
    if [ -n "$old" ] && [ "$old" -gt 0 ]; then sockbuf=$((old * 1024 * 1024)); fi
    sed -i '/^pcap:/,/^[^ ]/{/^pcap:/d;/^  /d}' "$f"
    sed -i '/^$/N;/^\n$/d' "$f"   # squeeze the blank lines the removal leaves
    if ! grep -q 'sockbuf:' "$f"; then
      sed -i '/^network:/a \  pcap:\n    sockbuf: '"$sockbuf" "$f"
    fi
    say "  pcap: moved under network (sockbuf: $sockbuf bytes)"
  fi
  if ! grep -q 'sockbuf:' "$f"; then
    sed -i '/^network:/a \  pcap:\n    sockbuf: 16777216' "$f"
  fi
  # Keys paqet2 had but paqet dropped: harmless to the parser, removed to keep the file tidy.
  sed -i -e '/^\s*promisc:/d' -e '/^\s*snaplen:/d' -e '/^\s*dscp:/d' -e '/^\s*tos:/d' "$f"
  # KCP needs an explicit connection count.
  if grep -q 'protocol: "kcp"' "$f" && ! grep -q 'conn:' "$f"; then
    sed -i '/protocol: "kcp"/a \  conn: 8' "$f"
  fi
  # Standard tuning, the same a fresh wizard run would write. block is kept: both ends must match.
  sed -i -e 's/conn: .*/conn: 8/' -e 's/mtu: .*/mtu: 1250/' \
    -e 's/sockbuf: .*/sockbuf: 16777216/' "$f"
  # Pin the old default for configs that never set block, or the new default (aes-128-gcm) would
  # make this end silently drop every packet from a peer still on salsa20.
  grep -qE '^[[:space:]]*block:' "$f" || sed -i '/^[[:space:]]*kcp:/a \    block: "salsa20"' "$f"
  sed -i -e '/^\s*dshard:/d' -e '/^\s*pshard:/d' "$f"
  sed -i '/^[[:space:]]*block:/a\    dshard: 0\n    pshard: 0' "$f"
  chmod 600 "$f" "$f.bak" 2>/dev/null || true
  good "Config migrated: cipher unchanged, conn 8, MTU 1250, FEC off, 16 MB buffer."
}

# paqet -> paqet-plus: move an install made under the old name to the new paths, keeping the
# config, backups and service drop-ins (GOMAXPROCS and the like). The update path below then
# replaces the binary and writes the new unit.
migrate_legacy() {
  local old_dir=/opt/paqet old_unit=/etc/systemd/system/paqet.service
  [ -f "$old_unit" ] || [ -d "$old_dir" ] || return 0
  if [ -e "$INSTALL_DIR" ]; then
    bad "Both $old_dir and $INSTALL_DIR exist: leaving the old paqet install alone."
    return 0
  fi
  say "${Y}Rename${N}: moving the paqet install to paqet-plus."
  systemctl disable --now paqet >/dev/null 2>&1 || true
  rm -f "$old_unit"
  if [ -d "$old_unit.d" ]; then mv "$old_unit.d" "/etc/systemd/system/$SERVICE_NAME.service.d"; fi
  systemctl daemon-reload
  if [ -d "$old_dir" ]; then
    mv "$old_dir" "$INSTALL_DIR"
    if [ -f "$INSTALL_DIR/paqet" ]; then mv "$INSTALL_DIR/paqet" "$INSTALL_DIR/$BIN_NAME"; fi
  fi
  rm -f /usr/local/bin/paqet-ctl
  if [ -f /etc/sysctl.d/99-paqet.conf ]; then mv /etc/sysctl.d/99-paqet.conf /etc/sysctl.d/99-paqet-plus.conf; fi
  good "Moved to $INSTALL_DIR (service $SERVICE_NAME, panel paqet-plus-ctl)."
}
migrate_legacy

if [ -f "/etc/systemd/system/$SERVICE_NAME.service" ] || [ -f "$INSTALL_DIR/$BIN_NAME" ]; then
  say ""
  say "${Y}Existing install found${N}: paqet-plus is already set up on this server."
  say "   1) Update: keep the config, replace the binary"
  say "   2) Fresh install: remove everything and configure again"
  ask "> " 1
  if [ "$REPLY" = 1 ]; then
    UPDATE=1
    systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
    say "${Y}Update${N}: migrating the config and replacing the binary."
    migrate_config
  else
    say "Removing the existing service..."
    systemctl disable --now "$SERVICE_NAME" >/dev/null 2>&1 || true
    rm -f "/etc/systemd/system/$SERVICE_NAME.service"
    systemctl daemon-reload
    if [ -f "$CONFIG_FILE" ]; then
      say "Backing up the old config to config.yaml.bak"
      mv "$CONFIG_FILE" "$CONFIG_FILE.bak"
    fi
    rm -f "$INSTALL_DIR/$BIN_NAME"
  fi
fi

say ""
say "${Y}Dependencies${N}: libpcap-dev iproute2 curl jq tar gzip ca-certificates iputils-ping"
if ! command -v apt-get >/dev/null 2>&1; then
  bad "This installer requires apt-get (Debian/Ubuntu)."
  exit 1
fi
if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1 \
  || ! command -v ping >/dev/null 2>&1 || [ ! -e /usr/include/pcap.h ]; then
  apt-get update -qq
  # On a fresh VPS unattended-upgrades often holds the dpkg lock for a while: wait for it.
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq -o DPkg::Lock::Timeout=300 \
    libpcap-dev iproute2 curl jq tar gzip ca-certificates iputils-ping \
    || { bad "apt could not install the dependencies."; exit 1; }
  good "Installed"
fi

case $(uname -m) in
  x86_64) FILENAME=amd64 ;;
  aarch64 | arm64) FILENAME=arm64 ;;
  armv7l | armv6l) FILENAME=arm32 ;;
  mips) FILENAME=mips ;;
  mipsel) FILENAME=mipsle ;;
  mips64) FILENAME=mips64 ;;
  mips64el) FILENAME=mips64le ;;
  *) bad "Unsupported architecture: $(uname -m)"; exit 1 ;;
esac
good "System: linux/$FILENAME"

install -d -m 0755 "$INSTALL_DIR"

say "${Y}Release${N}: looking up the latest paqet-plus release..."
LATEST_TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases?per_page=1" | jq -r '.[0].tag_name' 2>/dev/null || true)
VERSION='' ASSET_NAME=''
# Releases from before the rename ship paqet-linux-* archives holding paqet_linux_*.
find_asset() { # tag -> the archive name that release has for this arch, if any
  curl -fsSL "https://api.github.com/repos/$REPO/releases/tags/$1" | jq -r \
    --arg a "$BIN_NAME-linux-$FILENAME-$1.tar.gz" --arg b "paqet-linux-$FILENAME-$1.tar.gz" \
    '[.assets[].name] as $n | if any($n[]; . == $a) then $a elif any($n[]; . == $b) then $b else empty end' \
    2>/dev/null || true
}
if [ -z "$LATEST_TAG" ] || [ "$LATEST_TAG" = null ]; then
  bad "Could not fetch the latest release (GitHub API rate limit or no network)."
  ask "Version to install (e.g. v1.0.0-alpha.36): "
  VERSION=$REPLY
else
  # A release can exist before its assets are uploaded: the build pipeline needs a few minutes.
  ASSET_NAME=$(find_asset "$LATEST_TAG")
  if [ -n "$ASSET_NAME" ]; then
    VERSION=$LATEST_TAG
  else
    bad "Release $LATEST_TAG does not have a linux/$FILENAME archive yet."
    ask "Version to install instead, or wait for the pipeline and rerun: "
    VERSION=$REPLY
  fi
fi
if [ -z "$VERSION" ]; then VERSION=$LATEST_TAG; fi
# A typed version: ask the release which name it uses, else assume the current one.
[ -n "$ASSET_NAME" ] || ASSET_NAME=$(find_asset "$VERSION")
[ -n "$ASSET_NAME" ] || ASSET_NAME="$BIN_NAME-linux-$FILENAME-$VERSION.tar.gz"
case $ASSET_NAME in
  paqet-linux-*) BINARY_NAME="paqet_linux_$FILENAME" ;;
  *) BINARY_NAME="${BIN_NAME}_linux_$FILENAME" ;;
esac
DOWNLOAD_URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET_NAME"

say "${Y}Download${N}: $DOWNLOAD_URL"
TMP_DL=$(mktemp -d)
if ! curl -fL --connect-timeout 15 -o "$TMP_DL/$ASSET_NAME" "$DOWNLOAD_URL"; then
  bad "Download failed."
  say "  $DOWNLOAD_URL"
  rm -rf "$TMP_DL"
  exit 1
fi
tar -xzf "$TMP_DL/$ASSET_NAME" -C "$TMP_DL" || { bad "Not a valid archive."; rm -rf "$TMP_DL"; exit 1; }
if [ ! -f "$TMP_DL/$BINARY_NAME" ]; then
  bad "The archive did not contain $BINARY_NAME."
  ls -l "$TMP_DL"
  rm -rf "$TMP_DL"
  exit 1
fi
install -m 0755 "$TMP_DL/$BINARY_NAME" "$INSTALL_DIR/$BIN_NAME" \
  || { bad "Could not install the binary to $INSTALL_DIR."; rm -rf "$TMP_DL"; exit 1; }
rm -rf "$TMP_DL"
good "Installed $INSTALL_DIR/$BIN_NAME ($VERSION)"

# The binary must at least report its version, or the wizard below would fail later.
if ! "$INSTALL_DIR/$BIN_NAME" version >/dev/null 2>&1; then
  say "${Y}Warning${N}: '$BIN_NAME version' did not run cleanly (continuing anyway)."
fi

if [ "$UPDATE" = 1 ]; then
  say ""
  say "${Y}Config${N}: kept the existing $CONFIG_FILE."
else
  say ""
  say "${Y}Configuration${N}: the wizard writes $CONFIG_FILE, both servers must agree on it."
  say "${D}Run the wizard on the other server too; type the same values there.${N}"
  say "   1) Server (exit node, the destination)"
  say "   2) Client (entry node, the machine you connect from)"
  ask "Role [1/2] (Enter = 1, server): " 1
  case $REPLY in
    2 | [cC]*) ROLE=client ;;
    *) ROLE=server ;;
  esac

  # Raw packets need the interface, its IP, and the gateway MAC (the next hop they are sent to).
  AUTO_IFACE=$(ip route | awk '/^default/ { print $5; exit }')
  AUTO_GW='' AUTO_IP='' AUTO_MAC=''
  if [ -z "$AUTO_IFACE" ]; then
    bad "No default route found: enter the network details by hand."
  else
    AUTO_GW=$(ip route | awk '/^default/ { print $3; exit }')
    AUTO_IP=$(ip -4 -o addr show "$AUTO_IFACE" | awk '{ split($4, a, "/"); print a[1]; exit }')
    AUTO_MAC=$(ip neigh show "$AUTO_GW" 2>/dev/null | awk -v d="$AUTO_IFACE" 'index($0, d) { print $5; exit }')
    if [ -z "$AUTO_MAC" ]; then
      ping -c 1 -W 1 "$AUTO_GW" >/dev/null 2>&1 || true   # populates the ARP table
      AUTO_MAC=$(ip neigh show "$AUTO_GW" 2>/dev/null | awk -v d="$AUTO_IFACE" 'index($0, d) { print $5; exit }')
    fi
    say ""
    say "${Y}Network${N}: found on this server"
    say "  Interface:   $AUTO_IFACE"
    say "  Local IP:    $AUTO_IP"
    say "  Gateway MAC: $AUTO_MAC"
  fi
  if [ -z "$AUTO_IFACE" ] || ! yes_no "Use these settings? [Y/n] " y; then
    ask "Interface: " "$AUTO_IFACE"
    CFG_IFACE=$REPLY
    ask "Local IP (without port): " "$AUTO_IP"
    CFG_IP=$REPLY
    ask "Gateway MAC: " "$AUTO_MAC"
    CFG_MAC=$REPLY
  else
    CFG_IFACE=$AUTO_IFACE
    CFG_IP=$AUTO_IP
    CFG_MAC=$AUTO_MAC
  fi
  # Empty values would write a dead config: keep asking until all three are set.
  while [ -z "$CFG_IFACE" ] || [ -z "$CFG_IP" ] || [ -z "$CFG_MAC" ]; do
    bad "Network details incomplete; fill the missing field(s)."
    [ -n "$CFG_IFACE" ] || { ask "Interface: "; CFG_IFACE=$REPLY; }
    [ -n "$CFG_IP" ] || { ask "Local IP (without port): "; CFG_IP=$REPLY; }
    [ -n "$CFG_MAC" ] || { ask "Gateway MAC: "; CFG_MAC=$REPLY; }
  done

  say ""
  say "${Y}Secret key${N}: both servers must use the same one."
  ask "Key (Enter = generate): "
  if [ -z "$REPLY" ]; then
    CFG_KEY=$("$INSTALL_DIR/$BIN_NAME" secret)
    good "Generated key: $CFG_KEY"
    bad "Save this key: the other server must use it."
  else
    CFG_KEY=$REPLY
  fi

  say ""
  say "${Y}KCP mode${N}: 'normal' suits unstable or lossy paths; 'fast' is more aggressive."
  ask "Mode [normal/fast/fast2/fast3/manual] (Enter = normal): " normal
  KCP_MODE=$REPLY
  ask "MTU (Enter = 1250; lower it when large packets get dropped): " 1250
  KCP_MTU=$REPLY

  # sndwnd/rcvwnd tune for concurrency, not for one big download: the window is per-conn but the
  # path is shared, so `conn` multiplies what is in flight (8 conns of 4096 segs is a 33x BDP
  # overshoot — the over-admission behind the alpha.32 loss regression). Measured over a 37ms
  # path, 64 streams: 4096 -> 230 Mbit/s unstable; 1024 -> 258 Mbit/s, stable, ~10x more
  # consistent, ~100MB less server RSS (single-stream: 89 Mbit/s). Drop to 512 on lossy paths;
  # raise toward 4096 for few users pulling big files. smuxbuf/streambuf are a throughput ceiling
  # (streambuf/RTT per stream): 4MB measured within noise, so it stays. All four are overridable
  # from the environment.
  KCP_SNDWND=${KCP_SNDWND:-1024}
  KCP_RCVWND=${KCP_RCVWND:-1024}
  KCP_SMUXBUF=${KCP_SMUXBUF:-16777216}
  KCP_STREAMBUF=${KCP_STREAMBUF:-4194304}

  say ""
  say "${Y}Encryption${N}: both sides must pick the same."
  say "  aes-128-gcm                   AES-GCM: encrypts and detects tampering (default)"
  say "  salsa20                       Salsa20 stream cipher, no padding; does not detect tampering"
  say "  xor                           XOR, minimal CPU, basic header obfuscation"
  say "  none                          no encryption; use when the outer tunnel already encrypts"
  say "  aes, aes-128, aes-192         AES, hardware-accelerated"
  say "  sm4, blowfish, twofish, cast5, 3des, tea, xtea, null"
  ask "Encryption (Enter = aes-128-gcm): " aes-128-gcm
  KCP_BLOCK=$REPLY

  if yes_no "FEC: 10% overhead, recovers packet loss (recommended only on lossy links) [y/N] " n; then
    KCP_DSHARD=10 KCP_PSHARD=1
    good "FEC on (dshard=10, pshard=1)"
  else
    KCP_DSHARD=0 KCP_PSHARD=0
    good "FEC off"
  fi

  if [ "$ROLE" = server ]; then
    ask "Listen port (Enter = 9999): " 9999
    PORT=$REPLY
    say ""
    say "${Y}Config${N}: writing $CONFIG_FILE"
    cat > "$CONFIG_FILE" <<EOF
role: "server"
log:
  level: "info"
network:
  interface: "$CFG_IFACE"
  ipv4:
    addr: "$CFG_IP:$PORT"
    router_mac: "$CFG_MAC"
  pcap:
    sockbuf: 16777216
transport:
  protocol: "kcp"
  conn: 8
  kcp:
    mode: "$KCP_MODE"
    mtu: $KCP_MTU
    key: "$CFG_KEY"
    block: "$KCP_BLOCK"
    sndwnd: $KCP_SNDWND
    rcvwnd: $KCP_RCVWND
    smuxbuf: $KCP_SMUXBUF
    streambuf: $KCP_STREAMBUF
    dshard: $KCP_DSHARD
    pshard: $KCP_PSHARD
EOF
    chmod 600 "$CONFIG_FILE"
    good "Config written."

    # Kernel UDP buffers for the raw pcap path; written once and kept on later runs.
    if [ ! -f /etc/sysctl.d/99-paqet-plus.conf ]; then
      say "${Y}System tuning${N}: raising the kernel UDP buffers to 32 MB."
      cat > /etc/sysctl.d/99-paqet-plus.conf <<EOF
net.core.rmem_max=33554432
net.core.wmem_max=33554432
net.core.rmem_default=33554432
net.core.wmem_default=33554432
net.core.netdev_max_backlog=5000
EOF
      sysctl -p /etc/sysctl.d/99-paqet-plus.conf >/dev/null
      good "Applied /etc/sysctl.d/99-paqet-plus.conf"
    fi

    # The kernel must not track or reset packets on the paqet-plus port: without these rules it fights the
    # raw path with RSTs and conntrack.
    say "${Y}Firewall${N}: NOTRACK and RST suppression on TCP $PORT."
    if ! iptables -t raw -C PREROUTING -p tcp --dport "$PORT" -j NOTRACK 2>/dev/null; then
      iptables -t raw -A PREROUTING -p tcp --dport "$PORT" -j NOTRACK
      good "raw PREROUTING NOTRACK"
    fi
    if ! iptables -t raw -C OUTPUT -p tcp --sport "$PORT" -j NOTRACK 2>/dev/null; then
      iptables -t raw -A OUTPUT -p tcp --sport "$PORT" -j NOTRACK
      good "raw OUTPUT NOTRACK"
    fi
    if ! iptables -t mangle -C OUTPUT -p tcp --sport "$PORT" --tcp-flags RST RST -j DROP 2>/dev/null; then
      iptables -t mangle -A OUTPUT -p tcp --sport "$PORT" --tcp-flags RST RST -j DROP
      good "mangle OUTPUT DROP RST"
    fi
    # Preseed debconf: iptables-persistent otherwise prompts interactively and hangs `curl | sudo bash`.
    echo "iptables-persistent iptables-persistent/autosave_v4 boolean true" | debconf-set-selections
    echo "iptables-persistent iptables-persistent/autosave_v6 boolean true" | debconf-set-selections
    if DEBIAN_FRONTEND=noninteractive apt-get install -y -qq iptables-persistent; then
      netfilter-persistent save >/dev/null
      good "Rules saved (iptables-persistent)"
    else
      bad "Could not install iptables-persistent; save the rules by hand."
    fi
  else
    ask "Remote server IP: "
    SERVER_IP=$REPLY
    ask "Remote server port (Enter = 9999): " 9999
    SERVER_PORT=$REPLY

    say ""
    say "${Y}Client mode${N}"
    say "   1) SOCKS5 proxy"
    say "   2) Port forwarding"
    ask "> " 1
    CLIENT_MODE=$REPLY

    say "${Y}Config${N}: writing $CONFIG_FILE"
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
  conn: 8
  kcp:
    mode: "$KCP_MODE"
    mtu: $KCP_MTU
    key: "$CFG_KEY"
    block: "$KCP_BLOCK"
    sndwnd: $KCP_SNDWND
    rcvwnd: $KCP_RCVWND
    smuxbuf: $KCP_SMUXBUF
    streambuf: $KCP_STREAMBUF
    dshard: $KCP_DSHARD
    pshard: $KCP_PSHARD
EOF

    if [ "$CLIENT_MODE" = 1 ]; then
      ask "SOCKS5 listen address (Enter = 0.0.0.0:1080): " "0.0.0.0:1080"
      SOCKS_ADDR=$REPLY
      cat >> "$CONFIG_FILE" <<EOF
socks5:
  - listen: "$SOCKS_ADDR"
EOF
      good "SOCKS5 on $SOCKS_ADDR"
    else
      echo "forward:" >> "$CONFIG_FILE"
      while :; do
        say ""
        say "${Y}Forwarding rule${N}: '0.0.0.0:8080' binds one address; '8080 8081' maps each port 1:1."
        ask "Ports (Enter = done): "
        INPUT_VAL=$REPLY
        [ -n "$INPUT_VAL" ] || break
        if [[ $INPUT_VAL =~ [[:space:]] ]]; then
          # Bulk 1:1 mapping: the listen and the target ports are the same.
          TGT_IP=127.0.0.1
          ask "Protocol [tcp/udp] (Enter = tcp): " tcp
          FWD_PROTO=$REPLY
          for FWD_PORT in $INPUT_VAL; do
            cat >> "$CONFIG_FILE" <<EOF
  - listen: "0.0.0.0:$FWD_PORT"
    target: "$TGT_IP:$FWD_PORT"
    protocol: "$FWD_PROTO"
EOF
            good "0.0.0.0:$FWD_PORT -> $TGT_IP:$FWD_PORT"
          done
        else
          if [[ $INPUT_VAL == *:* ]]; then
            LISTEN_ADDR=$INPUT_VAL
            ask "Target (e.g. 127.0.0.1:80): "
            TARGET_ADDR=$REPLY
          else
            LISTEN_ADDR="0.0.0.0:$INPUT_VAL"
            ask "Target IP for port $INPUT_VAL (e.g. 127.0.0.1): "
            TGT_IP=$REPLY
            TARGET_ADDR="$TGT_IP:$INPUT_VAL"
          fi
          ask "Protocol [tcp/udp] (Enter = tcp): " tcp
          FWD_PROTO=$REPLY
          cat >> "$CONFIG_FILE" <<EOF
  - listen: "$LISTEN_ADDR"
    target: "$TARGET_ADDR"
    protocol: "$FWD_PROTO"
EOF
          good "$LISTEN_ADDR -> $TARGET_ADDR"
        fi
        yes_no "Add more rules? [y/N] " n || break
      done
    fi
    chmod 600 "$CONFIG_FILE"
    good "Config written."
  fi
fi

say ""
say "${Y}Control panel${N}: installing paqet-plus-ctl..."
HERE=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd)
if [ -n "$HERE" ] && [ -f "$HERE/manage-paqet-plus.sh" ]; then
  install -m 0755 "$HERE/manage-paqet-plus.sh" "$SELF"
  good "Control panel installed: paqet-plus-ctl"
elif curl -fsSL --connect-timeout 15 "https://raw.githubusercontent.com/$REPO/$BRANCH/manage-paqet-plus.sh" -o "$SELF.tmp"; then
  install -m 0755 "$SELF.tmp" "$SELF"
  rm -f "$SELF.tmp"
  good "Control panel installed: paqet-plus-ctl"
else
  rm -f "$SELF.tmp"
  say "${Y}Warning${N}: could not fetch paqet-plus-ctl (the panel will be missing)."
fi

# In update mode nobody set ROLE: read it back from the config for the unit description.
if [ -z "$ROLE" ]; then
  if grep -q 'role: "server"' "$CONFIG_FILE" 2>/dev/null; then ROLE=server; else ROLE=client; fi
fi

say "${Y}Service${N}: writing /etc/systemd/system/$SERVICE_NAME.service"
cat > "/etc/systemd/system/$SERVICE_NAME.service" <<EOF
[Unit]
Description=Paqet Plus Proxy Service ($ROLE)
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
systemctl enable --now "$SERVICE_NAME" >/dev/null 2>&1
sleep 2
if systemctl is-active -q "$SERVICE_NAME"; then
  good "Service is running (it starts on every boot too)."
else
  bad "Service failed to start. Recent logs:"
  journalctl -u "$SERVICE_NAME" -n 30 --no-pager
  say "${D}Fix the config, then: systemctl restart $SERVICE_NAME${N}"
fi

say ""
good "paqet-plus installed."
if [ "$ROLE" = server ]; then
  say "  Open TCP ${PORT:-9999} in your provider's firewall."
fi
if [ -x "$SELF" ]; then
  say "  Manage it any time with: sudo paqet-plus-ctl"
  say ""
  say "Starting the control panel..."
  sleep 1
  exec "$SELF"
fi
