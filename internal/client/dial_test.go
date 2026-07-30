package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"paqet/internal/conf"
	"paqet/internal/tnet"
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

// TestNewConnDoesNoIO guards the per-connection fast path. newConn runs once
// per proxied connection, and it used to do two smux stream round-trips there:
// a Ping(false) liveness probe and a re-send of the static TCPF config. Each
// cost a SYN+FIN control-frame pair on the wire, plus a server-side stream that
// read EOF and logged an error — three streams per connection where one does.
//
// The stub parks forever in Ping, so if a liveness probe ever returns to this
// path the test fails instead of silently costing a round-trip per connection.
func TestNewConnDoesNoIO(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	live := &stubConn{
		pingEntered: make(chan struct{}),
		pingBlock:   make(chan struct{}), // never closed: Ping would park here
	}
	c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: live})

	done := make(chan tnet.Conn, 1)
	go func() {
		_, conn, err := c.newConn(context.Background())
		if err != nil {
			t.Errorf("newConn: %v", err)
		}
		done <- conn
	}()

	select {
	case got := <-done:
		if got != live {
			t.Errorf("newConn returned %v, want the live conn", got)
		}
	case <-live.pingEntered:
		t.Fatal("newConn called Ping — a smux stream round-trip per proxied connection")
	case <-time.After(2 * time.Second):
		t.Fatal("newConn blocked; it must not do I/O on the per-connection path")
	}

	if !lockFree(c, 2*time.Second) {
		t.Fatal("c.mu left held by newConn")
	}
}

// TestRedialClosesStaleOutsideLock keeps the lock-scope guarantee pinned now
// that the blocking work on this path is the redial, not the health check.
// smux bounds Close at openCloseTimeout (30s), and c.mu gates every new proxied
// connection plus the stats reporter, the autoscaler and shutdown, so holding
// it across a teardown would serialise the whole client.
func TestRedialClosesStaleOutsideLock(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	// closeBlock is deliberately never closed: the goroutine parks inside Close,
	// so redial never reaches the real dial in createConn.
	stale := &stubConn{
		closeEntered: make(chan struct{}),
		closeBlock:   make(chan struct{}),
	}
	tc := &timedConn{cfg: cfg, conn: stale}
	c.iter.Items = append(c.iter.Items, tc)

	go c.redial(context.Background(), tc, stale) //nolint:errcheck // parks in Close by design

	select {
	case <-stale.closeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("redial never closed the stale conn")
	}

	if !lockFree(c, 2*time.Second) {
		t.Fatal("c.mu is held while the stale conn's Close blocks")
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

// TestPickPrefersLiveConn is the regression guard for connection starvation.
//
// smux reports NumStreams() == 0 for a closed session (session.go:324-327), so
// ranking purely by load made a dead conn out-rank every healthy one. It then
// failed Ping, redialed, and — if the redial kept failing — stayed dead and won
// the next scan too. With the installer's default of 8 conns, one un-redialable
// slot could absorb every stream request while seven healthy conns sat idle,
// and concurrent callers serialised on that slot's dialMu each doing their own
// failing dial.
func TestPickPrefersLiveConn(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	dead := &stubConn{streams: 40}
	dead.Close() // now reports IsClosed, and NumStreams() == 0
	live := &stubConn{streams: 40}

	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: dead},
		&timedConn{cfg: cfg, conn: live},
	)

	_, conn := c.pick()
	if conn != live {
		t.Error("pick chose a closed conn over a live one — a dead slot reports zero streams and would starve the pool")
	}
}

// TestPickFallsBackToDeadConn: when nothing is live, a dead slot must still be
// handed back so redial can revive it. Returning nothing would strand the
// client with no way to recover.
func TestPickFallsBackToDeadConn(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	a, b := &stubConn{}, &stubConn{}
	a.Close()
	b.Close()
	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: a},
		&timedConn{cfg: cfg, conn: b},
	)

	tc, conn := c.pick()
	if tc == nil || conn == nil {
		t.Fatal("pick gave up when every conn was dead; nothing would ever redial")
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

	if _, _, err := c.newConn(context.Background()); !errors.Is(err, errNoConns) {
		t.Fatalf("newConn on empty pool = %v, want errNoConns", err)
	}
}

// TestPublishRejectsRetiredSlot pins the invariant that closes a TOCTOU window
// in redial: the retired check happens before createConn, but a dial takes long
// enough for a scale-down tick to land — and a slot mid-redial holds a closed
// conn, which reports zero streams and is therefore exactly what scaleDown
// picks. Publishing into a retired slot strands the conn outside the pool where
// nothing closes it, with a tuner nothing cancels.
func TestPublishRejectsRetiredSlot(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	old := &stubConn{}
	tc := &timedConn{cfg: cfg, conn: old, retired: true}

	if c.publish(tc, &stubConn{}) {
		t.Fatal("publish accepted a conn into a retired slot")
	}

	c.mu.Lock()
	got := tc.conn
	c.mu.Unlock()
	if got != old {
		t.Error("publish overwrote the conn of a retired slot")
	}
}

func TestPublishInstallsConn(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	tc := &timedConn{cfg: cfg}
	fresh := &stubConn{}

	if !c.publish(tc, fresh) {
		t.Fatal("publish rejected a conn for a live slot")
	}

	c.mu.Lock()
	got := tc.conn
	c.mu.Unlock()
	if got != fresh {
		t.Error("publish did not install the conn")
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
	if _, _, err := c.newConn(ctx); err == nil {
		t.Fatal("newConn with a nil-conn slot and cancelled ctx: want error")
	}
	if !lockFree(c, 2*time.Second) {
		t.Fatal("c.mu left held after newConn returned")
	}
}
