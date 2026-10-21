package build

import (
	"runtime"
	"sync"

	"github.com/Ardx19/kiln/internal/graph"
)

// prefetchMin is the fewest unstatted nodes worth statting in parallel.
const prefetchMin = 256

// prefetch stats, in parallel, every node the scan from target is likely
// to stat: edge inputs and outputs, dyndep files, validations, and the
// inputs the deps log lists for each output. Ninja stats one node at a
// time as its scan reaches it; on a large tree that is most of a no-op
// build. The scan itself is unchanged and finds the mtimes already
// cached. A node whose stat fails is left unstatted, so the scan stats it
// again and reports the error where Ninja would.
func (s *Scanner) prefetch(target *graph.Node) {
	if len(s.walked) < len(s.state.Edges) {
		s.walked = append(s.walked, make([]bool, len(s.state.Edges)-len(s.walked))...)
	}
	var todo []*graph.Node
	add := func(n *graph.Node) {
		if n != nil && !n.Prefetched {
			n.Prefetched = true
			todo = append(todo, n)
		}
	}
	var stat []*graph.Node
	add(target)
	for len(todo) > 0 {
		n := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		if !n.StatusKnown() {
			stat = append(stat, n)
		}
		e := n.InEdge
		if e == nil || e.ID >= len(s.walked) || s.walked[e.ID] {
			continue
		}
		s.walked[e.ID] = true
		for _, in := range e.Inputs {
			add(in)
		}
		for _, out := range e.Outputs {
			add(out)
			if d := s.deps[out]; d != nil {
				for _, in := range d.inputs {
					add(in)
				}
			}
		}
		add(e.Dyndep)
		for _, v := range e.Validations {
			add(v)
		}
	}
	if len(stat) < prefetchMin {
		return
	}
	workers := min(32, 2*runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < len(stat); i += workers {
				_ = stat[i].Stat(s.disk) // on error the scan stats it again
			}
		}()
	}
	wg.Wait()
}
