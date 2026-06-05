package socket

import (
	"encoding/binary"
	"fmt"
	randv2 "math/rand/v2"
	"net"
	"paqet/internal/conf"
	"paqet/internal/pkg/hash"
	"paqet/internal/pkg/iterator"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
)

type TCPF struct {
	tcpF       iterator.Iterator[conf.TCPF]
	clientTCPF map[uint64]*iterator.Iterator[conf.TCPF]
	mu         sync.RWMutex
}

type SendHandle struct {
	handle      *pcap.Handle
	srcIPv4     net.IP
	srcIPv4RHWA net.HardwareAddr
	srcIPv6     net.IP
	srcIPv6RHWA net.HardwareAddr
	srcPort     uint16
	dscp        uint8
	baseTime    uint32
	tsCounter   uint32
	seqSeed     uint32
	tcpF        TCPF
	ethPool     sync.Pool
	ipv4Pool    sync.Pool
	ipv6Pool    sync.Pool
	tcpPool     sync.Pool
	bufPool     sync.Pool
	optsPool    sync.Pool // *tcpOpts; per-packet TCP option slices (see tcpOpts)
}

// tcpOpts holds the TCP option slices for a single in-flight packet. Each
// packet borrows its own holder from SendHandle.optsPool so that concurrent
// senders never share the timestamp backing array that buildTCPHeader rewrites
// per packet. syn and ack are prebuilt once per holder; only ts changes.
type tcpOpts struct {
	syn []layers.TCPOption
	ack []layers.TCPOption
	ts  [8]byte // backs OptionData of the Timestamps option in syn[2]/ack[2]
}

// newTCPOpts builds an option holder. mss is the per-handle randomized MSS
// bytes — read-only and safely shared across all holders. ts is private per
// holder so a packet's timestamp write is never observed by another goroutine.
func newTCPOpts(mss []byte) *tcpOpts {
	o := &tcpOpts{}
	o.syn = []layers.TCPOption{
		{OptionType: layers.TCPOptionKindMSS, OptionLength: 4, OptionData: mss},
		{OptionType: layers.TCPOptionKindSACKPermitted, OptionLength: 2},
		{OptionType: layers.TCPOptionKindTimestamps, OptionLength: 10, OptionData: o.ts[:]},
		{OptionType: layers.TCPOptionKindNop},
		{OptionType: layers.TCPOptionKindWindowScale, OptionLength: 3, OptionData: []byte{8}},
	}
	o.ack = []layers.TCPOption{
		{OptionType: layers.TCPOptionKindNop},
		{OptionType: layers.TCPOptionKindNop},
		{OptionType: layers.TCPOptionKindTimestamps, OptionLength: 10, OptionData: o.ts[:]},
	}
	return o
}

// fastRandUint32 returns an unpredictable uint32 for per-packet fingerprint
// jitter (TTL/window/timestamp/seq). It uses math/rand/v2's top-level
// generator, which is safe for concurrent use (per-thread runtime state, no
// syscall, no lock). This matters because buildTCPHeader runs concurrently
// across many KCP/smux streams: a shared *rand.Rand — as previously used here —
// is explicitly NOT safe for concurrent use (see math/rand/v2 docs) and races
// on the send path under `go test -race`.
func fastRandUint32() uint32 {
	return randv2.Uint32()
}

