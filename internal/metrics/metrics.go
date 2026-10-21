// Package metrics times named operations for -d stats, like Ninja's
// METRIC_RECORD, and prints Ninja's table of them.
package metrics

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type metric struct {
	name  string
	count int
	sum   time.Duration
}

var (
	enabled atomic.Bool
	mu      sync.Mutex
	list    []*metric
	byName  = map[string]*metric{}
)

// Enable turns recording on (-d stats).
func Enable() { enabled.Store(true) }

func noop() {}

// Record starts timing one occurrence of name; call the returned function
// when it's done. It costs nothing while recording is off.
func Record(name string) func() {
	if !enabled.Load() {
		return noop
	}
	start := time.Now()
	return func() {
		d := time.Since(start)
		mu.Lock()
		m := byName[name]
		if m == nil {
			m = &metric{name: name}
			byName[name] = m
			list = append(list, m)
		}
		m.count++
		m.sum += d
		mu.Unlock()
	}
}

// Report prints the metrics in the order they were first recorded, in
// Ninja's format.
func Report(w io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	width := 0
	for _, m := range list {
		width = max(width, len(m.name))
	}
	fmt.Fprintf(w, "%-*s\t%-6s\t%-9s\t%s\n", width, "metric", "count", "avg (us)", "total (ms)")
	for _, m := range list {
		micros := float64(m.sum.Microseconds())
		fmt.Fprintf(w, "%-*s\t%-6d\t%-8.1f\t%.1f\n", width, m.name, m.count, micros/float64(m.count), micros/1000)
	}
}
