package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"paqet/internal/flog"
	"paqet/internal/tnet"
)

var (
	errNoConns     = errors.New("client: no connections available")
	errSlotRetired = errors.New("client: connection slot retired")
)

// pick returns the least-loaded slot along with a snapshot of its conn. c.mu is
// held only for the scan; the caller health-checks and redials outside it.
func (c *Client) pick() (*timedConn, tnet.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var best, dead *timedConn
	bestStreams := int(^uint(0) >> 1) // max int
	for _, tc := range c.iter.Items {
		if tc.conn == nil {
			continue
		}
		if tc.conn.IsClosed() {
			// A closed session reports zero streams, so ranking it by load alone
			// would make it out-rank every healthy conn and absorb all traffic,
			// while its redial keeps failing. Keep it only as a last resort.
			if dead == nil {
				dead = tc
			}
			continue
		}
		if n := tc.conn.NumStreams(); n < bestStreams {
			bestStreams, best = n, tc
		}
	}

	if best == nil {
		// Nothing live: hand back a dead slot so it gets redialed rather than
		// failing outright. redial serialises on the slot, and callers that lose
		// that race reuse the winner's conn.
		best = dead
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

// newConn hands back a usable conn and the slot holding it, redialing when the
// slot is dead.
//
// This runs once per proxied connection, so it does no I/O of its own. It used
// to do two smux stream round-trips here — a Ping(false) liveness probe and a
// re-send of the static TCPF config — which cost a SYN+FIN control frame pair
// each, plus a server-side stream that read EOF and logged an error. Liveness
// is now a local IsClosed check, and TCPF is sent once per session by
// createConn, where it belongs.
//
// c.mu is held only to pick a slot and to publish a replacement, never across a
// redial: that can block for tens of seconds, and c.mu gates every new proxied
// connection plus the stats reporter, the autoscaler and shutdown.
func (c *Client) newConn(ctx context.Context) (*timedConn, tnet.Conn, error) {
	tc, conn := c.pick()
	if tc == nil {
		return nil, nil, errNoConns
	}

	if conn != nil && !conn.IsClosed() {
		return tc, conn, nil
	}
	if conn != nil {
		flog.Infof("connection lost, retrying....")
	}

	fresh, err := c.redial(ctx, tc, conn)
	if err != nil {
		return nil, nil, err
	}
	return tc, fresh, nil
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
	cur, retired := tc.conn, tc.retired
	c.mu.Unlock()
	if retired {
		// scaleDown dropped this slot while we waited; redialing it would leak
		// a conn nothing owns. Let the caller retry against a live slot.
		return nil, errSlotRetired
	}
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

	if !c.publish(tc, conn) {
		// Retired while we were dialing. The check above cannot cover this:
		// createConn takes long enough for a scale-down tick to land, and a slot
		// whose conn is closed reports zero streams — exactly what scaleDown
		// looks for. Publishing now would strand this conn outside the pool
		// where nothing closes it, with a tuner nothing cancels.
		conn.Close()
		return nil, errSlotRetired
	}

	// Rebind the auto-tuner: it captured the conn we just replaced, so without
	// this the old tuner would keep tuning a closed session and the new conn
	// would never be tuned at all.
	c.bindTuner(tc, conn)
	return conn, nil
}

// publish installs conn as tc's conn, reporting false if the slot was retired
// meanwhile — in which case the conn belongs to no one and the caller closes it.
func (c *Client) publish(tc *timedConn, conn tnet.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tc.retired {
		return false
	}
	tc.conn = conn
	return true
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
		tc, conn, err := c.newConn(ctx)
		if err != nil {
			flog.Debugf("session creation failed (attempt %d/%d), retrying: %v", i+1, maxRetries, err)
			continue
		}

		strm, err := conn.OpenStrm()
		if err == nil {
			return strm, nil
		}

		// OpenStrm is the real liveness probe, and the only one that costs
		// nothing extra: it fails when the session is closed, has gone away, or
		// has exhausted stream IDs — the last two of which IsClosed cannot see.
		// Redial the slot rather than retrying against a conn that will just
		// refuse again.
		flog.Debugf("failed to open stream (attempt %d/%d), redialing: %v", i+1, maxRetries, err)
		if _, rerr := c.redial(ctx, tc, conn); rerr != nil {
			flog.Debugf("redial after stream failure: %v", rerr)
		}
	}
	return nil, fmt.Errorf("failed to open stream after %d retries", maxRetries)
}