func NewSendHandle(cfg *conf.Network) (*SendHandle, error) {
	handle, err := newHandle(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to open pcap handle: %w", err)
	}

	// SetDirection is not fully supported on Windows Npcap, so skip it
	if runtime.GOOS != "windows" {
		if err := handle.SetDirection(pcap.DirectionOut); err != nil {
			return nil, fmt.Errorf("failed to set pcap direction out: %v", err)
		}
	}

	// MSS: randomize between 1360–1460 to avoid static fingerprint
	mssVal := 1360 + int(fastRandUint32()%101)
	mssBytes := []byte{byte(mssVal >> 8), byte(mssVal & 0xFF)}

	sh := &SendHandle{
		handle:   handle,
		srcPort:  uint16(cfg.Port),
		dscp:     uint8(cfg.PCAP.DSCP),
		baseTime: uint32(time.Now().UnixNano() / int64(time.Millisecond)),
		seqSeed:  fastRandUint32(),
		tcpF:     TCPF{tcpF: iterator.Iterator[conf.TCPF]{Items: cfg.TCP.LF}, clientTCPF: make(map[uint64]*iterator.Iterator[conf.TCPF])},
		ethPool: sync.Pool{
			New: func() any {
				return &layers.Ethernet{SrcMAC: cfg.Interface.HardwareAddr}
			},
		},
		ipv4Pool: sync.Pool{
			New: func() any {
				return &layers.IPv4{}
			},
		},
		ipv6Pool: sync.Pool{
			New: func() any {
				return &layers.IPv6{}
			},
		},
		tcpPool: sync.Pool{
			New: func() any {
				return &layers.TCP{}
			},
		},
		bufPool: sync.Pool{
			New: func() any {
				return gopacket.NewSerializeBuffer()
			},
		},
		optsPool: sync.Pool{
			New: func() any {
				return newTCPOpts(mssBytes)
			},
		},
	}
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

func (h *SendHandle) buildIPv4Header(dstIP net.IP) *layers.IPv4 {
	ip := h.ipv4Pool.Get().(*layers.IPv4)
	// TOS: DSCP value shifted left 2 bits (ECN=0), default 0 = normal traffic
	tos := h.dscp << 2
	// TTL: jitter 63–65 to avoid static fingerprint
	ttl := uint8(63 + fastRandUint32()%3)
	*ip = layers.IPv4{
		Version:  4,
		IHL:      5,
		TOS:      tos,
		TTL:      ttl,
		Flags:    layers.IPv4DontFragment,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    h.srcIPv4,
		DstIP:    dstIP,
	}
	return ip
}

func (h *SendHandle) buildIPv6Header(dstIP net.IP) *layers.IPv6 {
	ip := h.ipv6Pool.Get().(*layers.IPv6)
	tc := h.dscp << 2
	// HopLimit: jitter 63–65
	hl := uint8(63 + fastRandUint32()%3)
	*ip = layers.IPv6{
		Version:      6,
		TrafficClass: tc,
		HopLimit:     hl,
		NextHeader:   layers.IPProtocolTCP,
		SrcIP:        h.srcIPv6,
		DstIP:        dstIP,
	}
	return ip
}

func (h *SendHandle) buildTCPHeader(dstPort uint16, f conf.TCPF, opts *tcpOpts) *layers.TCP {
	tcp := h.tcpPool.Get().(*layers.TCP)
	// Window: randomize between 64240–65535 to avoid static fingerprint
	window := uint16(64240 + fastRandUint32()%1296)
	*tcp = layers.TCP{
		SrcPort: layers.TCPPort(h.srcPort),
		DstPort: layers.TCPPort(dstPort),
		FIN:     f.FIN, SYN: f.SYN, RST: f.RST, PSH: f.PSH, ACK: f.ACK, URG: f.URG, ECE: f.ECE, CWR: f.CWR, NS: f.NS,
		Window: window,
	}

	counter := atomic.AddUint32(&h.tsCounter, 1)
	// Timestamp: base + counter-derived value + random jitter (0–7ms)
	jitter := fastRandUint32() % 8
	tsVal := h.baseTime + (counter >> 3) + jitter
	if f.SYN {
		binary.BigEndian.PutUint32(opts.ts[0:4], tsVal)
		binary.BigEndian.PutUint32(opts.ts[4:8], 0)
		tcp.Options = opts.syn
		// Seq: seed-based + counter with jitter
		tcp.Seq = h.seqSeed + counter*1461 + fastRandUint32()%64
		tcp.Ack = 0
		if f.ACK {
			tcp.Ack = tcp.Seq + 1
		}
	} else {
		tsEcr := tsVal - (counter%200 + 50) - fastRandUint32()%10
		binary.BigEndian.PutUint32(opts.ts[0:4], tsVal)
		binary.BigEndian.PutUint32(opts.ts[4:8], tsEcr)
		tcp.Options = opts.ack
		// Seq/Ack: seed-based with realistic-looking increments and jitter
		dataLen := fastRandUint32()%1400 + 100
		seq := h.seqSeed + counter*1461 + fastRandUint32()%64
		tcp.Seq = seq
		tcp.Ack = seq - dataLen + fastRandUint32()%32
	}

	return tcp
}

func (h *SendHandle) Write(payload []byte, addr *net.UDPAddr) error {
	buf := h.bufPool.Get().(gopacket.SerializeBuffer)
	ethLayer := h.ethPool.Get().(*layers.Ethernet)
	defer func() {
		buf.Clear()
		h.bufPool.Put(buf)
		h.ethPool.Put(ethLayer)
	}()

	dstIP := addr.IP
	dstPort := uint16(addr.Port)

	f := h.getClientTCPF(dstIP, dstPort)
	tcpOpt := h.optsPool.Get().(*tcpOpts)
	defer h.optsPool.Put(tcpOpt)
	tcpLayer := h.buildTCPHeader(dstPort, f, tcpOpt)
	defer h.tcpPool.Put(tcpLayer)

	var ipLayer gopacket.SerializableLayer
	if dstIP.To4() != nil {
		ip := h.buildIPv4Header(dstIP)
		defer h.ipv4Pool.Put(ip)
		ipLayer = ip
		tcpLayer.SetNetworkLayerForChecksum(ip)
		ethLayer.DstMAC = h.srcIPv4RHWA
		ethLayer.EthernetType = layers.EthernetTypeIPv4
	} else {
		ip := h.buildIPv6Header(dstIP)
		defer h.ipv6Pool.Put(ip)
		ipLayer = ip
		tcpLayer.SetNetworkLayerForChecksum(ip)
		ethLayer.DstMAC = h.srcIPv6RHWA
		ethLayer.EthernetType = layers.EthernetTypeIPv6
	}

	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buf, opts, ethLayer, ipLayer, tcpLayer, gopacket.Payload(payload)); err != nil {
		return err
	}
	return h.handle.WritePacketData(buf.Bytes())
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
	a := *addr.(*net.UDPAddr)
	h.tcpF.mu.Lock()
	h.tcpF.clientTCPF[hash.IPAddr(a.IP, uint16(a.Port))] = &iterator.Iterator[conf.TCPF]{Items: f}
	h.tcpF.mu.Unlock()
}

func (h *SendHandle) Close() {
	if h.handle != nil {
		h.handle.Close()
	}
}
