package conf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromFileRejectsBadConfig(t *testing.T) {
	const network = `
network:
  interface: "eth0"
  ipv4:
    addr: "192.168.1.100:0"
    router_mac: "aa:bb:cc:dd:ee:ff"
`
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "server addr without port",
			yaml: `role: "client"` + network + `
server:
  addr: "10.0.0.100"
transport:
  protocol: "kcp"
  kcp:
    key: "k"
`,
			wantErr: "missing port",
		},
		{
			name: "no kcp block",
			yaml: `role: "client"` + network + `
server:
  addr: "10.0.0.100:9999"
transport:
  protocol: "kcp"
`,
			wantErr: "transport.kcp configuration is required",
		},
		{
			name: "invalid ipv4 with ipv6",
			yaml: `role: "client"
network:
  interface: "eth0"
  ipv4:
    addr: "not-an-addr"
    router_mac: "aa:bb:cc:dd:ee:ff"
  ipv6:
    addr: "[::1]:9999"
    router_mac: "aa:bb:cc:dd:ee:ff"
server:
  addr: "[::1]:9999"
transport:
  protocol: "kcp"
  kcp:
    key: "k"
`,
			wantErr: "not-an-addr",
		},
		{
			name: "streambuf larger than smuxbuf",
			yaml: `role: "client"` + network + `
server:
  addr: "10.0.0.100:9999"
transport:
  protocol: "kcp"
  kcp:
    key: "k"
    smuxbuf: 65536
    streambuf: 131072
`,
			wantErr: "streambuf",
		},
		{
			name: "keepalive interval longer than timeout",
			yaml: `role: "client"` + network + `
server:
  addr: "10.0.0.100:9999"
transport:
  protocol: "kcp"
  kcp:
    key: "k"
    smuxkalive: 120
    smuxktimeout: 60
`,
			wantErr: "smuxkalive",
		},
		{
			name: "server without a fixed port",
			yaml: `role: "server"` + network + `
transport:
  protocol: "kcp"
  kcp:
    key: "k"
`,
			wantErr: "fixed port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFromFile(path)
			if err == nil {
				t.Fatal("LoadFromFile returned nil error, want validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// A config that leaves the tuning keys out must get the settings that measured best on the
// live pair (78ms path, many users): the same ones the installer writes.
func TestTuningDefaultsMatchMeasuredBest(t *testing.T) {
	for _, role := range []string{"client", "server"} {
		tr := &Transport{Protocol: "kcp", KCP: &KCP{Key: "k"}}
		tr.setDefaults(role)
		k := tr.KCP
		if tr.Conn != 8 || k.Mode != "normal" || k.MTU != 1250 || k.Sndwnd != 1024 || k.Rcvwnd != 1024 ||
			k.Smuxbuf != 16*1024*1024 || k.Streambuf != 4*1024*1024 {
			t.Errorf("%s defaults: conn %d mode %q mtu %d sndwnd %d rcvwnd %d smuxbuf %d streambuf %d; want 8 normal 1250 1024 1024 16MB 4MB",
				role, tr.Conn, k.Mode, k.MTU, k.Sndwnd, k.Rcvwnd, k.Smuxbuf, k.Streambuf)
		}
		if errs := tr.validate(); len(errs) != 0 {
			t.Errorf("%s defaults fail validation: %v", role, errs)
		}
	}
}
