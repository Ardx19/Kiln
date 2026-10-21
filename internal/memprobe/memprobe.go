// Package memprobe measures each running job's memory. Every job runs in
// its own process group. Each tick the sampler lists /proc, learns each
// new process's group from its stat file (WSL's kernel has no
// /proc/<pid>/task/<tid>/children to walk), and sums the resident memory
// of the processes in each tracked group. The summed peak catches
// processes running at the same time (-pipe, LTO, parallel links); the
// caller adds wait4's ru_maxrss, which catches a single process's spike
// between ticks.
package memprobe

import (
	"bytes"
	"encoding/binary"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultInterval is how often jobs are sampled. Each tick costs about
// 50 µs per process on a loaded WSL kernel, so 200 ms keeps 14 jobs well
// under 1% of a core. Peaks come late in a compile (M0 spike: 83-99% of
// its runtime) and wait4's ru_maxrss catches a single process's spike
// between ticks, so a finer interval would add little.
const DefaultInterval = 200 * time.Millisecond

// Job is what the sampler has seen of one process group.
type Job struct {
	// Peak is the highest total resident memory of the group's
	// processes at any tick, in bytes.
	Peak uint64
	// Backend is set once a compiler backend or linker ran in the group
	// (cc1plus, clang -cc1, lto1, ld, ...): the job did real work, as
	// opposed to, say, a ccache hit.
	Backend bool
}

// proc is what the sampler remembers about a live process.
type proc struct {
	pgid, ppid int
	comm       string
	statm      *os.File // kept open for processes in tracked groups
}

// Sampler samples the tracked process groups.
type Sampler struct {
	mu       sync.Mutex
	jobs     map[int]*Job // by process group
	procs    map[int]proc // by pid, for processes alive at the last tick
	pageSize uint64
	stop     chan struct{}
	done     chan struct{}
	// proc is the procfs root ("/proc"); tests can point it elsewhere.
	proc    string
	procDir *os.File // kept open and rewound each tick
	buf     [128]byte
	dirents []byte
	live    map[int]bool   // reused each tick
	sums    map[int]uint64 // reused each tick
}

// New returns a sampler; Start begins ticking.
func New() *Sampler {
	return &Sampler{jobs: map[int]*Job{}, procs: map[int]proc{}, pageSize: uint64(os.Getpagesize()), proc: "/proc",
		dirents: make([]byte, 32<<10), live: map[int]bool{}, sums: map[int]uint64{}}
}

// Start samples every interval, calling after (if not nil) after each
// tick, until Stop.
func (s *Sampler) Start(interval time.Duration, after func()) {
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.Tick()
				if after != nil {
					after()
				}
			}
		}
	}()
}

// Stop ends sampling.
func (s *Sampler) Stop() {
	if s.stop != nil {
		close(s.stop)
		<-s.done
		s.stop = nil
	}
}

// Track starts measuring the process group pgid.
func (s *Sampler) Track(pgid int) {
	s.mu.Lock()
	s.jobs[pgid] = &Job{}
	s.mu.Unlock()
}

// Untrack stops measuring pgid and returns what was seen.
func (s *Sampler) Untrack(pgid int) Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[pgid]
	delete(s.jobs, pgid)
	if j == nil {
		return Job{}
	}
	return *j
}

// Current returns the latest sample of pgid's total resident memory and
// whether it's tracked.
func (s *Sampler) Current(pgid int) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[pgid]
	if j == nil {
		return Job{}, false
	}
	return *j, true
}

// backends are the programs whose presence means a compile or link
// really ran.
var backends = map[string]bool{
	"cc1": true, "cc1plus": true, "cc1obj": true, "cc1objplus": true, "f951": true,
	"lto1": true, "lto-wrapper": true, "collect2": true,
	"ld": true, "ld.bfd": true, "ld.gold": true, "ld.lld": true, "lld": true, "mold": true,
}

