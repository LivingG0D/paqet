package client

import (
	"context"
	"fmt"
	"time"

	"paqet/internal/flog"
	"paqet/internal/tnet"
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

	autoExpire := 300
	go bestTC.sendTCPF(bestTC.conn)
	err := bestTC.conn.Ping(false)
	if err != nil {
		flog.Infof("connection lost, retrying....")
		if bestTC.conn != nil {
			bestTC.conn.Close()
		}
		conn, err := bestTC.createConn()
		if err != nil {
			return nil, err
		}
		bestTC.conn = conn
		bestTC.expire = time.Now().Add(time.Duration(autoExpire) * time.Second)
	}
	return bestTC.conn, nil
}

func (c *Client) newStrm(ctx context.Context) (tnet.Strm, error) {
	const maxRetries = 5
	for i := range maxRetries {
		// Backoff before every attempt but the first: 100ms, 200ms, 400ms, 800ms.
		// Bounded + ctx-aware so a persistently failing server neither hot-loops
		// nor blocks shutdown.
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<(i-1)) * 100 * time.Millisecond):
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := c.newConn()
		if err != nil {
			flog.Debugf("session creation failed (attempt %d/%d), retrying: %v", i+1, maxRetries, err)
			continue
		}
		strm, err := conn.OpenStrm()
		if err != nil {
			flog.Debugf("failed to open stream (attempt %d/%d), retrying: %v", i+1, maxRetries, err)
			continue
		}
		return strm, nil
	}
	return nil, fmt.Errorf("failed to open stream after %d retries", maxRetries)
}
