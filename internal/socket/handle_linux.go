package socket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
	"golang.org/x/sys/unix"

	"paqet/internal/conf"
	"paqet/internal/flog"
)

// On Linux paqet uses raw AF_PACKET sockets rather than libpcap handles. A send
// is one write syscall instead of a cgo call into libpcap, and a read waits in
// Go's netpoller instead of a pcap poll loop that wakes every 100ms per handle.

// rawWriter sends on a protocol-0 packet socket. The kernel never delivers
// frames to such a socket, whereas a pcap handle is a capture socket: the host
// copies every frame it sends or receives to it and runs its filter.
type rawWriter struct{ f *os.File }

// ifindex looks the interface up by name on every open, as libpcap does. An
// interface that was recreated (wifi reload, replugged USB NIC) comes back with
// a new index, and a redial must bind to that one.
func ifindex(cfg *conf.Network) (int, error) {
	iface, err := net.InterfaceByName(cfg.Interface.Name)
	if err != nil {
		return 0, fmt.Errorf("failed to find interface %s: %w", cfg.Interface.Name, err)
	}
	return iface.Index, nil
}

func openSendHandle(cfg *conf.Network) (frameWriter, error) {
	idx, err := ifindex(cfg)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open packet socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Ifindex: idx}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("failed to bind packet socket to %s: %w", cfg.Interface.Name, err)
	}
	return &rawWriter{os.NewFile(uintptr(fd), "afpacket")}, nil
}

func (w *rawWriter) WritePacketData(frame []byte) error {
	_, err := w.f.Write(frame)
	if errors.Is(err, unix.ENOBUFS) {
		// The device queue is full. Drop the frame as a UDP socket would and let
		// KCP retransmit it; a send error would tear the whole session down.
		return nil
	}
	return err
}

func (w *rawWriter) Close() { w.f.Close() }

// recvBatch is the most frames one recvmmsg call returns. Under load frames
// queue up while the last batch is processed, and one syscall drains them.
const recvBatch = 64

type mmsghdr struct {
	hdr unix.Msghdr
	len uint32
}

// rawReader receives on a packet socket that sees only incoming frames
// matching the port filter. RecvHandle.mu serializes ReadFrame, so the batch
// state needs no lock of its own.
type rawReader struct {
	f  *os.File
	rc syscall.RawConn

	msgs [recvBatch]mmsghdr
	iovs [recvBatch]unix.Iovec
	bufs [recvBatch][]byte
	n    int // frames in the current batch
	next int // next frame to hand out
	err  error
	fill func(fd uintptr) bool // r.recvmmsg, bound once so rc.Read allocates nothing
}

func openRecvHandle(cfg *conf.Network) (frameReader, error) {
	// Protocol 0 receives nothing, so no unfiltered frame is queued before
	// setupRecv binds the socket.
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open packet socket: %w", err)
	}
	if err := setupRecv(fd, cfg); err != nil {
		unix.Close(fd)
		return nil, err
	}

	r := &rawReader{f: os.NewFile(uintptr(fd), "afpacket")}
	if r.rc, err = r.f.SyscallConn(); err != nil {
		r.f.Close()
		return nil, err
	}
	for i := range r.msgs {
		r.bufs[i] = make([]byte, cfg.PCAP.SnapLen)
		r.iovs[i].Base = &r.bufs[i][0]
		r.iovs[i].SetLen(len(r.bufs[i]))
		r.msgs[i].hdr.Iov = &r.iovs[i]
		r.msgs[i].hdr.SetIovlen(1)
	}
	r.fill = r.recvmmsg
	return r, nil
}

// rcvbufWarn limits the capped-buffer warning to once per process: every
// client connection opens its own receive socket.
var rcvbufWarn sync.Once

