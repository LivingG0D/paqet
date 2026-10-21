package socket

import (
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"

	"paqet/internal/conf"
)

// frameReader returns the next Ethernet frame, valid until the following call:
// a pcap handle, or on Linux a raw packet socket (handle_linux.go).
type frameReader interface {
	ReadFrame() ([]byte, error)
	SetReadDeadline(t time.Time) error
	Close()
}

type decoder struct {
	parser  *gopacket.DecodingLayerParser
	eth     layers.Ethernet
	ip4     layers.IPv4
	ip6     layers.IPv6
	tcp     layers.TCP
	decoded []gopacket.LayerType
}

type RecvHandle struct {
	handle frameReader
	mu     sync.Mutex // serializes Read: the frame and dec are reused for every packet
	dec    decoder
	src    *net.UDPAddr // last sender, guarded by mu
}

func recvFilter(port int) string {
	return fmt.Sprintf("tcp and dst port %d", port)
}

func NewRecvHandle(cfg *conf.Network) (*RecvHandle, error) {
	handle, err := openRecvHandle(cfg)
	if err != nil {
		return nil, err
	}

	h := &RecvHandle{handle: handle}
	h.dec.decoded = make([]gopacket.LayerType, 0, 4)
	h.dec.parser = gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, &h.dec.eth, &h.dec.ip4, &h.dec.ip6, &h.dec.tcp)
	h.dec.parser.IgnoreUnsupported = true
	return h, nil
}

func (h *RecvHandle) Read(data []byte) (int, net.Addr, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	frame, err := h.handle.ReadFrame()
	if err != nil {
		return 0, nil, err
	}

	d := &h.dec
	if err := d.parser.DecodeLayers(frame, &d.decoded); err != nil {
		return 0, nil, errNoPayload
	}

	var srcIP net.IP
	var srcPort int
	var payload []byte
	for _, t := range d.decoded {
		switch t {
		case layers.LayerTypeIPv4:
			srcIP = d.ip4.SrcIP
		case layers.LayerTypeIPv6:
			srcIP = d.ip6.SrcIP
		case layers.LayerTypeTCP:
			srcPort = int(d.tcp.SrcPort)
			payload = d.tcp.Payload
		}
	}

	if srcIP == nil || len(payload) == 0 {
		return 0, nil, errNoPayload
	}

	// Hand out the previous address while the sender is unchanged: callers only
	// read it, and a client receives everything from one server. srcIP points
	// into the frame buffer, so a new address needs its own copy.
	if h.src == nil || h.src.Port != srcPort || !h.src.IP.Equal(srcIP) {
		h.src = &net.UDPAddr{IP: slices.Clone(srcIP), Port: srcPort}
	}
	return copy(data, payload), h.src, nil
}

func (h *RecvHandle) SetReadDeadline(t time.Time) error {
	return h.handle.SetReadDeadline(t)
}

// Close does not take mu: a Read may be blocked holding it, and closing the
// handle is what wakes that Read.
func (h *RecvHandle) Close() {
	h.handle.Close()
}
