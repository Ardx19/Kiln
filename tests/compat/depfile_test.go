//go:build compat

package compat

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Ardx19/kiln/internal/depfile"
	"github.com/Ardx19/kiln/internal/graph"
)

// depfileCases are depfiles whose parsing must match Ninja's. Each is
// copied into place by an edge with deps = gcc, so Ninja parses it and
// `ninja -t deps` shows the result.
var depfileCases = []string{
	"out.o: a.h b.h\n",
	"out.o: \\\n  a.h \\\n  b/c.h\n",
	"out.o: a.h \\\r\n b.h\r\n",
	"out.o: dir\\ with\\ spaces/x.h y.h\n",
	"out.o: a\\\\ b.h\n",
	"out.o: a\\\\\\ b.h\n",
	"out.o: x\\#y.h\n",
	"out.o: p$$q.h\n",
	"out.o: p$q.h\n",
	"out.o: c\\:/x.h d\\: e.h\n",
	"out.o: a.h a.h ./a.h\n",
	"out.o: q?x.h amp&y.h quote\"z.h apos'w.h\n",
	"out.o: tab\tsep.h\n",
	"out.o: \\\n\n",
	"out.o:\n",
	"  \n",
	"out.o other.o: a.h\n",
	"out.o: a.h\nother: b.h\n",
	"out.o: a.h\na.h: b.h\n",
	"out.o: \xc3\xbcn\xc3\xaf.h\n",
	"out.o: a.h \\\n",
	"out.o: x\\y.h\n",
	"out.o: a.h\x00b.h\n",
	"out.o: a.h b.h",
	"foo.o: a.h\n",
}

func dialectOf(ninjaVersion string) depfile.Dialect {
	return depfile.DialectForLog(logVersionWrittenBy(ninjaVersion))
}

func TestDepfileCases(t *testing.T) {
	_, tools, ninjas, _ := matrix(t)
	_ = tools
	for _, n := range ninjas {
		t.Run("ninja-"+n.Version, func(t *testing.T) {
			for i, content := range depfileCases {
				dir := t.TempDir()
				write := func(name, s string) {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				write("src.d", content)
				write("build.ninja", "rule r\n  command = cp src.d $out.d && touch $out\n  depfile = $out.d\n  deps = gcc\nbuild out.o: r\n")
				build := exec.Command(n.Path, "-C", dir)
				buildOut, buildErr := build.CombinedOutput()

				_, ins, kerr := depfile.ParseDialect([]byte(content), dialectOf(n.Version))
				if (kerr != nil) != (buildErr != nil) {
					t.Errorf("case %d %q: kiln error %v, ninja build error %v\n%s", i, content, kerr, buildErr, buildOut)
					continue
				}
				if kerr != nil {
					continue // both rejected it
				}
				out, err := exec.Command(n.Path, "-C", dir, "-n", "-t", "deps", "out.o").Output()
				if err != nil {
					t.Fatalf("case %d: ninja -t deps: %v", i, err)
				}
				var theirs []string
				for _, line := range strings.Split(string(out), "\n") {
					if strings.HasPrefix(line, "    ") {
						theirs = append(theirs, strings.TrimPrefix(line, "    "))
					}
				}
				var mine []string
				for _, in := range ins {
					mine = append(mine, graph.CanonicalizePath(in))
				}
				if !slices.Equal(mine, theirs) {
					t.Errorf("case %d %q:\n kiln:  %q\n ninja: %q", i, content, mine, theirs)
				}
			}
		})
	}
}
