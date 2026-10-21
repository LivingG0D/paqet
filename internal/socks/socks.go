package socks

import (
	"context"
	"net"
	"paqet/internal/client"
	"paqet/internal/conf"
	"paqet/internal/flog"

	"github.com/txthinking/socks5"
)

type SOCKS5 struct {
	handle *Handler
}

func New(client *client.Client) (*SOCKS5, error) {
	return &SOCKS5{
		handle: &Handler{client: client},
	}, nil
}

func (s *SOCKS5) Start(ctx context.Context, cfg conf.SOCKS5) error {
	s.handle.ctx = ctx
	go s.listen(ctx, cfg)
	return nil
}

func (s *SOCKS5) listen(ctx context.Context, cfg conf.SOCKS5) error {
	listenAddr, _ := net.ResolveTCPAddr("tcp", cfg.Listen.String())
	server, err := socks5.NewClassicServer(listenAddr.String(), listenAddr.IP.String(), cfg.Username, cfg.Password, 10, 10)
	if err != nil {
		flog.Fatalf("SOCKS5 server failed to create on %s: %v", listenAddr.String(), err)
	}

	go func() {
		if err := server.ListenAndServe(s.handle); err != nil && ctx.Err() == nil {
			flog.Fatalf("SOCKS5 server failed on %s: %v", listenAddr.String(), err)
		}
	}()
	flog.Infof("SOCKS5 server listening on %s", listenAddr.String())

	<-ctx.Done()
	if err := server.Shutdown(); err != nil {
		flog.Debugf("SOCKS5 server shutdown with: %v", err)
	}
	return nil
}
