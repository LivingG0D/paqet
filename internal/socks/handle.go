package socks

import (
	"context"
	"net"
	"paqet/internal/client"

	"github.com/txthinking/socks5"
)

type Handler struct {
	client *client.Client
	ctx    context.Context
}

// writeReply sends a SOCKS5 success reply that carries the local address of conn.
func writeReply(conn *net.TCPConn) error {
	addr := conn.LocalAddr().(*net.TCPAddr)
	buf := make([]byte, 0, 4+16+2) // header + IPv6 + port
	buf = append(buf, socks5.Ver, socks5.RepSuccess, 0x00)
	if ip4 := addr.IP.To4(); ip4 != nil {
		buf = append(buf, socks5.ATYPIPv4)
		buf = append(buf, ip4...)
	} else {
		buf = append(buf, socks5.ATYPIPv6)
		buf = append(buf, addr.IP.To16()...)
	}
	buf = append(buf, byte(addr.Port>>8), byte(addr.Port&0xff))
	_, err := conn.Write(buf)
	return err
}
