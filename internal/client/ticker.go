package client

import (
	"context"
	"paqet/internal/flog"
	"paqet/internal/tnet/kcp"
	"time"
)

const (
	windowTuneInterval = 10 * time.Second
	connScaleInterval  = 30 * time.Second
	maxStreamsPerConn  = 64
	idleTimeout        = 60 * time.Second
)

func (c *Client) ticker(ctx context.Context) {
	windowTicker := time.NewTicker(windowTuneInterval)
	connScaleTicker := time.NewTicker(connScaleInterval)
	defer windowTicker.Stop()
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
