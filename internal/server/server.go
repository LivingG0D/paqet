package server

import (
	"context"
	"fmt"

	"paqet/internal/conf"
	"paqet/internal/flog"
	"paqet/internal/tnet"
	"paqet/internal/tnet/kcp"
	"time"
)

type Server struct {
	cfg      *conf.Conf
	listener tnet.Listener
	done     chan error
}

func New(cfg *conf.Conf) (*Server, error) {
	s := &Server{cfg: cfg, done: make(chan error, 1)}
	return s, nil
}

// Done delivers the error that stopped the accept loop outside of shutdown.
func (s *Server) Done() <-chan error { return s.done }

func (s *Server) Start(ctx context.Context) error {
	listener, err := kcp.Listen(s.cfg.Transport.KCP, s.cfg.Network)
	if err != nil {
		return fmt.Errorf("could not start KCP listener: %w", err)
	}
	s.listener = listener
	flog.Infof("server listening for packets on :%d", s.cfg.Network.Port)

	stats := flog.NewStatsReporter(30 * time.Second)
	stats.Start()

	go s.listen(ctx, listener)
	context.AfterFunc(ctx, func() {
		stats.Stop()
		listener.Close()
	})

	return nil
}

func (s *Server) listen(ctx context.Context, listener tnet.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				// kcp-go stops reading after one socket error and fails every Accept
				// from then on; report it so the process exits and gets restarted.
				s.done <- err
			}
			return
		}
		flog.Infof("accepted new connection from %s (local: %s)", conn.RemoteAddr(), conn.LocalAddr())

		go func() {
			defer conn.Close()
			defer s.listener.DeleteClientTCPF(conn.RemoteAddr())
			s.handleConn(ctx, conn)
		}()
	}
}
