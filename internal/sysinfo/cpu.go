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
	self, err := read("/proc/self/cgroup")
	if err != nil {
		return -1
	}
	mounts, err := read("/proc/self/mountinfo")
	if err != nil {
		return -1
	}
	// /proc/self/cgroup lines are "id:controllers:path"; v2 is "0::path".
	paths := map[string]string{}
	v2Path, haveV2 := "", false
	for _, line := range strings.Split(string(self), "\n") {
		id, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		controllers, path, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		if id == "0" && controllers == "" {
			v2Path, haveV2 = path, true
			continue
		}
		for _, c := range strings.Split(controllers, ",") {
			paths[c] = path
		}
	}
	var v1Dir, v2Dir string
	for _, line := range strings.Split(string(mounts), "\n") {
		m, ok := parseMount(line)
		if !ok {
			continue
		}
		switch m.fsType {
		case "cgroup":
			if v1Dir != "" || !slicesContain(m.superOptions, "cpu") {
				continue
			}
			if p, ok := paths["cpu"]; ok {
				v1Dir = m.translate(p)
			}
		case "cgroup2":
			if haveV2 && v2Dir == "" {
				v2Dir = m.mountPoint
				if v2Path != "/" {
					v2Dir += v2Path
				}
			}
		}
	}
	if v2Dir != "" {
		if q := cgroupV2Quota(read, v2Dir); q >= 0 {
			return q
		}
	}
	if v1Dir != "" {
		return cgroupV1Quota(read, v1Dir)
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

// mount is one /proc/self/mountinfo line.
type mount struct {
	root, mountPoint, fsType string
	superOptions             []string
}

// parseMount parses "id parent dev root mountpoint options [optional...] -
// fstype source superoptions".
func parseMount(line string) (mount, bool) {
	f := strings.Split(line, " ")
	if len(f) < 10 {
		return mount{}, false
	}
	sep := -1
	for i := 6; i < len(f); i++ {
		if f[i] == "-" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+4 != len(f) {
		return mount{}, false
	}
	return mount{root: f[3], mountPoint: f[4], fsType: f[sep+1],
		superOptions: strings.Split(f[sep+3], ",")}, true
}

// translate maps a cgroup path to a directory under this mount, or "" if
// the path isn't inside the mount's root.
func (m mount) translate(path string) string {
	rest, ok := strings.CutPrefix(path, m.root)
	if !ok || rest == ".." || strings.HasPrefix(rest, "../") {
		return ""
	}
	return strings.TrimSuffix(m.mountPoint+"/"+strings.TrimPrefix(rest, "/"), "/")
}

func slicesContain(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
