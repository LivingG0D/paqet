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

func (c *Client) ticker(ctx context.Context) {
	connScaleTicker := time.NewTicker(connScaleInterval)
	defer connScaleTicker.Stop()

	// Start auto-tuners for initial connections
	c.mu.Lock()
	for _, tc := range c.iter.Items {
		c.startAutoTuner(ctx, tc.conn)
	}
	c.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case <-connScaleTicker.C:
			c.scaleConnections(ctx)
		}
	}
}

// startAutoTuner launches a window auto-tuner for a KCP connection; it stops when the connection closes.
func (c *Client) startAutoTuner(ctx context.Context, conn tnet.Conn) {
	if kcpConn, ok := conn.(*kcp.Conn); ok {
		at := kcp.NewAutoTuner(
			kcpConn,
			c.cfg.Transport.KCP.Sndwnd,
			c.cfg.Transport.KCP.Rcvwnd,
		)
		go at.Run(ctx)
	}
}

// scaleConnections checks stream counts and spawns/drains connections.
func (c *Client) scaleConnections(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	numConns := len(c.iter.Items)

	// Check if all connections are overloaded
	allOverloaded := true
	for _, tc := range c.iter.Items {
		if tc.conn != nil && tc.conn.NumStreams() < maxStreamsPerConn {
			allOverloaded = false
			break
		}
	}

	// Scale up: all connections overloaded and below max
	if allOverloaded && numConns < c.maxConns {
		tc, err := newTimedConn(ctx, c.cfg)
		if err != nil {
			flog.Errorf("autoscale: failed to create new connection: %v", err)
			return
		}
		c.iter.Items = append(c.iter.Items, tc)
		c.startAutoTuner(ctx, tc.conn)

		flog.Infof("autoscale: added connection (%d → %d), all had >%d streams",
			numConns, len(c.iter.Items), maxStreamsPerConn)
	}

	// Scale down: find idle connections beyond minimum
	if numConns > c.minConns {
		for i := numConns - 1; i >= c.minConns; i-- {
			tc := c.iter.Items[i]
			if tc.conn != nil && tc.conn.NumStreams() == 0 {
				tc.close()
				c.iter.Items = append(c.iter.Items[:i], c.iter.Items[i+1:]...)
				flog.Infof("autoscale: removed idle connection (%d → %d)",
					numConns, len(c.iter.Items))
				break // Remove at most one per cycle
			}
		}
	}
}
