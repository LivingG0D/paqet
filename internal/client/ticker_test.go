package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"paqet/internal/conf"
)

// TestScaleDownSkipsFreshConn is the regression guard for the autoscaler.
//
// Splitting scaleConnections into scaleUp+scaleDown moved the numConns read to
// after scaleUp's append, which put the just-added conn inside the scale-down
// loop's range. A fresh conn has zero streams — exactly the victim predicate —
// so every scale-up was undone by the scale-down in the same tick: the pool
// could never grow, and under sustained load the client dialled and tore down a
// full pcap+KCP+smux session every 30s forever.
//
// scaleDown now only considers the slots that existed before scaleUp ran.
// The pool must sit ABOVE minConns here, otherwise the eligible <= minConns
// guard returns before the loop and the test would pass without ever
// exercising the bound that actually fixes this.
func TestScaleDownSkipsFreshConn(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)
	c.minConns, c.maxConns = 2, 8

	fresh := &stubConn{}
	saturated := []*stubConn{
		{streams: maxStreamsPerConn},
		{streams: maxStreamsPerConn},
		{streams: maxStreamsPerConn},
	}
	for _, s := range saturated {
		c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: s})
	}
	c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: fresh})

	// 3 slots existed before the (simulated) scale-up appended the fourth.
	c.scaleDown(3)

	if fresh.closed.Load() {
		t.Error("scale-down retired the conn scale-up just added")
	}
	for i, s := range saturated {
		if s.closed.Load() {
			t.Errorf("scale-down retired saturated conn %d", i)
		}
	}
	if got := len(c.iter.Items); got != 4 {
		t.Errorf("pool size = %d, want 4", got)
	}
}

// TestScaleDownClosesOutsideLock guards the lock-scope work. scaleConnections
// used to close the retired conn while holding c.mu, so a slow teardown (smux
// Close is bounded at 30s, then a KCP flush and a pcap handle close) blocked
// every newConn caller — on a 30s ticker, regardless of load.
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

	go c.scaleDown(2)

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
	conn := &stubConn{}
	tc := &timedConn{cfg: c.cfg, conn: conn}

	firstCtx, first := context.WithCancel(context.Background())
	if !c.swapTuner(tc, conn, first) {
		t.Fatal("swapTuner rejected the first binding")
	}

	secondCtx, second := context.WithCancel(context.Background())
	defer second()
	if !c.swapTuner(tc, conn, second) {
		t.Fatal("swapTuner rejected the rebind")
	}

	select {
	case <-firstCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("previous tuner was not stopped when a new conn was bound")
	}
	if secondCtx.Err() != nil {
		t.Error("newly bound tuner was cancelled")
	}
}

// TestSwapTunerRejectsStaleBinding covers the window between sampling a
// (slot, conn) pair and binding its tuner. bindTuner callers release c.mu in
// between, so the slot may have been retired by scaleDown or given a different
// conn by redial. Accepting the binding anyway would either strand a tuner on a
// slot nothing can cancel from, or cancel the live conn's tuner and leave a
// zombie running against a closed session.
func TestSwapTunerRejectsStaleBinding(t *testing.T) {
	c := newTestClient(&conf.Conf{})

	t.Run("conn replaced", func(t *testing.T) {
		current := &stubConn{}
		tc := &timedConn{cfg: c.cfg, conn: current}

		liveCtx, live := context.WithCancel(context.Background())
		defer live()
		if !c.swapTuner(tc, current, live) {
			t.Fatal("swapTuner rejected a current binding")
		}

		stale := &stubConn{} // the conn this binding was sampled for
		_, cancel := context.WithCancel(context.Background())
		defer cancel()
		if c.swapTuner(tc, stale, cancel) {
			t.Error("swapTuner accepted a binding for a conn the slot no longer holds")
		}
		if liveCtx.Err() != nil {
			t.Error("a stale binding cancelled the live conn's tuner")
		}
	})

	t.Run("slot retired", func(t *testing.T) {
		conn := &stubConn{}
		tc := &timedConn{cfg: c.cfg, conn: conn, retired: true}

		_, cancel := context.WithCancel(context.Background())
		defer cancel()
		if c.swapTuner(tc, conn, cancel) {
			t.Error("swapTuner accepted a binding for a retired slot")
		}
	})
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

	c.scaleDown(2)

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

	c.scaleDown(2)

	if a.closed.Load() || b.closed.Load() {
		t.Error("scale-down dropped below minConns")
	}
	if len(c.iter.Items) != 2 {
		t.Errorf("pool size = %d, want 2", len(c.iter.Items))
	}
}

// TestPoolSaturatedIgnoresDeadConn covers half of a recovery hole introduced
// when pick started routing around closed conns. A closed session reports zero
// streams, so a dead slot used to look like spare capacity and held
// allOverloaded false forever — which switched scale-up off entirely, no matter
// how saturated the surviving conns were.
func TestPoolSaturatedIgnoresDeadConn(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	dead := &stubConn{}
	dead.Close()
	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: &stubConn{streams: maxStreamsPerConn}},
		&timedConn{cfg: cfg, conn: &stubConn{streams: maxStreamsPerConn}},
		&timedConn{cfg: cfg, conn: dead},
	)

	saturated, n := c.poolSaturated()
	if !saturated {
		t.Error("a dead conn counted as spare capacity, so scale-up can never fire")
	}
	if n != 3 {
		t.Errorf("pool size = %d, want 3", n)
	}
}

// TestPoolSaturatedSeesRealCapacity is the converse: a live conn with room must
// still prevent scale-up.
func TestPoolSaturatedSeesRealCapacity(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)
	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: &stubConn{streams: maxStreamsPerConn}},
		&timedConn{cfg: cfg, conn: &stubConn{streams: 1}},
	)

	if saturated, _ := c.poolSaturated(); saturated {
		t.Error("pool reported saturated while a live conn still had room")
	}
}

// TestDeadSlotsFound covers the other half: repairDead must be able to see a
// dead slot that pick will never hand out, so something redials it.
func TestDeadSlotsFound(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	dead := &stubConn{}
	dead.Close()
	deadTC := &timedConn{cfg: cfg, conn: dead}
	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: &stubConn{streams: 3}},
		deadTC,
		&timedConn{cfg: cfg}, // never dialled: not dead, just empty
	)

	slots, conns := c.deadSlots()
	if len(slots) != 1 || slots[0] != deadTC {
		t.Fatalf("deadSlots found %d slots, want exactly the closed one", len(slots))
	}
	if len(conns) != 1 || conns[0] != dead {
		t.Error("deadSlots did not return the conn the slot was holding")
	}

	// pick must still route traffic away from it — repair is the only path.
	if _, conn := c.pick(); conn == dead {
		t.Error("pick handed out the dead conn; it should route around it")
	}
}
