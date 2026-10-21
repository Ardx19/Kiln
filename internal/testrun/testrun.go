// Package testrun judges a test run: it reads JUnit XML from CTest, Meson
// or a custom command, applies the expected-failure list, and says whether
// the run is good.
package testrun

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Result of one test.
type Result struct {
	Name    string
	Status  string // "pass", "fail" or "skip"
	Message string
}

type junitSuite struct {
	Cases []struct {
		Name    string `xml:"name,attr"`
		Class   string `xml:"classname,attr"`
		Failure *struct {
			Message string `xml:"message,attr"`
		} `xml:"failure"`
		Error *struct {
			Message string `xml:"message,attr"`
		} `xml:"error"`
		Skipped   *struct{} `xml:"skipped"`
		SystemOut string    `xml:"system-out"`
	} `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

type junitRoot struct {
	junitSuite
	XMLName xml.Name
}

// ParseJUnit reads a JUnit report (a <testsuites> or <testsuite> root).
func ParseJUnit(b []byte) ([]Result, error) {
	var root junitRoot
	if err := xml.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("reading the JUnit report: %v", err)
	}
	var out []Result
	var walk func(s junitSuite)
	walk = func(s junitSuite) {
		for _, c := range s.Cases {
			r := Result{Name: c.Name, Status: "pass"}
			switch {
			case c.Failure != nil:
				r.Status, r.Message = "fail", c.Failure.Message
			case c.Error != nil:
				r.Status, r.Message = "fail", c.Error.Message
			case c.Skipped != nil:
				r.Status = "skip"
			}
			out = append(out, r)
		}
		for _, sub := range s.Suites {
			walk(sub)
		}
	}
	walk(root.junitSuite)
	return out, nil
}

// Expected is the expected-failure list: test names, or /regex/ patterns,
// each optionally followed by a reason after whitespace or '#'.
type Expected struct {
	exact map[string]string
	regex []*regexp.Regexp
	why   []string
}

// ParseExpected reads a list; blank lines and # comments are ignored.
func ParseExpected(text string) (*Expected, error) {
	e := &Expected{exact: map[string]string{}}
	sc := bufio.NewScanner(strings.NewReader(text))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pat, why, _ := strings.Cut(line, "#")
		pat = strings.TrimSpace(pat)
		if strings.HasPrefix(pat, "/") {
			end := strings.LastIndex(pat, "/")
			if end <= 0 {
				return nil, fmt.Errorf("line %d: unterminated /regex/", n)
			}
			re, err := regexp.Compile(pat[1:end])
			if err != nil {
				return nil, fmt.Errorf("line %d: %v", n, err)
			}
			e.regex = append(e.regex, re)
			e.why = append(e.why, strings.TrimSpace(why+" "+pat[end+1:]))
			continue
		}
		name, rest, _ := strings.Cut(pat, " ")
		e.exact[name] = strings.TrimSpace(strings.TrimSpace(rest) + " " + strings.TrimSpace(why))
	}
	return e, sc.Err()
}

// Match reports whether name is an expected failure, and why.
func (e *Expected) Match(name string) (string, bool) {
	if e == nil {
		return "", false
	}
	if why, ok := e.exact[name]; ok {
		return why, true
	}
	for i, re := range e.regex {
		if re.MatchString(name) {
			return e.why[i], true
		}
	}
	return "", false
}

// Verdict is the outcome of a run.
type Verdict struct {
	Passed, Skipped int
	// Failed lists unexpected failures; XFail expected ones; XPass
	// expected failures that passed.
	Failed, XFail, XPass []Result
}

// Judge sorts results against the expected-failure list.
func Judge(results []Result, exp *Expected) Verdict {
	var v Verdict
	for _, r := range results {
		_, expected := exp.Match(r.Name)
		switch {
		case r.Status == "skip":
			v.Skipped++
		case r.Status == "fail" && expected:
			v.XFail = append(v.XFail, r)
		case r.Status == "fail":
			v.Failed = append(v.Failed, r)
		case expected:
			v.XPass = append(v.XPass, r)
		default:
			v.Passed++
		}
	}
	return v
}

// OK reports whether the run is good: nothing failed unexpectedly (and,
// with strict, nothing expected to fail passed).
func (v Verdict) OK(strict bool) bool {
	return len(v.Failed) == 0 && (!strict || len(v.XPass) == 0)
}

// Summary describes the verdict.
func (v Verdict) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d passed, %d failed", v.Passed+len(v.XPass), len(v.Failed))
	if len(v.XFail) > 0 {
		fmt.Fprintf(&b, ", %d expected failures", len(v.XFail))
	}
	if len(v.XPass) > 0 {
		fmt.Fprintf(&b, ", %d unexpectedly passed", len(v.XPass))
	}
	if v.Skipped > 0 {
		fmt.Fprintf(&b, ", %d skipped", v.Skipped)
	}
	return b.String()
}

// Names lists result names, sorted.
func Names(rs []Result) []string {
	n := make([]string, len(rs))
	for i, r := range rs {
		n[i] = r.Name
	}
	sort.Strings(n)
	return n
}

// ReadExpected loads the list at path; a missing file is an empty list.
func ReadExpected(path string) (*Expected, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Expected{exact: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseExpected(string(b))
}
