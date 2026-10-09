package socket

import (
	"encoding/binary"
	"fmt"
	randv2 "math/rand/v2"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"

	"paqet/internal/conf"
	"paqet/internal/pkg/hash"
	"paqet/internal/pkg/iterator"
)

type tcpF struct {
	tcpF       iterator.Iterator[conf.TCPF]
	clientTCPF map[uint64]*iterator.Iterator[conf.TCPF]
	mu         sync.RWMutex
}

// encoder holds the reusable layer structs and option backing arrays for one
// in-flight packet. Each Write borrows its own encoder from SendHandle.ePool,
// so concurrent senders never share the timestamp bytes buildTCPHeader rewrites
// per packet.
type encoder struct {
	eth layers.Ethernet
	ip4 layers.IPv4
	ip6 layers.IPv6
	tcp layers.TCP

	opts [5]layers.TCPOption
	ts   [8]byte
	mss  [2]byte
	ws   [1]byte

	buf gopacket.SerializeBuffer
}

type SendHandle struct {
	handle      *pcap.Handle
	writeMu     sync.Mutex
	closed      bool // guarded by writeMu; set by Close, checked by sendPacket
	srcIPv4     net.IP
	srcIPv4RHWA net.HardwareAddr
	srcIPv6     net.IP
	srcIPv6RHWA net.HardwareAddr
	srcPort     uint16
	dscp        uint8
	time        uint32
	tsCounter   atomic.Uint32
	seqSeed     uint32
	tcpF        tcpF
	ePool       sync.Pool
}

// fastRandUint32 returns an unpredictable uint32 for per-packet fingerprint
// jitter (TTL/window/timestamp/seq). It uses math/rand/v2's top-level
// generator, which is safe for concurrent use (per-thread runtime state, no
// syscall, no lock). This matters because buildTCPHeader runs concurrently
// across many KCP/smux streams: a shared *rand.Rand is explicitly NOT safe for
// concurrent use (see math/rand/v2 docs) and races on the send path under
// `go test -race`.
func fastRandUint32() uint32 {
	return randv2.Uint32()
}

func NewSendHandle(cfg *conf.Network) (*SendHandle, error) {
	handle, err := newHandle(cfg, 256*1024, 128, pcap.BlockForever)
	if err != nil {
		return nil, fmt.Errorf("failed to open pcap handle: %w", err)
	}

	// SetDirection is not fully supported on Windows Npcap, so skip it
	if runtime.GOOS != "windows" {
		if err := handle.SetDirection(pcap.DirectionOut); err != nil {
			return nil, fmt.Errorf("failed to set pcap direction out: %v", err)
		}
	}

	if err := handle.SetBPFFilter("less 0"); err != nil {
		return nil, fmt.Errorf("failed to set BPF filter: %w", err)
	}

	sh := newSendHandle(cfg.Interface.HardwareAddr, uint16(cfg.Port), uint8(cfg.PCAP.DSCP))
	sh.handle = handle
	sh.tcpF.tcpF = iterator.Iterator[conf.TCPF]{Items: cfg.TCP.LF}
	if cfg.IPv4.Addr != nil {
		sh.srcIPv4 = cfg.IPv4.Addr.IP
		sh.srcIPv4RHWA = cfg.IPv4.Router
	}
	if cfg.IPv6.Addr != nil {
		sh.srcIPv6 = cfg.IPv6.Addr.IP
		sh.srcIPv6RHWA = cfg.IPv6.Router
	}
	return sh, nil
}

