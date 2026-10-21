package state

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadBuildLog(t *testing.T) {
	p := write(t, []byte("# ninja log v6\n"+
		"1\t20\t1700000000123456789\ta.o\tdeadbeef\n"+
		"5\t9\t42\tb.o\t1\n"+
		"not a record\n"+
		"30\t40\t43\ta.o\tCAFE\n"+ // supersedes the first a.o record
		"50\t60\t44\tc.o\tff")) // no newline: ignored
	log, err := ReadBuildLog(p)
	if err != nil {
		t.Fatal(err)
	}
	if log.Version != 6 || len(log.Entries) != 2 || log.Total != 3 {
		t.Fatalf("version %d, %d entries, %d records", log.Version, len(log.Entries), log.Total)
	}
	a := log.Entries["a.o"]
	if a.StartMS != 30 || a.EndMS != 40 || a.Mtime != 43 || a.Hash != 0xcafe {
		t.Errorf("a.o = %+v", a)
	}
	if log.Entries["c.o"] != nil {
		t.Error("an unterminated last line must be ignored")
	}
}

func TestReadBuildLogVersions(t *testing.T) {
	for content, want := range map[string]int{
		"# ninja log v4\n":   4,
		"# ninja log v8\n":   8,
		"garbage\n":          -1,
		"# ninja log v5":     0, // supported, even unterminated
		"# ninja log v7\n\n": 0,
	} {
		_, err := ReadBuildLog(write(t, []byte(content)))
		var verr *VersionError
		switch {
		case want == 0 && err != nil:
			t.Errorf("%q: unexpected error %v", content, err)
		case want != 0 && (!errors.As(err, &verr) || verr.Version != want):
			t.Errorf("%q: got %v, want version error %d", content, err, want)
		}
	}
	log, err := ReadBuildLog(filepath.Join(t.TempDir(), "missing"))
	if err != nil || log.Version != 0 || len(log.Entries) != 0 {
		t.Errorf("missing log: %+v, %v", log, err)
	}
}

// depsFile builds a .ninja_deps image record by record.
type depsFile struct{ b []byte }

func newDepsFile(version int32) *depsFile {
	f := &depsFile{b: []byte(depsSignature)}
	return f.u32(uint32(version))
}

func (f *depsFile) u32(v uint32) *depsFile {
	f.b = binary.LittleEndian.AppendUint32(f.b, v)
	return f
}

func (f *depsFile) path(p string, id int) *depsFile {
	padded := []byte(p)
	for len(padded)%4 != 0 {
		padded = append(padded, 0)
	}
	f.u32(uint32(len(padded) + 4))
	f.b = append(f.b, padded...)
	return f.u32(^uint32(id))
}

func (f *depsFile) deps(out int, mtime int64, ins ...int) *depsFile {
	f.u32(uint32(12+4*len(ins)) | 1<<31)
	f.u32(uint32(out)).u32(uint32(mtime)).u32(uint32(mtime >> 32))
	for _, in := range ins {
		f.u32(uint32(in))
	}
	return f
}

func TestReadDepsLog(t *testing.T) {
	f := newDepsFile(4).
		path("out.o", 0).path("a.h", 1).path("b/c.h", 2).
		deps(0, 1<<40+7, 1, 2).
		path("other.o", 3).deps(3, 5).
		deps(0, 9, 2) // replaces out.o's first record
	log, err := ReadDepsLog(write(t, f.b))
	if err != nil {
		t.Fatal(err)
	}
	if log.Corrupt != "" || len(log.Paths) != 4 || log.Total != 3 || log.Unique != 2 {
		t.Fatalf("got %+v", log)
	}
	d := log.Deps[0]
	if d.Mtime != 9 || len(d.Inputs) != 1 || log.Paths[d.Inputs[0]] != "b/c.h" {
		t.Errorf("out.o deps = %+v", d)
	}
	if log.ValidEnd != int64(len(f.b)) {
		t.Errorf("ValidEnd %d, want %d", log.ValidEnd, len(f.b))
	}
}

func TestReadDepsLogCorruptTail(t *testing.T) {
	good := newDepsFile(4).path("out.o", 0).path("a.h", 1).deps(0, 3, 1)
	end := int64(len(good.b))
	for name, tail := range map[string][]byte{
		"truncated record": good.b[len(good.b)-6:],
		"bad checksum":     newDepsFile(4).path("x", 7).b[len(depsSignature)+4:],
		"unknown input":    newDepsFile(4).deps(0, 1, 99).b[len(depsSignature)+4:],
	} {
		img := append(append([]byte(nil), good.b...), tail...)
		log, err := ReadDepsLog(write(t, img))
		if err != nil {
			t.Fatal(err)
		}
		if log.Corrupt == "" || log.ValidEnd != end || log.Deps[0] == nil {
			t.Errorf("%s: corrupt %q, valid end %d (want %d)", name, log.Corrupt, log.ValidEnd, end)
		}
	}
	if _, err := ReadDepsLog(write(t, newDepsFile(3).b)); err == nil {
		t.Error("version 3 should be rejected")
	}
}
