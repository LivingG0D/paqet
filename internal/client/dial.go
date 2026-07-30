package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"paqet/internal/flog"
	"paqet/internal/tnet"
)

var errNoConns = errors.New("client: no connections available")

// pick returns the least-loaded slot along with a snapshot of its conn. c.mu is
// held only for the scan; the caller health-checks and redials outside it.
func (c *Client) pick() (*timedConn, tnet.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var best *timedConn
	bestStreams := int(^uint(0) >> 1) // max int
	for _, tc := range c.iter.Items {
		if tc.conn == nil {
			continue
		}
		if n := tc.conn.NumStreams(); n < bestStreams {
			bestStreams, best = n, tc
		}
	}

	if best == nil {
		if len(c.iter.Items) == 0 {
			return nil, nil
		}
		// Every slot is empty (all mid-dial) — fall back to round-robin.
		best = c.iter.Next()
	}
	return best, best.conn
}

// newConn hands back a usable conn, redialing the least-loaded slot when its
// health check fails.
//
// c.mu is held only to pick a slot and to publish a replacement — never across
// Ping, Close, or a redial. Each of those can block for tens of seconds (smux
// bounds OpenStream and Close at openCloseTimeout, 30s, and a data frame is
// bounded only by whatever deadline the caller sets), and c.mu gates every new
// proxied connection plus the stats reporter, the autoscaler and shutdown. Held
// across that I/O, one stalled conn wedges the whole client.
func (c *Client) newConn(ctx context.Context) (tnet.Conn, error) {
	tc, conn := c.pick()
	if tc == nil {
		return nil, errNoConns
	}

	if conn != nil {
		go func() {
			if err := tc.sendTCPF(conn); err != nil {
				flog.Debugf("failed to send TCPF config: %v", err)
			}
		}()
		if err := conn.Ping(false); err == nil {
			return conn, nil
		}
		flog.Infof("connection lost, retrying....")
	}

	return c.redial(ctx, tc, conn)
}

// redial replaces tc's conn with a fresh one. dialMu serialises redials of a
// single slot, so N callers that all find the same conn dead produce one dial
// rather than N. stale is the conn the caller found unhealthy; if the slot
// already holds something else by the time we get dialMu, another caller
// redialed it and we reuse their result.
//
// Lock order is dialMu -> c.mu, never the reverse: nothing that holds c.mu ever
// reaches for dialMu.
func (c *Client) redial(ctx context.Context, tc *timedConn, stale tnet.Conn) (tnet.Conn, error) {
	tc.dialMu.Lock()
	defer tc.dialMu.Unlock()

	c.mu.Lock()
	cur := tc.conn
	c.mu.Unlock()
	if cur != nil && cur != stale {
		return cur, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if stale != nil {
		stale.Close()
	}

	conn, err := tc.createConn()
	if err != nil {
		// Leave tc.conn pointing at the dead conn rather than nilling it: pick
		// skips nil slots, so a nilled slot would never be retried and the pool
		// would shrink permanently.
		return nil, err
	}

	c.mu.Lock()
	tc.conn = conn
	c.mu.Unlock()
	return conn, nil
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
		conn, err := c.newConn(ctx)
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
