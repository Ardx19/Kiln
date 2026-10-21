package main

import (
	"math"
	"os"
	"runtime"
	"runtime/debug"
)

// gcFreeHeap is how far the heap may grow before kiln's first garbage
// collection. Most no-op builds finish below it without collecting.
const gcFreeHeap = 64 << 20

// delayGC turns the garbage collector off until the heap reaches
// gcFreeHeap, then back to Go's defaults. A no-op build of a few thousand
// edges is short enough that collection is a large share of its time; a
// large tree is collected as usual once it passes the limit. GOGC or
// GOMEMLIMIT in the environment take precedence.
func delayGC() {
	if os.Getenv("GOGC") != "" || os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	debug.SetGCPercent(-1)
	debug.SetMemoryLimit(gcFreeHeap)
	// The first collection, forced by the limit, frees the sentinel and
	// runs its finalizer. (Finalizers on tiny allocations may never run,
	// hence the size.)
	sentinel := new([64]byte)
	runtime.SetFinalizer(sentinel, func(*[64]byte) {
		debug.SetGCPercent(100)
		debug.SetMemoryLimit(math.MaxInt64)
	})
}
