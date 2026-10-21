// Package sysinfo reads what the machine, and the cgroup kiln runs in,
// let it use.
package sysinfo

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// CPUs returns how many CPUs kiln may use, counted as Ninja 1.13 counts
// them for its default -j: the CPUs in the affinity mask (which
// runtime.NumCPU reads), capped by the cgroup's CPU quota rounded down.
// A quota under one CPU gives 0.
func CPUs() int {
	n := runtime.NumCPU()
	if q := cpuQuota(os.ReadFile); q >= 0 && q < n {
		return q
	}
	return n
}

// cpuQuota returns the cgroup CPU quota in whole CPUs, or -1 if there is
// none. It ports Ninja 1.13's ParseCPUFromCGroup with one change: Ninja
// stops at a cgroup v2 mount even when it sets no limit, so on a hybrid
// system (WSL, older systemd) it never sees a v1 quota. kiln falls back
// to v1.
func cpuQuota(read func(string) ([]byte, error)) int {
	cg := findCgroups(read)
	if cg.v2 != "" {
		if q := cgroupV2Quota(read, cg.v2); q >= 0 {
			return q
		}
	}
	if dir := cg.v1["cpu"]; dir != "" {
		return cgroupV1Quota(read, dir)
	}
	return -1
}

func cgroupV2Quota(read func(string) ([]byte, error), dir string) int {
	b, err := read(dir + "/cpu.max")
	if err != nil {
		return -1
	}
	line, _, _ := strings.Cut(string(b), "\n")
	quota, period, ok := strings.Cut(line, " ")
	if !ok || quota == "max" {
		return -1
	}
	q, err1 := strconv.ParseInt(quota, 10, 64)
	p, err2 := strconv.ParseInt(period, 10, 64)
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return -1
	}
	return int(q / p)
}

func cgroupV1Quota(read func(string) ([]byte, error), dir string) int {
	count := func(name string) (int64, bool) {
		b, err := read(dir + "/" + name)
		if err != nil {
			return 0, false
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return n, err == nil
	}
	q, ok := count("cpu.cfs_quota_us")
	if !ok || q == -1 {
		return -1
	}
	p, ok := count("cpu.cfs_period_us")
	if !ok || p == 0 {
		return -1
	}
	return int(q / p)
}
