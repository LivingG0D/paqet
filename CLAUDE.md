# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

paqet is a bidirectional, packet-level proxy that tunnels **KCP over raw TCP packets** crafted with **libpcap**, bypassing the kernel TCP stack to evade DPI/firewalls. This repo (`LivingG0D/paqet`) is a fork of `hanselime/paqet` with added DPI-evasion tuning, an installer/control-panel, and KCP autotuning.

## Build / Test / Run

**CGO + libpcap are mandatory** — the pcap path won't compile or link without them. Pure-Go (`CGO_ENABLED=0`) builds fail.

```bash
# Build (needs Go 1.25+, libpcap-dev, a C compiler)
CGO_ENABLED=1 go build -o paqet ./cmd

# Run — requires root (raw socket / pcap access)
sudo ./paqet run -c config.yaml

# Test (CGO required; socket pkg pulls in pcap)
CGO_ENABLED=1 go test ./...
go test ./internal/protocol/                              # single package
go test ./internal/protocol/ -run TestProtoRoundTripTCP   # single test
CGO_ENABLED=1 go test -race ./internal/socket/            # race-sensitive packet crafting
go vet ./...
```

- Only `internal/protocol` and `internal/socket` carry tests; everything else is exercised via the binary.
- **Windows hosts cannot build directly** (CGO+libpcap, cross-compile is painful). Build on Linux (native, WSL, or Docker). Windows uses Npcap + MinGW.
- **Release binaries** are static, multi-arch (linux amd64/arm64/arm32/mips*, windows, darwin) built by `.github/workflows/build.yml`: it compiles a **static libpcap with musl**, links via `-linkmode external -extldflags '-static'`, and injects build metadata with `-ldflags "-X 'paqet/cmd/version.Version=...'"` (also `GitCommit`, `GitTag`, `BuildTime`). Reproduce that recipe for production binaries, not a plain `go build`.

## Server runtime requirement (non-obvious, breaks silently)

Because paqet injects/reads raw TCP frames, the kernel's own stack will RST or mangle the connection. The **server must** have these `iptables` rules or it will not work:

```bash
sudo iptables -t raw    -A PREROUTING -p tcp --dport <port> -j NOTRACK
sudo iptables -t raw    -A OUTPUT     -p tcp --sport <port> -j NOTRACK
sudo iptables -t mangle -A OUTPUT     -p tcp --sport <port> --tcp-flags RST RST -j DROP
```

`setup-paqet.sh` applies these automatically. If raw traffic "connects but no data flows," check these first.

## Architecture — the transport stack

A connection is assembled bottom-up; understanding paqet means following this layering across packages:

1. **`internal/socket`** — `PacketConn` (implements `net.PacketConn`) backed by pcap. `send_handle.go` crafts raw TCP frames (randomized MSS/TTL/window/timestamps for fingerprint evasion); `recv_handle.go` reads them via a BPF filter (`tcp and dst port N`) using a zero-alloc `DecodingLayerParser`. This is the disguised "wire."
2. **`internal/tnet/kcp`** — wraps `PacketConn` in KCP (`xtaci/kcp-go`, with encryption `cfg.Block` + optional FEC `dshard`/`pshard`), then layers `smux` stream multiplexing on top. `dial.go` = client side, `listen.go` = server side, `kcp.go` = per-mode KCP tuning (`aplConf`) + smux config (`smuxConf`), `autotune.go` = adaptive window sizing.
3. **`internal/tnet`** — transport-agnostic abstractions: `Conn`, `Listener`, `Addr`, `strm`.
4. **`internal/protocol`** — per-smux-stream framing: a type byte (`PPING`/`PPONG`/`PTCPF`/`PTCP`/`PUDP`) + target `Addr` + TCP-flag config. This is how each stream declares what it carries.
5. **`internal/server`** — accepts smux streams, reads the protocol header, dials the real target (`tcp.go`/`udp.go` via `DialContext`), and pipes bytes. `ping.go` answers `PPING`.
6. **`internal/client`** — opens smux streams, writes the protocol header, bridges ingress traffic. `timed_conn.go` owns the `PacketConn`+KCP dial lifecycle and reconnection (`ticker.go`); `udp_pool.go` pools UDP sessions.
7. **Ingress (client side):** `internal/socks` (SOCKS5 server, dynamic forwarding) and/or `internal/forward` (static port forwarding). Both feed into the client streams above.

`cmd/run` is the entry: `conf.LoadFromFile` → `setDefaults`/`validate` → branch on `cfg.Role` (`client`|`server`). Other subcommands: `secret` (keygen), `iface` (list NICs), `ping` (connectivity test), `dump` (pcap debug), `version`.

## Configuration

`internal/conf` loads YAML into per-section structs (`network`, `pcap`, `kcp`, `transport`, `socks`, `forward`, `server`, `listen`, `log`), each with `setDefaults` + `validate`. Annotated examples live in `example/`. Fork defaults are DPI-tuned: `salsa20` cipher, smux keepalive 30s/90s, FEC off (`dshard:10 pshard:1` when enabled), conservative buffers. KCP `block` cipher and `key` must match on both ends; FEC settings must match too (they change the packet format).

## Fork specifics & upstream sync

- `setup-paqet.sh` — one-line installer (downloads release, systemd unit, iptables, installs `paqet-ctl`, migrates from legacy `paqet2`).
- `manage-paqet.sh` — the `paqet-ctl` control panel (logs, config edit, KCP/MTU/conn/buffer/cipher tuning, service control, sysctl optimizations).
- Upstream remote is `hanselime/paqet`. To pull upstream changes: `git fetch upstream && git merge upstream/master`, resolving conflicts to **keep the fork's DPI/perf/tuning work**, then verify with the build+test commands above.
