package envdecl

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const logHeader = "# kiln env log v1\n"

// Log is .kiln/env.log: the environment hash each output was last built
// under. It is kept apart from .ninja_log so that file stays exactly what
// Ninja writes.
type Log struct {
	path   string
	hashes map[string]string
	lines  int
	f      *os.File
}

// OpenLog reads the log at path (a missing log is empty).
func OpenLog(path string) (*Log, error) {
	l := &Log{path: path, hashes: map[string]string{}}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		hash, out, ok := strings.Cut(sc.Text(), "\t")
		if !ok || strings.HasPrefix(hash, "#") {
			continue
		}
		l.hashes[out] = hash
		l.lines++
	}
	return l, sc.Err()
}

// Changed reports whether output was built under a different declaration
// than hash. An output with no record counts as built under none (""), so
// declaring an environment for the first time rebuilds everything, and
// declaring nothing leaves a never-recorded tree alone.
func (l *Log) Changed(output, hash string) bool { return l.hashes[output] != hash }

// Record notes that outputs were built under hash.
func (l *Log) Record(outputs []string, hash string) error {
	var todo []string
	for _, o := range outputs {
		if l.hashes[o] != hash {
			todo = append(todo, o)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	if l.f == nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o777); err != nil {
			return err
		}
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
			f.WriteString(logHeader)
		}
		l.f = f
	}
	var b strings.Builder
	for _, o := range todo {
		fmt.Fprintf(&b, "%s\t%s\n", hash, o)
		l.hashes[o] = hash
		l.lines++
	}
	_, err := l.f.WriteString(b.String())
	return err
}

// Close closes the log, recompacting it first when most lines are stale.
func (l *Log) Close() error {
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
	if l.lines > 200 && l.lines > 3*len(l.hashes) {
		tmp := l.path + ".recompact"
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		w := bufio.NewWriter(f)
		w.WriteString(logHeader)
		outs := make([]string, 0, len(l.hashes))
		for o := range l.hashes {
			outs = append(outs, o)
		}
		sort.Strings(outs)
		for _, o := range outs {
			fmt.Fprintf(w, "%s\t%s\n", l.hashes[o], o)
		}
		if err := errors.Join(w.Flush(), f.Close()); err != nil {
			return err
		}
		return os.Rename(tmp, l.path)
	}
	return nil
}
