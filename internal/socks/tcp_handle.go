package socks

import (
	"net"
	"paqet/internal/flog"
	"paqet/internal/pkg/buffer"

	"github.com/txthinking/socks5"
)

func (h *Handler) TCPHandle(server *socks5.Server, conn *net.TCPConn, r *socks5.Request) error {
	if r.Cmd == socks5.CmdUDP {
		flog.Debugf("SOCKS5 UDP_ASSOCIATE from %s", conn.RemoteAddr())
		return h.handleUDPAssociate(conn)
	}

	if r.Cmd == socks5.CmdConnect {
		flog.Debugf("SOCKS5 CONNECT from %s to %s", conn.RemoteAddr(), r.Address())
		return h.handleTCPConnect(conn, r)
	}

	flog.Debugf("unsupported SOCKS5 command %d from %s", r.Cmd, conn.RemoteAddr())
	return nil
}

func (h *Handler) handleTCPConnect(conn *net.TCPConn, r *socks5.Request) error {
	flog.Infof("SOCKS5 accepted TCP connection %s -> %s", conn.RemoteAddr(), r.Address())

	if err := writeReply(conn); err != nil {
		return err
	}

	strm, err := h.client.TCP(r.Address())
	if err != nil {
		flog.Errorf("SOCKS5 failed to establish stream for %s -> %s: %v", conn.RemoteAddr(), r.Address(), err)
		return err
	}
	defer strm.Close()
	flog.Debugf("SOCKS5 stream %d created for %s -> %s", strm.SID(), conn.RemoteAddr(), r.Address())

	errCh := make(chan error, 2)
	go func() {
		err := buffer.CopyT(conn, strm)
		errCh <- err
	}()
	go func() {
		err := buffer.CopyT(strm, conn)
		errCh <- err
	}()

	select {
	case err := <-errCh:
		if err != nil {
			flog.Errorf("SOCKS5 stream %d failed for %s -> %s: %v", strm.SID(), conn.RemoteAddr(), r.Address(), err)
		}
	case <-h.ctx.Done():
		flog.Debugf("SOCKS5 connection %s -> %s closed due to shutdown", conn.RemoteAddr(), r.Address())
	}

	flog.Debugf("SOCKS5 connection %s -> %s closed", conn.RemoteAddr(), r.Address())
	return nil
}
