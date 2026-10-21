package kcp

import (
	"net"
	"paqet/internal/socket"
	"paqet/internal/tnet"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

type Conn struct {
	PacketConn *socket.PacketConn
	UDPSession *kcp.UDPSession
	Session    *smux.Session
}

func (c *Conn) OpenStrm() (tnet.Strm, error) {
	strm, err := c.Session.OpenStream()
	if err != nil {
		return nil, err
	}
	return &Strm{strm}, nil
}

func (c *Conn) AcceptStrm() (tnet.Strm, error) {
	strm, err := c.Session.AcceptStream()
	if err != nil {
		return nil, err
	}
	return &Strm{strm}, nil
}

func (c *Conn) IsClosed() bool { return c.Session.IsClosed() }

func (c *Conn) Close() error {
	var err error
	if c.UDPSession != nil {
		c.UDPSession.Close()
	}
	if c.Session != nil {
		c.Session.Close()
	}
	if c.PacketConn != nil {
		c.PacketConn.Close()
	}
	return err
}

func (c *Conn) LocalAddr() net.Addr                { return c.Session.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.Session.RemoteAddr() }
func (c *Conn) NumStreams() int                    { return c.Session.NumStreams() }
func (c *Conn) SetDeadline(t time.Time) error      { return c.Session.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.UDPSession.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.UDPSession.SetWriteDeadline(t) }

func (c *Conn) SetWindowSize(snd, rcv int) {
	c.UDPSession.SetWindowSize(snd, rcv)
}
