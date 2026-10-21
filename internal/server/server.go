package server

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"paqet/internal/conf"
	"paqet/internal/flog"
	"paqet/internal/socket"
	"paqet/internal/tnet"
	"paqet/internal/tnet/kcp"
	"time"
)

type Server struct {
	cfg   *conf.Conf
	pConn *socket.PacketConn
	wg    sync.WaitGroup
}

func New(cfg *conf.Conf) (*Server, error) {
	s := &Server{
		cfg: cfg,
	}

	return s, nil
}

func (s *Server) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		flog.Infof("Shutdown signal received, initiating graceful shutdown...")
		cancel()
	}()

	pConn, err := socket.New(ctx, &s.cfg.Network)
	if err != nil {
		return fmt.Errorf("could not create raw packet conn: %w", err)
	}
	s.pConn = pConn

	listener, err := kcp.Listen(s.cfg.Transport.KCP, pConn)
	if err != nil {
		return fmt.Errorf("could not start KCP listener: %w", err)
	}
	defer listener.Close()
	flog.Infof("Server started - listening for packets on :%d", s.cfg.Network.Port)

	stats := flog.NewStatsReporter(30 * time.Second)
	stats.Start()
	defer stats.Stop()

	err = s.listen(ctx, listener)
	cancel()
	s.wg.Wait()
	flog.Infof("Server shutdown completed")
	return err
}

func (s *Server) listen(ctx context.Context, listener tnet.Listener) error {
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// kcp-go stops reading after its first socket error and fails every Accept from then on,
			// so exit and let the service manager restart us.
			return fmt.Errorf("failed to accept connection: %w", err)
		}
		flog.Infof("accepted new connection from %s (local: %s)", conn.RemoteAddr(), conn.LocalAddr())

		s.wg.Go(func() {
			defer conn.Close()
			defer s.pConn.DelClientTCPF(conn.RemoteAddr())
			s.handleConn(ctx, conn)
		})
	}
}
