package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fixture builds a policy over a temp tree: a project, an outside directory,
// a fake malachi home and an extra hidden directory.
func fixture(t *testing.T) (p Policy, project, outside, home, secret string) {
	t.Helper()
	root := t.TempDir()
	project, outside = filepath.Join(root, "project"), filepath.Join(root, "outside")
	home, secret = filepath.Join(root, "malachi"), filepath.Join(root, "secrets")
	for _, d := range []string{project, outside, home, secret} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p = Build(Config{Cwd: project, Home: home, Hidden: []string{secret}})
	return p, Canonical(project), Canonical(outside), Canonical(home), Canonical(secret)
}

// projectOnly narrows p's writable roots to the project: the fixture lives
// in the temp directory, which is writable by default.
func projectOnly(p Policy, project string) Policy {
	p.Writable = []string{project}
	return p
}

func TestBuildDefaults(t *testing.T) {
	p, project, _, home, secret := fixture(t)
	if !p.Enabled || !p.Network {
		t.Fatalf("enabled=%v network=%v", p.Enabled, p.Network)
	}
	if !slices.Contains(p.Writable, project) || !slices.Contains(p.Writable, Canonical(os.TempDir())) ||
		p.CheckWrite(filepath.Join(os.TempDir(), "scratch")) != nil {
		t.Fatalf("writable %v lacks the project or temp", p.Writable)
	}
	if !slices.Contains(p.Hidden, home) || !slices.Contains(p.Hidden, secret) {
		t.Fatalf("hidden %v lacks the malachi home or the extra path", p.Hidden)
	}
	if userHome, err := os.UserHomeDir(); err == nil && !slices.Contains(p.Hidden, Canonical(filepath.Join(userHome, ".ssh"))) {
		t.Fatalf("hidden %v lacks ~/.ssh", p.Hidden)
	}
	for _, path := range append(p.Writable, p.Hidden...) {
		if path != Canonical(path) {
			t.Errorf("%s is not canonical", path)
		}
	}
	if p := Build(Config{Cwd: project, Disabled: true, NoNet: true}); p.Enabled || p.Network {
		t.Fatalf("flags ignored: %+v", p)
	}
}

func TestUnhiddenRemovesDefaults(t *testing.T) {
	_, project, _, home, _ := fixture(t)
	p := Build(Config{Cwd: project, Home: home, Unhidden: []string{home}})
	if slices.Contains(p.Hidden, home) {
		t.Fatal("unhidden path still hidden")
	}
}

func TestChecks(t *testing.T) {
	p, project, outside, home, secret := fixture(t)
	p = projectOnly(p, project)
	allowed := func(err error) bool { return err == nil }
	cases := []struct {
		name        string
		path        string
		read, write bool
	}{
		{"project file", filepath.Join(project, "a.go"), true, true},
		{"new nested file", filepath.Join(project, "new/dir/b.go"), true, true},
		{"outside", filepath.Join(outside, "x"), true, false},
		{"project parent via ..", filepath.Join(project, "../outside/x"), true, false},
		{"malachi home", filepath.Join(home, ".env"), false, false},
		{"hidden dir itself", secret, false, false},
	}
	for _, c := range cases {
		if got := allowed(p.CheckRead(c.path)); got != c.read {
			t.Errorf("%s: read allowed=%v, want %v", c.name, got, c.read)
		}
		if got := allowed(p.CheckWrite(c.path)); got != c.write {
			t.Errorf("%s: write allowed=%v, want %v", c.name, got, c.write)
		}
	}
	if err := p.CheckWrite(filepath.Join(outside, "x")); !errors.Is(err, ErrDenied) {
		t.Fatalf("denial does not wrap ErrDenied: %v", err)
	}
	off := p
	off.Enabled = false
	if off.CheckRead(filepath.Join(home, ".env")) != nil || off.CheckWrite(filepath.Join(outside, "x")) != nil {
		t.Fatal("a disabled policy denied something")
	}
}

// Symlinks are judged by their targets, for existing files and for new files
// created through a linked directory.
func TestChecksFollowSymlinks(t *testing.T) {
	p, project, outside, home, _ := fixture(t)
	p = projectOnly(p, project)
	if err := os.WriteFile(filepath.Join(home, ".env"), []byte("KEY=x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustLink := func(target, link string) {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	mustLink(filepath.Join(home, ".env"), filepath.Join(project, "env-link"))
	mustLink(outside, filepath.Join(project, "out-link"))
	if p.CheckRead(filepath.Join(project, "env-link")) == nil {
		t.Error("read through a link into the malachi home allowed")
	}
	if p.CheckWrite(filepath.Join(project, "out-link", "new.txt")) == nil {
		t.Error("new file through a link to an outside directory allowed")
	}
}

func TestCanonical(t *testing.T) {
	dir := t.TempDir()
	real, _ := filepath.EvalSymlinks(dir)
	if got := Canonical(filepath.Join(dir, "missing", "x")); got != filepath.Join(real, "missing", "x") {
		t.Fatalf("Canonical = %s, want under %s", got, real)
	}
}
