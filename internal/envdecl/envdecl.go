// Package envdecl applies the declared environment: the variables every
// command kiln runs (builds, tests, kiln exec) sees, whatever the shell
// that started kiln had. It also hashes the declaration, so that changing
// it rebuilds what was built under the old one.
package envdecl

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"

	"github.com/Ardx19/kiln/internal/config"
)

// Decl is a merged environment declaration.
type Decl struct {
	Clean   bool
	Allow   []string
	Unset   []string
	Set     map[string]string
	Prepend map[string][]string
	Append  map[string][]string
}

// cleanBasics are kept in clean mode: without them most tools fail.
var cleanBasics = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TMPDIR", "TZ", "PWD"}

// Merge combines declarations, least specific first (user, then source,
// then build): later scalars win, lists add up, and a more specific
// prepend goes in front of a less specific one.
func Merge(envs ...config.Env) Decl {
	d := Decl{Set: map[string]string{}, Prepend: map[string][]string{}, Append: map[string][]string{}}
	for _, e := range envs {
		switch e.Mode {
		case "clean":
			d.Clean = true
		case "inherit":
			d.Clean = false
		}
		d.Allow = append(d.Allow, e.Allow...)
		d.Unset = append(d.Unset, e.Unset...)
		for k, v := range e.Set {
			d.Set[k] = v
		}
		for k, v := range e.Prepend {
			d.Prepend[k] = append(append([]string(nil), v...), d.Prepend[k]...)
		}
		for k, v := range e.Append {
			d.Append[k] = append(d.Append[k], v...)
		}
	}
	return d
}

// Empty reports whether nothing is declared.
func (d Decl) Empty() bool {
	return !d.Clean && len(d.Allow) == 0 && len(d.Unset) == 0 && len(d.Set) == 0 && len(d.Prepend) == 0 && len(d.Append) == 0
}

// Apply returns base (a list of NAME=value) changed as declared. A nil
// base means the current process environment.
func (d Decl) Apply(base []string) []string {
	if base == nil {
		base = os.Environ()
	}
	env := map[string]string{}
	var order []string
	put := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	keep := map[string]bool{}
	if d.Clean {
		for _, k := range cleanBasics {
			keep[k] = true
		}
		for _, k := range d.Allow {
			keep[k] = true
		}
	}
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || (d.Clean && !keep[k] && !strings.HasPrefix(k, "LC_")) {
			continue
		}
		put(k, v)
	}
	for _, k := range d.Unset {
		delete(env, k)
	}
	for k, v := range d.Set {
		put(k, v)
	}
	for _, k := range sortedKeys(d.Prepend) {
		put(k, strings.Join(append(append([]string(nil), d.Prepend[k]...), nonEmpty(env[k])...), ":"))
	}
	for _, k := range sortedKeys(d.Append) {
		put(k, strings.Join(append(nonEmpty(env[k]), d.Append[k]...), ":"))
	}
	out := make([]string, 0, len(env))
	for _, k := range order {
		if v, ok := env[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func nonEmpty(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Hash identifies the declaration; "" when nothing is declared. It covers
// only what's declared, not the whole environment, so unrelated variables
// changing never rebuilds anything.
func (d Decl) Hash() string {
	if d.Empty() {
		return ""
	}
	var b strings.Builder
	if d.Clean {
		b.WriteString("clean\n")
	}
	list := func(tag string, xs []string) {
		xs = append([]string(nil), xs...)
		sort.Strings(xs)
		for _, x := range xs {
			b.WriteString(tag + " " + x + "\n")
		}
	}
	list("allow", d.Allow)
	list("unset", d.Unset)
	for _, k := range sortedKeys(d.Set) {
		b.WriteString("set " + k + "=" + d.Set[k] + "\n")
	}
	for _, k := range sortedKeys(d.Prepend) {
		b.WriteString("prepend " + k + "=" + strings.Join(d.Prepend[k], ":") + "\n")
	}
	for _, k := range sortedKeys(d.Append) {
		b.WriteString("append " + k + "=" + strings.Join(d.Append[k], ":") + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}
