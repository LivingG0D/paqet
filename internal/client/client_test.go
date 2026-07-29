package client

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"paqet/internal/conf"
	"paqet/internal/pkg/iterator"
	"paqet/internal/tnet"
)

// stubConn is a no-op tnet.Conn that records whether Close was called.
type stubConn struct{ closed atomic.Bool }

func (s *stubConn) OpenStrm() (tnet.Strm, error)     { return nil, nil }
func (s *stubConn) AcceptStrm() (tnet.Strm, error)   { return nil, nil }
func (s *stubConn) NumStreams() int                  { return 0 }
func (s *stubConn) Ping(bool) error                  { return nil }
func (s *stubConn) Close() error                     { s.closed.Store(true); return nil }
func (s *stubConn) LocalAddr() net.Addr              { return nil }
func (s *stubConn) RemoteAddr() net.Addr             { return nil }
func (s *stubConn) SetDeadline(time.Time) error      { return nil }
func (s *stubConn) SetReadDeadline(time.Time) error  { return nil }
func (s *stubConn) SetWriteDeadline(time.Time) error { return nil }

func newTestClient(cfg *conf.Conf) *Client {
	return &Client{
		cfg:      cfg,
		iter:     &iterator.Iterator[*timedConn]{},
		udpPool:  &udpPool{strms: make(map[uint64]tnet.Strm)},
		minConns: 1,
		maxConns: 8,
	}
}

func TestCloseConnsClosesAll(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	stubs := make([]*stubConn, 4)
	for i := range stubs {
		stubs[i] = &stubConn{}
		c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: stubs[i]})
	}
	// A timedConn whose dial never completed has a nil conn — must be skipped,
	// not dereferenced.
	c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg})

	c.closeConns()

	for i, s := range stubs {
		if !s.closed.Load() {
			t.Errorf("conn %d was not closed", i)
		}
	}
}

// TestCloseConnsRace drives closeConns against the two mutators that really
// race it in production: scaleConnections rewriting c.iter.Items, and newConn
// reassigning timedConn.conn. Both mutate under c.mu, so closeConns must take
// c.mu to read either one. Run under -race: dropping the lock in closeConns
// (for example by ranging c.iter.Items directly, as the shutdown goroutine
// used to) trips the detector here.
func TestCloseConnsRace(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)
	for range 8 {
		c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: &stubConn{}})
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Mimics scaleConnections: grows and shrinks the pool under c.mu.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.mu.Lock()
			c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: &stubConn{}})
			if len(c.iter.Items) > 8 {
				c.iter.Items = c.iter.Items[:len(c.iter.Items)-1]
			}
			c.mu.Unlock()
		}
	}()

	// Mimics newConn: reassigns timedConn.conn after a reconnect, under c.mu.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			c.mu.Lock()
			if len(c.iter.Items) > 0 {
				c.iter.Items[0].conn = &stubConn{}
			}
			c.mu.Unlock()
		}
	}()

	for range 200 {
		c.closeConns()
	}

	close(stop)
	wg.Wait()
}
