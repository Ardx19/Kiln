package sysinfo

import "testing"

const (
	meminfo = "MemTotal:       12253312 kB\nMemFree:         1000000 kB\nMemAvailable:    9000000 kB\n"
	vmstat  = "pswpin 10\npswpout 20\noom_kill 3\n"
)

func TestReadMemory(t *testing.T) {
	for _, c := range []struct {
		name  string
		cg    cgroups
		files map[string]string
		want  Memory
	}{
		{"host only", cgroups{}, map[string]string{}, Memory{Available: 9000000 * 1024, SwapIn: 10, SwapOut: 20, OOMKills: 3}},
		{"v2 limit", cgroups{v2: "/cg"}, map[string]string{
			"/cg/memory.max":     "2147483648\n",
			"/cg/memory.current": "1073741824\n",
			"/cg/memory.stat":    "anon 900000000\ninactive_file 73741824\n",
			"/cg/memory.events":  "low 0\nhigh 0\nmax 4\noom 1\noom_kill 1\n",
		}, Memory{Available: 9000000 * 1024, Limit: 2147483648, WorkingSet: 1000000000, SwapIn: 10, SwapOut: 20, OOMKills: 1}},
		{"v2 without a limit falls back to v1", cgroups{v2: "/cg", v1: map[string]string{"memory": "/v1"}}, map[string]string{
			"/cg/memory.max":            "max\n",
			"/v1/memory.limit_in_bytes": "4294967296\n",
			"/v1/memory.usage_in_bytes": "2000000000\n",
			"/v1/memory.stat":           "cache 5\ntotal_inactive_file 500000000\n",
			"/v1/memory.oom_control":    "oom_kill_disable 0\nunder_oom 0\noom_kill 2\n",
		}, Memory{Available: 9000000 * 1024, Limit: 4294967296, WorkingSet: 1500000000, SwapIn: 10, SwapOut: 20, OOMKills: 2}},
		{"unlimited v1", cgroups{v1: map[string]string{"memory": "/v1"}}, map[string]string{
			"/v1/memory.limit_in_bytes": "9223372036854771712\n",
		}, Memory{Available: 9000000 * 1024, SwapIn: 10, SwapOut: 20, OOMKills: 3}},
	} {
		c.files["/proc/meminfo"], c.files["/proc/vmstat"] = meminfo, vmstat
		if got := readMemory(fakeFiles(c.files), c.cg); got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestHeadroom(t *testing.T) {
	if h := (Memory{Available: 8 << 30}).Headroom(); h != 8<<30 {
		t.Errorf("host only: %d", h)
	}
	if h := (Memory{Available: 8 << 30, Limit: 2 << 30, WorkingSet: 1 << 30}).Headroom(); h != 1<<30 {
		t.Errorf("cgroup is tighter: %d", h)
	}
	if h := (Memory{Available: 8 << 30, Limit: 2 << 30, WorkingSet: 3 << 30}).Headroom(); h != 0 {
		t.Errorf("over the limit: %d", h)
	}
}

func TestReadMemoryHere(t *testing.T) {
	if m := ReadMemory(); m.Available == 0 {
		t.Errorf("no MemAvailable: %+v", m)
	}
}
