package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"paqet-plus/internal/conf"
	"paqet-plus/internal/tnet"
)

// failingListener mimics kcp-go after a socket read error: every Accept fails at once.
type failingListener struct{}

func (failingListener) Accept() (tnet.Conn, error)          { return nil, errors.New("socket read error") }
func (failingListener) Close() error                        { return nil }
func (failingListener) Addr() net.Addr                      { return nil }
func (failingListener) SetClientTCPF(net.Addr, []conf.TCPF) {}
func (failingListener) DeleteClientTCPF(net.Addr)           {}

func TestListenReportsAcceptFailure(t *testing.T) {
	s, err := New(&conf.Conf{})
	if err != nil {
		t.Fatal(err)
	}
	go s.listen(context.Background(), failingListener{})

	select {
	case err := <-s.Done():
		if err == nil {
			t.Fatal("Done delivered nil, want the accept error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listen kept spinning on a failing Accept instead of reporting it")
	}
}
