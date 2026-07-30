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
	c.startAutoTuners(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-connScaleTicker.C:
			c.scaleConnections(ctx)
		}
	}
}

// startAutoTuners launches a window auto-tuner goroutine per KCP connection.
func (c *Client) startAutoTuners(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, tc := range c.iter.Items {
		if kcpConn, ok := tc.conn.(*kcp.Conn); ok {
			at := kcp.NewAutoTuner(
				kcpConn,
				c.cfg.Transport.KCP.Sndwnd,
				c.cfg.Transport.KCP.Rcvwnd,
			)
			go at.Run(ctx)
		}
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

	// Start auto-tuner for new connection
	if kcpConn, ok := tc.conn.(*kcp.Conn); ok {
		at := kcp.NewAutoTuner(
			kcpConn,
			c.cfg.Transport.KCP.Sndwnd,
			c.cfg.Transport.KCP.Rcvwnd,
		)
		go at.Run(ctx)
	}

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
	for i := numConns - 1; i >= c.minConns; i-- {
		tc := c.iter.Items[i]
		if tc.conn != nil && tc.conn.NumStreams() == 0 {
			victim, tc.retired = tc.conn, true
			c.iter.Items = append(c.iter.Items[:i], c.iter.Items[i+1:]...)
			break
		}
	}
	total := len(c.iter.Items)
	c.mu.Unlock()

	if victim == nil {
		return
	}
	victim.Close()
	flog.Infof("autoscale: removed idle connection (%d → %d)", numConns, total)
}
