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
	handle, err := newHandle(cfg)
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

func (h *RecvHandle) Read() ([]byte, net.Addr, error) {
	data, _, err := h.handle.ReadPacketData()
	if err != nil {
		return nil, nil, err
	}

	addr := &net.UDPAddr{}
	h.decoded = h.decoded[:0]

	if err := h.parser.DecodeLayers(data, &h.decoded); err != nil {
		// Partial decode is ok — we may still have the layers we need
	}

	for _, lt := range h.decoded {
		switch lt {
		case layers.LayerTypeIPv4:
			addr.IP = h.ip4.SrcIP
		case layers.LayerTypeIPv6:
			addr.IP = h.ip6.SrcIP
		case layers.LayerTypeTCP:
			addr.Port = int(h.tcp.SrcPort)
		}
	}

	payload := h.tcp.LayerPayload()
	if len(payload) == 0 {
		return nil, addr, nil
	}
	return payload, addr, nil
}

func (h *RecvHandle) Close() {
	if h.handle != nil {
		h.handle.Close()
	}
}