// newSendHandle builds everything but the pcap handle and addresses, so tests
// can exercise the packet-building path without a capture device.
func newSendHandle(srcMAC net.HardwareAddr, srcPort uint16, dscp uint8) *SendHandle {
	// MSS: randomize between 1360–1460 once per handle to avoid a static fingerprint
	mss := 1360 + uint16(fastRandUint32()%101)

	sh := &SendHandle{
		srcPort: srcPort,
		dscp:    dscp,
		time:    uint32(time.Now().UnixNano() / int64(time.Millisecond)),
		seqSeed: fastRandUint32(),
		tcpF:    tcpF{clientTCPF: make(map[uint64]*iterator.Iterator[conf.TCPF])},
	}
	sh.ePool.New = func() any {
		return &encoder{
			eth: layers.Ethernet{SrcMAC: srcMAC},
			mss: [2]byte{byte(mss >> 8), byte(mss)},
			ws:  [1]byte{8},
			buf: gopacket.NewSerializeBuffer(),
		}
	}
	return sh
}

func (h *SendHandle) buildIPv4Header(e *encoder, dstIP net.IP) {
	e.ip4 = layers.IPv4{
		Version: 4,
		IHL:     5,
		// TOS: DSCP value shifted left 2 bits (ECN=0), default 0 = normal traffic
		TOS: h.dscp << 2,
		// TTL: jitter 63–65 to avoid a static fingerprint
		TTL:      uint8(63 + fastRandUint32()%3),
		Flags:    layers.IPv4DontFragment,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    h.srcIPv4,
		DstIP:    dstIP,
	}
}

func (h *SendHandle) buildIPv6Header(e *encoder, dstIP net.IP) {
	e.ip6 = layers.IPv6{
		Version:      6,
		TrafficClass: h.dscp << 2,
		// HopLimit: jitter 63–65
		HopLimit:   uint8(63 + fastRandUint32()%3),
		NextHeader: layers.IPProtocolTCP,
		SrcIP:      h.srcIPv6,
		DstIP:      dstIP,
	}
}

func (h *SendHandle) buildTCPHeader(e *encoder, dstPort uint16, f conf.TCPF) {
	e.tcp = layers.TCP{
		SrcPort: layers.TCPPort(h.srcPort),
		DstPort: layers.TCPPort(dstPort),
		FIN:     f.FIN, SYN: f.SYN, RST: f.RST, PSH: f.PSH, ACK: f.ACK, URG: f.URG, ECE: f.ECE, CWR: f.CWR, NS: f.NS,
		// Window: randomize between 64240–65535 to avoid a static fingerprint
		Window: uint16(64240 + fastRandUint32()%1296),
	}

	counter := h.tsCounter.Add(1)
	// Timestamp: base + counter-derived value + random jitter (0–7ms)
	tsVal := h.time + (counter >> 3) + fastRandUint32()%8
	// Seq: seed-based + counter with jitter
	seq := h.seqSeed + counter*1461 + fastRandUint32()%64
	opts := e.opts[:0]
	if f.SYN {
		binary.BigEndian.PutUint32(e.ts[0:4], tsVal)
		binary.BigEndian.PutUint32(e.ts[4:8], 0)
		opts = append(opts,
			layers.TCPOption{OptionType: layers.TCPOptionKindMSS, OptionLength: 4, OptionData: e.mss[:]},
			layers.TCPOption{OptionType: layers.TCPOptionKindSACKPermitted, OptionLength: 2},
			layers.TCPOption{OptionType: layers.TCPOptionKindTimestamps, OptionLength: 10, OptionData: e.ts[:]},
			layers.TCPOption{OptionType: layers.TCPOptionKindNop},
			layers.TCPOption{OptionType: layers.TCPOptionKindWindowScale, OptionLength: 3, OptionData: e.ws[:]},
		)
		e.tcp.Seq = seq
		e.tcp.Ack = 0
		if f.ACK {
			e.tcp.Ack = e.tcp.Seq + 1
		}
	} else {
		tsEcr := tsVal - (counter%200 + 50) - fastRandUint32()%10
		binary.BigEndian.PutUint32(e.ts[0:4], tsVal)
		binary.BigEndian.PutUint32(e.ts[4:8], tsEcr)
		opts = append(opts,
			layers.TCPOption{OptionType: layers.TCPOptionKindNop},
			layers.TCPOption{OptionType: layers.TCPOptionKindNop},
			layers.TCPOption{OptionType: layers.TCPOptionKindTimestamps, OptionLength: 10, OptionData: e.ts[:]},
		)
		// Ack: realistic-looking lag behind seq, with jitter
		dataLen := fastRandUint32()%1400 + 100
		e.tcp.Seq = seq
		e.tcp.Ack = seq - dataLen + fastRandUint32()%32
	}
	e.tcp.Options = opts
}

