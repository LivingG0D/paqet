package client

import (
	"net"
	"paqet/internal/tnet"
	"testing"
)

type fakeStrm struct {
	net.Conn
	closed bool
}

func (f *fakeStrm) Close() error { f.closed = true; return nil }
func (f *fakeStrm) SID() int     { return 0 }

func TestUDPPoolKeepsFirstStreamOnRace(t *testing.T) {
	p := &udpPool{strms: make(map[uint64]tnet.Strm)}
	first, second := &fakeStrm{}, &fakeStrm{}

	if got, added := p.add(1, first); !added || got != first {
		t.Fatalf("add(first) = %v, %v; want first, true", got, added)
	}
	if got, added := p.add(1, second); added || got != first {
		t.Fatalf("add(second) = %v, %v; want first, false", got, added)
	}
}

func TestUDPPoolDeleteStaleStreamKeepsLiveOne(t *testing.T) {
	p := &udpPool{strms: make(map[uint64]tnet.Strm)}
	live, stale := &fakeStrm{}, &fakeStrm{}
	p.add(1, live)

	p.delete(1, stale)
	if !stale.closed {
		t.Error("stale stream not closed")
	}
	if live.closed || p.strms[1] != live {
		t.Fatal("deleting a stale stream closed or removed the live one")
	}

	p.delete(1, live)
	if !live.closed || p.strms[1] != nil {
		t.Fatal("live stream not closed and removed")
	}
}
