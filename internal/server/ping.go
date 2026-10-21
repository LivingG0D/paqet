package server

import (
	"paqet-plus/internal/flog"
	"paqet-plus/internal/protocol"
	"paqet-plus/internal/tnet"
)

func (s *Server) handlePing(strm tnet.Strm) {
	p := protocol.Proto{Type: protocol.PPONG}
	if err := p.Write(strm); err != nil {
		flog.Errorf("failed to send pong on stream %d: %v", strm.SID(), err)
	}
}
