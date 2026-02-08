package conf

import (
	"fmt"
	"paqet/internal/flog"
)

type PCAP struct {
	Sockbuf int  `yaml:"sockbuf"`
	Promisc bool `yaml:"promisc"`
	SnapLen int  `yaml:"snaplen"`
	DSCP    int  `yaml:"dscp"`
}

func (p *PCAP) setDefaults(role string) {
	if p.Sockbuf == 0 {
		if role == "server" {
			p.Sockbuf = 32 * 1024 * 1024 // 32MB: servers handle many concurrent streams
		} else {
			p.Sockbuf = 16 * 1024 * 1024 // 16MB: clients typically fewer streams
		}
	}
	// Promisc defaults to false (zero value) — saves CPU
	if p.SnapLen == 0 {
		p.SnapLen = 1600
	}
	// DSCP defaults to 0 (normal traffic, no special marking)
}

func (p *PCAP) validate() []error {
	var errors []error

	if p.Sockbuf < 1024 {
		errors = append(errors, fmt.Errorf("PCAP sockbuf must be >= 1024 bytes"))
	}

	if p.Sockbuf > 100*1024*1024 {
		errors = append(errors, fmt.Errorf("PCAP sockbuf too large (max 100MB)"))
	}

	// Should be power of 2 for optimal performance, but not required
	if p.Sockbuf&(p.Sockbuf-1) != 0 {
		flog.Warnf("PCAP sockbuf (%d bytes) is not a power of 2 - consider using values like 4MB, 8MB, or 16MB for better performance", p.Sockbuf)
	}

	if p.SnapLen < 256 || p.SnapLen > 65536 {
		errors = append(errors, fmt.Errorf("PCAP snaplen must be between 256-65536"))
	}

	if p.DSCP < 0 || p.DSCP > 63 {
		errors = append(errors, fmt.Errorf("PCAP DSCP must be between 0-63"))
	}

	return errors
}
