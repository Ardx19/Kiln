// Package memdb keeps each output's memory history in .kiln/mem.log and
// predicts how much memory an edge will need from it.
package memdb

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
)

const header = "# kiln mem log v1\n"

// Record is what's known about one output's memory use.
type Record struct {
	Output string
	// Estimate is the guarded maximum peak, in bytes, before the margin.
	Estimate uint64
	// Last is the most recent observed peak.
	Last uint64
	// DurationMS is how long the last full-size run took.
	DurationMS int64
	// Hash is the command hash of the last run.
	Hash uint64
}

// Log is .kiln/mem.log. Every observation appends a line, and the latest
// line for an output wins: "estimate last duration_ms hash output", with
// sizes in KiB and the hash in hex, separated by tabs. Like .ninja_log it
// is recompacted when most lines are stale.
type Log struct {
	path    string
	Records map[string]*Record
	lines   int
	f       *os.File
}

// Open reads the log at path. A missing log is empty; a malformed line is
// skipped.
func Open(path string) (*Log, error) {
	l := &Log{path: path, Records: map[string]*Record{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(data, []byte(header)) {
		return l, nil // an unknown format: start over
	}
	for _, line := range bytes.Split(data[len(header):], []byte("\n")) {
		f := bytes.Split(line, []byte("\t"))
		if len(f) != 5 {
			continue
		}
		est, err1 := strconv.ParseUint(string(f[0]), 10, 64)
		last, err2 := strconv.ParseUint(string(f[1]), 10, 64)
		dur, err3 := strconv.ParseInt(string(f[2]), 10, 64)
		hash, err4 := strconv.ParseUint(string(f[3]), 16, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		out := string(f[4])
		l.Records[out] = &Record{Output: out, Estimate: est << 10, Last: last << 10, DurationMS: dur, Hash: hash}
		l.lines++
	}
	return l, nil
}

// Observe records a run of the edge whose first output is output: its
// peak memory, duration and command hash. full says whether the run did
// the work its estimate is for (a compiler backend or linker ran, or it
// took about as long as last time); only full-size runs may lower the
// estimate, and only gradually, so a ccache hit can't erase what a real
// compile needs.
func (l *Log) Observe(output string, peak uint64, durationMS int64, hash uint64, full bool) error {
	r := l.Records[output]
	if r == nil {
		r = &Record{Output: output, Estimate: peak}
		l.Records[output] = r
	}
	switch {
	case peak > r.Estimate:
		r.Estimate = peak
	case full && peak < r.Estimate:
		r.Estimate -= (r.Estimate - peak) / 4
	}
	r.Last, r.Hash = peak, hash
	if full {
		r.DurationMS = durationMS
	}
	return l.append(r)
}

// IsFullSize reports whether a run that took durationMS, with or without
// a compiler backend, counts as full-size for output.
func (l *Log) IsFullSize(output string, durationMS int64, backend bool) bool {
	if backend {
		return true
	}
	r := l.Records[output]
	return r == nil || r.DurationMS == 0 || durationMS*2 >= r.DurationMS
}

func (l *Log) append(r *Record) error {
	if l.f == nil {
		if err := os.MkdirAll(path.Dir(l.path), 0o777); err != nil {
			return err
		}
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
			f.WriteString(header)
		}
		l.f = f
	}
	l.lines++
	_, err := fmt.Fprintf(l.f, "%d\t%d\t%d\t%x\t%s\n", r.Estimate>>10, r.Last>>10, r.DurationMS, r.Hash, r.Output)
	return err
}

// Forget drops the records of outputs matching the glob pattern
// (path.Match syntax), returning how many there were.
func (l *Log) Forget(pattern string) (int, error) {
	if _, err := path.Match(pattern, ""); err != nil {
		return 0, err
	}
	n := 0
	for out := range l.Records {
		if ok, _ := path.Match(pattern, out); ok {
			delete(l.Records, out)
			n++
		}
	}
	if n > 0 {
		return n, l.Recompact()
	}
	return 0, nil
}

// Close closes the log, recompacting it first if most of its lines are
// stale (as Ninja does: over 100 lines, more than 3 per record).
func (l *Log) Close() error {
	if l.lines > 100 && l.lines > 3*len(l.Records) {
		return l.Recompact()
	}
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Recompact rewrites the log with one line per record.
func (l *Log) Recompact() error {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	if err := os.MkdirAll(path.Dir(l.path), 0o777); err != nil {
		return err
	}
	tmp := l.path + ".recompact"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	w.WriteString(header)
	outs := make([]string, 0, len(l.Records))
	for o := range l.Records {
		outs = append(outs, o)
	}
	sort.Strings(outs)
	for _, o := range outs {
		r := l.Records[o]
		fmt.Fprintf(w, "%d\t%d\t%d\t%x\t%s\n", r.Estimate>>10, r.Last>>10, r.DurationMS, r.Hash, r.Output)
	}
	if err := errors.Join(w.Flush(), f.Close()); err != nil {
		return err
	}
	l.lines = len(outs)
	return os.Rename(tmp, l.path)
}
