//go:build !linux

package socket

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/gopacket/gopacket/pcap"

	"paqet-plus/internal/conf"
)

func openSendHandle(cfg *conf.Network) (frameWriter, error) {
	handle, err := newHandle(cfg, 256*1024, 128, pcap.BlockForever)
	if err != nil {
		return nil, fmt.Errorf("failed to open pcap handle: %w", err)
	}

	// SetDirection is not fully supported on Windows Npcap, so skip it
	if runtime.GOOS != "windows" {
		if err := handle.SetDirection(pcap.DirectionOut); err != nil {
			handle.Close()
			return nil, fmt.Errorf("failed to set pcap direction out: %v", err)
		}
	}

	if err := handle.SetBPFFilter("less 0"); err != nil {
		handle.Close()
		return nil, fmt.Errorf("failed to set BPF filter: %w", err)
	}
	return handle, nil
}

// pcapReader copies each frame out of pcap's buffer. gopacket drops its own
// lock before returning that zero-copy buffer, so mu keeps Close from freeing
// it mid-copy. The 100ms read timeout bounds how long Close waits.
type pcapReader struct {
	handle *pcap.Handle
	mu     sync.Mutex
	buf    []byte
}

func openRecvHandle(cfg *conf.Network) (frameReader, error) {
	handle, err := newHandle(cfg, cfg.PCAP.Sockbuf, cfg.PCAP.SnapLen, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("failed to open pcap handle: %w", err)
	}

	// SetDirection is not fully supported on Windows Npcap, so skip it
	if runtime.GOOS != "windows" {
		if err := handle.SetDirection(pcap.DirectionIn); err != nil {
			handle.Close()
			return nil, fmt.Errorf("failed to set pcap direction in: %v", err)
		}
	}

	if err := handle.SetBPFFilter(recvFilter(cfg.Port)); err != nil {
		handle.Close()
		return nil, fmt.Errorf("failed to set BPF filter: %w", err)
	}
	return &pcapReader{handle: handle, buf: make([]byte, cfg.PCAP.SnapLen)}, nil
}

func (r *pcapReader) ReadFrame() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, _, err := r.handle.ZeroCopyReadPacketData()
	if err != nil {
		return nil, err
	}
	return r.buf[:copy(r.buf, data)], nil
}

// SetReadDeadline is a no-op: reads time out every 100ms, and
// PacketConn.ReadFrom checks the deadline between them.
func (r *pcapReader) SetReadDeadline(time.Time) error { return nil }

func (r *pcapReader) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handle.Close()
}

func newHandle(cfg *conf.Network, sockbuf int, snapLen int, timeout time.Duration) (*pcap.Handle, error) {
	// On Windows, use the GUID field to construct the NPF device name
	// On other platforms, use the interface name directly
	ifaceName := cfg.Interface.Name
	if runtime.GOOS == "windows" {
		ifaceName = cfg.GUID
	}

	inactive, err := pcap.NewInactiveHandle(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to create inactive pcap handle for %s: %v", cfg.Interface.Name, err)
	}
	defer inactive.CleanUp()

	if err = inactive.SetBufferSize(sockbuf); err != nil {
		return nil, fmt.Errorf("failed to set pcap buffer size to %d: %v", sockbuf, err)
	}

	if err = inactive.SetSnapLen(snapLen); err != nil {
		return nil, fmt.Errorf("failed to set pcap snap length to %d: %v", snapLen, err)
	}
	if err = inactive.SetPromisc(cfg.PCAP.Promisc); err != nil {
		return nil, fmt.Errorf("failed to set promiscuous mode: %v", err)
	}
	if err = inactive.SetTimeout(timeout); err != nil {
		return nil, fmt.Errorf("failed to set pcap timeout: %v", err)
	}
	if err = inactive.SetImmediateMode(true); err != nil {
		return nil, fmt.Errorf("failed to enable immediate mode: %v", err)
	}

	handle, err := inactive.Activate()
	if err != nil {
		return nil, fmt.Errorf("failed to activate pcap handle on %s: %v", cfg.Interface.Name, err)
	}

	return handle, nil
}
