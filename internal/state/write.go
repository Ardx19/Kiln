package state

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"

	"github.com/Ardx19/kiln/internal/metrics"
)

// LogWriter appends records to a .ninja_log in a fixed version, the way
// Ninja's BuildLog does: the file is opened lazily in append mode, and a
// new or empty file gets the header first.
type LogWriter struct {
	path    string
	version int
	log     *BuildLog
	f       *os.File
}

// NewLogWriter writes to path in log's version (or version, for a log
// that doesn't exist yet). log is kept up to date with what's written.
func NewLogWriter(path string, log *BuildLog, version int) *LogWriter {
	if log.Version != 0 {
		version = log.Version
	}
	log.Version = version
	return &LogWriter{path: path, version: version, log: log}
}

// Version is the format being written.
func (w *LogWriter) Version() int { return w.version }

func (w *LogWriter) open() error {
	if w.f != nil {
		return nil
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if fi, err := f.Stat(); err != nil {
		f.Close()
		return err
	} else if fi.Size() == 0 {
		if _, err := fmt.Fprintf(f, "# ninja log v%d\n", w.version); err != nil {
			f.Close()
			return err
		}
	}
	w.f = f
	return nil
}

// Record appends one line per output for a finished command.
func (w *LogWriter) Record(outputs []string, startMS, endMS int, mtime int64, hash uint64) error {
	if err := w.open(); err != nil {
		return err
	}
	var buf []byte
	for _, o := range outputs {
		e := w.log.Entries[o]
		if e == nil {
			e = &LogEntry{Output: o}
			w.log.Entries[o] = e
		}
		e.StartMS, e.EndMS, e.Mtime, e.Hash = startMS, endMS, mtime, hash
		w.log.Total++
		buf = appendEntry(buf, e)
	}
	_, err := w.f.Write(buf)
	return err
}

func appendEntry(buf []byte, e *LogEntry) []byte {
	return fmt.Appendf(buf, "%d\t%d\t%d\t%s\t%x\n", e.StartMS, e.EndMS, e.Mtime, e.Output, e.Hash)
}

// Close closes the file; the next Record reopens it. Ninja does this
// before running a generator, which may itself run `ninja -t restat` on
// the log. The file is created first if it doesn't exist yet.
func (w *LogWriter) Close() error {
	if err := w.open(); err != nil {
		return err
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Recompact rewrites the log with one record per live output, dropping
// outputs for which dead returns true, through a temporary file and a
// rename, as Ninja does.
func (w *LogWriter) Recompact(dead func(output string) bool) error {
	defer metrics.Record(".ninja_log recompact")()
	if w.f != nil {
		w.f.Close()
		w.f = nil
	}
	tmp := w.path + ".recompact"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	fmt.Fprintf(bw, "# ninja log v%d\n", w.version)
	outputs := make([]string, 0, len(w.log.Entries))
	for o := range w.log.Entries {
		outputs = append(outputs, o)
	}
	sort.Strings(outputs)
	for _, o := range outputs {
		if dead(o) {
			delete(w.log.Entries, o)
			continue
		}
		bw.Write(appendEntry(nil, w.log.Entries[o]))
	}
	w.log.Total = len(w.log.Entries)
	if err := errors.Join(bw.Flush(), f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, w.path)
}

// DepsWriter appends to a .ninja_deps, assigning ids to new paths and
// skipping records that wouldn't change anything, like Ninja's DepsLog.
type DepsWriter struct {
	path string
	log  *DepsLog
	ids  map[string]int
	f    *os.File
}

// NewDepsWriter writes to path. log must be what ReadDepsLog returned for
// it; a corrupt tail is truncated before the first write, as Ninja does
// when it loads such a log.
func NewDepsWriter(path string, log *DepsLog) *DepsWriter {
	w := &DepsWriter{path: path, log: log, ids: make(map[string]int, len(log.Paths))}
	for id, p := range log.Paths {
		w.ids[p] = id
	}
	return w
}

func (w *DepsWriter) open() error {
	if w.f != nil {
		return nil
	}
	if w.log.Corrupt != "" {
		if err := os.Truncate(w.path, w.log.ValidEnd); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		w.log.Corrupt = ""
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if fi, err := f.Stat(); err != nil {
		f.Close()
		return err
	} else if fi.Size() == 0 {
		hdr := binary.LittleEndian.AppendUint32([]byte(depsSignature), DepsLogVersion)
		if _, err := f.Write(hdr); err != nil {
			f.Close()
			return err
		}
	}
	w.f = f
	return nil
}

// id returns path's id, appending a path record if it's new.
func (w *DepsWriter) id(path string, buf *[]byte) int {
	if id, ok := w.ids[path]; ok {
		return id
	}
	id := len(w.log.Paths)
	padded := len(path) + (4-len(path)%4)%4
	*buf = binary.LittleEndian.AppendUint32(*buf, uint32(padded+4))
	*buf = append(*buf, path...)
	*buf = append(*buf, make([]byte, padded-len(path))...)
	*buf = binary.LittleEndian.AppendUint32(*buf, ^uint32(id))
	w.ids[path] = id
	w.log.Paths = append(w.log.Paths, path)
	return id
}

// Record stores an output's discovered inputs and mtime, unless that's
// exactly what the log already says.
func (w *DepsWriter) Record(output string, mtime int64, inputs []string) error {
	var buf []byte
	out := w.id(output, &buf)
	ins := make([]int, len(inputs))
	for i, in := range inputs {
		ins[i] = w.id(in, &buf)
	}
	if len(buf) == 0 { // no new paths: is anything different?
		if out < len(w.log.Deps) {
			if d := w.log.Deps[out]; d != nil && d.Mtime == mtime && sameInts(d.Inputs, ins) {
				return nil
			}
		}
	}
	size := 4 * (3 + len(ins))
	if size > maxRecordSize {
		return fmt.Errorf("deps record for %s is too large", output)
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(size)|1<<31)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(out))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(mtime))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(mtime>>32))
	for _, in := range ins {
		buf = binary.LittleEndian.AppendUint32(buf, uint32(in))
	}
	if err := w.open(); err != nil {
		return err
	}
	if _, err := w.f.Write(buf); err != nil {
		return err
	}
	for out >= len(w.log.Deps) {
		w.log.Deps = append(w.log.Deps, nil)
	}
	w.log.Total++
	if w.log.Deps[out] == nil {
		w.log.Unique++
	}
	w.log.Deps[out] = &Deps{Mtime: mtime, Inputs: ins}
	return nil
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Close closes the file.
func (w *DepsWriter) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Recompact rewrites the log keeping only the deps of outputs for which
// live returns true, renumbering paths in order of first use, as Ninja
// does.
func (w *DepsWriter) Recompact(live func(output string) bool) error {
	defer metrics.Record(".ninja_deps recompact")()
	w.Close()
	tmp := w.path + ".recompact"
	os.Remove(tmp)
	old := w.log
	fresh := &DepsLog{}
	nw := &DepsWriter{path: tmp, log: fresh, ids: map[string]int{}}
	for id, d := range old.Deps {
		if d == nil || id >= len(old.Paths) || !live(old.Paths[id]) {
			continue
		}
		ins := make([]string, len(d.Inputs))
		for i, in := range d.Inputs {
			ins[i] = old.Paths[in]
		}
		if err := nw.Record(old.Paths[id], d.Mtime, ins); err != nil {
			nw.Close()
			return err
		}
	}
	if err := nw.open(); err != nil { // an empty log still gets a header
		return err
	}
	if err := nw.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, w.path); err != nil {
		return err
	}
	*w.log = *fresh
	w.ids = nw.ids
	return nil
}
