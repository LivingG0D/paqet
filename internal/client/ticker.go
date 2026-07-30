package client

import (
	"context"
	"paqet/internal/flog"
	"paqet/internal/tnet"
	"time"
)

const (
	connScaleInterval = 30 * time.Second
	// streamsHigh arms scale-up: the pool grows when the MEAN stream count per
	// routable conn exceeds it. One proxied connection is one stream, so this is
	// the concurrent-user signal. 32 is the watermark flog's stats reporter
	// already alerts on ("stream_overload — increase conn count", stats.go:174);
	// the autoscaler now acts on it instead of asking the operator to.
	streamsHigh = 32
	// streamsLow arms scale-down. The 4:1 gap against streamsHigh is hysteresis,
	// and it is what makes it arithmetically impossible for scale-down to retire
	// the conn scale-up just added: growing needs streams > (usable-1)*32,
	// shrinking needs streams < usable*8, and both hold only when usable < 2 —
	// which scaleDown already rejects at the minConns gate.
	streamsLow = 8
)

// ticker drives connection autoscaling. The pool tracks live stream count,
// which is the proxy for concurrent users.
//
// Window sizes are not tuned here. They are set once per session from the
// operator's config (tnet/kcp.aplConf). A loss-driven window controller used to
// live in this file; it was removed because on this transport 8% retransmission
// is the documented normal operating point (commit 9d5c72c), so any loss
// threshold low enough to react is below the baseline and only ratchets the
// window down — and on a DPI-evasion tunnel that hands a censor a throttle.
func (c *Client) ticker(ctx context.Context) {
	t := time.NewTicker(connScaleInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.repairDead(ctx)
			c.scaleUp()
			c.scaleDown()
		}
	}
}

// deadSlots returns the slots whose session has died, with the conn each was
// holding when we looked.
func (c *Client) deadSlots() ([]*timedConn, []tnet.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var slots []*timedConn
	var conns []tnet.Conn
	for _, tc := range c.iter.Items {
		if tc.conn != nil && tc.conn.IsClosed() {
			slots = append(slots, tc)
			conns = append(conns, tc.conn)
		}
	}
	return slots, conns
}

// repairDead redials slots whose session has died.
//
// Nothing on the request path will do it: pick deliberately routes traffic away
// from a closed conn, so a dead slot sitting alongside live ones is never
// handed out and therefore never redialed. Without this it stays dead for the
// life of the process — and because a closed conn reports zero streams, it also
// drags the pool mean down, which switches scale-up off (see poolLoad). One
// blip would otherwise cost a connection permanently and disable autoscaling
// with it.
func (c *Client) repairDead(ctx context.Context) {
	slots, conns := c.deadSlots()
	for i, tc := range slots {
		if ctx.Err() != nil {
			return
		}
		if _, err := c.redial(ctx, tc, conns[i]); err != nil {
			flog.Debugf("autoscale: failed to repair dead connection: %v", err)
			continue
		}
		flog.Infof("autoscale: repaired a dead connection")
	}
}

// poolLoad reports the live stream total over the conns that can actually carry
// streams, how many such conns there are, and the pool size.
//
// A nil conn (mid-dial) or a closed one is not capacity — repairDead owns the
// closed ones — so counting either would understate the mean and switch
// scale-up off exactly when the pool is most loaded.
func (c *Client) poolLoad() (streams, usable, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, tc := range c.iter.Items {
		if tc.conn == nil || tc.conn.IsClosed() {
			continue
		}
		streams += tc.conn.NumStreams()
		usable++
	}
	return streams, usable, len(c.iter.Items)
}

// scaleUp adds one conn when the mean load per routable conn is above
// streamsHigh.
//
// The predicate used to be universally quantified — every conn had to be at the
// cap — so one conn a single stream short vetoed growth for the whole pool, and
// each conn scale-up did add arrived with zero streams and vetoed the next tick
// itself. With conn: 8 that needed ~512 concurrent streams before the pool grew
// at all, which is why in practice it never did. A mean is a closed loop:
// adding a conn lowers it because it added capacity.
//
// The dial happens outside c.mu. Holding the lock across a full pcap+KCP+smux
// dial would block every new proxied connection for its duration, and it lands
// exactly when the pool is already overloaded.
func (c *Client) scaleUp() {
	streams, usable, total := c.poolLoad()
	if usable == 0 || streams <= usable*streamsHigh || total >= c.maxConns {
		return
	}

	tc, err := newTimedConn(c.cfg)
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
	grown := len(c.iter.Items)
	c.mu.Unlock()

	flog.Infof("autoscale: added connection (%d → %d), %d streams over %d conns",
		total, grown, streams, usable)
}

// scaleDown retires at most one idle conn when the mean load falls below
// streamsLow.
//
// The victim must still carry zero streams. pick is join-shortest-queue, so a
// conn holding even one stream is one a user is actively on; the pool therefore
// drains as connections finish rather than on demand. That is deliberate — the
// alternative is a drain state machine, and a slot marked for draining that
// never empties is a conn nothing ever closes.
func (c *Client) scaleDown() {
	streams, usable, _ := c.poolLoad()
	if usable <= c.minConns || streams >= usable*streamsLow {
		return
	}

	// The conn is snapshotted under the lock and closed outside it: smux Close
	// is bounded at 30s, then a KCP flush and a pcap handle close, and c.mu
	// gates every new proxied connection. The slot is marked retired so an
	// in-flight newConn caller holding a stale reference cannot redial it back
	// to life.
	//
	// A closed conn also reports zero streams and so is eligible here. That is
	// intended and keeps this consistent with pick, which deprioritises closed
	// conns: both treat a dead conn as something to shed rather than to route
	// traffic to.
	var victim tnet.Conn
	c.mu.Lock()
	before := len(c.iter.Items)
	for i := before - 1; i >= 0; i-- {
		tc := c.iter.Items[i]
		if tc.conn != nil && tc.conn.NumStreams() == 0 {
			victim = tc.conn
			tc.retired = true
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
	flog.Infof("autoscale: removed idle connection (%d → %d), %d streams over %d conns",
		before, total, streams, usable)
}
