package run

import (
	"os"
	"runtime"
)

// maxProcs caps Go's scheduler on hosts with more CPUs. On an 8-vCPU client, 2 Ps cut CPU by
// about 20% (fewer idle Ps spinning for work), but at ~700 Mbit/s paqet needed 1.55 cores, so 4
// keeps headroom.
const maxProcs = 4

// procsCap returns the GOMAXPROCS to set, or 0 to keep Go's default.
func procsCap(numCPU int, envSet bool) int {
	if envSet || numCPU <= maxProcs {
		return 0
	}
	return maxProcs
}

func capProcs() {
	_, set := os.LookupEnv("GOMAXPROCS")
	if n := procsCap(runtime.NumCPU(), set); n > 0 {
		runtime.GOMAXPROCS(n)
	}
}
