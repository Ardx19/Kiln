package build

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Ardx19/kiln/internal/graph"
)

// DefaultStatusFormat is Ninja's default NINJA_STATUS.
const DefaultStatusFormat = "[%f/%t] "

// Verbosity of the status output.
type Verbosity int

// Verbosity levels, as in Ninja.
const (
	Normal  Verbosity = iota
	Quiet             // --quiet: no status lines, only failures and output
	Verbose           // -v: full commands instead of descriptions
)

// Status reports build progress like Ninja's StatusPrinter: on a "smart"
// terminal one status line is rewritten in place and elided to fit;
// otherwise each edge gets its own line.
type Status struct {
	printer   *linePrinter
	format    string
	verbosity Verbosity
	// prev returns an edge's previous duration, for the ETA.
	prev func(*graph.Edge) (int64, bool)

	start                      time.Time
	started, finished, running int
	total                      int
	timeMillis, cpuMillis      int64
	rate                       slidingRate
	predictableTotal           int
	predictableRemaining       int
	unpredictableRemaining     int
	predictableCPUTotalMillis  int64
	predictableCPURemainingMs  int64
	predictedFraction          float64
	statusFormatError          error
}

// NewStatus writes to out. format follows NINJA_STATUS ("" means the
// default); parallelism sizes the window of the current-rate placeholder.
func NewStatus(out io.Writer, format string, verbosity Verbosity, parallelism int) *Status {
	if format == "" {
		format = DefaultStatusFormat
	}
	s := &Status{printer: newLinePrinter(out), format: format, verbosity: verbosity,
		rate: slidingRate{n: max(parallelism, 1), last: -1, rate: -1}, start: time.Now()}
	s.statusFormatError = checkStatusFormat(format)
	return s
}

// FormatError reports an unknown placeholder in the NINJA_STATUS format.
func (s *Status) FormatError() error { return s.statusFormatError }

// Info prints a message such as "no work to do." on its own line.
func (s *Status) Info(format string, args ...any) {
	s.printer.printOnNewLine(fmt.Sprintf("kiln: "+format+"\n", args...))
}

// Warn prints a warning to stderr, below the status line.
func (s *Status) Warn(msg string) {
	s.printer.printOnNewLine("")
	fmt.Fprintf(os.Stderr, "kiln: warning: %s\n", msg)
}

func (s *Status) buildStarted() {
	s.started, s.finished, s.running = 0, 0, 0
	s.start = time.Now()
}

func (s *Status) buildFinished() {
	s.printer.setConsoleLocked(false)
	s.printer.printOnNewLine("")
}

func (s *Status) prevDuration(e *graph.Edge) (int64, bool) {
	if s.prev == nil {
		return 0, false
	}
	return s.prev(e)
}

func (s *Status) edgeAdded(e *graph.Edge) {
	s.total++
	if d, ok := s.prevDuration(e); ok {
		s.predictableTotal++
		s.predictableRemaining++
		s.predictableCPUTotalMillis += d
		s.predictableCPURemainingMs += d
	} else {
		s.unpredictableRemaining++
	}
}

func (s *Status) edgeRemoved(e *graph.Edge) {
	s.total--
	if d, ok := s.prevDuration(e); ok {
		s.predictableTotal--
		s.predictableRemaining--
		s.predictableCPUTotalMillis -= d
		s.predictableCPURemainingMs -= d
	} else {
		s.unpredictableRemaining--
	}
}

func (s *Status) now() int64 { return time.Since(s.start).Milliseconds() }

// edgeStarted announces console edges, and every edge on a smart terminal.
func (s *Status) edgeStarted(e *graph.Edge) {
	s.started++
	s.running++
	s.timeMillis = s.now()
	console := e.Pool == graph.ConsolePool
	if console || s.printer.smart {
		s.printStatus(e)
	}
	if console {
		s.printer.setConsoleLocked(true)
	}
}

// edgeFinished prints the status line (for non-console edges), then for
// a failure the failed outputs and command, then the command's output.
func (s *Status) edgeFinished(e *graph.Edge, res *result, elapsedMillis int64) {
	s.timeMillis = s.now()
	s.finished++
	s.cpuMillis += elapsedMillis
	if d, ok := s.prevDuration(e); ok {
		s.predictableRemaining--
		s.predictableCPURemainingMs -= d
	} else {
		s.unpredictableRemaining--
	}
	console := e.Pool == graph.ConsolePool
	if console {
		s.printer.setConsoleLocked(false)
	}
	if s.verbosity == Quiet && res.success() {
		s.running--
		return
	}
	if !console {
		s.printStatus(e)
	}
	s.running--
	if !res.success() {
		var outs strings.Builder
		for _, o := range e.Outputs {
			outs.WriteString(o.Path + " ")
		}
		failed := fmt.Sprintf("FAILED: [code=%d] ", res.status())
		if s.printer.color {
			failed = "\x1b[31m" + failed + "\x1b[0m"
		}
		s.printer.printOnNewLine(failed + outs.String() + "\n")
		cmd, _ := e.Command(false)
		s.printer.printOnNewLine(cmd + "\n")
		if res.err != nil {
			s.printer.printOnNewLine(res.err.Error() + "\n")
		}
	}
	if out := res.output; out != "" {
		// Commands write to a pipe, so tools that color only for terminals
		// don't; others' escape codes are stripped unless kiln itself is
		// writing to a terminal.
		if !s.printer.color {
			out = stripANSI(out)
		}
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		s.printer.printOnNewLine(out)
	}
}

