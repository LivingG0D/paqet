# paqet

A bidirectional packet-level proxy that tunnels KCP over raw TCP packets with encryption. Built for environments where standard VPN protocols are blocked or throttled.

## How It Works

paqet operates at the packet level using **libpcap** to craft and inject raw TCP frames directly onto the wire, bypassing the kernel's TCP stack. Traffic is multiplexed over [KCP](https://github.com/xtaci/kcp-go) — a fast, reliable ARQ protocol — and wrapped inside what appears to be normal TCP traffic. This approach avoids the signatures that firewalls and DPI systems use to detect tunneling protocols.

```
Client                                    Server
┌────────────┐    raw TCP packets    ┌────────────┐
│ SOCKS5     │ ─────────────────────>│            │
│ or         │    KCP + encryption   │  Internet  │
│ Port Fwd   │ <─────────────────────│  Gateway   │
└────────────┘    via libpcap        └────────────┘
```

## Features

- **Raw packet transport** — Crafts TCP packets via pcap, invisible to kernel connection tracking
- **KCP protocol** — Low-latency reliable transport with tunable aggressiveness
- **Encryption** — AES, Salsa20, Blowfish, Twofish, XOR, SM4, and more
- **SOCKS5 proxy** — Dynamic forwarding with optional authentication
- **Port forwarding** — Map remote TCP/UDP ports to local
- **Multi-connection** — Configurable connection pooling (1–256)
- **Cross-platform** — Linux (amd64, arm64, arm32), Windows, macOS
- **Firewall evasion** — RST suppression via iptables, NOTRACK rules

## Quick Install (Linux)

One-line installer that downloads the latest release, configures the service, and sets up firewall rules:

```bash
curl -fsSL https://raw.githubusercontent.com/LivingG0D/paqet/master/setup-paqet.sh | sudo bash
```

This will:
1. Install dependencies (`libpcap-dev`, `iproute2`, `curl`, `jq`)
2. Download the latest release binary
3. Run an interactive configuration wizard (server or client)
4. Create a systemd service
5. Apply iptables rules (server mode)
6. Install the `paqet-ctl` management tool

### Management

After installation, use the control panel:

```bash
sudo paqet-ctl
```

Options include: view logs, edit config, tune performance (KCP mode, MTU, connections, buffer size, encryption), restart/stop service, update, and apply system optimizations.

## Manual Installation

### Prerequisites

- Go 1.25+
- libpcap development headers (`libpcap-dev` on Debian/Ubuntu)
- Root/admin privileges (required for raw socket access)

### Build from Source

```bash
git clone https://github.com/LivingG0D/paqet.git
cd paqet
CGO_ENABLED=1 go build -o paqet ./cmd
```

### Run

```bash
sudo ./paqet run -c config.yaml
```

## Configuration

Configuration uses YAML. See the [`example/`](example/) directory for complete annotated examples.

### Server

```yaml
role: "server"
log:
  level: "info"
listen:
  addr: ":9999"
network:
  interface: "eth0"
  ipv4:
    addr: "10.0.0.100:9999"
    router_mac: "aa:bb:cc:dd:ee:ff"
  pcap:
    sockbuf: 8388608           # 8MB buffer (bytes)
transport:
  protocol: "kcp"
  conn: 1
  kcp:
    mode: "fast"               # normal, fast, fast2, fast3, manual
    mtu: 1350
    key: "your-secret-key"
    block: "aes"               # aes, xor, salsa20, blowfish, none, etc.
```

### Client

```yaml
role: "client"
log:
  level: "info"
socks5:
  - listen: "127.0.0.1:1080"
network:
  interface: "en0"
  ipv4:
    addr: "192.168.1.100:0"    # port 0 = random
    router_mac: "aa:bb:cc:dd:ee:ff"
server:
  addr: "10.0.0.100:9999"
transport:
  protocol: "kcp"
  conn: 1
  kcp:
    mode: "fast"
    key: "your-secret-key"
    block: "aes"
```

### Server Firewall (Required)

The server **must** have these iptables rules to prevent kernel interference with raw packets:

```bash
sudo iptables -t raw -A PREROUTING -p tcp --dport 9999 -j NOTRACK
sudo iptables -t raw -A OUTPUT -p tcp --sport 9999 -j NOTRACK
sudo iptables -t mangle -A OUTPUT -p tcp --sport 9999 --tcp-flags RST RST -j DROP
```

Replace `9999` with your listen port.

## CLI Commands

| Command | Description |
|---------|-------------|
| `paqet run -c config.yaml` | Start the proxy (server or client) |
| `paqet secret` | Generate a random encryption key |
| `paqet iface` | List available network interfaces |
| `paqet ping` | Connectivity test |
| `paqet dump` | Packet capture debug tool |
| `paqet version` | Show version and build info |

## KCP Modes

| Mode | Latency | Bandwidth | Use Case |
|------|---------|-----------|----------|
| `normal` | Higher | Lower overhead | Stable networks, bandwidth-sensitive |
| `fast` | Low | Moderate | General purpose (default) |
| `fast2` | Lower | Higher overhead | Real-time applications |
| `fast3` | Lowest | Highest overhead | Ultra-low-latency |
| `manual` | Custom | Custom | Full control over KCP parameters |

## Encryption Options

`aes` · `aes-128` · `aes-128-gcm` · `aes-192` · `salsa20` · `blowfish` · `twofish` · `cast5` · `3des` · `tea` · `xtea` · `xor` · `sm4` · `none`

> **Tip:** Use `xor` for minimal CPU overhead with basic obfuscation, or `aes` for strong encryption.

## Upgrading from paqet2

Servers running the older `paqet2` can upgrade in-place. Run the installer and select **Option 1 (Update & Optimize)**:

```bash
curl -fsSL https://raw.githubusercontent.com/LivingG0D/paqet/master/setup-paqet.sh | sudo bash
```

The migration automatically handles:
- Remapping removed KCP modes (`turbo` → `fast2`, `eco` → `normal`)
- Moving top-level `pcap:` config under `network:` 
- Converting `buffer_mb` to `sockbuf` (bytes)
- Cleaning up deprecated config keys

## License

[MIT](LICENSE)
