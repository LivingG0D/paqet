package kcp

import (
	"math"
	"testing"
	"time"
)

func TestPaceRate(t *testing.T) {
	cases := []struct {
		name         string
		sndwnd, mtu  int
		srttMs       int32
		want         uint32
		tolerancePct float64
	}{
		// 1024 x 1250 B / 78 ms = 16.41 MB/s (131.3 Mbit/s): the window rate itself
		{"live config at 78ms", 1024, 1250, 78, 16_410_256, 0.01},
		{"no rtt sample yet", 1024, 1250, 0, 0, 0},
		{"negative rtt", 1024, 1250, -5, 0, 0},
		// 32768 x 1500 / 1ms = 49 GB/s: beyond uint32, so it clamps instead of wrapping
		{"clamps at uint32", 32768, 1500, 1, math.MaxUint32, 0},
	}
	for _, c := range cases {
		got := paceRate(c.sndwnd, c.mtu, c.srttMs)
		if diff := math.Abs(float64(got) - float64(c.want)); diff > c.tolerancePct/100*float64(c.want) {
			t.Errorf("%s: paceRate(%d, %d, %d) = %d, want %d", c.name, c.sndwnd, c.mtu, c.srttMs, got, c.want)
		}
	}
}

// Every update replaces kcp-go's limiter, which refills its burst, so the pacer
// only applies a new rate when the RTT moved it by 10% or more.
func TestPacerAppliesOnlyRealChanges(t *testing.T) {
	var p pacer
	steps := []struct {
		srttMs int32
		apply  bool
	}{
		{0, false},   // no sample yet: stay unpaced
		{78, true},   // first sample
		{80, false},  // 2.5% slower: wobble
		{86, false},  // 9.3% below the applied rate
		{100, true},  // 22% below
		{100, false}, // unchanged
		{60, true},   // faster again
	}
	for i, s := range steps {
		rate, apply := p.next(1024, 1250, s.srttMs)
		if apply != s.apply {
			t.Fatalf("step %d (srtt %dms): apply = %v, want %v", i, s.srttMs, apply, s.apply)
		}
		if apply && rate != paceRate(1024, 1250, s.srttMs) {
			t.Fatalf("step %d: rate %d, want %d", i, rate, paceRate(1024, 1250, s.srttMs))
		}
	}
}

// pace must return once the session closes, or every redial would leak a goroutine.
func TestPaceStopsWhenSessionCloses(t *testing.T) {
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		pace(nil, 1024, 1250, done) // the ticker never fires before done, so conn is unused
		close(returned)
	}()
	close(done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("pace still running 1s after done closed")
	}
}
