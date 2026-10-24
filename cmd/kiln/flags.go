package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Ardx19/kiln/internal/config"
	"github.com/Ardx19/kiln/internal/sysinfo"
)

// options is Ninja's command line.
type options struct {
	dir, file     string
	dryRun        bool
	verbose       bool
	quiet         bool
	explain       bool
	stats         bool
	tool          string
	targets       []string
	jobs, failOK  int
	jobsSet       bool
	keepGoingSet  bool
	keepDepfile   bool
	keepRsp       bool
	maxLoad       float64
	phonyCycleErr bool
	// kiln-only options (not accepted when running as ninja).
	memBudget  string // --mem-budget, as written ("" = unset)
	minJobs    int    // --min-jobs
	memMode    string // --mem: "on", "off" or "" (unset)
	schedDebug bool   // -d sched
}

// exit ends a command line before any work is done: --version, -h, a
// -d/-w list, or a bad flag. code and the messages are Ninja's.
type exit struct {
	code           int
	stdout, stderr string
}

// parseArgs reads Ninja's command line like Ninja's getopt_long loop:
// bundled short flags (-nv), attached values (-j8, -Cdir), options after
// targets, unambiguous prefixes of long options, and "--" to end options.
// Flags that end the run (--version, -h, -d list) act in argv order, as
// in Ninja. Deprecated -w flags are warned about on stderr as they're read.
func parseArgs(p program, args []string, stderr io.Writer) (options, *exit) {
	o := options{file: "build.ninja"}
	withValue := "Cdfjkltw"
	usageExit := func(msg string) *exit {
		return &exit{code: 1, stderr: p.getoptName() + ": " + msg + "\n" + usage(p, o)}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.targets = append(o.targets, args[i+1:]...)
			return o, nil
		case strings.HasPrefix(a, "--") && !p.dropIn && isKilnLong(a):
			name, value, hasValue := strings.Cut(a[2:], "=")
			if !hasValue {
				if i+1 == len(args) {
					return o, usageExit(fmt.Sprintf("option '--%s' requires an argument", name))
				}
				i++
				value = args[i]
			}
			switch name {
			case "mem-budget":
				if _, err := config.ParseSize(value); err != nil || value == "" {
					return o, &exit{code: 1, stderr: p.name + ": fatal: --mem-budget: invalid size '" + value + "'\n"}
				}
				o.memBudget = value
			case "min-jobs":
				n, err := strconv.Atoi(value)
				if err != nil || n < 1 {
					return o, &exit{code: 1, stderr: p.name + ": fatal: invalid --min-jobs parameter\n"}
				}
				o.minJobs = n
			case "mem":
				if value != "on" && value != "off" {
					return o, &exit{code: 1, stderr: p.name + ": fatal: --mem must be 'on' or 'off'\n"}
				}
				o.memMode = value
			}
			continue
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a[2:], "=")
			long, err := matchLong(name)
			if err != "" {
				return o, usageExit(err)
			}
			if hasValue {
				return o, usageExit(fmt.Sprintf("option '--%s' doesn't allow an argument", long))
			}
			switch long {
			case "version":
				out := version + "\n"
				if p.dropIn {
					out = ninjaVersion + "\n"
				}
				return o, &exit{code: 0, stdout: out}
			case "help":
				return o, &exit{code: 1, stderr: usage(p, o)}
			case "verbose":
				o.verbose = true
			case "quiet":
				o.quiet = true
			}
			continue
		case len(a) < 2 || a[0] != '-':
			o.targets = append(o.targets, a)
			continue
		}
		for j := 1; j < len(a); j++ {
			c := a[j]
			if !strings.ContainsRune(withValue, rune(c)) {
				switch c {
				case 'n':
					o.dryRun = true
				case 'v':
					o.verbose = true
				case 'h':
					return o, &exit{code: 1, stderr: usage(p, o)}
				default:
					return o, usageExit(fmt.Sprintf("invalid option -- '%c'", c))
				}
				continue
			}
			value := a[j+1:]
			if value == "" {
				if i+1 == len(args) {
					return o, usageExit(fmt.Sprintf("option requires an argument -- '%c'", c))
				}
				i++
				value = args[i]
			}
			if e := o.set(p, c, value, stderr); e != nil {
				return o, e
			}
			if c == 't' {
				// Everything after the tool name belongs to the tool.
				o.targets = append(o.targets, args[i+1:]...)
				return o, nil
			}
			break
		}
	}
	return o, nil
}

// matchLong resolves a long option or an unambiguous prefix of one, with
// getopt_long's messages.
func matchLong(name string) (string, string) {
	var found []string
	for _, l := range []string{"help", "version", "verbose", "quiet"} {
		if l == name {
			return l, ""
		}
		if name != "" && strings.HasPrefix(l, name) {
			found = append(found, l)
		}
	}
	switch len(found) {
	case 1:
		return found[0], ""
	case 0:
		return "", fmt.Sprintf("unrecognized option '--%s'", name)
	}
	return "", fmt.Sprintf("option '--%s' is ambiguous; possibilities: '--%s'", name, strings.Join(found, "' '--"))
}

