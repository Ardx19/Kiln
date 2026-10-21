package sysinfo

import (
	"io/fs"
	"testing"
)

func fakeFiles(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, fs.ErrNotExist
	}
}

const (
	v2Mount     = "30 23 0:26 / /sys/fs/cgroup rw,nosuid shared:4 - cgroup2 cgroup2 rw,nsdelegate\n"
	hybridMount = "123 122 0:21 / /sys/fs/cgroup/unified rw,nosuid shared:28 - cgroup2 cgroup2 rw,nsdelegate\n" +
		"127 122 0:60 / /sys/fs/cgroup/cpu rw,nosuid,relatime shared:32 - cgroup cgroup rw,cpu\n" +
		"129 122 0:61 / /sys/fs/cgroup/cpuacct rw,nosuid,relatime shared:34 - cgroup cgroup rw,cpuacct\n"
)

func TestCPUQuota(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		want  int
	}{
		{"no cgroup files", nil, -1},
		{"v2 with a quota", map[string]string{
			"/proc/self/cgroup":                      "0::/build.slice/job\n",
			"/proc/self/mountinfo":                   v2Mount,
			"/sys/fs/cgroup/build.slice/job/cpu.max": "250000 100000\n",
		}, 2},
		{"v2 without a quota", map[string]string{
			"/proc/self/cgroup":      "0::/\n",
			"/proc/self/mountinfo":   v2Mount,
			"/sys/fs/cgroup/cpu.max": "max 100000\n",
		}, -1},
		{"v2 quota under one CPU", map[string]string{
			"/proc/self/cgroup":      "0::/\n",
			"/proc/self/mountinfo":   v2Mount,
			"/sys/fs/cgroup/cpu.max": "50000 100000\n",
		}, 0},
		{"hybrid: v2 sets no limit, so v1's counts", map[string]string{
			"/proc/self/cgroup":                               "3:cpuacct:/\n2:cpu:/docker/abc\n0::/\n",
			"/proc/self/mountinfo":                            hybridMount,
			"/sys/fs/cgroup/cpu/docker/abc/cpu.cfs_quota_us":  "400000\n",
			"/sys/fs/cgroup/cpu/docker/abc/cpu.cfs_period_us": "100000\n",
		}, 4},
		{"v1 without a quota", map[string]string{
			"/proc/self/cgroup":                    "2:cpu:/\n",
			"/proc/self/mountinfo":                 hybridMount,
			"/sys/fs/cgroup/cpu/cpu.cfs_quota_us":  "-1\n",
			"/sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000\n",
		}, -1},
	} {
		if got := cpuQuota(fakeFiles(c.files)); got != c.want {
			t.Errorf("%s: cpuQuota = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestCPUs(t *testing.T) {
	if n := CPUs(); n < 0 {
		t.Errorf("CPUs = %d", n)
	}
}
