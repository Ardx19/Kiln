// Package proctree finds and kills process trees through /proc, for the
// benchmark tools: ninja and kiln put every job in its own process group,
// so signalling a group never reaches the whole build.
package proctree

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Proc is one line of the process table.
type Proc struct {
	PID, PPID, Session int
}

// List reads the process table. Processes that exit while it reads are
// left out.
func List() []Proc {
	ents, _ := os.ReadDir("/proc")
	var ps []Proc
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// The command name is in parentheses and may hold spaces.
		rest := string(b[strings.LastIndexByte(string(b), ')')+1:])
		var state string
		var ppid, pgrp, sid int
		if _, err := fmt.Sscanf(rest, " %s %d %d %d", &state, &ppid, &pgrp, &sid); err == nil {
			ps = append(ps, Proc{PID: pid, PPID: ppid, Session: sid})
		}
	}
	return ps
}

// Descendants returns root and every process below it.
func Descendants(root int) []int {
	children := map[int][]int{}
	for _, p := range List() {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	pids := []int{root}
	for i := 0; i < len(pids); i++ {
		pids = append(pids, children[pids[i]]...)
	}
	return pids
}

// InSession returns the processes of session sid.
func InSession(sid int) []int {
	var pids []int
	for _, p := range List() {
		if p.Session == sid {
			pids = append(pids, p.PID)
		}
	}
	return pids
}

// Kill kills root and everything it started, however the descendants
// regrouped themselves.
func Kill(root int) {
	for range 3 { // a few passes, in case something forks meanwhile
		for _, pid := range Descendants(root) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// RSS sums the resident memory of pids, in bytes. Pages shared between
// them (a compiler's own code, say) count once per process, so the sum
// is an upper bound.
func RSS(pids []int) uint64 {
	page := uint64(os.Getpagesize())
	var total uint64
	for _, pid := range pids {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/statm")
		if err != nil {
			continue
		}
		f := strings.Fields(string(b))
		if len(f) < 2 {
			continue
		}
		if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
			total += n * page
		}
	}
	return total
}
