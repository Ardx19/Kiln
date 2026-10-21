package graph

// CanonicalizePath normalises a path exactly as Ninja does on Linux, so
// the same file always maps to the same node:
//
//   - "." components and empty components ("a//b") are dropped
//   - "x/.." pairs are folded, but leading "../" components are kept
//   - a trailing "/" is dropped, and a path that folds to nothing is "."
//
// Symlinks are not resolved. This is a direct port of Ninja's
// CanonicalizePath (src/util.cc), including its edge cases, such as "/.."
// staying "/..".
func CanonicalizePath(path string) string {
	if isCanonical(path) {
		return path // the common case, without allocating
	}
	return canonicalize(path)
}

// canonicalize is the full algorithm behind CanonicalizePath.
func canonicalize(path string) string {
	if path == "" {
		return path
	}
	buf := []byte(path)
	end := len(buf)
	src, dst := 0, 0
	dstStart := 0

	if buf[0] == '/' {
		// Keep the leading separator of an absolute path.
		src, dst = 1, 1
		dstStart = dst
	} else {
		// Skip leading "../" components; they can't be folded.
		for src+3 <= end && buf[src] == '.' && buf[src+1] == '.' && buf[src+2] == '/' {
			src += 3
			dst += 3
		}
	}

	// Every component but the last, each with its trailing separator.
	components := 0
	dst0 := dst
	for src < end {
		next := indexByte(buf, src, end, '/')
		if next < 0 {
			break // the last component is handled below
		}
		srcNext := next + 1
		n := next - src
		if n <= 2 {
			if n == 0 {
				src = srcNext
				continue // "a//b"
			}
			if buf[src] == '.' {
				if n == 1 {
					src = srcNext
					continue // "./a"
				}
				if buf[src+1] == '.' {
					if components > 0 {
						// Back up to the start of the previous component.
						components--
						for dst--; dst > dst0 && buf[dst-1] != '/'; dst-- {
						}
					} else {
						buf[dst], buf[dst+1], buf[dst+2] = '.', '.', buf[src+2]
						dst += 3
					}
					src = srcNext
					continue
				}
			}
		}
		components++
		dst += copy(buf[dst:], buf[src:srcNext])
		src = srcNext
	}

	// The last component, which has no trailing separator.
	if n := end - src; n > 0 {
		switch {
		case buf[src] == '.' && n == 1:
			// "a/." drops the trailing "."
		case buf[src] == '.' && n == 2 && buf[src+1] == '.':
			if components > 0 {
				for dst--; dst > dst0 && buf[dst-1] != '/'; dst-- {
				}
			} else {
				buf[dst], buf[dst+1] = '.', '.'
				dst += 2
			}
		default:
			dst += copy(buf[dst:], buf[src:end])
		}
	}

	if dst > dstStart && buf[dst-1] == '/' {
		dst--
	}
	if dst == 0 {
		return "."
	}
	return string(buf[:dst])
}

// isCanonical reports whether CanonicalizePath would return path
// unchanged: no empty, "." or ".." components (other than leading "../"
// in a relative path) and no trailing "/".
func isCanonical(path string) bool {
	if path == "" || path == "." || path == "/" {
		return true
	}
	i := 0
	if path[0] == '/' {
		i = 1
	} else {
		for len(path)-i >= 3 && path[i] == '.' && path[i+1] == '.' && path[i+2] == '/' {
			i += 3
		}
	}
	start := i
	for j := i; j <= len(path); j++ {
		if j == len(path) || path[j] == '/' {
			switch path[start:j] {
			case "", ".", "..":
				return false
			}
			start = j + 1
		}
	}
	return true
}

func indexByte(b []byte, from, to int, c byte) int {
	for i := from; i < to; i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// ShellEscape quotes s for /bin/sh the way Ninja quotes $in and $out.
// Strings made only of letters, digits and _+-./ are left alone; anything
// else is wrapped in single quotes, and each embedded quote becomes:
//
//	'\''
func ShellEscape(s string) string {
	safe := true
	for i := 0; i < len(s); i++ {
		if !shellSafe(s[i]) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	out := make([]byte, 0, len(s)+2)
	out = append(out, '\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, `'\''`...)
		} else {
			out = append(out, s[i])
		}
	}
	return string(append(out, '\''))
}

func shellSafe(c byte) bool {
	switch {
	case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		return true
	}
	switch c {
	case '_', '+', '-', '.', '/':
		return true
	}
	return false
}