func (s *Status) printStatus(e *graph.Edge) {
	if s.verbosity == Quiet {
		return
	}
	s.recalculatePrediction()
	text, _ := e.Binding("description")
	if text == "" || s.verbosity == Verbose {
		text, _ = e.Command(false)
	}
	s.printer.print(s.progress()+text, s.verbosity != Verbose)
}

// recalculatePrediction estimates what fraction of the build's CPU time is
// done, using previous durations where they look trustworthy (a port of
// Ninja's RecalculateProgressPrediction).
func (s *Status) recalculatePrediction() {
	s.predictedFraction = 0
	usePrevious := s.predictableRemaining > 0 && s.predictableCPURemainingMs > 0
	if usePrevious && s.total > 0 && s.finished > 0 && s.timeMillis >= 15000 &&
		float64(s.finished)/float64(s.total) >= 0.05 {
		actual := float64(s.cpuMillis) / float64(s.finished)
		previous := float64(s.predictableCPUTotalMillis) / float64(s.predictableTotal)
		usePrevious = max(actual, previous)/min(actual, previous) < 10
	}
	known := s.finished
	if usePrevious {
		known += s.predictableRemaining
	}
	if known == 0 {
		return
	}
	unknown := s.total - s.finished
	if usePrevious {
		unknown = s.unpredictableRemaining
	}
	knownMillis := s.cpuMillis
	if usePrevious {
		knownMillis += s.predictableCPURemainingMs
	}
	average := float64(knownMillis) / float64(known)
	remaining := average * float64(unknown)
	if usePrevious {
		remaining += float64(s.predictableCPURemainingMs)
	}
	if total := float64(s.cpuMillis) + remaining; total != 0 {
		s.predictedFraction = float64(s.cpuMillis) / total
	}
}

// checkStatusFormat rejects placeholders Ninja doesn't know.
func checkStatusFormat(format string) error {
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		if i == len(format) || !strings.ContainsRune("%strufocpewEWP", rune(format[i])) {
			c := ""
			if i < len(format) {
				c = string(format[i])
			}
			return fmt.Errorf("unknown placeholder '%%%s' in $NINJA_STATUS", c)
		}
	}
	return nil
}

// progress expands NINJA_STATUS placeholders, as Ninja's
// FormatProgressStatus does.
func (s *Status) progress() string {
	var b strings.Builder
	elapsed := float64(s.timeMillis) / 1e3
	eta := -1.0
	if s.predictedFraction != 0 {
		eta = (float64(s.timeMillis)/s.predictedFraction - float64(s.timeMillis)) / 1e3
	}
	for i := 0; i < len(s.format); i++ {
		c := s.format[i]
		if c != '%' || i+1 == len(s.format) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s.format[i] {
		case '%':
			b.WriteByte('%')
		case 's':
			fmt.Fprint(&b, s.started)
		case 't':
			fmt.Fprint(&b, s.total)
		case 'r':
			fmt.Fprint(&b, s.running)
		case 'u':
			fmt.Fprint(&b, s.total-s.started)
		case 'f':
			fmt.Fprint(&b, s.finished)
		case 'o':
			writeRate(&b, float64(s.finished)/elapsed, elapsed > 0)
		case 'c':
			s.rate.update(s.finished, s.timeMillis)
			writeRate(&b, s.rate.rate, s.rate.rate != -1)
		case 'p':
			pct := 0
			if s.finished != 0 && s.total != 0 {
				pct = 100 * s.finished / s.total
			}
			fmt.Fprintf(&b, "%3d%%", pct)
		case 'e', 'w', 'E', 'W':
			sec := elapsed
			if s.format[i] == 'E' || s.format[i] == 'W' {
				sec = eta
			}
			switch {
			case sec < 0:
				b.WriteByte('?')
			case s.format[i] == 'e' || s.format[i] == 'E':
				fmt.Fprintf(&b, "%.3f", sec)
			default:
				t := int64(sec)
				if elapsed >= 3600 || eta >= 3600 {
					fmt.Fprintf(&b, "%d:%02d:%02d", t/3600, t%3600/60, t%60)
				} else {
					fmt.Fprintf(&b, "%02d:%02d", t/60, t%60)
				}
			}
		case 'P':
			fmt.Fprintf(&b, "%3d%%", int(100*s.predictedFraction))
		default:
			b.WriteByte('%')
			b.WriteByte(s.format[i])
		}
	}
	return b.String()
}

