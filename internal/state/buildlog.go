package state

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
)

// Build log versions kiln reads: v5 (Ninja 1.10–1.11), v6 (1.12) and v7
// (1.13). Ninja 1.12 and later delete a log of any other version; kiln
// never deletes state it doesn't understand.
const (
	MinLogVersion = 5
	MaxLogVersion = 7
)

// VersionError means a state file is in a format kiln doesn't read.
// Ninja would delete such a file and start over; kiln leaves it alone and
// lets the caller decide.
type VersionError struct {
	Path    string
	Version int // -1 when the header is unrecognisable
}

func (e *VersionError) Error() string {
	if e.Version < 0 {
		return fmt.Sprintf("%s: unrecognised header", e.Path)
	}
	return fmt.Sprintf("%s: unsupported version %d", e.Path, e.Version)
}

// LogEntry is the latest record for one output.
type LogEntry struct {
	Output  string
	StartMS int    // when the command started, ms after the build began
	EndMS   int    // when it finished
	Mtime   int64  // recorded mtime in ns (meaning depends on the version)
	Hash    uint64 // HashCommand of the command that built it
}

// BuildLog is a parsed .ninja_log.
type BuildLog struct {
	Version int // 0 when the file doesn't exist or is empty
	Entries map[string]*LogEntry
	// Total counts every record read, including ones a later record for
	// the same output superseded; Ninja recompacts when this grows large.
	Total int
}

// ReadBuildLog reads a .ninja_log. A missing file is an empty log.
func ReadBuildLog(path string) (*BuildLog, error) {
	log := &BuildLog{Entries: map[string]*LogEntry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return log, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return log, nil
	}
	// The version comes from the first line, even an unterminated one.
	log.Version = headerVersion(data)
	if log.Version < MinLogVersion || log.Version > MaxLogVersion {
		v := log.Version
		if v == 0 {
			v = -1
		}
		return nil, &VersionError{Path: path, Version: v}
	}
	for {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			// Ninja only acts on newline-terminated lines; a partial last
			// line (an interrupted write) is ignored.
			break
		}
		log.parseLine(data[:nl])
		data = data[nl+1:]
	}
	return log, nil
}

// headerVersion parses "# ninja log v<N>" like Ninja's sscanf; 0 means
// no version.
func headerVersion(line []byte) int {
	rest, ok := bytes.CutPrefix(line, []byte("# ninja log v"))
	if !ok {
		return 0
	}
	return int(leadingInt(rest, 10))
}

// parseLine reads "start\tend\tmtime\toutput\thash". Lines without four
// tabs, the header among them, are skipped, as Ninja does.
func (log *BuildLog) parseLine(line []byte) {
	var fields [4][]byte
	rest := line
	for i := range fields {
		tab := bytes.IndexByte(rest, '\t')
		if tab < 0 {
			return
		}
		fields[i], rest = rest[:tab], rest[tab+1:]
	}
	output := string(fields[3])
	e := log.Entries[output]
	if e == nil {
		e = &LogEntry{Output: output}
		log.Entries[output] = e
	}
	log.Total++
	e.StartMS = int(leadingInt(fields[0], 10))
	e.EndMS = int(leadingInt(fields[1], 10))
	e.Mtime = leadingInt(fields[2], 10)
	h, _ := leadingUint(rest, 16)
	e.Hash = h
}

// NeedsRecompaction reports whether superseded records outnumber live
// ones three to one (past 100 records), Ninja's trigger for rewriting the
// log. Ninja also rewrites a log from an older version; kiln keeps the
// version it found, so that case doesn't arise.
func (log *BuildLog) NeedsRecompaction() bool {
	return log.Total > 100 && log.Total > 3*len(log.Entries)
}

// leadingInt parses an optionally signed integer prefix, like C's
// atoi/strtoll: anything unparsable is 0.
func leadingInt(b []byte, base int) int64 {
	neg := false
	if len(b) > 0 && (b[0] == '-' || b[0] == '+') {
		neg = b[0] == '-'
		b = b[1:]
	}
	u, _ := leadingUint(b, base)
	if neg {
		return -int64(u)
	}
	return int64(u)
}

// leadingUint parses the longest prefix of digits in base, like strtoull.
func leadingUint(b []byte, base int) (uint64, int) {
	n := 0
	for n < len(b) && digitValue(b[n]) < base {
		n++
	}
	if n == 0 {
		return 0, 0
	}
	u, err := strconv.ParseUint(string(b[:n]), base, 64)
	if err != nil {
		return ^uint64(0), n // overflow saturates, as strtoull does
	}
	return u, n
}

func digitValue(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return 99
}
