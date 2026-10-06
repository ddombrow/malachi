package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCarve(t *testing.T) {
	root := Canonical(t.TempDir())
	for _, p := range []string{"home/u/.ssh/id", "home/u/src/a.go", "home/u/.docker/config.json", "home/u/.docker/cli-plugins/x", "etc/hosts", "home/v/x"} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	j := func(p string) string { return filepath.Join(root, p) }
	hidden := []string{j("home/u/.ssh"), j("home/u/.docker/config.json"), j("home/u/.aws")} // .aws missing

	grant, split := carve(root, hidden)
	slices.Sort(grant)
	slices.Sort(split)
	wantGrant := []string{j("etc"), j("home/u/.docker/cli-plugins"), j("home/u/src"), j("home/v")}
	wantSplit := []string{root, j("home"), j("home/u"), j("home/u/.docker")}
	if !slices.Equal(grant, wantGrant) {
		t.Errorf("grant %v\nwant  %v", grant, wantGrant)
	}
	if !slices.Equal(split, wantSplit) {
		t.Errorf("split %v\nwant  %v", split, wantSplit)
	}
	// A root with nothing hidden under it is granted whole; a hidden root not at all.
	if g, s := carve(j("home/u/src"), hidden); !slices.Equal(g, []string{j("home/u/src")}) || s != nil {
		t.Errorf("unsplit root: %v %v", g, s)
	}
	if g, _ := carve(j("home/u/.ssh"), hidden); g != nil {
		t.Errorf("hidden root granted: %v", g)
	}
}

func TestCarveIgnoresMissingHiddenPaths(t *testing.T) {
	root := Canonical(t.TempDir())
	if g, s := carve(root, []string{filepath.Join(root, "absent")}); !slices.Equal(g, []string{root}) || s != nil {
		t.Fatalf("a missing hidden path split the root: %v %v", g, s)
	}
}
