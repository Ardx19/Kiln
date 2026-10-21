package graph

import "strings"

// Env looks up variables while evaluating an EvalString.
type Env interface {
	LookupVariable(name string) string
}

// EvalString is a string with unexpanded $variable references, as read
// from a manifest.
type EvalString struct {
	parts []evalPart
}

type evalPart struct {
	text     string
	variable bool // text is a variable name rather than literal text
}

// AddText appends literal text.
func (e *EvalString) AddText(s string) {
	if n := len(e.parts); n > 0 && !e.parts[n-1].variable {
		e.parts[n-1].text += s
		return
	}
	e.parts = append(e.parts, evalPart{text: s})
}

// AddVariable appends a reference to the named variable.
func (e *EvalString) AddVariable(name string) {
	e.parts = append(e.parts, evalPart{text: name, variable: true})
}

// Empty reports whether nothing has been added.
func (e *EvalString) Empty() bool { return len(e.parts) == 0 }

// Evaluate expands the string with env.
func (e *EvalString) Evaluate(env Env) string {
	if len(e.parts) == 1 && !e.parts[0].variable {
		return e.parts[0].text
	}
	var b strings.Builder
	for _, p := range e.parts {
		if p.variable {
			b.WriteString(env.LookupVariable(p.text))
		} else {
			b.WriteString(p.text)
		}
	}
	return b.String()
}

// Serialize renders the parts for tests: [text] for literal text and
// [$name] for variables, matching Ninja's EvalString::Serialize.
func (e *EvalString) Serialize() string {
	var b strings.Builder
	for _, p := range e.parts {
		b.WriteByte('[')
		if p.variable {
			b.WriteByte('$')
		}
		b.WriteString(p.text)
		b.WriteByte(']')
	}
	return b.String()
}

// Scope holds variable bindings and rules. The file scope of a manifest
// is a Scope; subninja creates a child scope, and an edge with its own
// bindings gets a child of the scope it was declared in.
type Scope struct {
	parent *Scope
	vars   map[string]string
	rules  map[string]*Rule
}

// NewScope returns an empty scope whose lookups fall back to parent
// (which may be nil).
func NewScope(parent *Scope) *Scope {
	return &Scope{parent: parent, vars: map[string]string{}}
}

// Parent returns the enclosing scope, or nil.
func (s *Scope) Parent() *Scope { return s.parent }

// AddBinding sets a variable in this scope.
func (s *Scope) AddBinding(name, value string) { s.vars[name] = value }

// LookupVariable finds a variable in this scope or an enclosing one.
// Unknown variables expand to "".
func (s *Scope) LookupVariable(name string) string {
	for sc := s; sc != nil; sc = sc.parent {
		if v, ok := sc.vars[name]; ok {
			return v
		}
	}
	return ""
}

// lookupWithFallback is how an edge resolves a variable: this scope's own
// bindings first, then the rule's binding (evaluated in env), then the
// enclosing scopes. When an edge has no bindings of its own, its scope is
// the file scope, so file-level variables shadow rule bindings; Ninja
// behaves the same way.
func (s *Scope) lookupWithFallback(name string, eval *EvalString, env Env) string {
	if v, ok := s.vars[name]; ok {
		return v
	}
	if eval != nil {
		return eval.Evaluate(env)
	}
	if s.parent != nil {
		return s.parent.LookupVariable(name)
	}
	return ""
}

// AddRule adds a rule to this scope. Most scopes (one per edge with
// bindings) never get a rule, so the map is created on first use.
func (s *Scope) AddRule(r *Rule) {
	if s.rules == nil {
		s.rules = map[string]*Rule{}
	}
	s.rules[r.Name] = r
}

// LookupRule finds a rule in this scope or an enclosing one.
func (s *Scope) LookupRule(name string) *Rule {
	for sc := s; sc != nil; sc = sc.parent {
		if r, ok := sc.rules[name]; ok {
			return r
		}
	}
	return nil
}

// LookupRuleCurrentScope finds a rule in this scope only.
func (s *Scope) LookupRuleCurrentScope(name string) *Rule { return s.rules[name] }
