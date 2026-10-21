package sysinfo

import "strings"

// cgroups is where this process's cgroups are: one directory per v1
// controller (on a v1 or hybrid system) and the v2 directory (on a v2 or
// hybrid system). Empty means not found.
type cgroups struct {
	v1 map[string]string
	v2 string
}

// findCgroups reads /proc/self/cgroup and /proc/self/mountinfo, as Ninja
// 1.13's ParseSelfCGroup and ParseMountInfo do.
func findCgroups(read func(string) ([]byte, error)) cgroups {
	cg := cgroups{v1: map[string]string{}}
	self, err := read("/proc/self/cgroup")
	if err != nil {
		return cg
	}
	mounts, err := read("/proc/self/mountinfo")
	if err != nil {
		return cg
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
	for _, line := range strings.Split(string(mounts), "\n") {
		m, ok := parseMount(line)
		if !ok {
			continue
		}
		switch m.fsType {
		case "cgroup":
			for _, opt := range m.superOptions {
				if p, ok := paths[opt]; ok && cg.v1[opt] == "" {
					cg.v1[opt] = m.translate(p)
				}
			}
		case "cgroup2":
			if haveV2 && cg.v2 == "" {
				cg.v2 = m.mountPoint
				if v2Path != "/" {
					cg.v2 += v2Path
				}
			}
		}
	}
	return cg
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
