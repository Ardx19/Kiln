package manifest

import (
	"fmt"
	"strings"

	"github.com/Ardx19/kiln/internal/graph"
)

type token int

const (
	tokError token = iota
	tokBuild
	tokColon
	tokDefault
	tokEquals
	tokIdent
	tokInclude
	tokIndent
	tokNewline
	tokPipe
	tokPipe2
	tokPipeAt
	tokPool
	tokRule
	tokSubninja
	tokEOF
)

func (t token) String() string {
	return [...]string{
		tokError: "lexing error", tokBuild: "'build'", tokColon: "':'",
		tokDefault: "'default'", tokEquals: "'='", tokIdent: "identifier",
		tokInclude: "'include'", tokIndent: "indent", tokNewline: "newline",
		tokPipe: "'|'", tokPipe2: "'||'", tokPipeAt: "'|@'", tokPool: "'pool'",
		tokRule: "'rule'", tokSubninja: "'subninja'", tokEOF: "eof",
	}[t]
}

// errorHint adds advice to "expected X" errors.
func (t token) errorHint() string {
	if t == tokColon {
		return " ($ also escapes ':')"
	}
	return ""
}

var keywords = map[string]token{
	"build": tokBuild, "pool": tokPool, "rule": tokRule, "default": tokDefault,
	"include": tokInclude, "subninja": tokSubninja,
}

// lexer is a port of Ninja's re2c lexer (src/lexer.in.cc). The input is
// NUL-terminated, and a NUL anywhere ends it, as in Ninja.
type lexer struct {
	filename  string
	in        []byte
	ofs       int
	lastToken int // start of the last token, or -1 before the first
	// text collects literal text across escapes and separators, so each
	// run becomes one string instead of one per piece.
	text []byte
}

func newLexer(filename string, input []byte) *lexer {
	in := make([]byte, len(input)+1)
	copy(in, input)
	return &lexer{filename: filename, in: in, lastToken: -1}
}

func isVarChar(c byte) bool { // [a-zA-Z0-9_.-]
	return isSimpleVarChar(c) || c == '.'
}

func isSimpleVarChar(c byte) bool { // [a-zA-Z0-9_-]
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-'
}

// readToken returns the next token. Comment lines are skipped; a comment
// needs a trailing newline (at end of file without one it's a lexing
// error, as in Ninja).
func (l *lexer) readToken() token {
	p := l.ofs
	var start int
	var tok token
	for {
		start = p
		in := l.in
		spaces := 0
		for in[p+spaces] == ' ' {
			spaces++
		}
		q := p + spaces
		if in[q] == '#' {
			end := q
			for in[end] != 0 && in[end] != '\n' {
				end++
			}
			if in[end] == '\n' {
				p = end + 1
				continue // comment line
			}
		}
		if in[q] == '\r' && in[q+1] == '\n' {
			p, tok = q+2, tokNewline
			break
		}
		if in[q] == '\n' {
			p, tok = q+1, tokNewline
			break
		}
		if spaces > 0 {
			p, tok = q, tokIndent
			break
		}
		switch c := in[p]; {
		case c == 0:
			p, tok = p+1, tokEOF
		case c == '=':
			p, tok = p+1, tokEquals
		case c == ':':
			p, tok = p+1, tokColon
		case c == '|' && in[p+1] == '@':
			p, tok = p+2, tokPipeAt
		case c == '|' && in[p+1] == '|':
			p, tok = p+2, tokPipe2
		case c == '|':
			p, tok = p+1, tokPipe
		case isVarChar(c):
			end := p
			for isVarChar(in[end]) {
				end++
			}
			tok = tokIdent
			if kw, ok := keywords[string(in[p:end])]; ok {
				tok = kw
			}
			p = end
		default:
			p, tok = p+1, tokError
		}
		break
	}
	l.lastToken = start
	l.ofs = p
	if tok != tokNewline && tok != tokEOF {
		l.eatWhitespace()
	}
	return tok
}

// peekToken consumes the next token if it is t.
func (l *lexer) peekToken(t token) bool {
	if l.readToken() == t {
		return true
	}
	l.unreadToken()
	return false
}

func (l *lexer) unreadToken() { l.ofs = l.lastToken }

// eatWhitespace skips spaces and $-escaped line continuations.
func (l *lexer) eatWhitespace() {
	p := l.ofs
	for {
		l.ofs = p
		switch {
		case l.in[p] == ' ':
			for l.in[p] == ' ' {
				p++
			}
		case l.in[p] == '$' && l.in[p+1] == '\r' && l.in[p+2] == '\n':
			p += 3
		case l.in[p] == '$' && l.in[p+1] == '\n':
			p += 2
		default:
			return
		}
	}
}

