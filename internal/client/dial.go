package client

import (
	"fmt"
	"paqet/internal/flog"
	"paqet/internal/tnet"
	"time"
)

func (c *Client) newConn() (tnet.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Find the least-loaded connection (fewest active streams)
	var bestTC *timedConn
	bestStreams := int(^uint(0) >> 1) // max int
	for _, tc := range c.iter.Items {
		if tc.conn == nil {
			continue
		}
		n := tc.conn.NumStreams()
		if n < bestStreams {
			bestStreams = n
			bestTC = tc
		}
	}

	if bestTC == nil {
		// Fallback to round-robin
		bestTC = c.iter.Next()
	}

	// No network I/O under c.mu unless the session is already dead.
	if bestTC.conn.IsClosed() {
		flog.Infof("connection lost, retrying....")
		bestTC.conn.Close()
		if conn, err := bestTC.createConn(); err == nil {
			bestTC.conn = conn
			c.startAutoTuner(bestTC.ctx, conn)
		}
	}
	return bestTC.conn, nil
}

func (c *Client) newStrm() (tnet.Strm, error) {
	const maxRetries = 5
	for i := range maxRetries {
		conn, err := c.newConn()
		if err != nil {
			flog.Debugf("session creation failed (attempt %d/%d), retrying", i+1, maxRetries)
			time.Sleep(time.Duration(1<<i) * 100 * time.Millisecond)
			continue
		}
		strm, err := conn.OpenStrm()
		if err != nil {
			flog.Debugf("failed to open stream (attempt %d/%d), retrying: %v", i+1, maxRetries, err)
			time.Sleep(time.Duration(1<<i) * 100 * time.Millisecond)
			continue
		}
		return strm, nil
	}
	return nil, fmt.Errorf("failed to open stream after %d retries", maxRetries)
}
