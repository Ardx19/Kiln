// Package depfile parses Makefile-style dependency files as written by
// compilers (gcc -MD, clang -MD) and code generators. It is a port of
// Ninja's DepfileParser (src/depfile_parser.in.cc), escaping rules
// included, in each of the variants Ninja 1.10 through 1.13 use.
package depfile

import (
	"errors"
	"slices"
)

// Dialect selects the parsing rules of a Ninja release.
type Dialect int

// Dialects, named after the first Ninja release that used them.
const (
	// Ninja110 (1.10, 1.11): a depfile with no names at all, not even
	// whitespace, still needs a colon.
	Ninja110 Dialect = iota
	// Ninja112 (1.12): a depfile without names is fine.
	Ninja112
	// Ninja113 (1.13): ? " ' and & may appear in names; before, each one
	// ended the name.
	Ninja113
)

// DialectForLog returns the dialect of the Ninja that writes a build log
// of this version (0, no log yet, means the newest).
func DialectForLog(version int) Dialect {
	switch version {
	case 5:
		return Ninja110
	case 6:
		return Ninja112
	}
	return Ninja113
}

// Parse parses with the newest rules. See ParseDialect.
func Parse(content []byte) (outs, ins []string, err error) {
	return ParseDialect(content, Ninja113)
}

// ParseDialect returns the targets (outs) and prerequisites (ins) a
// depfile lists, de-escaped, in order, without duplicates.
func ParseDialect(content []byte, d Dialect) (outs, ins []string, err error) {
	p := parser{in: content, dialect: d}
	return p.parse()
}

type parser struct {
	in      []byte
	i       int
	dialect Dialect
}

// at returns the byte at offset k from the cursor, or NUL past the end,
// matching the NUL that terminates Ninja's buffer.
func (p *parser) at(k int) byte {
	if p.i+k < len(p.in) {
		return p.in[p.i+k]
	}
	return 0
}

// plain reports whether c can appear unescaped in a file name.
func (p *parser) plain(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c >= 0x80:
		return true
	}
	switch c {
	case '+', ',', '/', '_', ':', '.', '~', '(', ')', '}', '{', '%', '=', '@', '[', ']', '!', '-':
		return true
	case '?', '"', '\'', '&':
		return p.dialect >= Ninja113
	}
	return false
}

func (p *parser) parse() (outs, ins []string, err error) {
	haveTarget, parsingTargets, poisoned, empty := false, true, false, true
	for p.i < len(p.in) {
		haveNewline := false
		var name []byte
	token:
		for {
			c := p.at(0)
			switch {
			case c == '\\':
				k := 0
				for p.at(k) == '\\' {
					k++
				}
				next := p.at(k)
				switch {
				case next == ' ' && k%2 == 1:
					// 2N+1 backslashes and a space: N backslashes and an
					// escaped space.
					name = appendBackslashes(name, k/2)
					name = append(name, ' ')
					p.i += k + 1
				case next == ' ':
					// 2N backslashes and a space: the backslashes, then
					// the end of the name.
					name = appendBackslashes(name, k)
					p.i += k + 1
					break token
				case next == '#':
					name = appendBackslashes(name, k-1)
					name = append(name, '#')
					p.i += k + 1
				case next == ':' && isSpace(p.at(k+1)):
					// A target's colon that happens to follow backslashes.
					name = appendBackslashes(name, k)
					name = append(name, ':')
					if p.at(k+1) == '\n' {
						haveNewline = true
					}
					p.i += k + 2
					break token
				case next == ':':
					name = appendBackslashes(name, k-1)
					name = append(name, ':')
					p.i += k + 1
				case next != 0 && next != '\r' && next != '\n':
					// Backslashes before anything else are literal.
					name = append(name, p.in[p.i:p.i+k+1]...)
					p.i += k + 1
				case k >= 2:
					// Before a newline or the end, the last backslash is
					// taken as the "anything else" character.
					name = append(name, p.in[p.i:p.i+k]...)
					p.i += k
				case next == '\n':
					p.i += 2 // a line continuation ends the name
					break token
				case next == '\r' && p.at(k+1) == '\n':
					p.i += 3
					break token
				default:
					p.i++ // a lone backslash before \r or the end
					break token
				}
			case c == '$' && p.at(1) == '$':
				name = append(name, '$')
				p.i += 2
			case p.plain(c):
				start := p.i
				for p.i < len(p.in) && p.plain(p.in[p.i]) {
					p.i++
				}
				name = append(name, p.in[start:p.i]...)
			case c == 0 && p.i >= len(p.in):
				break token
			case c == 0:
				p.i++ // a NUL byte ends the name
				break token
			case c == '\n':
				p.i++
				haveNewline = true
				break token
			case c == '\r' && p.at(1) == '\n':
				p.i += 2
				haveNewline = true
				break token
			default:
				p.i++ // whitespace or anything else: swallow it
				break token
			}
		}

		isDependency := !parsingTargets
		if n := len(name); n > 0 && name[n-1] == ':' {
			name = name[:n-1]
			parsingTargets = false
			haveTarget = true
		}
		if len(name) > 0 {
			empty = false
			s := string(name)
			switch {
			case !slices.Contains(ins, s):
				if isDependency {
					if poisoned {
						return nil, nil, errors.New("inputs may not also have inputs")
					}
					ins = append(ins, s)
				} else if !slices.Contains(outs, s) {
					outs = append(outs, s)
				}
			case !isDependency:
				// An earlier input appears as a target; any further
				// inputs on this line are an error.
				poisoned = true
			}
		}
		if haveNewline {
			parsingTargets = true
			poisoned = false
		}
	}
	if !haveTarget && (!empty || p.dialect < Ninja112) {
		return nil, nil, errors.New("expected ':' in depfile")
	}
	return outs, ins, nil
}

func isSpace(c byte) bool {
	return c == 0 || c == ' ' || c == '\r' || c == '\n' || c == '\t'
}

func appendBackslashes(b []byte, n int) []byte {
	for range n {
		b = append(b, '\\')
	}
	return b
}
