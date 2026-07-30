package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"paqet/internal/conf"
)

// TestScaleDownClosesOutsideLock guards the second half of the lock-scope work.
// scaleConnections used to close the retired conn while holding c.mu, so a slow
// teardown (smux Close is bounded at 30s, then a KCP flush and a pcap handle
// close) blocked every newConn caller — and it ran on a 30s ticker regardless
// of load.
//
// It also pins the retired flag: once a slot leaves the pool, an in-flight
// newConn caller holding a stale reference must not redial it back to life.
func TestScaleDownClosesOutsideLock(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)
	c.minConns = 1

	busy := &stubConn{streams: 5}
	idle := &stubConn{
		closeEntered: make(chan struct{}),
		closeBlock:   make(chan struct{}),
	}
	defer close(idle.closeBlock)

	idleTC := &timedConn{cfg: cfg, conn: idle}
	c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: busy}, idleTC)

	go c.scaleDown()

	select {
	case <-idle.closeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("idle conn was never closed")
	}

	if !lockFree(c, 2*time.Second) {
		t.Fatal("c.mu is held while the retired conn's Close blocks")
	}

	c.mu.Lock()
	total, retired := len(c.iter.Items), idleTC.retired
	c.mu.Unlock()

	if total != 1 {
		t.Errorf("pool size after scale-down = %d, want 1", total)
	}
	if !retired {
		t.Error("retired slot was not marked, so a redial could revive it")
	}

	if _, err := c.redial(context.Background(), idleTC, idle); !errors.Is(err, errSlotRetired) {
		t.Errorf("redial of retired slot = %v, want errSlotRetired", err)
	}
}

// TestSwapTunerStopsPrevious covers the reconnect-orphan half of the tuner
// lifecycle. AutoTuner captures its conn at construction and never rebinds, so
// binding a tuner to a slot's new conn must stop the one bound to the old conn
// — otherwise the orphan tunes a closed session every 10s until process exit
// while the live conn is never tuned at all.
func TestSwapTunerStopsPrevious(t *testing.T) {
	c := newTestClient(&conf.Conf{})
	tc := &timedConn{cfg: c.cfg}

	firstCtx, first := context.WithCancel(context.Background())
	c.swapTuner(tc, first)

	secondCtx, second := context.WithCancel(context.Background())
	defer second()
	c.swapTuner(tc, second)

	select {
	case <-firstCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("previous tuner was not stopped when a new conn was bound")
	}
	if secondCtx.Err() != nil {
		t.Error("newly bound tuner was cancelled")
	}

	c.mu.Lock()
	got := tc.stopTuner
	c.mu.Unlock()
	if got == nil {
		t.Error("stopTuner was not recorded on the slot")
	}
}

// TestScaleDownStopsTuner covers the scale-down half. Once a slot leaves the
// pool nothing else holds a reference to its tuner, so failing to cancel here
// leaks the goroutine and pins the closed session graph out of GC — one per
// scale cycle, on a daemon meant to run for weeks.
func TestScaleDownStopsTuner(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)
	c.minConns = 1

	tunerCtx, cancel := context.WithCancel(context.Background())
	idleTC := &timedConn{cfg: cfg, conn: &stubConn{}, stopTuner: cancel}
	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: &stubConn{streams: 5}},
		idleTC,
	)

	c.scaleDown()

	select {
	case <-tunerCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("retired conn's auto-tuner was not stopped")
	}

	c.mu.Lock()
	got := idleTC.stopTuner
	c.mu.Unlock()
	if got != nil {
		t.Error("stopTuner was not cleared when the slot was retired")
	}
}

// TestScaleDownKeepsMinConns checks the floor is respected: with the pool
// already at minConns, an idle conn must not be retired.
func TestScaleDownKeepsMinConns(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)
	c.minConns = 2

	a, b := &stubConn{}, &stubConn{} // both idle
	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: a},
		&timedConn{cfg: cfg, conn: b},
	)

	c.scaleDown()

	if a.closed.Load() || b.closed.Load() {
		t.Error("scale-down dropped below minConns")
	}
	if len(c.iter.Items) != 2 {
		t.Errorf("pool size = %d, want 2", len(c.iter.Items))
	}
}
