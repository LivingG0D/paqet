package socket

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"testing"

	"paqet/internal/conf"

	"github.com/gopacket/gopacket/layers"
)

// newTestSendHandle builds a SendHandle with only the fields buildTCPHeader
// touches. It deliberately leaves handle nil: buildTCPHeader never reads the
// pcap handle, so the test needs no capture device and no root privileges.
func newTestSendHandle() *SendHandle {
	mss := []byte{0x05, 0xb4} // 1460
	return &SendHandle{
		srcPort:  12345,
		baseTime: 1_000_000,
		seqSeed:  0xdeadbeef,
		tcpPool:  sync.Pool{New: func() any { return &layers.TCP{} }},
		optsPool: sync.Pool{New: func() any { return newTCPOpts(mss) }},
	}
}

// TestBuildTCPHeaderConcurrent is the regression guard for the send-path data
// race. It calls buildTCPHeader from many goroutines (mirroring concurrent
// Write across KCP/smux streams) and reads back each packet's option bytes,
// mimicking gopacket.SerializeLayers. Run with -race.
//
// Before the fix, buildTCPHeader wrote the per-packet timestamp into a single
// []layers.TCPOption shared on the handle (h.synOptions/h.ackOptions), so
// concurrent senders raced on the same 8-byte OptionData backing array. With
// each packet borrowing its own *tcpOpts from optsPool, there is no shared
// mutable state and -race stays clean. Reintroducing the shared slices trips
// the detector here.
func TestBuildTCPHeaderConcurrent(t *testing.T) {
	h := newTestSendHandle()

	const goroutines, iters = 24, 4000
	sinks := make([]uint32, goroutines) // keeps the reads from being elided
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func(id int) {
			defer wg.Done()
			var acc uint32
			for i := range iters {
				opts := h.optsPool.Get().(*tcpOpts)
				// Alternate SYN and ACK packets. ACK is set on both so the SYN
				// branch also computes tcp.Ack, matching the production paths.
				f := conf.TCPF{SYN: (id+i)%2 == 0, ACK: true}
				tcp := h.buildTCPHeader(80, f, opts)

				// Concurrent read of the timestamp bytes: this is the access
				// that races against another goroutine's write when the backing
				// array is shared.
				for _, opt := range tcp.Options {
					if len(opt.OptionData) >= 4 {
						acc += binary.BigEndian.Uint32(opt.OptionData[:4])
					}
				}

				h.tcpPool.Put(tcp)
				h.optsPool.Put(opts)
			}
			sinks[id] = acc
		}(g)
	}
	wg.Wait()
	_ = sinks
}

// TestBuildTCPHeaderOptionShape pins the on-wire TCP option layout so the
// race fix cannot silently alter the fake-TCP fingerprint. SYN carries
// MSS/SACK/Timestamps/Nop/WindowScale; ACK carries Nop/Nop/Timestamps. Both
// Timestamps options must hold an 8-byte value (TSval||TSecr).
func TestBuildTCPHeaderOptionShape(t *testing.T) {
	h := newTestSendHandle()

	synOpts := h.optsPool.Get().(*tcpOpts)
	syn := h.buildTCPHeader(80, conf.TCPF{SYN: true, ACK: true}, synOpts)
	wantSyn := []layers.TCPOptionKind{
		layers.TCPOptionKindMSS,
		layers.TCPOptionKindSACKPermitted,
		layers.TCPOptionKindTimestamps,
		layers.TCPOptionKindNop,
		layers.TCPOptionKindWindowScale,
	}
	assertOptionKinds(t, "SYN", syn.Options, wantSyn)
	if got := len(syn.Options[0].OptionData); got != 2 {
		t.Errorf("SYN MSS OptionData = %d bytes, want 2", got)
	}
	if got := len(syn.Options[2].OptionData); got != 8 {
		t.Errorf("SYN Timestamps OptionData = %d bytes, want 8", got)
	}
	h.tcpPool.Put(syn)
	h.optsPool.Put(synOpts)

	ackOpts := h.optsPool.Get().(*tcpOpts)
	ack := h.buildTCPHeader(80, conf.TCPF{ACK: true}, ackOpts)
	wantAck := []layers.TCPOptionKind{
		layers.TCPOptionKindNop,
		layers.TCPOptionKindNop,
		layers.TCPOptionKindTimestamps,
	}
	assertOptionKinds(t, "ACK", ack.Options, wantAck)
	if got := len(ack.Options[2].OptionData); got != 8 {
		t.Errorf("ACK Timestamps OptionData = %d bytes, want 8", got)
	}
	h.tcpPool.Put(ack)
	h.optsPool.Put(ackOpts)
}

