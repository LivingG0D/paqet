package socket

import (
	"fmt"
	"net"
	"paqet/internal/conf"
	"runtime"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
)

type RecvHandle struct {
	handle  *pcap.Handle
	eth     layers.Ethernet
	ip4     layers.IPv4
	ip6     layers.IPv6
	tcp     layers.TCP
	parser  *gopacket.DecodingLayerParser
	decoded []gopacket.LayerType
}

func NewRecvHandle(cfg *conf.Network) (*RecvHandle, error) {
	handle, err := newHandle(cfg, cfg.PCAP.Sockbuf)
	if err != nil {
		return nil, fmt.Errorf("failed to open pcap handle: %w", err)
	}

	// SetDirection is not fully supported on Windows Npcap, so skip it
	if runtime.GOOS != "windows" {
		if err := handle.SetDirection(pcap.DirectionIn); err != nil {
			return nil, fmt.Errorf("failed to set pcap direction in: %v", err)
		}
	}

	filter := fmt.Sprintf("tcp and dst port %d", cfg.Port)
	if err := handle.SetBPFFilter(filter); err != nil {
		return nil, fmt.Errorf("failed to set BPF filter: %w", err)
	}

	rh := &RecvHandle{
		handle:  handle,
		decoded: make([]gopacket.LayerType, 0, 4),
	}

	// Pre-allocate decoder — avoids per-packet allocations
	rh.parser = gopacket.NewDecodingLayerParser(
		layers.LayerTypeEthernet,
		&rh.eth, &rh.ip4, &rh.ip6, &rh.tcp,
	)
	rh.parser.IgnoreUnsupported = true

	return rh, nil
}

// Read copies the TCP payload of the next packet into data. Not safe for concurrent callers.
// ReadPacketData, not ZeroCopyReadPacketData: a zero-copy slice can be freed by a concurrent
// Close, and guarding it with a lock would stop Close from interrupting a blocked read.
func (h *RecvHandle) Read(data []byte) (int, net.Addr, error) {
	pkt, _, err := h.handle.ReadPacketData()
	if err != nil {
		return 0, nil, err
	}

	addr := &net.UDPAddr{}
	h.decoded = h.decoded[:0]
	_ = h.parser.DecodeLayers(pkt, &h.decoded) // partial decode is ok; we check which layers we got

	hasTCP := false
	for _, lt := range h.decoded {
		switch lt {
		case layers.LayerTypeIPv4:
			addr.IP = h.ip4.SrcIP
		case layers.LayerTypeIPv6:
			addr.IP = h.ip6.SrcIP
		case layers.LayerTypeTCP:
			addr.Port = int(h.tcp.SrcPort)
			hasTCP = true
		}
	}

	// Without a decoded TCP layer, h.tcp still holds the previous packet.
	if !hasTCP {
		return 0, addr, nil
	}
	return copy(data, h.tcp.LayerPayload()), addr, nil
}

func (h *RecvHandle) Close() {
	if h.handle != nil {
		h.handle.Close()
	}
}
