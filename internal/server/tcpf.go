package server

import (
	"paqet-plus/internal/protocol"
	"paqet-plus/internal/tnet"
)

func (s *Server) handleTCPF(strm tnet.Strm, p *protocol.Proto) {
	if len(p.TCPF) == 0 {
		s.listener.DeleteClientTCPF(strm.RemoteAddr())
		return
	}
	s.listener.SetClientTCPF(strm.RemoteAddr(), p.TCPF)
}
