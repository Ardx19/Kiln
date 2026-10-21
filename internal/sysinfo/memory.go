package sysinfo

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

// Memory is a snapshot of how much memory there is to spare.
type Memory struct {
	// Available is the host's MemAvailable. Inside a container it's the
	// host's, which is why the cgroup's numbers matter.
	Available uint64
	// Limit is kiln's cgroup memory limit (0 when there's none), and
	// WorkingSet its usage minus inactive file pages, which the kernel
	// reclaims before it OOM-kills anything.
	Limit, WorkingSet uint64
	// SwapIn and SwapOut count pages swapped since boot.
	SwapIn, SwapOut uint64
	// OOMKills counts OOM kills: the cgroup's when it has a limit (and
	// the kernel reports it), otherwise the host's.
	OOMKills uint64
}

// Headroom is how much more memory can be used before something gives:
// the tighter of the host and the cgroup.
func (m Memory) Headroom() uint64 {
	h := m.Available
	if m.Limit > 0 {
		left := uint64(0)
		if m.WorkingSet < m.Limit {
			left = m.Limit - m.WorkingSet
		}
		h = min(h, left)
	}
	return h
}

var (
	cgroupOnce sync.Once
	ownCgroups cgroups
)

// ReadMemory samples /proc/meminfo, /proc/vmstat and kiln's cgroup.
func ReadMemory() Memory {
	cgroupOnce.Do(func() { ownCgroups = findCgroups(os.ReadFile) })
	return readMemory(os.ReadFile, ownCgroups)
}

func readMemory(read func(string) ([]byte, error), cg cgroups) Memory {
	var m Memory
	meminfo := fields(read, "/proc/meminfo")
	m.Available = meminfo["MemAvailable:"] * 1024
	vmstat := fields(read, "/proc/vmstat")
	m.SwapIn, m.SwapOut, m.OOMKills = vmstat["pswpin"], vmstat["pswpout"], vmstat["oom_kill"]

	if cg.v2 != "" {
		if limit, ok := number(read, cg.v2+"/memory.max"); ok && limit > 0 {
			m.Limit = limit
			current, _ := number(read, cg.v2+"/memory.current")
			m.WorkingSet = sub(current, fields(read, cg.v2+"/memory.stat")["inactive_file"])
			if n, ok := fields(read, cg.v2+"/memory.events")["oom_kill"]; ok {
				m.OOMKills = n
			}
			return m
		}
	}
	if dir := cg.v1["memory"]; dir != "" {
		// An unlimited v1 cgroup reports a limit near 2^63.
		if limit, ok := number(read, dir+"/memory.limit_in_bytes"); ok && limit < 1<<62 {
			m.Limit = limit
			usage, _ := number(read, dir+"/memory.usage_in_bytes")
			m.WorkingSet = sub(usage, fields(read, dir+"/memory.stat")["total_inactive_file"])
			if n, ok := fields(read, dir+"/memory.oom_control")["oom_kill"]; ok {
				m.OOMKills = n
			}
		}
	}
	return m
}

func sub(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// number reads a file holding one number; "max" (no limit) reads as 0.
func number(read func(string) ([]byte, error), path string) (uint64, bool) {
	b, err := read(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, true
	}
	n, err := strconv.ParseUint(s, 10, 64)
	return n, err == nil
}

// fields reads "key value [unit]" lines, as in /proc/meminfo, /proc/vmstat
// and the cgroup stat files.
func fields(read func(string) ([]byte, error), path string) map[string]uint64 {
	m := map[string]uint64{}
	b, err := read(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			m[f[0]] = n
		}
	}
	return m
}
