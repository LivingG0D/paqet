package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"paqet/internal/conf"
)

// TestPoolLoadArmsOnMeanNotOnEveryConn is the regression guard for the
// autoscaler's growth predicate.
//
// The old predicate was universally quantified: EVERY conn had to be at the
// stream cap before the pool grew. A single conn one stream short vetoed growth
// for the whole pool — and since every conn scale-up added arrived with zero
// streams, it vetoed the next tick itself. With conn: 8 that needed ~512
// concurrent streams before the pool grew at all, so in practice it never did.
//
// The two subtests pin both directions of the mean.
func TestPoolLoadArmsOnMeanNotOnEveryConn(t *testing.T) {
	cfg := &conf.Conf{}

	t.Run("one idle conn does not veto growth", func(t *testing.T) {
		c := newTestClient(cfg)
		c.iter.Items = append(c.iter.Items,
			&timedConn{cfg: cfg, conn: &stubConn{streams: streamsHigh*2 + 1}},
			&timedConn{cfg: cfg, conn: &stubConn{}}, // idle: the old veto
		)

		streams, usable, total := c.poolLoad()
		if streams != streamsHigh*2+1 || usable != 2 || total != 2 {
			t.Fatalf("poolLoad = (%d, %d, %d), want (%d, 2, 2)",
				streams, usable, total, streamsHigh*2+1)
		}
		if streams <= usable*streamsHigh {
			t.Errorf("mean %d/%d did not arm scale-up; an idle conn still vetoes growth",
				streams, usable)
		}
	})

	t.Run("a pool with room does not arm growth", func(t *testing.T) {
		c := newTestClient(cfg)
		c.iter.Items = append(c.iter.Items,
			&timedConn{cfg: cfg, conn: &stubConn{streams: streamsHigh}},
			&timedConn{cfg: cfg, conn: &stubConn{streams: 1}},
		)

		streams, usable, _ := c.poolLoad()
		if streams > usable*streamsHigh {
			t.Errorf("mean %d/%d armed scale-up while the pool still had room",
				streams, usable)
		}
	})
}

// TestScaleDownSkipsFreshConn is the regression guard for the other half.
//
// Splitting scaleConnections into scaleUp+scaleDown once put the just-added
// conn inside scale-down's victim range. A fresh conn has zero streams —
// exactly the victim predicate — so every scale-up was undone by the scale-down
// in the same tick: the pool could never grow, and under sustained load the
// client dialled and tore down a full pcap+KCP+smux session every 30s forever.
//
// That used to need a positional bound threaded from scaleUp into scaleDown.
// The streamsHigh:streamsLow hysteresis band now rules it out arithmetically —
// a pool loaded enough to have grown cannot be idle enough to shrink — so this
// asserts the property directly, with no plumbing left to pass.
func TestScaleDownSkipsFreshConn(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)
	c.minConns, c.maxConns = 2, 8

	// Three conns loaded just past the growth watermark: 99 > 3*32.
	loaded := []*stubConn{
		{streams: streamsHigh + 1},
		{streams: streamsHigh + 1},
		{streams: streamsHigh + 1},
	}
	for _, s := range loaded {
		c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: s})
	}
	// The conn scale-up just appended, still carrying nothing.
	fresh := &stubConn{}
	c.iter.Items = append(c.iter.Items, &timedConn{cfg: cfg, conn: fresh})

	c.scaleDown()

	if fresh.closed.Load() {
		t.Error("scale-down retired the conn scale-up just added")
	}
	for i, s := range loaded {
		if s.closed.Load() {
			t.Errorf("scale-down retired loaded conn %d", i)
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

	// 5 streams over 2 conns is under the drain watermark, so scale-down arms.
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

// TestPoolLoadIgnoresDeadConn covers half of a recovery hole introduced when
// pick started routing around closed conns. A closed session reports zero
// streams, so a dead slot used to look like spare capacity — it dragged the
// pool mean down and switched scale-up off entirely, no matter how loaded the
// surviving conns were.
func TestPoolLoadIgnoresDeadConn(t *testing.T) {
	cfg := &conf.Conf{}
	c := newTestClient(cfg)

	dead := &stubConn{}
	dead.Close()
	c.iter.Items = append(c.iter.Items,
		&timedConn{cfg: cfg, conn: &stubConn{streams: streamsHigh * 2}},
		&timedConn{cfg: cfg, conn: &stubConn{streams: streamsHigh * 2}},
		&timedConn{cfg: cfg, conn: dead},
	)

	streams, usable, total := c.poolLoad()
	if usable != 2 {
		t.Errorf("usable conns = %d, want 2: a dead conn is not capacity", usable)
	}
	if streams != streamsHigh*4 {
		t.Errorf("streams = %d, want %d", streams, streamsHigh*4)
	}
	if total != 3 {
		t.Errorf("pool size = %d, want 3", total)
	}
	if streams <= usable*streamsHigh {
		t.Error("a dead conn diluted the mean, so scale-up can never fire")
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
