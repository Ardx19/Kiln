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
