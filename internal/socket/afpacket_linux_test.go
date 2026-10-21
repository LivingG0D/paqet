//go:build linux

package socket

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"paqet/internal/conf"
)

// loNetwork returns a config bound to the loopback device. The tests need root
// for raw packet sockets, but no real network.
func loNetwork(tb testing.TB, port int) *conf.Network {
	tb.Helper()
	if os.Geteuid() != 0 {
		tb.Skip("raw packet sockets need root")
	}
	lo, err := net.InterfaceByName("lo")
	if err != nil {
		tb.Skipf("no loopback device: %v", err)
	}
	iface := *lo
	iface.HardwareAddr = make(net.HardwareAddr, 6) // lo has no MAC; frames carry zeros
	return &conf.Network{
		Interface: &iface,
		Port:      port,
		IPv4: conf.Addr{
			Addr:   &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
			Router: make(net.HardwareAddr, 6),
		},
		PCAP: conf.PCAP{Sockbuf: 1 << 20, SnapLen: 1600},
		TCP:  conf.TCP{LF: []conf.TCPF{{PSH: true, ACK: true}}},
	}
}

func TestRoundTripLoopback(t *testing.T) {
	const port = 47001
	pc, err := New(loNetwork(t, port))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	payload := []byte("paqet round trip")
	if _, err := pc.WriteTo(payload, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, addr, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("payload = %q, want %q", buf[:n], payload)
	}
	if ua, ok := addr.(*net.UDPAddr); !ok || !ua.IP.Equal(net.IPv4(127, 0, 0, 1)) || ua.Port != port {
		t.Fatalf("addr = %v, want 127.0.0.1:%d", addr, port)
	}
}

func TestReadDeadlineLoopback(t *testing.T) {
	pc, err := New(loNetwork(t, 47002))
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	pc.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	start := time.Now()
	_, _, err = pc.ReadFrom(make([]byte, 2048))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("ReadFrom error = %v, want a timeout", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("deadline fired after %v, want ~150ms", took)
	}
}

func TestCloseUnblocksReadLoopback(t *testing.T) {
	pc, err := New(loNetwork(t, 47003))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := pc.ReadFrom(make([]byte, 2048))
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	pc.Close()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ReadFrom returned a nil error after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("ReadFrom still blocked 1s after Close")
	}
}

// The kernel copies every frame the host sends or receives to each running
// packet socket and runs its filter, so a socket that only sends must not be
// one.
func TestSendHandleIsNotCaptureSocket(t *testing.T) {
	cfg := loNetwork(t, 47004)
	before := runningPacketSockets(t)
	h, err := NewSendHandle(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if added := runningPacketSockets(t) - before; added != 0 {
		t.Fatalf("NewSendHandle added %d running packet socket(s), want 0", added)
	}
}

// The receive filter must reject the host's own outgoing frames by itself:
// PACKET_IGNORE_OUTGOING only exists from Linux 4.20. On lo every frame sent
// is seen twice, once outgoing and once incoming.
func TestRecvFilterRejectsOutgoing(t *testing.T) {
	const sport, dport = 47011, 47012
	send, err := NewSendHandle(loNetwork(t, sport))
	if err != nil {
		t.Fatal(err)
	}
	defer send.Close()

	cfg := loNetwork(t, dport)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := attachFilter(fd, cfg); err != nil {
		t.Fatal(err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: cfg.Interface.Index}); err != nil {
		t.Fatal(err)
	}

	if err := send.Write([]byte("which way"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: dport}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	var incoming, outgoing int
	buf := make([]byte, 2048)
	for {
		_, from, err := unix.Recvfrom(fd, buf, unix.MSG_DONTWAIT)
		if err != nil {
			break
		}
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
			outgoing++
		} else {
			incoming++
		}
	}
	if outgoing != 0 || incoming != 1 {
		t.Fatalf("got %d outgoing and %d incoming copies, want 0 and 1", outgoing, incoming)
	}
}

// runningPacketSockets counts this process's packet sockets that the kernel
// delivers frames to (R=1 in /proc/net/packet).
func runningPacketSockets(t *testing.T) int {
	t.Helper()
	own := map[string]bool{}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		link, err := os.Readlink("/proc/self/fd/" + fd.Name())
		if inode, ok := strings.CutPrefix(link, "socket:["); err == nil && ok {
			own[strings.TrimSuffix(inode, "]")] = true
		}
	}

	f, err := os.Open("/proc/net/packet")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// sk RefCnt Type Proto Iface R Rmem User Inode
		fields := strings.Fields(sc.Text())
		if len(fields) >= 9 && fields[5] == "1" && own[fields[8]] {
			n++
		}
	}
	return n
}

func BenchmarkSendLoopback(b *testing.B) {
	h, err := NewSendHandle(loNetwork(b, 47005))
	if err != nil {
		b.Fatal(err)
	}
	defer h.Close()
	payload := make([]byte, 1200)
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 47006}
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		if err := h.Write(payload, dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRoundTripLoopback(b *testing.B) {
	const port = 47007
	pc, err := New(loNetwork(b, port))
	if err != nil {
		b.Fatal(err)
	}
	defer pc.Close()
	payload := make([]byte, 1200)
	buf := make([]byte, 2048)
	dst := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		if _, err := pc.WriteTo(payload, dst); err != nil {
			b.Fatal(err)
		}
		if _, _, err := pc.ReadFrom(buf); err != nil {
			b.Fatal(err)
		}
	}
}
