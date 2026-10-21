package state

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// testdata/hash_vectors.json holds hashes that real Ninja wrote to
// .ninja_log for commands of every length that exercises a different
// branch of each hash.
func TestHashVectors(t *testing.T) {
	b, err := os.ReadFile("testdata/hash_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Vectors []struct {
			Command   string `json:"command"`
			Murmur64A string `json:"murmur64a"`
			Rapidhash string `json:"rapidhash"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) < 40 {
		t.Fatalf("only %d vectors", len(file.Vectors))
	}
	for _, v := range file.Vectors {
		for version, want := range map[int]string{6: v.Murmur64A, 7: v.Rapidhash} {
			w, err := strconv.ParseUint(want, 16, 64)
			if err != nil {
				t.Fatal(err)
			}
			if got := HashCommand(version, v.Command); got != w {
				t.Errorf("v%d hash of %d-byte %q = %x, ninja recorded %x", version, len(v.Command), v.Command, got, w)
			}
		}
	}
}
