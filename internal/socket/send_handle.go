package socket

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
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
	synOptions  []layers.TCPOption
	ackOptions  []layers.TCPOption
	baseTime    uint32
	tsCounter   uint32
	seqSeed     uint32
	tcpF        TCPF
	ethPool     sync.Pool
	ipv4Pool    sync.Pool
	ipv6Pool    sync.Pool
	tcpPool     sync.Pool
	bufPool     sync.Pool
}

// cryptoRandUint32 returns a crypto/rand uint32 for unpredictable seeds.
func cryptoRandUint32() uint32 {
	n, _ := rand.Int(rand.Reader, big.NewInt(1<<32))
	return uint32(n.Uint64())
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
	mssVal := 1360 + int(cryptoRandUint32()%101)
	mssBytes := []byte{byte(mssVal >> 8), byte(mssVal & 0xFF)}

	synOptions := []layers.TCPOption{
		{OptionType: layers.TCPOptionKindMSS, OptionLength: 4, OptionData: mssBytes},
		{OptionType: layers.TCPOptionKindSACKPermitted, OptionLength: 2},
		{OptionType: layers.TCPOptionKindTimestamps, OptionLength: 10, OptionData: make([]byte, 8)},
		{OptionType: layers.TCPOptionKindNop},
		{OptionType: layers.TCPOptionKindWindowScale, OptionLength: 3, OptionData: []byte{8}},
	}

	ackOptions := []layers.TCPOption{
		{OptionType: layers.TCPOptionKindNop},
		{OptionType: layers.TCPOptionKindNop},
		{OptionType: layers.TCPOptionKindTimestamps, OptionLength: 10, OptionData: make([]byte, 8)},
	}

	sh := &SendHandle{
		handle:     handle,
		srcPort:    uint16(cfg.Port),
		dscp:       uint8(cfg.PCAP.DSCP),
		synOptions: synOptions,
		ackOptions: ackOptions,
		baseTime:   uint32(time.Now().UnixNano() / int64(time.Millisecond)),
		seqSeed:    cryptoRandUint32(),
		tcpF:       TCPF{tcpF: iterator.Iterator[conf.TCPF]{Items: cfg.TCP.LF}, clientTCPF: make(map[uint64]*iterator.Iterator[conf.TCPF])},
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
	ttl := uint8(63 + cryptoRandUint32()%3)
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
	hl := uint8(63 + cryptoRandUint32()%3)
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

func (h *SendHandle) buildTCPHeader(dstPort uint16, f conf.TCPF) *layers.TCP {
	tcp := h.tcpPool.Get().(*layers.TCP)
	// Window: randomize between 64240–65535 to avoid static fingerprint
	window := uint16(64240 + cryptoRandUint32()%1296)
	*tcp = layers.TCP{
		SrcPort: layers.TCPPort(h.srcPort),
		DstPort: layers.TCPPort(dstPort),
		FIN:     f.FIN, SYN: f.SYN, RST: f.RST, PSH: f.PSH, ACK: f.ACK, URG: f.URG, ECE: f.ECE, CWR: f.CWR, NS: f.NS,
		Window: window,
	}

	counter := atomic.AddUint32(&h.tsCounter, 1)
	// Timestamp: base + counter-derived value + random jitter (0–7ms)
	jitter := cryptoRandUint32() % 8
	tsVal := h.baseTime + (counter >> 3) + jitter
	if f.SYN {
		binary.BigEndian.PutUint32(h.synOptions[2].OptionData[0:4], tsVal)
		binary.BigEndian.PutUint32(h.synOptions[2].OptionData[4:8], 0)
		tcp.Options = h.synOptions
		// Seq: seed-based + counter with jitter
		tcp.Seq = h.seqSeed + counter*1461 + cryptoRandUint32()%64
		tcp.Ack = 0
		if f.ACK {
			tcp.Ack = tcp.Seq + 1
		}
	} else {
		tsEcr := tsVal - (counter%200 + 50) - cryptoRandUint32()%10
		binary.BigEndian.PutUint32(h.ackOptions[2].OptionData[0:4], tsVal)
		binary.BigEndian.PutUint32(h.ackOptions[2].OptionData[4:8], tsEcr)
		tcp.Options = h.ackOptions
		// Seq/Ack: seed-based with realistic-looking increments and jitter
		dataLen := cryptoRandUint32()%1400 + 100
		seq := h.seqSeed + counter*1461 + cryptoRandUint32()%64
		tcp.Seq = seq
		tcp.Ack = seq - dataLen + cryptoRandUint32()%32
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
	tcpLayer := h.buildTCPHeader(dstPort, f)
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
