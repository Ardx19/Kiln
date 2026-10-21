package tools

import (
	"bufio"
	"io"
	"strings"

	"github.com/Ardx19/kiln/internal/graph"
)

// Compdb writes a JSON compilation database like Ninja 1.13's
// `-t compdb [-x] [rules]`: one object per edge with inputs (all edges, or
// those using one of rules), in manifest order. dir is the working
// directory as getcwd(3) reports it. With expand, an "@rspfile" (or
// "-f rspfile", "--option-file=rspfile") argument is replaced by the
// response file's content.
func Compdb(w io.Writer, st *graph.State, dir string, rules []string, expand bool) error {
	bw := bufio.NewWriter(w)
	first := true
	write := func(e *graph.Edge) error {
		cmd, err := commandWithRspfile(e, expand)
		if err != nil {
			return err
		}
		if !first {
			bw.WriteByte(',')
		}
		first = false
		bw.WriteString("\n  {\n    \"directory\": \"")
		bw.WriteString(encodeJSONString(dir))
		bw.WriteString("\",\n    \"command\": \"")
		bw.WriteString(encodeJSONString(cmd))
		bw.WriteString("\",\n    \"file\": \"")
		bw.WriteString(encodeJSONString(e.Inputs[0].Path))
		bw.WriteString("\",\n    \"output\": \"")
		bw.WriteString(encodeJSONString(e.Outputs[0].Path))
		bw.WriteString("\"\n  }")
		return nil
	}
	bw.WriteByte('[')
	for _, e := range st.Edges {
		if len(e.Inputs) == 0 {
			continue
		}
		if len(rules) == 0 {
			if err := write(e); err != nil {
				return err
			}
			continue
		}
		for _, r := range rules {
			if e.Rule.Name == r {
				if err := write(e); err != nil {
					return err
				}
			}
		}
	}
	bw.WriteString("\n]\n")
	return bw.Flush()
}

// commandWithRspfile is Ninja's EvaluateCommandWithRspfile.
func commandWithRspfile(e *graph.Edge, expand bool) (string, error) {
	cmd, err := e.Command(false)
	if err != nil || !expand {
		return cmd, err
	}
	rspfile, err := e.UnescapedBinding("rspfile")
	if err != nil || rspfile == "" {
		return cmd, err
	}
	i := strings.Index(cmd, rspfile)
	if i <= 0 {
		return cmd, nil
	}
	// The first occurrence of each spelling must sit right before it.
	at := func(prefix string) bool {
		return i >= len(prefix) && strings.Index(cmd, prefix) == i-len(prefix)
	}
	content, err := e.Binding("rspfile_content")
	if err != nil {
		return cmd, err
	}
	content = strings.ReplaceAll(content, "\n", " ")
	for _, prefix := range []string{"@", "-f ", "--option-file="} {
		if (prefix == "@" && cmd[i-1] == '@') || (prefix != "@" && at(prefix)) {
			return cmd[:i-len(prefix)] + content + cmd[i+len(rspfile):], nil
		}
	}
	return cmd, nil
}

// encodeJSONString escapes s as Ninja 1.12+ does: quotes, backslashes and
// control characters. Bytes from 0x80 up pass through unchanged.
func encodeJSONString(s string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		case c == '\\':
			b.WriteString(`\\`)
		case c == '"':
			b.WriteString(`\"`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
