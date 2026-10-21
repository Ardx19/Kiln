//go:build compat

package compat

import "testing"

// TestRealState checks kiln's .ninja_log and .ninja_deps readers against
// Ninja on real trees (see CompareState for why this is safe to run on
// trees you care about).
func TestRealState(t *testing.T) {
	_, tools, _, _ := matrix(t)
	names, trees := realTrees(t)
	for i, dir := range trees {
		t.Run(names[i], func(t *testing.T) {
			r := CompareState(dir, tools)
			for _, d := range r.Diffs {
				t.Error(d)
			}
			if r.Skipped != "" {
				t.Logf("skipped: %s", r.Skipped)
			}
			t.Logf("log v%d via ninja %s: %d hashes compared (%d dirty, not comparable), %d deps outputs",
				r.LogVersion, r.Ninja, r.HashesCompared, r.HashesDirty, r.DepsOutputs)
		})
	}
}
