package conf

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/xtaci/kcp-go/v5"
)

type KCP struct {
	Mode         string `yaml:"mode"`
	NoDelay      int    `yaml:"nodelay"`
	Interval     int    `yaml:"interval"`
	Resend       int    `yaml:"resend"`
	NoCongestion int    `yaml:"nocongestion"`
	WDelay       bool   `yaml:"wdelay"`
	AckNoDelay   bool   `yaml:"acknodelay"`

	MTU    int `yaml:"mtu"`
	Rcvwnd int `yaml:"rcvwnd"`
	Sndwnd int `yaml:"sndwnd"`
	Dshard int `yaml:"dshard"`
	Pshard int `yaml:"pshard"`

	Block_ string `yaml:"block"`
	Key    string `yaml:"key"`

	Smuxbuf   int `yaml:"smuxbuf"`
	Streambuf int `yaml:"streambuf"`

	Smuxkalive_   int `yaml:"smuxkalive"`
	Smuxktimeout_ int `yaml:"smuxktimeout"`

	Smuxkalive   time.Duration  `yaml:"-"`
	Smuxktimeout time.Duration  `yaml:"-"`
	Block        kcp.BlockCrypt `yaml:"-"`
}

func (k *KCP) setDefaults() {
	// Defaults are the settings that measured best on a 78ms path with many users, the same the
	// installer writes: fast3 and larger windows (2048, 4096) raised retransmits to
	// 20-44% and lost 10-30% of aggregate throughput. The window is per conn and conn multiplies
	// it, so 1024 suits both roles.
	if k.Mode == "" {
		k.Mode = "normal"
	}
	if k.MTU == 0 {
		k.MTU = 1250
	}
	if k.Rcvwnd == 0 {
		k.Rcvwnd = 1024
	}
	if k.Sndwnd == 0 {
		k.Sndwnd = 1024
	}

	// FEC disabled by default: saves 10% bandwidth per connection.
	// WARNING: FEC changes KCP packet format — both sides MUST match.
	// Users can enable FEC by setting dshard/pshard in config (e.g. dshard: 10, pshard: 1).

	if k.Block_ == "" {
		k.Block_ = "aes-128-gcm" // AEAD: the other ciphers only carry a CRC32, which an on-path box can forge
	}

	// A stream cannot exceed streambuf/RTT and a conn cannot exceed smuxbuf/RTT: 64KB capped a
	// stream at ~7 Mbit/s on a 78ms path. 4MB/16MB lift both above the window rate.
	if k.Smuxbuf == 0 {
		k.Smuxbuf = 16 * 1024 * 1024
	}
	if k.Streambuf == 0 {
		k.Streambuf = 4 * 1024 * 1024
	}

	if k.Smuxkalive_ == 0 {
		k.Smuxkalive_ = 30 // tuned: longer keepalive interval = less overhead/stealthier under high concurrency
	}
	if k.Smuxktimeout_ == 0 {
		k.Smuxktimeout_ = 90
	}
}

func (k *KCP) validate() []error {
	var errors []error

	validModes := []string{"normal", "fast", "fast2", "fast3", "manual"}
	if !slices.Contains(validModes, k.Mode) {
		errors = append(errors, fmt.Errorf("KCP mode must be one of: %v", validModes))
	}

	if k.MTU < 50 || k.MTU > 1500 {
		errors = append(errors, fmt.Errorf("KCP MTU must be between 50-1500 bytes"))
	}

	if k.Rcvwnd < 1 || k.Rcvwnd > 32768 {
		errors = append(errors, fmt.Errorf("KCP rcvwnd must be between 1-32768"))
	}
	if k.Sndwnd < 1 || k.Sndwnd > 32768 {
		errors = append(errors, fmt.Errorf("KCP sndwnd must be between 1-32768"))
	}

	validBlocks := []string{"aes", "aes-128", "aes-128-gcm", "aes-192", "salsa20", "blowfish", "twofish", "cast5", "3des", "tea", "xtea", "xor", "sm4", "none", "null"}
	if !slices.Contains(validBlocks, k.Block_) {
		errors = append(errors, fmt.Errorf("KCP encryption block must be one of: %v", validBlocks))
	}
	if !slices.Contains([]string{"none", "null"}, k.Block_) && len(k.Key) == 0 {
		errors = append(errors, fmt.Errorf("KCP encryption key is required"))
	}
	b, err := newBlock(k.Block_, k.Key)
	if err != nil {
		errors = append(errors, err)
	}
	k.Block = b

	if k.Smuxbuf < 1024 {
		errors = append(errors, fmt.Errorf("KCP smuxbuf must be >= 1024 bytes"))
	}
	if k.Streambuf < 1024 {
		errors = append(errors, fmt.Errorf("KCP streambuf must be >= 1024 bytes"))
	}
	// smux rejects these at connect time, which would fail every session
	if k.Streambuf > k.Smuxbuf || k.Smuxbuf > math.MaxInt32 {
		errors = append(errors, fmt.Errorf("KCP streambuf (%d) must be <= smuxbuf (%d) <= %d", k.Streambuf, k.Smuxbuf, math.MaxInt32))
	}
	if k.Smuxkalive_ < 1 || k.Smuxkalive_ > k.Smuxktimeout_ {
		errors = append(errors, fmt.Errorf("KCP smuxkalive (%d) must be between 1 and smuxktimeout (%d)", k.Smuxkalive_, k.Smuxktimeout_))
	}

	k.Smuxkalive = time.Duration(k.Smuxkalive_) * time.Second
	k.Smuxktimeout = time.Duration(k.Smuxktimeout_) * time.Second

	return errors
}
