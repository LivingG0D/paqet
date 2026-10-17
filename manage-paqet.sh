#!/usr/bin/env bash
# paqet-ctl — control panel for paqet (interactive menu).
#
# Installed by setup-paqet.sh as /usr/local/bin/paqet-ctl. Run as root:
#   sudo paqet-ctl
set -uo pipefail

REPO=LivingG0D/paqet
BRANCH="${PAQET_BRANCH:-master}"
INSTALL_DIR=/opt/paqet
BIN="$INSTALL_DIR/paqet"
CONFIG_FILE="$INSTALL_DIR/config.yaml"
SERVICE_NAME=paqet

if [ -t 1 ]; then Y=$'\e[1;33m' G=$'\e[32m' R=$'\e[31m' D=$'\e[2m' N=$'\e[0m'; else Y='' G='' R='' D='' N=''; fi

say()  { printf '%s\n' "$*"; }
good() { printf '%s✓%s %s\n' "$G" "$N" "$*"; }
bad()  { printf '%s✗%s %s\n' "$R" "$N" "$*"; }

# Read one answer into REPLY (default when empty). /dev/tty, so the panel also opens right after a
# `curl | bash` install, whose stdin is the installer script.
ask() { # prompt [default]
  read -r -p "$1" REPLY < /dev/tty || { echo; exit 0; }
  REPLY="${REPLY:-${2:-}}"
}
yes_no() { # prompt default(y|n)
  ask "$1" "$2"
  [[ $REPLY == [yY]* ]]
}

# Ctrl+C stops the followed stream, not this menu (a handler, not ignore: an ignored SIGINT would be
# inherited by the command too).
interruptible() { # command...
  trap ':' INT
  "$@"
  trap - INT
}

installed_ver() {
  local v=''
  if [ -x "$BIN" ]; then
    v=$("$BIN" version 2>/dev/null | sed -n 's/^Version:[[:space:]]*//p' | head -n1)
  fi
  echo "${v:-unknown}"
}
show_header() {
  local status
  if systemctl is-active -q "$SERVICE_NAME"; then status="${G}running${N}"; else status="${R}stopped${N}"; fi
  say ""
  say "${Y}=== paqet ===${N}  service: $status, version: $(installed_ver), config: $CONFIG_FILE"
}

view_logs() { # journalctl -f: everything the service prints
  say "${D}Live logs (last 50, following). Press Ctrl+C to go back.${N}"
  interruptible journalctl -u "$SERVICE_NAME" -n 50 -f
}
view_stats() { # the [STATS] lines the binary emits
  say "${D}Live stats: [STATS] lines only. Press Ctrl+C to go back.${N}"
  interruptible journalctl -u "$SERVICE_NAME" -n 200 -f -g '\[STATS\]'
}
view_alerts() { # [ALERT] lines: bottleneck detection, with a summary by type
  local recent count
  say ""
  say "${Y}Alerts${N}: bottleneck detection over the last 500 log lines."
  recent=$(journalctl -u "$SERVICE_NAME" -n 500 --no-pager 2>/dev/null | grep '\[ALERT\]')
  if [ -z "$recent" ]; then
    good "No bottleneck alerts in recent logs."
    return 0
  fi
  count=$(printf '%s\n' "$recent" | wc -l)
  bad "$count alert(s) in recent logs:"
  printf '%s\n' "$recent" | tail -20
  say ""
  say "${Y}By type${N}"
  printf '%s\n' "$recent" | grep -oP 'BOTTLENECK: \K[a-z_]+' | sort | uniq -c | sort -rn | while read -r n t; do
    printf '  %-22s %s\n' "$t" "${R}${n}x${N}"
  done
  ask "Press Enter to return, or f to follow live alerts: "
  if [[ $REPLY == [fF]* ]]; then
    say "${D}Following live alerts. Press Ctrl+C to go back.${N}"
    interruptible journalctl -u "$SERVICE_NAME" -n 200 -f -g '\[ALERT\]'
  fi
}
view_diagnostics() { # one report: stats, alerts, service state
  local alerts since
  say ""
  say "${Y}Diagnostics${N}"
  say "  stats (last 4):"
  journalctl -u "$SERVICE_NAME" -n 200 --no-pager 2>/dev/null | grep '\[STATS\]' | tail -4
  alerts=$(journalctl -u "$SERVICE_NAME" -n 200 --no-pager 2>/dev/null | grep '\[ALERT\]' | tail -5)
  if [ -z "$alerts" ]; then
    good "  no alerts"
  else
    say "  alerts (last 5):"
    printf '%s\n' "$alerts"
  fi
  since=$(systemctl show "$SERVICE_NAME" --property=ActiveEnterTimestamp --value 2>/dev/null)
  if systemctl is-active -q "$SERVICE_NAME"; then
    good "  running since ${since:-unknown}"
  else
    bad "  stopped"
  fi
}

