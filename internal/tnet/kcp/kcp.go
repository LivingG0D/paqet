package kcp

import (
	"paqet-plus/internal/conf"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

func aplConf(conn *kcp.UDPSession, cfg *conf.KCP) {
	noDelay, interval, resend, noCongestion, wDelay, ackNoDelay := modeParams(cfg)

	conn.SetNoDelay(noDelay, interval, resend, noCongestion)
	conn.SetWindowSize(cfg.Sndwnd, cfg.Rcvwnd)
	conn.SetMtu(cfg.MTU)
	conn.SetWriteDelay(wDelay)
	conn.SetACKNoDelay(ackNoDelay)
}

// modeParams returns the KCP settings for cfg.Mode.
//
// Every preset flushes on write (wDelay false). Holding writes for the next update
// tick added up to one interval per direction to each round trip: on a 78ms path,
// normal mode measured 113ms median, flush-on-write 74ms, at the same throughput,
// retransmit rate and CPU. ackNoDelay stays off in normal/fast: acking every
// segment at once cut 16-stream throughput by a third there.
func modeParams(cfg *conf.KCP) (noDelay, interval, resend, noCongestion int, wDelay, ackNoDelay bool) {
	switch cfg.Mode {
	case "normal":
		noDelay, interval, resend, noCongestion = 0, 40, 2, 1
		wDelay, ackNoDelay = false, false
	case "fast":
		noDelay, interval, resend, noCongestion = 1, 30, 2, 1
		wDelay, ackNoDelay = false, false
	case "fast2":
		noDelay, interval, resend, noCongestion = 1, 20, 2, 1
		wDelay, ackNoDelay = false, true
	case "fast3":
		noDelay, interval, resend, noCongestion = 1, 15, 2, 1
		wDelay, ackNoDelay = false, true
	case "manual":
		noDelay, interval, resend, noCongestion = cfg.NoDelay, cfg.Interval, cfg.Resend, cfg.NoCongestion
		wDelay, ackNoDelay = cfg.WDelay, cfg.AckNoDelay
	}
	return
}

func smuxConf(cfg *conf.KCP) *smux.Config {
	var sconf = smux.DefaultConfig()
	sconf.Version = 2
	// Keepalive is configurable (upstream #111); defaults tuned to 30s/90s in conf.setDefaults.
	sconf.KeepAliveInterval = cfg.Smuxkalive
	sconf.KeepAliveTimeout = cfg.Smuxktimeout
	// 16384: conservative frame size to limit per-stream memory under high concurrency.
	sconf.MaxFrameSize = 16384
	sconf.MaxReceiveBuffer = cfg.Smuxbuf
	sconf.MaxStreamBuffer = cfg.Streambuf
	return sconf
}
