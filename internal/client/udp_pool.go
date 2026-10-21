package client

import (
	"paqet/internal/flog"
	"paqet/internal/tnet"
	"sync"
)

type udpPool struct {
	strms map[uint64]tnet.Strm
	mu    sync.RWMutex
}

// add stores strm under key unless another stream got there first; it returns the stream to use.
func (p *udpPool) add(key uint64, strm tnet.Strm) (tnet.Strm, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, exists := p.strms[key]; exists {
		return old, false
	}
	p.strms[key] = strm
	return strm, true
}

// delete closes strm and drops it from the pool only if it is still the stream stored under key.
func (p *udpPool) delete(key uint64, strm tnet.Strm) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.strms[key] == strm {
		delete(p.strms, key)
	}
	flog.Debugf("closing UDP session stream %d", strm.SID())
	strm.Close()

	return nil
}
