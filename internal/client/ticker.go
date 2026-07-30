package client

import (
	"context"
	"paqet/internal/flog"
	"paqet/internal/tnet"
	"paqet/internal/tnet/kcp"
	"time"
)

const (
	connScaleInterval = 30 * time.Second
	maxStreamsPerConn = 64
)

// ticker drives connection autoscaling. Window tuning is not done here — each
// connection gets its own kcp.AutoTuner goroutine (see startAutoTuners), which
// runs its own tuning loop.
func (c *Client) ticker(ctx context.Context) {
	connScaleTicker := time.NewTicker(connScaleInterval)
	defer connScaleTicker.Stop()

	// Start auto-tuners for initial connections
	c.startAutoTuners()

	for {
		select {
		case <-ctx.Done():
			return
		case <-connScaleTicker.C:
			c.scaleConnections(ctx)
		}
	}
}

// startAutoTuners binds a window auto-tuner to each connection in the pool.
func (c *Client) startAutoTuners() {
	type binding struct {
		tc   *timedConn
		conn tnet.Conn
	}

	c.mu.Lock()
	bindings := make([]binding, 0, len(c.iter.Items))
	for _, tc := range c.iter.Items {
		if tc.conn != nil {
			bindings = append(bindings, binding{tc, tc.conn})
		}
	}
	c.mu.Unlock()

	for _, b := range bindings {
		c.bindTuner(b.tc, b.conn)
	}
}

// bindTuner starts a window auto-tuner for conn and records its canceller on
// tc, stopping whatever tuner tc held before.
//
// AutoTuner captures its conn at construction and never rebinds, so a tuner is
// only ever valid for one conn: without this, replacing a conn left the old
// tuner running against a closed session while the new conn was never tuned.
//
// Tuners are parented on the client-lifetime ctx, not on the per-request ctx
// that reaches redial — a tuner must outlive the proxied connection that
// happened to trigger the redial.
func (c *Client) bindTuner(tc *timedConn, conn tnet.Conn) {
	kcpConn, ok := conn.(*kcp.Conn)
	if !ok {
		return
	}
	at := kcp.NewAutoTuner(
		kcpConn,
		c.cfg.Transport.KCP.Sndwnd,
		c.cfg.Transport.KCP.Rcvwnd,
	)
	ctx, cancel := context.WithCancel(c.tunerCtx)
	if !c.swapTuner(tc, conn, cancel) {
		cancel()
		return
	}
	go at.Run(ctx)
}

// swapTuner installs cancel as tc's tuner canceller and stops the previous one,
// reporting false if the binding is already stale.
//
// Callers reach here holding a (tc, conn) pair sampled before c.mu was
// released, so by now the slot may have been retired by scaleDown or given a
// different conn by redial. Storing the canceller anyway would either strand a
// tuner on a slot nobody can cancel from, or cancel the live conn's tuner and
// leave a zombie running against a closed session. Both are checked under the
// same lock that publishes tc.conn.
//
// The old canceller runs outside c.mu — cancelling is cheap, but the lock gates
// every new proxied connection and is not worth holding for it.
func (c *Client) swapTuner(tc *timedConn, conn tnet.Conn, cancel context.CancelFunc) bool {
	c.mu.Lock()
	if tc.retired || tc.conn != conn {
		c.mu.Unlock()
		return false
	}
	old := tc.stopTuner
	tc.stopTuner = cancel
	c.mu.Unlock()

	if old != nil {
		old()
	}
	return true
}

// scaleConnections grows or drains the pool by at most one conn per tick.
//
// Like newConn, it keeps dials and teardowns outside c.mu: holding the lock
// across either would block every new proxied connection for the duration,
// which on the scale-up path is a full pcap+KCP+smux dial and lands exactly
// when the pool is already saturated.
func (c *Client) scaleConnections(ctx context.Context) {
	// scaleUp reports how many slots existed before it ran, and only those are
	// eligible for retirement. A conn added this tick has zero streams, which is
	// exactly what scaleDown selects, so without that bound scale-down would
	// retire the conn scale-up just added — on every tick, forever.
	c.scaleDown(c.scaleUp(ctx))
}

// scaleUp adds one conn when every existing conn is saturated. It returns the
// number of slots that existed beforehand, whether or not it grew the pool.
func (c *Client) scaleUp(ctx context.Context) int {
	c.mu.Lock()
	numConns := len(c.iter.Items)
	allOverloaded := true
	for _, tc := range c.iter.Items {
		if tc.conn != nil && tc.conn.NumStreams() < maxStreamsPerConn {
			allOverloaded = false
			break
		}
	}
	c.mu.Unlock()

	if !allOverloaded || numConns >= c.maxConns {
		return numConns
	}

	tc, err := newTimedConn(c.cfg) // dial outside the lock
	if err != nil {
		flog.Errorf("autoscale: failed to create new connection: %v", err)
		return numConns
	}

	// tc is still unpublished here, so reading its conn needs no lock.
	conn := tc.conn

	c.mu.Lock()
	if len(c.iter.Items) >= c.maxConns {
		// The pool grew while we were dialing — discard rather than overshoot.
		c.mu.Unlock()
		tc.close()
		return numConns
	}
	c.iter.Items = append(c.iter.Items, tc)
	total := len(c.iter.Items)
	c.mu.Unlock()

	c.bindTuner(tc, conn)

	flog.Infof("autoscale: added connection (%d → %d), all had >%d streams",
		numConns, total, maxStreamsPerConn)
	return numConns
}

// scaleDown retires at most one idle conn, considering only the first
// `eligible` slots — see scaleConnections for why anything newer is off limits.
func (c *Client) scaleDown(eligible int) {
	c.mu.Lock()
	numConns := len(c.iter.Items)
	if eligible > numConns {
		eligible = numConns // the pool shrank under us
	}
	if eligible <= c.minConns {
		c.mu.Unlock()
		return
	}

	// Retire at most one idle conn per cycle. The conn is snapshotted under the
	// lock and closed outside it, and the slot is marked retired so an in-flight
	// newConn caller holding a stale reference cannot redial it back to life.
	//
	// A closed conn also reports zero streams and so is eligible here. That is
	// intended and keeps this consistent with pick, which deprioritises closed
	// conns: both treat a dead conn as something to shed rather than to route
	// traffic to. Retiring one only shrinks the pool above minConns, and
	// scaleUp regrows it under load.
	var victim tnet.Conn
	var stopTuner context.CancelFunc
	for i := eligible - 1; i >= c.minConns; i-- {
		tc := c.iter.Items[i]
		if tc.conn != nil && tc.conn.NumStreams() == 0 {
			victim, stopTuner = tc.conn, tc.stopTuner
			tc.retired, tc.stopTuner = true, nil
			c.iter.Items = append(c.iter.Items[:i], c.iter.Items[i+1:]...)
			break
		}
	}
	total := len(c.iter.Items)
	c.mu.Unlock()

	if victim == nil {
		return
	}
	// Stop the tuner before dropping the conn: nothing else holds a reference
	// to it once the slot leaves the pool, so it would otherwise tune a closed
	// session until process exit.
	if stopTuner != nil {
		stopTuner()
	}
	victim.Close()
	flog.Infof("autoscale: removed idle connection (%d → %d)", numConns, total)
}
