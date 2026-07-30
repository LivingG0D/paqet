package client

import (
	"context"
	"sync"

	"paqet/internal/conf"
	"paqet/internal/flog"
	"paqet/internal/pkg/iterator"
	"paqet/internal/tnet"
	"time"
)

type Client struct {
	cfg      *conf.Conf
	iter     *iterator.Iterator[*timedConn]
	udpPool  *udpPool
	mu       sync.Mutex
	minConns int
	maxConns int
	// tunerCtx is the client-lifetime context, replaced by the real one in
	// Start. Auto-tuners are parented on it rather than on the per-request ctx
	// that reaches redial, so a tuner is not killed by the proxied connection
	// that happened to trigger its rebind.
	tunerCtx context.Context
}

func New(cfg *conf.Conf) (*Client, error) {
	c := &Client{
		cfg:      cfg,
		iter:     &iterator.Iterator[*timedConn]{},
		udpPool:  &udpPool{strms: make(map[uint64]tnet.Strm)},
		minConns: cfg.Transport.Conn,
		maxConns: cfg.Transport.Conn * 2,
		tunerCtx: context.Background(),
	}
	return c, nil
}

// closeConns snapshots the live conns under c.mu, then closes them outside it.
// Both c.iter.Items (rewritten by scaleConnections) and timedConn.conn
// (reassigned by newConn) are mutated under c.mu, so neither the slice header
// nor the field is safe to read unsynchronized. Closing happens outside the
// lock so teardown never blocks newConn on the per-stream dial path.
func (c *Client) closeConns() {
	c.mu.Lock()
	conns := make([]tnet.Conn, 0, len(c.iter.Items))
	for _, tc := range c.iter.Items {
		if tc.conn != nil {
			conns = append(conns, tc.conn)
		}
	}
	c.mu.Unlock()

	for _, conn := range conns {
		conn.Close()
	}
}

func (c *Client) Start(ctx context.Context) error {
	// Auto-tuners parent off this; set before any conn exists so no tuner is
	// ever created against the placeholder from New.
	c.tunerCtx = ctx

	for i := range c.cfg.Transport.Conn {
		tc, err := newTimedConn(c.cfg)
		if err != nil {
			flog.Errorf("failed to create connection %d: %v", i+1, err)
			return err
		}
		flog.Debugf("client connection %d created successfully", i+1)
		c.iter.Items = append(c.iter.Items, tc)
	}

	stats := flog.NewStatsReporter(30 * time.Second)
	stats.SetConnFunc(func() []flog.ConnStats {
		c.mu.Lock()
		defer c.mu.Unlock()
		result := make([]flog.ConnStats, 0, len(c.iter.Items))
		for _, tc := range c.iter.Items {
			if tc.conn != nil {
				result = append(result, flog.ConnStats{
					Remote:  tc.conn.RemoteAddr().String(),
					Streams: tc.conn.NumStreams(),
				})
			}
		}
		return result
	})
	stats.Start()

	// Snapshot the startup count before the ticker goroutine can resize the pool.
	numConns := len(c.iter.Items)

	go c.ticker(ctx)
	go func() {
		<-ctx.Done()
		stats.Stop()
		c.closeConns()
	}()

	ipv4Addr := "<nil>"
	ipv6Addr := "<nil>"
	if c.cfg.Network.IPv4.Addr != nil {
		ipv4Addr = c.cfg.Network.IPv4.Addr.IP.String()
	}
	if c.cfg.Network.IPv6.Addr != nil {
		ipv6Addr = c.cfg.Network.IPv6.Addr.IP.String()
	}
	flog.Infof("Client started: IPv4:%s IPv6:%s -> %s (%d connections)", ipv4Addr, ipv6Addr, c.cfg.Server.Addr, numConns)
	return nil
}