edit_config() { # open the config in an editor, then optionally restart
  say "${Y}Config editor${N}: $CONFIG_FILE"
  if command -v nano >/dev/null 2>&1; then nano "$CONFIG_FILE"; else vi "$CONFIG_FILE"; fi
  if yes_no "Restart the service to apply the changes? [Y/n] " y; then
    systemctl restart "$SERVICE_NAME" && good "Restarted"
  else
    say "Not restarted; the changes apply on the next start."
  fi
}
tune_performance() { # KCP mode, MTU, connection pool, pcap buffer, encryption; empty keeps the value
  local mode mtu conn buf crypt bytes
  say ""
  say "${Y}Tune${N}: empty answers keep the current value. Both servers must match."
  say "${D}Current:${N}"
  grep -E 'mode|mtu|sockbuf' "$CONFIG_FILE" || true
  ask "KCP mode [fast/normal/fast2/fast3/manual] (Enter = keep): "
  mode=$(printf '%s' "$REPLY" | tr -cd '[:alnum:]')
  ask "MTU [1200-1500] (Enter = keep): "
  mtu=$(printf '%s' "$REPLY" | tr -cd '[:digit:]')
  ask "Connections [e.g. 8] (Enter = keep): "
  conn=$(printf '%s' "$REPLY" | tr -cd '[:digit:]')
  ask "PCAP buffer (MB) [e.g. 16] (Enter = keep): "
  buf=$(printf '%s' "$REPLY" | tr -cd '[:digit:]')
  ask "Encryption [salsa20/xor/aes/...] (Enter = keep): "
  crypt=$(printf '%s' "$REPLY" | tr -cd '[:alnum:]')

  if [ -n "$mode" ]; then
    if grep -q 'mode:' "$CONFIG_FILE"; then
      sed -i "s/mode: \".*\"/mode: \"$mode\"/" "$CONFIG_FILE"
    else
      sed -i "/kcp:/a \  mode: \"$mode\"" "$CONFIG_FILE"
    fi
    good "Mode: $mode"
  fi
  if [ -n "$mtu" ]; then
    if grep -q 'mtu:' "$CONFIG_FILE"; then
      sed -i "s/mtu: [0-9]*/mtu: $mtu/" "$CONFIG_FILE"
    else
      sed -i "/kcp:/a \  mtu: $mtu" "$CONFIG_FILE"
    fi
    good "MTU: $mtu"
  fi
  if [ -n "$conn" ]; then
    if grep -q 'conn:' "$CONFIG_FILE"; then
      sed -i "s/conn: [0-9]*/conn: $conn/" "$CONFIG_FILE"
    elif grep -q 'transport:' "$CONFIG_FILE"; then
      sed -i "/transport:/a \  conn: $conn" "$CONFIG_FILE"
    else
      printf '%s\n' 'transport:' "  conn: $conn" >> "$CONFIG_FILE"
    fi
    good "Connections: $conn"
  fi
  if [ -n "$buf" ]; then
    bytes=$((buf * 1024 * 1024))
    if grep -q 'sockbuf:' "$CONFIG_FILE"; then
      sed -i "s/sockbuf: [0-9]*/sockbuf: $bytes/" "$CONFIG_FILE"
    elif ! grep -q 'pcap:' "$CONFIG_FILE"; then
      sed -i '/^network:/a \  pcap:\n    sockbuf: '"$bytes" "$CONFIG_FILE"
    else
      sed -i "/pcap:/a \    sockbuf: $bytes" "$CONFIG_FILE"
    fi
    good "PCAP buffer: ${buf} MB ($bytes bytes)"
  fi
  if [ -n "$crypt" ]; then
    if grep -q 'block:' "$CONFIG_FILE"; then
      sed -i "s/block: \".*\"/block: \"$crypt\"/" "$CONFIG_FILE"
    else
      sed -i "/kcp:/a \  block: \"$crypt\"" "$CONFIG_FILE"
    fi
    good "Encryption: $crypt"
  fi
  if [ -n "$mode" ] || [ -n "$mtu" ] || [ -n "$conn" ] || [ -n "$buf" ] || [ -n "$crypt" ]; then
    systemctl restart "$SERVICE_NAME" && good "Applied and restarted"
  else
    say "No changes."
  fi
}
apply_sysctl() { # raise the kernel UDP buffers (kept in /etc/sysctl.conf)
  say ""
  say "${Y}System tuning${N}: raises the kernel UDP buffers to 32 MB."
  if ! yes_no "Apply? [y/N] " n; then
    say "Cancelled."
    return 0
  fi
  if grep -q 'paqet optimizations' /etc/sysctl.conf; then
    good "Already present in /etc/sysctl.conf"
  else
    cat >> /etc/sysctl.conf <<EOF
# paqet optimizations
net.core.rmem_max=33554432
net.core.wmem_max=33554432
net.core.rmem_default=33554432
net.core.wmem_default=33554432
net.core.netdev_max_backlog=5000
EOF
    good "Added to /etc/sysctl.conf"
  fi
  sysctl -p >/dev/null
}
update_system() { # rerun the installer: it finds the install and takes the update path
  say ""
  say "${Y}Update${N}: downloads the latest setup-paqet.sh from $REPO ($BRANCH)."
  say "${D}The installer keeps the existing config.${N}"
  if yes_no "Continue? [y/N] " n; then
    curl -fsSL "https://raw.githubusercontent.com/$REPO/$BRANCH/setup-paqet.sh" | bash
    good "Update finished. Reloading the control panel..."
    exec "$0"
  else
    say "Cancelled."
  fi
}

