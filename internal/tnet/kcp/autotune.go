package kcp

import (
	"context"
	"paqet/internal/flog"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
)

// AutoTuner monitors KCP stats and dynamically adjusts window sizes.
// Uses global kcp.DefaultSnmp since kcp-go v5 doesn't expose per-connection stats.
type AutoTuner struct {
	conn     *Conn
	maxSnd   int
	maxRcv   int
	curSnd   int
	curRcv   int
	lastSent uint64
	lastRecv uint64
	mu       sync.Mutex
}

const (
	minWindow    = 128
	tuneInterval = 10 * time.Second
	growFactor   = 1.5
	shrinkFactor = 0.75
)

// NewAutoTuner creates a window auto-tuner for a KCP connection.
// maxSnd/maxRcv are the configured maximum window sizes.
func NewAutoTuner(conn *Conn, maxSnd, maxRcv int) *AutoTuner {
	// Start at half the max — room to grow or shrink
	startSnd := maxSnd / 2
	startRcv := maxRcv / 2
	if startSnd < minWindow {
		startSnd = minWindow
	}
	if startRcv < minWindow {
		startRcv = minWindow
	}
	conn.SetWindowSize(startSnd, startRcv)

	return &AutoTuner{
		conn:   conn,
		maxSnd: maxSnd,
		maxRcv: maxRcv,
		curSnd: startSnd,
		curRcv: startRcv,
	}
}

// Run starts the auto-tuning loop. Blocks until ctx is cancelled.
func (at *AutoTuner) Run(ctx context.Context) {
	ticker := time.NewTicker(tuneInterval)
	defer ticker.Stop()

	// Snapshot initial stats
	snap := kcp.DefaultSnmp.Copy()
	at.lastSent = snap.BytesSent
	at.lastRecv = snap.BytesReceived

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			at.tune()
		}
	}
}

func (at *AutoTuner) tune() {
	at.mu.Lock()
	defer at.mu.Unlock()

	snap := kcp.DefaultSnmp.Copy()

	// Compute deltas since last check
	sentDelta := snap.BytesSent - at.lastSent
	recvDelta := snap.BytesReceived - at.lastRecv
	at.lastSent = snap.BytesSent
	at.lastRecv = snap.BytesReceived

	totalDelta := sentDelta + recvDelta

	// Compute retransmission rate from global stats
	retransRate := float64(0)
	if snap.OutSegs > 0 {
		retransRate = float64(snap.RetransSegs) / float64(snap.OutSegs)
	}

	oldSnd := at.curSnd
	oldRcv := at.curRcv

	if retransRate > 0.05 {
		// High retransmission (>5%) → shrink windows (congestion signal)
		at.curSnd = int(float64(at.curSnd) * shrinkFactor)
		at.curRcv = int(float64(at.curRcv) * shrinkFactor)
	} else if totalDelta > 0 {
		// Good throughput, low retrans → grow windows
		at.curSnd = int(float64(at.curSnd) * growFactor)
		at.curRcv = int(float64(at.curRcv) * growFactor)
	}
	// else: no traffic — leave windows as-is

	// Clamp to bounds
	at.curSnd = clamp(at.curSnd, minWindow, at.maxSnd)
	at.curRcv = clamp(at.curRcv, minWindow, at.maxRcv)

	if at.curSnd != oldSnd || at.curRcv != oldRcv {
		at.conn.SetWindowSize(at.curSnd, at.curRcv)
		flog.Debugf("autotune: window %d/%d → %d/%d (retrans=%.1f%%, bytes=%d)",
			oldSnd, oldRcv, at.curSnd, at.curRcv, retransRate*100, totalDelta)
	}
}

func clamp(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
