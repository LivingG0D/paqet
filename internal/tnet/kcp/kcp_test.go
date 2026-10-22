package kcp

import (
	"testing"

	"paqet-plus/internal/conf"
)

type params struct {
	noDelay, interval, resend, noCongestion int
	wDelay, ackNoDelay                      bool
}

func paramsOf(cfg *conf.KCP) params {
	var p params
	p.noDelay, p.interval, p.resend, p.noCongestion, p.wDelay, p.ackNoDelay = modeParams(cfg)
	return p
}

// No preset may delay writes to the next update tick: in normal mode that added up
// to one 40ms interval per direction to every round trip, with no throughput gain.
func TestModeParamsFlushOnWrite(t *testing.T) {
	want := map[string]params{
		"normal": {noDelay: 0, interval: 40, resend: 2, noCongestion: 1},
		"fast":   {noDelay: 1, interval: 30, resend: 2, noCongestion: 1},
		"fast2":  {noDelay: 1, interval: 20, resend: 2, noCongestion: 1, ackNoDelay: true},
		"fast3":  {noDelay: 1, interval: 15, resend: 2, noCongestion: 1, ackNoDelay: true},
	}
	for mode, w := range want {
		if got := paramsOf(&conf.KCP{Mode: mode}); got != w {
			t.Errorf("mode %s: got %+v, want %+v", mode, got, w)
		}
	}
}

func TestModeParamsManualPassesConfigThrough(t *testing.T) {
	cfg := &conf.KCP{Mode: "manual", NoDelay: 1, Interval: 25, Resend: 3, NoCongestion: 0, WDelay: true, AckNoDelay: true}
	w := params{noDelay: 1, interval: 25, resend: 3, noCongestion: 0, wDelay: true, ackNoDelay: true}
	if got := paramsOf(cfg); got != w {
		t.Errorf("manual: got %+v, want %+v", got, w)
	}
}
