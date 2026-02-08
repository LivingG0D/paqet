package client

import (
	"context"
	"paqet/internal/conf"
	"paqet/internal/flog"
	"paqet/internal/pkg/iterator"
	"paqet/internal/tnet"
	"sync"
	"time"
)

type Client struct {
	cfg      *conf.Conf
	iter     *iterator.Iterator[*timedConn]
	udpPool  *udpPool
	mu       sync.Mutex
	minConns int
	maxConns int
}

func New(cfg *conf.Conf) (*Client, error) {
	c := &Client{
		cfg:      cfg,
		iter:     &iterator.Iterator[*timedConn]{},
		udpPool:  &udpPool{strms: make(map[uint64]tnet.Strm)},
		minConns: cfg.Transport.Conn,
		maxConns: cfg.Transport.Conn * 2,
	}
	return c, nil
}

func (c *Client) Start(ctx context.Context) error {
	for i := range c.cfg.Transport.Conn {
		tc, err := newTimedConn(ctx, c.cfg)
		if err != nil {
			flog.Errorf("failed to create connection %d: %v", i+1, err)
			return err
		}
		flog.Debugf("client connection %d created successfully", i+1)
		c.iter.Items = append(c.iter.Items, tc)
	}
	go c.ticker(ctx)

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

	go func() {
		<-ctx.Done()
		stats.Stop()
		for _, tc := range c.iter.Items {
			tc.close()
		}
		flog.Infof("client shutdown complete")
	}()

	ipv4Addr := "<nil>"
	ipv6Addr := "<nil>"
	if c.cfg.Network.IPv4.Addr != nil {
		ipv4Addr = c.cfg.Network.IPv4.Addr.IP.String()
	}
	if c.cfg.Network.IPv6.Addr != nil {
		ipv6Addr = c.cfg.Network.IPv6.Addr.IP.String()
	}
	flog.Infof("Client started: IPv4:%s IPv6:%s -> %s (%d connections)", ipv4Addr, ipv6Addr, c.cfg.Server.Addr, len(c.iter.Items))
	return nil
}
