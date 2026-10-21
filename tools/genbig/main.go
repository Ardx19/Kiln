// Command genbig writes a large synthetic build for load and no-op speed
// tests: dirs × files one-to-one "compile" edges (each touches its output),
// one archive edge per dir, and an "all" phony. The defaults give the
// 500k-edge manifest the M1 speed target is measured on.
//
// Usage:
//
//	go run ./tools/genbig -out DIR [-dirs 500] [-files 1000]
//
// Then build it once with Ninja, and time no-op runs:
//
//	ninja -C DIR
//	ninja -C DIR -n; kiln -C DIR -n
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	out := flag.String("out", "", "output directory (required, must not exist)")
	dirs := flag.Int("dirs", 500, "number of source directories")
	files := flag.Int("files", 1000, "source files per directory")
	flag.Parse()
	if *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if _, err := os.Stat(*out); err == nil {
		fmt.Fprintf(os.Stderr, "genbig: %s already exists\n", *out)
		os.Exit(1)
	}
	if err := write(*out, *dirs, *files); err != nil {
		fmt.Fprintln(os.Stderr, "genbig:", err)
		os.Exit(1)
	}
}

func write(root string, dirs, files int) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(root, "build.ninja"))
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprint(w, "rule cc\n  command = touch $out\n  description = CC $out\n")
	fmt.Fprint(w, "rule ar\n  command = touch $out\n  description = AR $out\n")
	var libs []string
	for d := range dirs {
		for _, sub := range []string{"src", "obj"} {
			if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("%s/d%d", sub, d)), 0o755); err != nil {
				return err
			}
		}
		lib := fmt.Sprintf("lib/libd%d.a", d)
		var objs []byte
		for n := range files {
			src := fmt.Sprintf("src/d%d/f%d.c", d, n)
			if err := os.WriteFile(filepath.Join(root, src), nil, 0o644); err != nil {
				return err
			}
			obj := fmt.Sprintf("obj/d%d/f%d.o", d, n)
			fmt.Fprintf(w, "build %s: cc %s\n  cflags = -O2 -DDIR=%d\n", obj, src, d)
			objs = append(append(objs, ' '), obj...)
		}
		fmt.Fprintf(w, "build %s: ar%s\n", lib, objs)
		libs = append(libs, lib)
	}
	fmt.Fprint(w, "build all: phony")
	for _, l := range libs {
		fmt.Fprint(w, " ", l)
	}
	fmt.Fprint(w, "\ndefault all\n")
	if err := w.Flush(); err != nil {
		return err
	}
	return f.Close()
}
