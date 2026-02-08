package flog

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/xtaci/kcp-go/v5"
)

// ConnStats holds per-connection diagnostics.
type ConnStats struct {
	Remote  string
	Streams int
}

// StatsReporter periodically logs transport metrics and detects bottlenecks.
type StatsReporter struct {
	ctx      context.Context
	cancel   context.CancelFunc
	interval time.Duration
	tick     uint64

	prevSnmp       *kcp.Snmp
	prevTime       time.Time
	prevGoroutines int

	connFn func() []ConnStats
}

// NewStatsReporter creates a reporter that logs every interval.
func NewStatsReporter(interval time.Duration) *StatsReporter {
	ctx, cancel := context.WithCancel(context.Background())
	return &StatsReporter{
		ctx:            ctx,
		cancel:         cancel,
		interval:       interval,
		prevSnmp:       kcp.DefaultSnmp.Copy(),
		prevTime:       time.Now(),
		prevGoroutines: runtime.NumGoroutine(),
	}
}

// SetConnFunc sets the function that returns current connection stats.
func (s *StatsReporter) SetConnFunc(fn func() []ConnStats) {
	s.connFn = fn
}

// Start begins the periodic reporting goroutine.
func (s *StatsReporter) Start() {
	go s.run()
}

// Stop halts the reporter.
func (s *StatsReporter) Stop() {
	s.cancel()
}

func (s *StatsReporter) run() {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.report()
		}
	}
}

func (s *StatsReporter) report() {
	tick := atomic.AddUint64(&s.tick, 1)
	now := time.Now()
	elapsed := now.Sub(s.prevTime).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}

	// ── KCP Transport Stats ──
	curr := kcp.DefaultSnmp.Copy()
	prev := s.prevSnmp

	inBytes := curr.InBytes - prev.InBytes
	outBytes := curr.OutBytes - prev.OutBytes
	inPkts := curr.InPkts - prev.InPkts
	outPkts := curr.OutPkts - prev.OutPkts
	retrans := curr.RetransSegs - prev.RetransSegs
	outSegs := curr.OutSegs - prev.OutSegs
	lost := curr.LostSegs - prev.LostSegs
	inErrs := curr.InErrs - prev.InErrs

	var retransRate float64
	if outSegs > 0 {
		retransRate = float64(retrans) / float64(outSegs) * 100
	}

	sndQ := curr.RingBufferSndQueue
	rcvQ := curr.RingBufferRcvQueue
	sndBuf := curr.RingBufferSndBuffer

	logf(Info, "[STATS] kcp: ↑%s/s ↓%s/s | pkt ↑%s/s ↓%s/s | retrans %.1f%% (%d) | lost %d | err %d | sndQ %d rcvQ %d sndBuf %d",
		fmtBytes(float64(outBytes)/elapsed),
		fmtBytes(float64(inBytes)/elapsed),
		fmtCount(float64(outPkts)/elapsed),
		fmtCount(float64(inPkts)/elapsed),
		retransRate, retrans,
		lost,
		inErrs,
		sndQ, rcvQ, sndBuf,
	)

	// FEC stats (only if FEC is active)
	fecRecov := curr.FECRecovered - prev.FECRecovered
	fecErrs := curr.FECErrs - prev.FECErrs
	fecParity := curr.FECParityShards - prev.FECParityShards
	if fecParity > 0 {
		logf(Info, "[STATS] fec: recovered %d | errors %d | parity %d", fecRecov, fecErrs, fecParity)
	}

	// ── Bottleneck Detection ──
	totalStreams := 0
	numConns := 0
	if s.connFn != nil {
		conns := s.connFn()
		numConns = len(conns)
		perConn := make([]string, 0, len(conns))
		for _, c := range conns {
			totalStreams += c.Streams
			perConn = append(perConn, fmt.Sprintf("%d", c.Streams))
		}
		if numConns > 0 {
			logf(Info, "[STATS] conn: %d active | streams %d | [%s]",
				numConns, totalStreams, join(perConn, ", "))
		}
	}

	// Bottleneck: packet loss
	if retransRate > 5 {
		logf(Warn, "[ALERT] BOTTLENECK: packet_loss — %.1f%% retransmission (>5%%). Causes: pcap buffer overflow, network congestion, ISP throttling", retransRate)
	}

	// Bottleneck: pcap drops
	if lost > 100 {
		logf(Warn, "[ALERT] BOTTLENECK: pcap_drops — %d lost segments (>100). pcap buffer can't keep up. Try: increase sockbuf, reduce conn count", lost)
	}

	// Bottleneck: send buffer saturated
	if sndBuf > 512 {
		logf(Warn, "[ALERT] BOTTLENECK: send_saturated — sndBuf=%d (>512). KCP can't push data fast enough. Try: increase sndwnd, check network", sndBuf)
	}

	// Bottleneck: receive buffer saturated
	if rcvQ > 256 {
		logf(Warn, "[ALERT] BOTTLENECK: recv_saturated — rcvQ=%d (>256). Application not reading fast enough", rcvQ)
	}

	// Bottleneck: read errors
	if inErrs > 0 {
		logf(Warn, "[ALERT] BOTTLENECK: read_errors — %d socket/pcap read failures this interval", inErrs)
	}

	// Bottleneck: throughput collapse (traffic exists but barely moving)
	outBytesPerSec := float64(outBytes) / elapsed
	if totalStreams > 0 && outBytesPerSec < 100*1024 && outBytesPerSec > 0 {
		logf(Warn, "[ALERT] BOTTLENECK: throughput_collapse — %s/s with %d active streams. Traffic exists but barely moving", fmtBytes(outBytesPerSec), totalStreams)
	}

	// Bottleneck: stream overload
	if numConns > 0 {
		avgStreams := float64(totalStreams) / float64(numConns)
		if avgStreams > 32 {
			logf(Warn, "[ALERT] BOTTLENECK: stream_overload — %.0f avg streams/conn (>32). Try: increase conn count", avgStreams)
		}
	}

	s.prevSnmp = curr
	s.prevTime = now

	// ── Runtime Stats (every 5th tick ≈ 2.5 min) ──
	goroutines := runtime.NumGoroutine()
	if tick%5 == 0 {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		logf(Info, "[STATS] mem: heap %s | goroutines %d | gc_pause %s | alloc %s | sys %s",
			fmtBytes(float64(m.HeapInuse)),
			goroutines,
			time.Duration(m.PauseNs[(m.NumGC+255)%256]),
			fmtBytes(float64(m.Alloc)),
			fmtBytes(float64(m.Sys)),
		)
	}

	// Bottleneck: goroutine leak (>20% growth between intervals)
	if s.prevGoroutines > 0 && goroutines > 100 {
		growth := float64(goroutines-s.prevGoroutines) / float64(s.prevGoroutines) * 100
		if growth > 20 {
			logf(Warn, "[ALERT] BOTTLENECK: goroutine_leak — %d → %d (+%.0f%%). Goroutines growing fast, possible leak", s.prevGoroutines, goroutines, growth)
		}
	}
	s.prevGoroutines = goroutines
}

// ── Formatting helpers ──

func fmtBytes(b float64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1fGB", b/1024/1024/1024)
	case b >= 1024*1024:
		return fmt.Sprintf("%.1fMB", b/1024/1024)
	case b >= 1024:
		return fmt.Sprintf("%.1fKB", b/1024)
	default:
		return fmt.Sprintf("%.0fB", b)
	}
}

func fmtCount(n float64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", n/1_000)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}

func join(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	r := ss[0]
	for _, s := range ss[1:] {
		r += sep + s
	}
	return r
}
