package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"paqet/internal/conf"
)

// lockFree reports whether c.mu can be acquired within d.
func lockFree(c *Client, d time.Duration) bool {
	got := make(chan struct{})
	go func() {
		c.mu.Lock()
		c.mu.Unlock()
		close(got)
	}()
	select {
	case <-got:
		return true
	case <-time.After(d):
		return false
	}
}

// TestNewConnDoesNotHoldLockDuringPing is the regression guard for the
// head-of-line blocking defect: newConn used to hold c.mu across the health
// check, so a single unresponsive conn froze every other caller — and c.mu
// gates all new proxied connections plus the stats reporter, the autoscaler
// and shutdown.
//
// smux bounds OpenStream and Close at openCloseTimeout (30s), so a real stall
// here held the lock for up to a minute. Ping is stubbed to block forever;
// c.mu must still be acquirable.
func TestNewConnDoesNotHoldLockDuringPing(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	blocking := &stubConn{
		pingEntered: make(chan struct{}),
		pingBlock:   make(chan struct{}),
	}
	defer close(blocking.pingBlock)
	c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: blocking})

	go c.newConn(context.Background()) //nolint:errcheck // parks in Ping by design

	select {
	case <-blocking.pingEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Ping was never called")
	}

	if !lockFree(c, 2*time.Second) {
		t.Fatal("c.mu is held while Ping blocks — newConn serialises the whole client")
	}
}

// TestPickChoosesLeastLoaded pins the load-balancing behaviour that the lock
// rework moved into pick().
func TestPickChoosesLeastLoaded(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	busy := &stubConn{streams: 40}
	idle := &stubConn{streams: 2}
	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: busy},
		&timedConn{cfg: cfg, conn: idle},
	)

	tc, conn := c.pick()
	if tc == nil {
		t.Fatal("pick returned no slot")
	}
	if conn != idle {
		t.Errorf("pick chose the busier conn: got %d streams, want %d", conn.NumStreams(), idle.streams)
	}
	if !lockFree(c, time.Second) {
		t.Error("pick did not release c.mu")
	}
}

// TestPickEmptyPool guards the fallback path. pick's round-robin fallback runs
// only when every slot is empty, and iterator.Next indexes Items directly — so
// an empty pool must be rejected before it is reached rather than panicking.
func TestPickEmptyPool(t *testing.T) {
	c := newTestClient(&conf.Conf{})

	tc, conn := c.pick()
	if tc != nil || conn != nil {
		t.Fatalf("pick on empty pool = (%v, %v), want (nil, nil)", tc, conn)
	}

	if _, err := c.newConn(context.Background()); !errors.Is(err, errNoConns) {
		t.Fatalf("newConn on empty pool = %v, want errNoConns", err)
	}
}

// TestNewConnNilConnSlot covers a slot whose dial never completed. The old code
// dereferenced bestTC.conn unconditionally, so reaching this path through the
// round-robin fallback panicked.
func TestNewConnNilConnSlot(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)
	c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg}) // conn == nil

	// No server to redial, so this fails — the point is that it returns rather
	// than panicking, and does not leave c.mu held.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.newConn(ctx); err == nil {
		t.Fatal("newConn with a nil-conn slot and cancelled ctx: want error")
	}
	if !lockFree(c, 2*time.Second) {
		t.Fatal("c.mu left held after newConn returned")
	}
}
