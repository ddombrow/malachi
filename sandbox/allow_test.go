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

func TestReadGrants(t *testing.T) {
	root := Canonical(t.TempDir())
	home := filepath.Join(root, "home")
	for _, d := range []string{"proj", "other", ".ssh"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := Policy{Enabled: true, Private: []string{home}, Readable: []string{filepath.Join(home, "proj")}, Hidden: []string{filepath.Join(home, ".ssh")}}
	grant, split := readGrants(p)
	if !slices.Contains(grant, filepath.Join(home, "proj")) || !slices.Contains(grant, filepath.Join(root, "sys")) {
		t.Fatalf("grants %v lack the read root or a path outside home", grant)
	}
	for _, g := range grant {
		if within(g, home) && g != filepath.Join(home, "proj") {
			t.Errorf("granted %s inside the private home", g)
		}
	}
	// The home directory itself is not split, so not listable.
	if slices.Contains(split, home) || !slices.Contains(split, root) {
		t.Errorf("split %v", split)
	}

	// A read root holding the private directory makes it readable, hidden
	// paths aside.
	p.Readable = []string{root}
	grant, _ = readGrants(p)
	if !slices.Contains(grant, filepath.Join(home, "other")) || slices.Contains(grant, filepath.Join(home, ".ssh")) {
		t.Errorf("grants %v with the home under a read root", grant)
	}
}
