package run

import "testing"

func TestProcsCap(t *testing.T) {
	cases := []struct {
		cpus   int
		envSet bool
		want   int
	}{
		{8, false, 4}, // big host: cap, the scheduler spins across idle Ps otherwise
		{4, false, 0}, // already at or under the cap: leave Go's default
		{2, false, 0},
		{8, true, 0}, // GOMAXPROCS set by the operator always wins
	}
	for _, c := range cases {
		if got := procsCap(c.cpus, c.envSet); got != c.want {
			t.Errorf("procsCap(%d, %v) = %d, want %d", c.cpus, c.envSet, got, c.want)
		}
	}
}
