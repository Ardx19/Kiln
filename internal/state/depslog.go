package state

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
)

// DepsLogVersion is the only .ninja_deps format kiln reads; Ninja 1.10
// through 1.13 all write it.
const DepsLogVersion = 4

const (
	depsSignature = "# ninjadeps\n"
	maxRecordSize = 1<<19 - 1
)

// Deps are the discovered dependencies of one output.
type Deps struct {
	Mtime  int64 // ns; the output's mtime when these deps were recorded
	Inputs []int // node ids, indexing DepsLog.Paths
}

// DepsLog is a parsed .ninja_deps: a list of paths, each with an id, and
// for some of them (outputs) the dependencies their depfile listed.
//
// The file is a sequence of records. A path record assigns the next id to
// a path; a deps record (high bit set in its size) lists an output id, its
// mtime and its input ids. A later deps record for the same output
// replaces the earlier one.
type DepsLog struct {
	Paths []string
	// Deps is indexed by output id; nil where there are none.
	Deps []*Deps
	// Total and Unique count deps records, for Ninja's recompaction rule.
	Total, Unique int
	// Corrupt is set when the file has an invalid or incomplete tail.
	// Records before ValidEnd were read; Ninja would truncate the rest.
	Corrupt  string
	ValidEnd int64
}

// ReadDepsLog reads a .ninja_deps. A missing file is an empty log.
func ReadDepsLog(path string) (*DepsLog, error) {
	log := &DepsLog{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return log, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) < len(depsSignature)+4 || !bytes.HasPrefix(data, []byte(depsSignature)) {
		return nil, &VersionError{Path: path, Version: -1}
	}
	if v := int32(binary.LittleEndian.Uint32(data[len(depsSignature):])); v != DepsLogVersion {
		return nil, &VersionError{Path: path, Version: int(v)}
	}
	off := len(depsSignature) + 4
	log.ValidEnd = int64(off)
	seen := map[string]bool{}
	fail := func(why string) { log.Corrupt = why }
	for off < len(data) {
		if len(data)-off < 4 {
			fail("premature end of file")
			break
		}
		size := binary.LittleEndian.Uint32(data[off:])
		isDeps := size>>31 != 0
		size &= 0x7fffffff
		if size > maxRecordSize || len(data)-off-4 < int(size) {
			fail("premature end of file")
			break
		}
		rec := data[off+4 : off+4+int(size)]
		if isDeps {
			// Ninja reads a record shorter than 12 bytes without checking;
			// kiln treats it as corruption.
			if size%4 != 0 || size < 12 {
				fail("invalid deps record")
				break
			}
			out := int32(binary.LittleEndian.Uint32(rec))
			lo := binary.LittleEndian.Uint32(rec[4:])
			hi := binary.LittleEndian.Uint32(rec[8:])
			d := &Deps{Mtime: int64(uint64(hi)<<32 | uint64(lo))}
			bad := out < 0
			for i := 12; i < len(rec) && !bad; i += 4 {
				id := int32(binary.LittleEndian.Uint32(rec[i:]))
				if id < 0 || int(id) >= len(log.Paths) {
					bad = true
					break
				}
				d.Inputs = append(d.Inputs, int(id))
			}
			if bad {
				fail("deps record refers to an unknown path")
				break
			}
			for int(out) >= len(log.Deps) {
				log.Deps = append(log.Deps, nil)
			}
			log.Total++
			if log.Deps[out] == nil {
				log.Unique++
			}
			log.Deps[out] = d
		} else {
			n := int(size) - 4
			if n <= 0 {
				fail("invalid path record")
				break
			}
			// Up to three bytes of NUL padding.
			for i := 0; i < 3 && rec[n-1] == 0; i++ {
				n--
			}
			p := string(rec[:n])
			checksum := binary.LittleEndian.Uint32(rec[size-4:])
			if int32(^checksum) != int32(len(log.Paths)) || seen[p] {
				// Two Ninja processes wrote to the log at once.
				fail("path record out of sequence")
				break
			}
			seen[p] = true
			log.Paths = append(log.Paths, p)
		}
		off += 4 + int(size)
		log.ValidEnd = int64(off)
	}
	return log, nil
}

// NeedsRecompaction reports whether superseded deps records outnumber
// live ones three to one (past 1000 records), Ninja's rule.
func (log *DepsLog) NeedsRecompaction() bool {
	return log.Total > 1000 && log.Total > 3*log.Unique
}
