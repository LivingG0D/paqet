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
	c.swapTuner(tc, cancel)
	go at.Run(ctx)
}

// swapTuner installs cancel as tc's tuner canceller and stops the previous one.
// The old canceller runs outside c.mu — cancel itself is cheap, but the lock
// gates every new proxied connection and is not worth holding for it.
func (c *Client) swapTuner(tc *timedConn, cancel context.CancelFunc) {
	c.mu.Lock()
	old := tc.stopTuner
	tc.stopTuner = cancel
	c.mu.Unlock()

	if old != nil {
		old()
	}
}

// scaleConnections grows or drains the pool by at most one conn per tick.
//
// Like newConn, it keeps dials and teardowns outside c.mu: holding the lock
// across either would block every new proxied connection for the duration,
// which on the scale-up path is a full pcap+KCP+smux dial and lands exactly
// when the pool is already saturated.
func (c *Client) scaleConnections(ctx context.Context) {
	c.scaleUp(ctx)
	c.scaleDown()
}

func (c *Client) scaleUp(ctx context.Context) {
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
		return
	}

	tc, err := newTimedConn(c.cfg) // dial outside the lock
	if err != nil {
		flog.Errorf("autoscale: failed to create new connection: %v", err)
		return
	}

	// tc is still unpublished here, so reading its conn needs no lock.
	conn := tc.conn

	c.mu.Lock()
	if len(c.iter.Items) >= c.maxConns {
		// The pool grew while we were dialing — discard rather than overshoot.
		c.mu.Unlock()
		tc.close()
		return
	}
	c.iter.Items = append(c.iter.Items, tc)
	total := len(c.iter.Items)
	c.mu.Unlock()

	c.bindTuner(tc, conn)

	flog.Infof("autoscale: added connection (%d → %d), all had >%d streams",
		numConns, total, maxStreamsPerConn)
}

func (c *Client) scaleDown() {
	c.mu.Lock()
	numConns := len(c.iter.Items)
	if numConns <= c.minConns {
		c.mu.Unlock()
		return
	}

	// Retire at most one idle conn per cycle. The conn is snapshotted under the
	// lock and closed outside it, and the slot is marked retired so an in-flight
	// newConn caller holding a stale reference cannot redial it back to life.
	var victim tnet.Conn
	var stopTuner context.CancelFunc
	for i := numConns - 1; i >= c.minConns; i-- {
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
