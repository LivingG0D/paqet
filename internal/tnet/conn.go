package tnet

import (
	"net"
	"time"
)

type Conn interface {
	OpenStrm() (Strm, error)
	AcceptStrm() (Strm, error)
	NumStreams() int
	// IsClosed reports whether the session is dead. NumStreams returns 0 for a
	// closed session, which is indistinguishable from idle, so callers ranking
	// conns by load need this to tell the two apart.
	IsClosed() bool
	Ping(wait bool) error
	Close() error
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}
