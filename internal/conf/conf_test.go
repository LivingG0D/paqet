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
