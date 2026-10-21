package main

import (
	"strings"
	"testing"
)

func TestRedirect(t *testing.T) {
	cmd := "ccache /usr/bin/c++ -DX -I../Code -MD -MT a/b.cpp.o -MF a/b.cpp.o.d -o a/b.cpp.o -c /src/b.cpp"
	got, ok := redirect(cmd, "a/b.cpp.o", "/tmp/0.o", "/tmp/0.d")
	if !ok {
		t.Fatal("redirect reported no -o argument")
	}
	want := "ccache /usr/bin/c++ -DX -I../Code -MD -MT a/b.cpp.o -MF /tmp/0.d -o /tmp/0.o -c /src/b.cpp"
	if got != want {
		t.Errorf("redirect:\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "-o a/b.cpp.o") || strings.Contains(got, "-MF a/") {
		t.Error("redirected command still writes into the build tree")
	}
	if _, ok := redirect("cp x y", "y", "/tmp/0.o", "/tmp/0.d"); ok {
		t.Error("redirect accepted a command without -o")
	}
}