func (o *options) set(p program, c byte, value string, stderr io.Writer) *exit {
	fatal := func(msg string) *exit { return &exit{code: 1, stderr: p.name + ": fatal: " + msg + "\n"} }
	errorf := func(msg string) *exit { return &exit{code: 1, stderr: p.name + ": error: " + msg + "\n"} }
	switch c {
	case 'C':
		o.dir = value
	case 'f':
		o.file = value
	case 't':
		o.tool = value
	case 'd':
		switch value {
		case "list":
			return &exit{code: 1, stdout: debugList}
		case "explain":
			o.explain = true
		case "keepdepfile":
			o.keepDepfile = true
		case "keeprsp":
			o.keepRsp = true
		case "stats":
			o.stats = true
		case "sched":
			if p.dropIn {
				return errorf("unknown debug setting 'sched'")
			}
			o.schedDebug = true
		case "nostatcache":
		default:
			msg := fmt.Sprintf("unknown debug setting '%s'", value)
			if s := spellcheck(value, "stats", "explain", "keepdepfile", "keeprsp", "nostatcache"); s != "" {
				msg += fmt.Sprintf(", did you mean '%s'?", s)
			}
			return errorf(msg)
		}
	case 'w':
		switch value {
		case "list":
			return &exit{code: 1, stdout: "warning flags:\n  phonycycle={err,warn}  phony build statement references itself\n"}
		case "phonycycle=err":
			o.phonyCycleErr = true
		case "phonycycle=warn":
			o.phonyCycleErr = false
		case "dupbuild=err", "dupbuild=warn":
			fmt.Fprintf(stderr, "%s: warning: deprecated warning 'dupbuild'\n", p.name)
		case "depfilemulti=err", "depfilemulti=warn":
			fmt.Fprintf(stderr, "%s: warning: deprecated warning 'depfilemulti'\n", p.name)
		default:
			msg := fmt.Sprintf("unknown warning flag '%s'", value)
			if s := spellcheck(value, "phonycycle=err", "phonycycle=warn"); s != "" {
				msg += fmt.Sprintf(", did you mean '%s'?", s)
			}
			return errorf(msg)
		}
	case 'j':
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fatal("invalid -j parameter")
		}
		o.jobs, o.jobsSet = n, true
	case 'k':
		n, err := strconv.Atoi(value)
		if err != nil {
			return fatal("-k parameter not numeric; did you mean -k 0?")
		}
		o.failOK, o.keepGoingSet = n, true
	case 'l':
		f, ok := leadingFloat(value)
		if !ok {
			return fatal("-l parameter not numeric: did you mean -l 0.0?")
		}
		o.maxLoad = f
	}
	return nil
}

// leadingFloat parses the longest number at the start of s, like strtod.
func leadingFloat(s string) (float64, bool) {
	for end := len(s); end > 0; end-- {
		if f, err := strconv.ParseFloat(strings.TrimLeft(s[:end], " \t"), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// spellcheck returns the candidate closest to s within an edit distance
// of 3, like Ninja's SpellcheckString, or "".
func spellcheck(s string, candidates ...string) string {
	best, bestDist := "", 4
	for _, c := range candidates {
		if d := editDistance(s, c); d < bestDist {
			best, bestDist = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

const debugList = `debugging modes:
  stats        print operation counts/timing info
  explain      explain what caused a command to execute
  keepdepfile  don't delete depfiles after they're read by ninja
  keeprsp      don't delete @response files on success
multiple modes can be enabled via -d FOO -d BAR
`

// usage is Ninja 1.13's usage text, which -h and bad flags print.
func usage(p program, o options) string {
	v := version
	if p.dropIn {
		v = ninjaVersion
	}
	return fmt.Sprintf(`usage: %s [options] [targets...]

if targets are unspecified, builds the 'default' target (see manual).

options:
  --version      print %s version ("%s")
  -v, --verbose  show all command lines while building
  --quiet        don't show progress status, just command output

  -C DIR   change to DIR before doing anything else
  -f FILE  specify input build file [default=build.ninja]

  -j N     run N jobs in parallel (0 means infinity) [default=%d on this system]
  -k N     keep going until N jobs fail (0 means infinity) [default=1]
  -l N     do not start new jobs if the load average is greater than N
  -n       dry run (don't run commands but act like they succeeded)

  -d MODE  enable debugging (use '-d list' to list modes)
  -t TOOL  run a subtool (use '-t list' to list subtools)
    terminates toplevel options; further flags are passed to the tool
  -w FLAG  adjust warnings (use '-w list' to list warnings)
`, p.name, p.name, v, o.parallelism())
}

// isKilnLong reports whether arg is one of kiln's own long options.
func isKilnLong(arg string) bool {
	name, _, _ := strings.Cut(arg[2:], "=")
	return name == "mem-budget" || name == "min-jobs" || name == "mem"
}

func (o options) parallelism() int {
	if o.jobsSet {
		return o.jobs // 0 means no limit, as in Ninja
	}
	return defaultJobs(sysinfo.CPULimit())
}

// defaultJobs is the -j used when none is given. It is Ninja's (two more
// than the CPUs we may run on), except under a cgroup CPU quota, such as a
// container's --cpus, where it is the quota: the extra jobs only share the
// same CPU time, and each compile gets slower. Measured in a 4-CPU
// container: 24 RDKit compiles took a median 18% longer 6 at a time than 4
// at a time, and kiln's whole RDKit build at -j6 was 12-15% slower than
// Ninja's -j4.
func defaultJobs(cpus int, quota bool) int {
	switch {
	case quota:
		return max(cpus, 1)
	case cpus <= 1:
		return 2
	case cpus == 2:
		return 3
	default:
		return cpus + 2
	}
}

func (o options) failuresAllowed() int {
	if !o.keepGoingSet {
		return 1
	}
	if o.failOK <= 0 {
		return 1 << 30 // -k 0: keep going no matter what
	}
	return o.failOK
}