// readIdent reads a variable, rule or pool name.
func (l *lexer) readIdent() (string, bool) {
	p := l.ofs
	start := p
	for isVarChar(l.in[p]) {
		p++
	}
	if p == start {
		l.lastToken = start
		return "", false
	}
	l.lastToken = start
	l.ofs = p
	l.eatWhitespace()
	return string(l.in[start:p]), true
}

// readPath reads a path, which ends at an unescaped space, ':', '|' or
// newline.
func (l *lexer) readPath(eval *graph.EvalString) error { return l.readEvalString(eval, true) }

// readVarValue reads a variable value, which ends at a newline.
func (l *lexer) readVarValue(eval *graph.EvalString) error { return l.readEvalString(eval, false) }

func (l *lexer) readEvalString(eval *graph.EvalString, path bool) error {
	in := l.in
	p := l.ofs
	var start int
	text := l.text[:0]
	defer func() { l.text = text[:0] }()
	flush := func() {
		if len(text) > 0 {
			eval.AddText(string(text))
			text = text[:0]
		}
	}
loop:
	for {
		start = p
		c := in[p]
		switch {
		case c != '$' && c != ' ' && c != ':' && c != '\r' && c != '\n' && c != '|' && c != 0:
			for {
				c := in[p]
				if c == '$' || c == ' ' || c == ':' || c == '\r' || c == '\n' || c == '|' || c == 0 {
					break
				}
				p++
			}
			text = append(text, in[start:p]...)
		case c == '\r' && in[p+1] == '\n':
			if !path {
				p += 2
			}
			break loop
		case c == ' ' || c == ':' || c == '|' || c == '\n':
			if path {
				break loop
			}
			p++
			if c == '\n' {
				break loop
			}
			text = append(text, c)
		case c == '$':
			d := in[p+1]
			switch {
			case d == '$':
				text = append(text, '$')
				p += 2
			case d == ' ':
				text = append(text, ' ')
				p += 2
			case d == '\r' && in[p+2] == '\n':
				p += 3
				for in[p] == ' ' {
					p++
				}
			case d == '\n':
				p += 2
				for in[p] == ' ' {
					p++
				}
			case d == '{':
				end := p + 2
				for isVarChar(in[end]) {
					end++
				}
				if end == p+2 || in[end] != '}' {
					l.lastToken = start
					return l.error("bad $-escape (literal $ must be written as $$)")
				}
				flush()
				eval.AddVariable(string(in[p+2 : end]))
				p = end + 1
			case isSimpleVarChar(d):
				end := p + 1
				for isSimpleVarChar(in[end]) {
					end++
				}
				flush()
				eval.AddVariable(string(in[p+1 : end]))
				p = end
			case d == ':':
				text = append(text, ':')
				p += 2
			default:
				l.lastToken = start
				return l.error("bad $-escape (literal $ must be written as $$)")
			}
		case c == 0:
			l.lastToken = start
			return l.error("unexpected EOF")
		default: // a lone '\r'
			l.lastToken = start
			return l.error(l.describeLastError())
		}
	}
	flush()
	l.lastToken = start
	l.ofs = p
	if path {
		l.eatWhitespace()
	}
	return nil
}

func (l *lexer) describeLastError() string {
	if l.lastToken >= 0 && l.in[l.lastToken] == '\t' {
		return "tabs are not allowed, use spaces"
	}
	return "lexing error"
}

// Error is a manifest syntax or semantic error with its location.
type Error struct {
	File    string
	Line    int
	Msg     string
	Context string // the offending line and a caret, or ""
}

func (e *Error) Error() string {
	s := fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Msg)
	if e.Context != "" {
		s += "\n" + e.Context
	}
	return s
}

// error builds an Error at the last token, with the line and a caret like
// Ninja's messages.
func (l *lexer) error(msg string) error {
	line, lineStart := 1, 0
	for p := 0; p < l.lastToken; p++ {
		if l.in[p] == '\n' {
			line++
			lineStart = p + 1
		}
	}
	e := &Error{File: l.filename, Line: line, Msg: msg}
	const truncate = 72
	if col := l.lastToken - lineStart; l.lastToken >= 0 && col > 0 && col < truncate {
		n, truncated := 0, true
		for ; n < truncate; n++ {
			if c := l.in[lineStart+n]; c == 0 || c == '\n' {
				truncated = false
				break
			}
		}
		ctx := string(l.in[lineStart : lineStart+n])
		if truncated {
			ctx += "..."
		}
		e.Context = ctx + "\n" + strings.Repeat(" ", col) + "^ near here"
	}
	return e
}