func (h *SendHandle) Write(payload []byte, addr *net.UDPAddr) error {
	e := h.ePool.Get().(*encoder)
	defer func() {
		e.buf.Clear()
		h.ePool.Put(e)
	}()

	dstIP := addr.IP
	dstPort := uint16(addr.Port)

	h.buildTCPHeader(e, dstPort, h.getClientTCPF(dstIP, dstPort))

	var ipLayer gopacket.SerializableLayer
	if dstIP.To4() != nil {
		h.buildIPv4Header(e, dstIP)
		ipLayer = &e.ip4
		e.tcp.SetNetworkLayerForChecksum(&e.ip4)
		e.eth.DstMAC = h.srcIPv4RHWA
		e.eth.EthernetType = layers.EthernetTypeIPv4
	} else {
		h.buildIPv6Header(e, dstIP)
		ipLayer = &e.ip6
		e.tcp.SetNetworkLayerForChecksum(&e.ip6)
		e.eth.DstMAC = h.srcIPv6RHWA
		e.eth.EthernetType = layers.EthernetTypeIPv6
	}

	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(e.buf, opts, &e.eth, ipLayer, &e.tcp, gopacket.Payload(payload)); err != nil {
		return err
	}

	return h.sendPacket(e.buf.Bytes())
}

// sendPacket writes one serialized frame to the pcap handle.
//
// pcap_sendpacket is not guaranteed thread-safe, and gopacket's write path adds
// no protection of its own: Handle.WritePacketData calls pcap_sendpacket with
// the raw handle pointer, taking no lock and never checking whether the handle
// is still open. Its read path does both, which is why only sends need this.
//
// So writeMu serves two purposes here — it serializes concurrent sends, and it
// orders a send against Close, which frees that pointer. h.closed then rejects
// any send that arrives after the close, since the freed pointer would
// otherwise be dereferenced inside libpcap.
func (h *SendHandle) sendPacket(b []byte) error {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if h.closed {
		return net.ErrClosed
	}
	return h.handle.WritePacketData(b)
}

func (h *SendHandle) getClientTCPF(dstIP net.IP, dstPort uint16) conf.TCPF {
	h.tcpF.mu.RLock()
	defer h.tcpF.mu.RUnlock()
	if ff := h.tcpF.clientTCPF[hash.IPAddr(dstIP, dstPort)]; ff != nil {
		return ff.Next()
	}
	return h.tcpF.tcpF.Next()
}

func (h *SendHandle) setClientTCPF(addr net.Addr, f []conf.TCPF) {
	a, ok := addr.(*net.UDPAddr)
	if !ok {
		return
	}
	h.tcpF.mu.Lock()
	h.tcpF.clientTCPF[hash.IPAddr(a.IP, uint16(a.Port))] = &iterator.Iterator[conf.TCPF]{Items: f}
	h.tcpF.mu.Unlock()
}

func (h *SendHandle) deleteClientTCPF(addr net.Addr) {
	a, ok := addr.(*net.UDPAddr)
	if !ok {
		return
	}
	h.tcpF.mu.Lock()
	delete(h.tcpF.clientTCPF, hash.IPAddr(a.IP, uint16(a.Port)))
	h.tcpF.mu.Unlock()
}

// Close shuts down the pcap handle. It is safe to call concurrently with Write
// and safe to call more than once.
func (h *SendHandle) Close() {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	if h.handle != nil {
		h.handle.Close()
	}
}
