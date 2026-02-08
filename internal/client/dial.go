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
	autoExpire := 300
	tc := c.iter.Next()
	go tc.sendTCPF(tc.conn)
	err := tc.conn.Ping(false)
	if err != nil {
		flog.Infof("connection lost, retrying....")
		if tc.conn != nil {
			tc.conn.Close()
		}
		if c, err := tc.createConn(); err == nil {
			tc.conn = c
		}
		tc.expire = time.Now().Add(time.Duration(autoExpire) * time.Second)
	}
	return tc.conn, nil
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
