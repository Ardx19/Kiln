package memdb

import (
	"path"
	"slices"
	"strings"
	"time"
)

// Kind is what an edge's command does, for edges with no history.
type Kind int

// Command kinds.
const (
	Other Kind = iota
	Compile
	Link
	Archive
	Codegen
)

var kindNames = [...]string{"other", "compile", "link", "archive", "codegen"}

func (k Kind) String() string { return kindNames[k] }

// Margin is added to an estimate borrowed from similar edges. An edge's
// own history needs none: its estimate is already the guarded maximum of
// its runs (see Log.Observe), and adding 10% to it held back jobs that
// would have fit (measured: DuckDB's units in a 6 GiB container).
const Margin = 1.10

// DefaultPeaks are the estimates for an edge with no history and no
// similar edge either. They're deliberately generous, because a first
// guess that is too low overcommits the machine for the first wave of jobs
// (measured: RDKit's heaviest compiles need 1.3 GiB, which a 768 MiB guess
// let twelve at a time start on an 11.7 GiB laptop), while one that is too
// high only delays the first few jobs: as soon as similar edges finish, the
// build has learned real values.
var DefaultPeaks = map[Kind]uint64{
	Compile: 1536 << 20,
	Link:    3072 << 20,
	Archive: 64 << 20,
	Codegen: 256 << 20,
	Other:   256 << 20,
}

// Edge describes an edge to the predictor.
type Edge struct {
	Output  string // first output: the history key
	Rule    string
	Command string
}

// Predictor estimates edges' peak memory.
type Predictor struct {
	log    *Log
	groups map[string]*group // "rule\x00dir", "rule", "kind:<k>"
}

// group collects the estimates of similar edges.
type group struct {
	peaks  []uint64
	sorted bool
}

// p90 is the group's 90th-percentile estimate (the maximum for fewer
// than five members), so one huge outlier doesn't make every similar
// edge look huge.
func (g *group) p90() uint64 {
	if !g.sorted {
		slices.Sort(g.peaks)
		g.sorted = true
	}
	if len(g.peaks) < 5 {
		return g.peaks[len(g.peaks)-1]
	}
	return g.peaks[len(g.peaks)*9/10]
}

// median is the group's middle estimate. It is what an edge gets from the
// group of everything of its kind: those edges may be nothing alike (CMake
// gives every target its own compile rule), and taking their maximum let
// one big unity build make every other compile look as big (measured:
// DuckDB's 12 heaviest units, 1.0-2.8 GiB, were all predicted at 3 GiB
// after the 2.8 GiB one finished, and ran one at a time).
func (g *group) median() uint64 {
	if !g.sorted {
		slices.Sort(g.peaks)
		g.sorted = true
	}
	return g.peaks[len(g.peaks)/2]
}

// NewPredictor builds the predictor from the log, using edges to group
// the history by rule, output directory and kind.
func NewPredictor(log *Log, edges []Edge) *Predictor {
	p := &Predictor{log: log, groups: map[string]*group{}}
	for _, e := range edges {
		if r := log.Records[e.Output]; r != nil {
			p.add(e, r.Estimate)
		}
	}
	return p
}

func (p *Predictor) keys(e Edge) [3]string {
	return [3]string{e.Rule + "\x00" + path.Dir(e.Output), e.Rule, "kind:" + Classify(e.Rule, e.Command).String()}
}

func (p *Predictor) add(e Edge, peak uint64) {
	for _, k := range p.keys(e) {
		g := p.groups[k]
		if g == nil {
			g = &group{}
			p.groups[k] = g
		}
		g.peaks = append(g.peaks, peak)
		g.sorted = false
	}
}

// Predict returns the memory e is expected to need: its own estimate, or
// one borrowed from similar edges with the margin.
func (p *Predictor) Predict(e Edge) uint64 {
	if r := p.log.Records[e.Output]; r != nil {
		return r.Estimate
	}
	keys := p.keys(e)
	for _, k := range keys[:2] {
		if g := p.groups[k]; g != nil {
			return uint64(float64(g.p90()) * Margin)
		}
	}
	if g := p.groups[keys[2]]; g != nil {
		return uint64(float64(g.median()) * Margin)
	}
	return DefaultPeaks[Classify(e.Rule, e.Command)]
}

// Duration is how long e's last full-size run took, or 0 if it has no
// history of its own.
func (p *Predictor) Duration(e Edge) time.Duration {
	if r := p.log.Records[e.Output]; r != nil {
		return time.Duration(r.DurationMS) * time.Millisecond
	}
	return 0
}

// Observe records a run and teaches the groups.
func (p *Predictor) Observe(e Edge, peak uint64, durationMS int64, hash uint64, backend bool) error {
	full := p.log.IsFullSize(e.Output, durationMS, backend)
	err := p.log.Observe(e.Output, peak, durationMS, hash, full)
	p.add(e, p.log.Records[e.Output].Estimate)
	return err
}

// compilers are basenames (after removing a cross prefix like
// "x86_64-linux-gnu-" and a version suffix like "-14") that compile or
// link C-family code.
var compilers = map[string]bool{
	"cc": true, "c++": true, "gcc": true, "g++": true, "clang": true, "clang++": true,
	"icx": true, "icpx": true, "nvcc": true, "gfortran": true,
}

// Classify guesses what an edge does from its rule name (CMake and
// Meson name their rules predictably) or, failing that, its command.
func Classify(rule, command string) Kind {
	switch {
	case strings.Contains(rule, "STATIC_LIBRARY_LINKER"), rule == "STATIC_LINKER":
		return Archive
	case strings.Contains(rule, "_LINKER"):
		return Link
	case strings.Contains(rule, "_COMPILER"):
		return Compile
	case strings.HasPrefix(rule, "CUSTOM_COMMAND"):
		return Codegen
	}
	words := strings.Fields(command)
	// Skip "cd dir &&", environment assignments and launchers.
	for skipping := true; skipping && len(words) > 0; {
		switch w := words[0]; {
		case w == "cd" && len(words) > 2 && words[2] == "&&":
			words = words[3:]
		case strings.Contains(w, "=") && !strings.HasPrefix(w, "-"):
			words = words[1:]
		case slices.Contains([]string{"ccache", "sccache", "distcc", "env"}, path.Base(w)):
			words = words[1:]
		default:
			skipping = false
		}
	}
	if len(words) == 0 {
		return Other
	}
	name := path.Base(words[0])
	switch {
	case isCompiler(name):
		if slices.Contains(words, "-c") {
			return Compile
		}
		return Link
	case name == "ar" || strings.HasSuffix(name, "-ar") || name == "ranlib" || strings.HasSuffix(name, "-ranlib"):
		return Archive
	case name == "ld" || strings.HasPrefix(name, "ld.") || strings.HasSuffix(name, "-ld") || name == "lld" || name == "mold":
		return Link
	}
	return Other
}

func isCompiler(name string) bool {
	// Strip a version suffix (gcc-14, clang++-18) and a cross prefix.
	if i := strings.LastIndexByte(name, '-'); i > 0 && strings.Trim(name[i+1:], "0123456789.") == "" {
		name = name[:i]
	}
	if compilers[name] {
		return true
	}
	for c := range compilers {
		if strings.HasSuffix(name, "-"+c) {
			return true
		}
	}
	return false
}
