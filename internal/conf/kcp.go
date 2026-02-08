package conf

import (
	"fmt"
	"slices"

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

	Block kcp.BlockCrypt `yaml:"-"`
}

func (k *KCP) setDefaults(role string) {
	if k.Mode == "" {
		k.Mode = "fast"
	}
	if k.MTU == 0 {
		k.MTU = 1350
	}

	if k.Rcvwnd == 0 {
		if role == "server" {
			k.Rcvwnd = 2048
		} else {
			k.Rcvwnd = 1024
		}
	}
	if k.Sndwnd == 0 {
		if role == "server" {
			k.Sndwnd = 2048
		} else {
			k.Sndwnd = 1024
		}
	}

	// FEC defaults: dshard=0, pshard=0 (disabled)
	// FEC adds bandwidth overhead and doesn't help with bursty pcap drops.
	// Users can explicitly set dshard/pshard in config to enable it.
	// When both are 0, kcp-go natively disables FEC.

	if k.Block_ == "" {
		k.Block_ = "none"
	}

	if k.Smuxbuf == 0 {
		k.Smuxbuf = 512 * 1024
	}
	if k.Streambuf == 0 {
		k.Streambuf = 64 * 1024
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

	return errors
}