func setupRecv(fd int, cfg *conf.Network) error {
	idx, err := ifindex(cfg)
	if err != nil {
		return err
	}
	if err := attachFilter(fd, cfg); err != nil {
		return err
	}

	// Skip the host's outgoing frames before they reach the filter (Linux
	// 4.20+). Older kernels run the filter, which rejects them too.
	_ = unix.SetsockoptInt(fd, unix.SOL_PACKET, unix.PACKET_IGNORE_OUTGOING, 1)

	// SO_RCVBUFFORCE needs CAP_NET_ADMIN; SO_RCVBUF is capped at net.core.rmem_max.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, cfg.PCAP.Sockbuf); err != nil {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, cfg.PCAP.Sockbuf); err != nil {
			return fmt.Errorf("failed to set receive buffer to %d: %w", cfg.PCAP.Sockbuf, err)
		}
		// The kernel reports twice the size it granted.
		if got, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF); err == nil && got/2 < cfg.PCAP.Sockbuf {
			rcvbufWarn.Do(func() {
				flog.Warnf("receive buffer capped at %d bytes (pcap.sockbuf is %d): raise net.core.rmem_max or grant CAP_NET_ADMIN", got/2, cfg.PCAP.Sockbuf)
			})
		}
	}

	if cfg.PCAP.Promisc {
		mreq := unix.PacketMreq{Ifindex: int32(idx), Type: unix.PACKET_MR_PROMISC}
		if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &mreq); err != nil {
			return fmt.Errorf("failed to set promiscuous mode: %w", err)
		}
	}

	// Binding with a protocol starts delivery, now that the filter is attached.
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: idx}); err != nil {
		return fmt.Errorf("failed to bind packet socket to %s: %w", cfg.Interface.Name, err)
	}
	return nil
}

// skfAdPkttype is SKF_AD_OFF + SKF_AD_PKTTYPE, the BPF load offset of the
// frame's packet type (x/sys/unix does not define the SKF_AD constants).
const skfAdPkttype = 0xfffff000 + 4

// attachFilter installs the port filter, compiled by libpcap so it matches what
// a pcap handle would run, behind a check that rejects outgoing frames.
func attachFilter(fd int, cfg *conf.Network) error {
	insns, err := pcap.CompileBPFFilter(layers.LinkTypeEthernet, cfg.PCAP.SnapLen, recvFilter(cfg.Port))
	if err != nil {
		return fmt.Errorf("failed to compile BPF filter: %w", err)
	}
	if len(insns) > 255 {
		return fmt.Errorf("BPF filter too long: %d instructions", len(insns))
	}
	// Jumps are relative, so the compiled program runs unchanged between the
	// direction check and the final reject it jumps to.
	prog := make([]unix.SockFilter, 0, len(insns)+3)
	prog = append(prog,
		unix.SockFilter{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: skfAdPkttype},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.PACKET_OUTGOING, Jt: uint8(len(insns))},
	)
	for _, in := range insns {
		prog = append(prog, unix.SockFilter{Code: in.Code, Jt: in.Jt, Jf: in.Jf, K: in.K})
	}
	prog = append(prog, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: 0})
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &fprog); err != nil {
		return fmt.Errorf("failed to attach BPF filter: %w", err)
	}
	return nil
}

func (r *rawReader) ReadFrame() ([]byte, error) {
	if r.next == r.n {
		// Waits in the netpoller until a frame arrives, Close, or the deadline.
		if err := r.rc.Read(r.fill); err != nil {
			return nil, err
		}
		if r.err != nil {
			return nil, r.err
		}
	}
	i := r.next
	r.next++
	return r.bufs[i][:r.msgs[i].len], nil
}

// recvmmsg reports false when no frame is queued, so rc.Read waits for one.
func (r *rawReader) recvmmsg(fd uintptr) bool {
	for {
		n, _, errno := unix.Syscall6(unix.SYS_RECVMMSG, fd, uintptr(unsafe.Pointer(&r.msgs[0])), recvBatch, unix.MSG_DONTWAIT, 0, 0)
		switch errno {
		case 0:
			r.n, r.next, r.err = int(n), 0, nil
			return true
		case unix.EINTR:
			continue
		case unix.EAGAIN:
			return false
		default:
			r.n, r.next, r.err = 0, 0, errno
			return true
		}
	}
}

func (r *rawReader) SetReadDeadline(t time.Time) error { return r.f.SetReadDeadline(t) }

func (r *rawReader) Close() { r.f.Close() }

func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}