func writeRate(b *strings.Builder, rate float64, ok bool) {
	if !ok {
		b.WriteByte('?')
		return
	}
	fmt.Fprintf(b, "%.1f", rate)
}

// slidingRate is the finishing rate over the last n edges.
type slidingRate struct {
	n     int
	times []int64
	last  int
	rate  float64
}

func (r *slidingRate) update(hint int, timeMillis int64) {
	if hint == r.last {
		return
	}
	r.last = hint
	if len(r.times) == r.n {
		r.times = r.times[1:]
	}
	r.times = append(r.times, timeMillis)
	if first, last := r.times[0], r.times[len(r.times)-1]; first != last {
		r.rate = float64(len(r.times)) / (float64(last-first) / 1e3)
	}
}

// linePrinter is a port of Ninja's LinePrinter.
type linePrinter struct {
	out          io.Writer
	fd           int
	smart, color bool
	blankLine    bool // the cursor is at the start of an empty line
	consoleLock  bool
	lineBuffer   string
	lineElide    bool
	outputBuffer strings.Builder
}

func newLinePrinter(out io.Writer) *linePrinter {
	p := &linePrinter{out: out, fd: -1, blankLine: true}
	if f, ok := out.(*os.File); ok {
		p.fd = int(f.Fd())
		term := os.Getenv("TERM")
		_, err := unix.IoctlGetTermios(p.fd, unix.TCGETS)
		p.smart = err == nil && term != "" && term != "dumb"
	}
	p.color = p.smart
	if !p.color {
		if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
			p.color = true
		}
	}
	return p
}

// print shows a status line: in place on a smart terminal, elided to the
// terminal's width; otherwise on a line of its own.
func (p *linePrinter) print(text string, elide bool) {
	if p.consoleLock {
		p.lineBuffer, p.lineElide = text, elide
		return
	}
	if !p.smart {
		io.WriteString(p.out, text+"\n")
		return
	}
	if elide {
		if ws, err := unix.IoctlGetWinsize(p.fd, unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
			text = elideMiddle(text, int(ws.Col))
		}
	}
	io.WriteString(p.out, "\r"+text+"\x1b[K")
	p.blankLine = false
}

// printOnNewLine prints text below the status line.
func (p *linePrinter) printOnNewLine(text string) {
	if p.consoleLock && p.lineBuffer != "" {
		p.outputBuffer.WriteString(p.lineBuffer + "\n")
		p.lineBuffer = ""
	}
	if !p.blankLine {
		p.write("\n")
	}
	if text != "" {
		p.write(text)
	}
	p.blankLine = text == "" || strings.HasSuffix(text, "\n")
}

func (p *linePrinter) write(s string) {
	if p.consoleLock {
		p.outputBuffer.WriteString(s)
		return
	}
	io.WriteString(p.out, s)
}

// setConsoleLocked holds back other output while a console-pool command
// owns the terminal, and releases it afterwards.
func (p *linePrinter) setConsoleLocked(locked bool) {
	if locked == p.consoleLock {
		return
	}
	if locked {
		p.printOnNewLine("")
	}
	p.consoleLock = locked
	if !locked {
		io.WriteString(p.out, p.outputBuffer.String())
		p.outputBuffer.Reset()
		if p.lineBuffer != "" {
			p.print(p.lineBuffer, p.lineElide)
			p.lineBuffer = ""
		}
	}
}

// elideMiddle shortens s to width by replacing its middle with "...".
func elideMiddle(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}
	left := (width - 3) / 2
	right := width - 3 - left
	return s[:left] + "..." + s[len(s)-right:]
}

// stripANSI removes ANSI CSI escape sequences, like Ninja's
// StripAnsiEscapeCodes.
func stripANSI(in string) string {
	if !strings.Contains(in, "\x1b") {
		return in
	}
	var b strings.Builder
	for i := 0; i < len(in); i++ {
		if in[i] != '\x1b' {
			b.WriteByte(in[i])
			continue
		}
		if i+1 >= len(in) {
			break
		}
		if in[i+1] != '[' {
			continue // not a CSI: drop the escape only
		}
		i += 2
		for i < len(in) && !('a' <= in[i] && in[i] <= 'z' || 'A' <= in[i] && in[i] <= 'Z') {
			i++
		}
	}
	return b.String()
}