// Tick takes one sample of every tracked group.
func (s *Sampler) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		return
	}
	if s.procDir == nil {
		d, err := os.Open(s.proc)
		if err != nil {
			return
		}
		s.procDir = d
	} else if _, err := s.procDir.Seek(0, 0); err != nil {
		return
	}
	live, sums := s.live, s.sums
	clear(live)
	clear(sums)
	for _, pid := range s.pids() {
		live[pid] = true
		p, ok := s.procs[pid]
		if !ok {
			if p, ok = s.readStat(pid); !ok {
				continue
			}
			s.procs[pid] = p
		}
		j := s.jobs[p.pgid]
		if j == nil {
			continue
		}
		if p.statm == nil {
			var err error
			if p.statm, err = os.Open(s.proc + "/" + strconv.Itoa(pid) + "/statm"); err != nil {
				continue
			}
			s.procs[pid] = p
		}
		sums[p.pgid] += s.rss(p.statm)
		if !j.Backend && (backends[p.comm] || s.isClangCC1(p)) {
			j.Backend = true
		}
	}
	for pid, p := range s.procs {
		if !live[pid] {
			if p.statm != nil {
				p.statm.Close()
			}
			delete(s.procs, pid)
		}
	}
	for pgid, sum := range sums {
		if j := s.jobs[pgid]; sum > j.Peak {
			j.Peak = sum
		}
	}
}

// pids lists the numeric entries of /proc by parsing getdents64 records
// (ino, off, reclen, type, name) straight from a reused buffer.
func (s *Sampler) pids() []int {
	var pids []int
	fd := int(s.procDir.Fd())
	for {
		n, err := unix.Getdents(fd, s.dirents)
		if err != nil || n <= 0 {
			return pids
		}
		for b := s.dirents[:n]; len(b) >= 19; {
			reclen := int(binary.NativeEndian.Uint16(b[16:18]))
			if reclen == 0 || reclen > len(b) {
				break
			}
			pid, ok := 0, false
			for _, c := range b[19:reclen] {
				if c == 0 {
					break
				}
				if c < '0' || c > '9' {
					ok = false
					break
				}
				pid, ok = pid*10+int(c-'0'), true
			}
			if ok {
				pids = append(pids, pid)
			}
			b = b[reclen:]
		}
	}
}

// isClangCC1 recognizes clang's backend: clang re-runs itself as
// "clang -cc1", so a clang whose parent is clang is the backend.
func (s *Sampler) isClangCC1(p proc) bool {
	if !bytes.HasPrefix([]byte(p.comm), []byte("clang")) {
		return false
	}
	parent, ok := s.procs[p.ppid]
	return ok && bytes.HasPrefix([]byte(parent.comm), []byte("clang"))
}

// readStat parses /proc/<pid>/stat: "pid (comm) state ppid pgrp ...".
// comm may hold spaces and parentheses, so fields are counted from the
// last ')'.
func (s *Sampler) readStat(pid int) (proc, bool) {
	b, err := os.ReadFile(s.proc + "/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return proc{}, false
	}
	lp, rp := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if lp < 0 || rp < lp {
		return proc{}, false
	}
	f := bytes.Fields(b[rp+1:])
	if len(f) < 3 {
		return proc{}, false
	}
	ppid, err1 := strconv.Atoi(string(f[1]))
	pgid, err2 := strconv.Atoi(string(f[2]))
	if err1 != nil || err2 != nil {
		return proc{}, false
	}
	return proc{pgid: pgid, ppid: ppid, comm: string(b[lp+1 : rp])}, true
}

// rss is a process's resident memory in bytes, re-read from its open
// statm file ("size resident shared ..." in pages) with one pread.
func (s *Sampler) rss(statm *os.File) uint64 {
	n, _ := statm.ReadAt(s.buf[:], 0)
	b := s.buf[:n]
	sp := bytes.IndexByte(b, ' ')
	if sp < 0 {
		return 0
	}
	var pages uint64
	for _, c := range b[sp+1:] {
		if c < '0' || c > '9' {
			break
		}
		pages = pages*10 + uint64(c-'0')
	}
	return pages * s.pageSize
}

// Close releases the sampler's open files.
func (s *Sampler) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pid, p := range s.procs {
		if p.statm != nil {
			p.statm.Close()
		}
		delete(s.procs, pid)
	}
	if s.procDir != nil {
		s.procDir.Close()
		s.procDir = nil
	}
}
