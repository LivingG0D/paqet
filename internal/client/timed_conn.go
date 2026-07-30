package client

import (
	"context"
	"sync"
	"time"

	"paqet/internal/conf"
	"paqet/internal/protocol"
	"paqet/internal/tnet"
	"paqet/internal/tnet/kcp"
)

// tcpfWriteTimeout bounds the PTCPF control write. smux applies no write
// deadline of its own, so without this a stalled peer parks the write until the
// session's keepalive timeout (90s by default here).
const tcpfWriteTimeout = 10 * time.Second

type timedConn struct {
	cfg *conf.Conf
	// dialMu serialises redials of this slot. Held across the dial, so it must
	// never be taken while holding Client.mu (see Client.redial).
	dialMu sync.Mutex
	// conn, retired and stopTuner are guarded by Client.mu. retired marks a slot
	// that scaleDown has removed from the pool, so a redial cannot revive it.
	// stopTuner cancels the auto-tuner bound to the current conn.
	conn      tnet.Conn
	retired   bool
	stopTuner context.CancelFunc
}

func newTimedConn(cfg *conf.Conf) (*timedConn, error) {
	var err error
	tc := timedConn{cfg: cfg}
	tc.conn, err = tc.createConn()
	if err != nil {
		return nil, err
	}

	return &tc, nil
}

func (tc *timedConn) createConn() (tnet.Conn, error) {
	conn, err := kcp.Dial(tc.cfg.Server.Addr, tc.cfg.Transport.KCP, tc.cfg.Network)
	if err != nil {
		return nil, err
	}
	if err := tc.sendTCPF(conn); err != nil {
		// The dial succeeded, so close it rather than leaking the session.
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (tc *timedConn) sendTCPF(conn tnet.Conn) error {
	strm, err := conn.OpenStrm()
	if err != nil {
		return err
	}
	defer strm.Close()

	if err := strm.SetWriteDeadline(time.Now().Add(tcpfWriteTimeout)); err != nil {
		return err
	}

	p := protocol.Proto{Type: protocol.PTCPF, TCPF: tc.cfg.Network.TCP.RF}
	return p.Write(strm)
}

func (tc *timedConn) close() {
	if tc.conn != nil {
		tc.conn.Close()
	}
}