// TestSendPacketAfterClose pins the guard that keeps a send from reaching a
// freed pcap handle. gopacket's WritePacketData dereferences the raw handle
// pointer with no lock and no open check, and Close frees it, so a send that
// arrives after Close must be rejected before it ever touches the handle.
//
// handle is nil here, so the test doubles as proof that sendPacket short-
// circuits on h.closed: if it fell through to h.handle.WritePacketData it
// would panic instead of returning.
func TestSendPacketAfterClose(t *testing.T) {
	h := newTestSendHandle()

	h.Close()

	if err := h.sendPacket([]byte{0xde, 0xad}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("sendPacket after Close = %v, want net.ErrClosed", err)
	}
}

// TestCloseIdempotent guards against a double Close reaching pcap_close twice
// on the same pointer. PacketConn.Close is reachable more than once (kcp/smux
// teardown and the client shutdown path both close conns).
func TestCloseIdempotent(t *testing.T) {
	h := newTestSendHandle()

	h.Close()
	h.Close() // must not panic, must not re-enter pcap_close

	if err := h.sendPacket(nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("sendPacket after double Close = %v, want net.ErrClosed", err)
	}
}

// TestCloseConcurrent races Close against itself on a handle that is still
// open, so the winner writes h.closed while the losers read it. That write is
// what writeMu has to cover in Close; dropping the lock there trips -race here.
//
// It must start from an open handle: once h.closed is set, every Close returns
// at the guard without writing, and a concurrent-Close test that pre-closes
// would pass with no lock at all.
func TestCloseConcurrent(t *testing.T) {
	h := newTestSendHandle()

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			h.Close()
		}()
	}
	wg.Wait()

	if err := h.sendPacket([]byte{0x01}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("sendPacket after concurrent Close = %v, want net.ErrClosed", err)
	}
}

// TestSendAfterCloseConcurrent covers the read side: many senders hitting the
// h.closed guard at once, all of which must be rejected rather than reaching
// the freed handle. Run with -race.
//
// The handle is closed up front on purpose. handle is nil in tests, so a send
// that slipped past the guard would panic instead of failing an assertion —
// which means this cannot also cover a Close landing while a send is genuinely
// in flight inside libpcap. That case is what writeMu enforces, and observing
// it needs a real handle (root plus a capture device), so it is not covered by
// any test here.
func TestSendAfterCloseConcurrent(t *testing.T) {
	h := newTestSendHandle()
	h.Close()

	const goroutines, iters = 16, 500
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for range goroutines {
		go func() {
			defer wg.Done()
			for range iters {
				h.Close()
			}
		}()
		go func() {
			defer wg.Done()
			for range iters {
				if err := h.sendPacket([]byte{0x01}); !errors.Is(err, net.ErrClosed) {
					t.Errorf("sendPacket = %v, want net.ErrClosed", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func assertOptionKinds(t *testing.T, name string, got []layers.TCPOption, want []layers.TCPOptionKind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s option count = %d, want %d", name, len(got), len(want))
	}
	for i, k := range want {
		if got[i].OptionType != k {
			t.Errorf("%s option[%d] = %v, want %v", name, i, got[i].OptionType, k)
		}
	}
}
