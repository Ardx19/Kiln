package build

import (
	"strings"
	"testing"
	"time"
)

func TestClockWatch(t *testing.T) {
	c := newClockWatch()
	if msg := c.check(); msg != "" {
		t.Fatalf("no step yet, got %q", msg)
	}
	c.offset -= 3 * time.Second // as if the wall clock had been 3 s behind
	if msg := c.check(); msg != "" {
		t.Fatalf("a forward step keeps mtimes in order, got %q", msg)
	}
	c.offset += 1800 * time.Millisecond // as if the wall clock had been 1.8 s ahead
	msg := c.check()
	if !strings.Contains(msg, "jumped back by 1.8s") {
		t.Errorf("got %q", msg)
	}
	c.offset += time.Second
	if msg := c.check(); msg != "" {
		t.Errorf("should warn only once, got %q", msg)
	}
}

func TestWatermarks(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		budget        uint64
		limited       bool
		low, critical uint64
	}{
		{9400 * mib, false, 1024 * mib, 768 * mib}, // a laptop: the floors
		{9400 * mib, true, 940 * mib, 470 * mib},   // a container: a tenth and a twentieth
		{5400 * mib, true, 540 * mib, 270 * mib},
		{2000 * mib, false, 500 * mib, 250 * mib},    // a small machine: a quarter and an eighth
		{40000 * mib, false, 4000 * mib, 2000 * mib}, // a big one: the fractions are above the floors
		{1000 * mib, true, 256 * mib, 128 * mib},     // the old minimums still hold
	} {
		low, critical := watermarks(c.budget, c.limited)
		if low != c.low || critical != c.critical {
			t.Errorf("watermarks(%d MiB, limited %v) = %d, %d MiB; want %d, %d", c.budget/mib, c.limited, low/mib, critical/mib, c.low/mib, c.critical/mib)
		}
	}
}
