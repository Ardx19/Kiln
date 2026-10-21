package build

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Ardx19/kiln/internal/graph"
)

// DefaultStatusFormat is Ninja's default NINJA_STATUS.
const DefaultStatusFormat = "[%f/%t] "

// Status prints one line per edge, prefixed by a NINJA_STATUS-style
// progress string, as Ninja does on a terminal that isn't "smart".
type Status struct {
	out     io.Writer
	format  string
	verbose bool
	// started, finished and total count non-phony edges.
	started, finished, total int
}

// NewStatus writes to out. format follows NINJA_STATUS; "" means the
// default. verbose prints commands instead of descriptions.
func NewStatus(out io.Writer, format string, verbose bool) *Status {
	if format == "" {
		format = DefaultStatusFormat
	}
	return &Status{out: out, format: format, verbose: verbose}
}

func (s *Status) edgeAdded() { s.total++ }

func (s *Status) edgeRemoved() { s.total-- }

// Console-pool edges are announced when they start; others when they
// finish.
func (s *Status) edgeStarted(e *graph.Edge) {
	s.started++
	if e.Pool == graph.ConsolePool {
		s.print(e)
	}
}

// edgeFinished prints the status line, then, for a failure, the failed
// outputs and command, then whatever the command printed.
func (s *Status) edgeFinished(e *graph.Edge, res *result) {
	s.finished++
	if e.Pool != graph.ConsolePool {
		s.print(e)
	}
	if !res.success() {
		var outs strings.Builder
		for _, o := range e.Outputs {
			outs.WriteString(o.Path + " ")
		}
		cmd, _ := e.Command(false)
		fmt.Fprintf(s.out, "FAILED: [code=%d] %s\n%s\n", res.status(), outs.String(), cmd)
		if res.err != nil {
			fmt.Fprintln(s.out, res.err)
		}
	}
	if res.output != "" {
		io.WriteString(s.out, res.output)
		if !strings.HasSuffix(res.output, "\n") {
			io.WriteString(s.out, "\n")
		}
	}
}

func (s *Status) print(e *graph.Edge) {
	text, _ := e.Binding("description")
	if s.verbose || text == "" {
		text, _ = e.Command(false)
	}
	fmt.Fprintln(s.out, s.progress()+text)
}

// progress expands the subset of NINJA_STATUS placeholders whose values
// kiln tracks: %s started, %t total, %r running, %u unstarted,
// %f finished, %p percent and %%. Others are printed as they are.
func (s *Status) progress() string {
	var b strings.Builder
	for i := 0; i < len(s.format); i++ {
		c := s.format[i]
		if c != '%' || i+1 == len(s.format) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s.format[i] {
		case 's':
			b.WriteString(strconv.Itoa(s.started))
		case 't':
			b.WriteString(strconv.Itoa(s.total))
		case 'r':
			b.WriteString(strconv.Itoa(s.started - s.finished))
		case 'u':
			b.WriteString(strconv.Itoa(s.total - s.started))
		case 'f':
			b.WriteString(strconv.Itoa(s.finished))
		case 'p':
			pct := 0
			if s.total > 0 {
				pct = 100 * s.finished / s.total
			}
			fmt.Fprintf(&b, "%3d%%", pct)
		case '%':
			b.WriteByte('%')
		default:
			b.WriteByte('%')
			b.WriteByte(s.format[i])
		}
	}
	return b.String()
}
