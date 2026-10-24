//go:build linux

package kcp

import (
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"golang.org/x/net/ipv4"
)

// batchConn is a net.PacketConn that is not a UDP socket but batches writes,
// like socket.PacketConn on Linux. It counts how kcp-go sends.
type batchConn struct {
	writeTo, writeBatch atomic.Int32
	closed              chan struct{}
}

func (c *batchConn) ReadFrom([]byte) (int, net.Addr, error) {
	<-c.closed
	return 0, nil, net.ErrClosed
}
func (c *batchConn) WriteTo(b []byte, _ net.Addr) (int, error) { c.writeTo.Add(1); return len(b), nil }
func (c *batchConn) WriteBatch(ms []ipv4.Message, _ int) (int, error) {
	c.writeBatch.Add(1)
	return len(ms), nil
}
func (c *batchConn) Close() error                     { close(c.closed); return nil }
func (c *batchConn) LocalAddr() net.Addr              { return nil }
func (c *batchConn) SetDeadline(time.Time) error      { return nil }
func (c *batchConn) SetReadDeadline(time.Time) error  { return nil }
func (c *batchConn) SetWriteDeadline(time.Time) error { return nil }

// The vendored kcp-go (third_party/kcp-go) must send through WriteBatch when the
// conn offers it; upstream kcp-go batches only for a real *net.UDPConn.
func TestKCPSendsThroughWriteBatch(t *testing.T) {
	pc := &batchConn{closed: make(chan struct{})}
	sess, err := kcp.NewConn("127.0.0.1:9", nil, 0, 0, pc)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	sess.SetWriteDelay(false)
	if _, err := sess.Write(make([]byte, 64*1024)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for pc.writeBatch.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pc.writeBatch.Load() == 0 || pc.writeTo.Load() != 0 {
		t.Fatalf("WriteBatch calls = %d, WriteTo calls = %d; want batches only", pc.writeBatch.Load(), pc.writeTo.Load())
	}
}
