package kcp

import (
	"math"
	"time"

	"github.com/xtaci/kcp-go/v5"

	"paqet-plus/internal/flog"
)

// Pacing spreads each session's sends across the round trip. KCP runs with
// congestion control off, so without it a whole window leaves in one burst, path
// queues overflow, and the burst comes back as retransmits. Each session is paced
// at its window rate (sndwnd x MTU / smoothed RTT), which follows the RTT on any
// path. On a 78ms path with 8 connections and 16 streams, two interleaved rounds:
//
//	              up        down      retrans up  retrans down  server CPU down
//	unpaced       646 Mbps  603 Mbps  16.6%       21.0%         154-161%
//	1.2x window   665       597       12.8%       15.5%         151-153%
//	1.0x window   653       584       11.0%       10.6%         142-146%
//
// Throughput stayed within run-to-run noise. 1.2x paced each session at 150-170
// Mbit/s, above the ~85 Mbit/s it carried, so most bursts got through unsmoothed.
// A session that does fill its window loses up to its retransmit share to pacing,
// since retransmits count against the rate too.
const (
	paceHeadroom = 1.0         // multiple of the window rate to pace at
	paceEvery    = time.Second // how often the rate follows the smoothed RTT
	paceMinStep  = 0.1         // skip changes under 10%: each update refills kcp-go's limiter burst
)

// paceRate is the pacing rate in bytes/s for sndwnd packets of mtu bytes per
// srttMs round trip. 0 means no RTT sample yet, so no pacing.
func paceRate(sndwnd, mtu int, srttMs int32) uint32 {
	if srttMs <= 0 || sndwnd <= 0 || mtu <= 0 {
		return 0
	}
	r := paceHeadroom * float64(sndwnd) * float64(mtu) * 1000 / float64(srttMs)
	if r >= math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(r)
}

// pacer remembers the rate last applied to a session.
type pacer struct{ applied uint32 }

// next returns the rate for srttMs and whether it moved far enough from the
// applied rate to apply.
func (p *pacer) next(sndwnd, mtu int, srttMs int32) (uint32, bool) {
	r := paceRate(sndwnd, mtu, srttMs)
	if r == 0 {
		return 0, false
	}
	if p.applied != 0 && math.Abs(float64(r)-float64(p.applied)) < paceMinStep*float64(p.applied) {
		return r, false
	}
	p.applied = r
	return r, true
}

// pace keeps conn's send rate at paceRate of its smoothed RTT until done closes.
func pace(conn *kcp.UDPSession, sndwnd, mtu int, done <-chan struct{}) {
	t := time.NewTicker(paceEvery)
	defer t.Stop()
	var p pacer
	for {
		select {
		case <-done:
			return
		case <-t.C:
			srtt := conn.GetSRTT()
			if r, ok := p.next(sndwnd, mtu, srtt); ok {
				conn.SetRateLimit(r)
				flog.Debugf("kcp session %d paced at %.1f Mbit/s (srtt %dms)", conn.GetConv(), float64(r)*8/1e6, srtt)
			}
		}
	}
}