main() {
  [ "$(id -u)" -eq 0 ] || { bad "Run as root: sudo paqet-ctl"; exit 1; }
  while :; do
    show_header
    say "   1) View logs"
    say "   2) View stats (live)"
    say "   3) View alerts (bottleneck detection)"
    say "   4) Diagnostics snapshot"
    say "   5) Edit configuration (ports, rules)"
    say "   6) Tune performance (mode, MTU, connections, buffer, encryption)"
    say "   7) Restart service"
    say "   8) Stop service"
    say "   9) Update system"
    say "  10) Apply system optimization (UDP buffers)"
    say "   0) Exit"
    ask "> "
    case $REPLY in
      1) view_logs ;;
      2) view_stats ;;
      3) view_alerts ;;
      4) view_diagnostics ;;
      5) edit_config ;;
      6) tune_performance ;;
      7) systemctl restart "$SERVICE_NAME" && good "Restarted" ;;
      8) systemctl stop "$SERVICE_NAME" && good "Stopped" ;;
      9) update_system ;;
      10) apply_sysctl ;;
      0 | q) exit 0 ;;
    esac
  done
}

# `curl … | bash` feeds the script on stdin where the menu needs the keyboard: refuse it and point at
# `bash <(curl …)`, which keeps the keyboard (the panel itself reads /dev/tty either way).
if [ -z "${BASH_SOURCE[0]:-}" ]; then
  bad "Run it as: bash <(curl -fsSL https://raw.githubusercontent.com/$REPO/$BRANCH/manage-paqet.sh)   (not curl | bash)"
  exit 1
elif [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
