package client

import (
	"testing"

	"paqet-plus/internal/conf"
)

// A second conn on a fixed source port makes the server see a new KCP conv from
// the same address, and kcp-go resets the live session for it.
func TestNewNoAutoscaleWithFixedPort(t *testing.T) {
	cfg := &conf.Conf{
		Transport: conf.Transport{Conn: 1},
		Network:   conf.Network{Port: 5000},
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.maxConns != 1 {
		t.Fatalf("maxConns = %d with a fixed client port, want 1", c.maxConns)
	}
}
