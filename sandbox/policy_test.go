package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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

func TestEnvironmentDerivedWritableRootsCannotCoverFilesystemOrHome(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("TMPDIR", string(filepath.Separator))
	t.Setenv("XDG_CACHE_HOME", string(filepath.Separator))
	t.Setenv("GOCACHE", string(filepath.Separator))
	t.Setenv("GOMODCACHE", string(filepath.Separator))
	p := Build(Config{Cwd: cwd, Home: filepath.Join(cwd, "malachi")})
	if slices.Contains(p.Writable, Canonical(string(filepath.Separator))) {
		t.Fatal("an environment-derived cache granted the filesystem root")
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range p.Writable {
		if within(Canonical(userHome), root) {
			t.Fatalf("an environment-derived root covers the user's home: %s", root)
		}
	}
}

func TestUnhiddenRemovesDefaults(t *testing.T) {
	_, project, _, home, _ := fixture(t)
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	settings := Canonical(filepath.Join(userHome, ".m2/settings.xml"))
	ssh := Canonical(filepath.Join(userHome, ".ssh"))
	mine := Canonical(filepath.Join(userHome, ".m2/settings-security.xml"))
	if p := Build(Config{Cwd: project, Home: home}); !slices.Contains(p.Hidden, settings) {
		t.Fatal("Maven's settings are not hidden by default")
	}
	p := Build(Config{Cwd: project, Home: home, Unhidden: []string{settings}})
	if slices.Contains(p.Hidden, settings) {
		t.Error("unhidden path still hidden")
	}
	if !slices.Contains(p.Hidden, ssh) {
		t.Error("unhiding one default dropped another")
	}

	// Not malachi's home, and not what the user hid, even when a listed
	// path covers them.
	p = Build(Config{Cwd: project, Home: home, Hidden: []string{mine}, Unhidden: []string{home, userHome, "/"}})
	if !slices.Contains(p.Hidden, Canonical(home)) {
		t.Error("malachi's home was unhidden")
	}
	if !slices.Contains(p.Hidden, mine) {
		t.Error("a path the user hid was unhidden")
	}
	if slices.Contains(p.Hidden, ssh) {
		t.Error("a default under an unhidden directory is still hidden")
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

// privateFixture points HOME at a fake home directory holding a project, a
// toolchain, other private files and a credential directory. It returns the
// canonical home and project, and builds the default policy for the project
// with c, whose paths may lie in that home.
func privateFixture(t *testing.T, c func(userHome string) Config) (p Policy, userHome, project string) {
	t.Helper()
	root := t.TempDir()
	userHome = filepath.Join(root, "home")
	project = filepath.Join(userHome, "src", "proj")
	for _, f := range []string{
		filepath.Join(project, "main.go"),
		filepath.Join(userHome, "other", "notes.txt"),
		filepath.Join(userHome, ".cargo", "bin", "cargo"),
		filepath.Join(userHome, ".ssh", "id_ed25519"),
		filepath.Join(userHome, "extra", "ref.go"),
		filepath.Join(root, "outside", "x"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", userHome)
	var cfg Config
	if c != nil {
		cfg = c(userHome)
	}
	cfg.Cwd, cfg.Home = project, filepath.Join(root, "malachi")
	return Build(cfg), Canonical(userHome), Canonical(project)
}

func TestPrivateHomeContainers(t *testing.T) {
	// The helper only includes known containers when the current home or cwd
	// is within them; arbitrary temporary directories are not home containers.
	root := t.TempDir()
	if got := privateHomeContainers(filepath.Join(root, "home", "current"), filepath.Join(root, "home", "current")); len(got) != 0 {
		t.Fatalf("unexpected non-system containers: %v", got)
	}
}

func TestPrivateHome(t *testing.T) {
	p, userHome, project := privateFixture(t, nil)
	if !slices.Contains(p.Private, userHome) {
		t.Fatalf("private %v lacks the user's home", p.Private)
	}
	outside := filepath.Join(filepath.Dir(userHome), "outside", "x")
	for _, c := range []struct {
		name string
		path string
		read bool
	}{
		{"project file", filepath.Join(project, "main.go"), true},
		{"toolchain", filepath.Join(userHome, ".cargo", "bin", "cargo"), true},
		{"other private file", filepath.Join(userHome, "other", "notes.txt"), false},
		{"home itself", userHome, false},
		{"project's parent", filepath.Dir(project), false},
		{"outside home", outside, true},
		{"credentials", filepath.Join(userHome, ".ssh", "id_ed25519"), false},
	} {
		if got := p.CheckRead(c.path) == nil; got != c.read {
			t.Errorf("%s: read allowed=%v, want %v", c.name, got, c.read)
		}
	}
	err := p.CheckRead(filepath.Join(userHome, "other", "notes.txt"))
	if !errors.Is(err, ErrDenied) || !strings.Contains(err.Error(), "readable directories") {
		t.Fatalf("denial = %v", err)
	}
	// A write outside the writable roots is refused for that reason, even
	// where reading is refused too.
	if err := p.CheckWrite(filepath.Join(userHome, "other", "new.txt")); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("write denial = %v", err)
	}
	off := p
	off.Enabled = false
	if off.CheckRead(filepath.Join(userHome, "other", "notes.txt")) != nil {
		t.Fatal("a disabled policy denied a read")
	}
}

func TestReadRootsExtendTheDefaults(t *testing.T) {
	p, userHome, _ := privateFixture(t, func(h string) Config {
		return Config{Readable: []string{filepath.Join(h, "extra")}, Writable: []string{filepath.Join(h, "other")}}
	})
	if err := p.CheckRead(filepath.Join(userHome, "extra", "ref.go")); err != nil {
		t.Fatalf("read root: %v", err)
	}
	if err := p.CheckRead(filepath.Join(userHome, "other", "notes.txt")); err != nil {
		t.Fatalf("writable root not readable: %v", err)
	}
	// Hidden paths win over read roots.
	p.Readable = append(p.Readable, userHome)
	if p.CheckRead(filepath.Join(userHome, ".ssh", "id_ed25519")) == nil {
		t.Fatal("a read root uncovered a hidden path")
	}
}

func TestReadAnywhere(t *testing.T) {
	p, userHome, _ := privateFixture(t, func(string) Config { return Config{ReadAnywhere: true} })
	if slices.Contains(p.Private, userHome) {
		t.Fatalf("current home remained private with ReadAnywhere: %v", p.Private)
	}
	// ReadAnywhere applies to the current home; hidden paths remain denied.
	if err := p.CheckRead(filepath.Join(userHome, "other", "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if p.CheckRead(filepath.Join(userHome, ".ssh", "id_ed25519")) == nil {
		t.Fatal("ReadAnywhere uncovered a hidden path")
	}
}

// The default read roots stay inside the home directory and never cover it
// whole, whatever PATH holds.
func TestDefaultReadableStaysNarrow(t *testing.T) {
	root := t.TempDir()
	userHome := filepath.Join(root, "home")
	t.Setenv("PATH", strings.Join([]string{userHome, filepath.Join(userHome, "bin"), "/usr/bin", "."}, string(filepath.ListSeparator)))
	t.Setenv("GOPATH", filepath.Join(userHome, "gp"))
	got := defaultReadable(userHome)
	home := Canonical(userHome)
	for _, r := range got {
		if c := Canonical(r); c == home || !within(c, home) {
			t.Errorf("default read root %s is the home directory or outside it", r)
		}
	}
	for _, want := range []string{filepath.Join(userHome, "bin"), filepath.Join(userHome, "gp", "bin"), filepath.Join(userHome, ".gitconfig")} {
		if !slices.Contains(got, want) {
			t.Errorf("default read roots %v lack %s", got, want)
		}
	}
}

func TestOpenReadRefusesPrivateTargets(t *testing.T) {
	p, userHome, project := privateFixture(t, nil)
	link := filepath.Join(project, "notes-link")
	if err := os.Symlink(filepath.Join(userHome, "other", "notes.txt"), link); err != nil {
		t.Fatal(err)
	}
	if p.CheckRead(link) == nil {
		t.Error("CheckRead allowed a link to a private file")
	}
	if f, err := p.OpenRead(link); err == nil || !errors.Is(err, ErrDenied) {
		if f != nil {
			f.Close()
		}
		t.Errorf("OpenRead through a link to a private file: %v", err)
	}
	f, err := p.OpenRead(filepath.Join(project, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// A Go installed under the home directory is readable whole, since go reads
// its standard library from beside the bin on PATH; other bins stay alone.
func TestDefaultReadableIncludesGoRoots(t *testing.T) {
	userHome := filepath.Join(t.TempDir(), "home")
	goroot := filepath.Join(userHome, "toolcache", "go", "1.25", "x64")
	other := filepath.Join(userHome, "node", "bin")
	for _, d := range []string{filepath.Join(goroot, "bin"), filepath.Join(goroot, "src", "runtime"), other} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", strings.Join([]string{filepath.Join(goroot, "bin"), other}, string(filepath.ListSeparator)))
	t.Setenv("GOROOT", "")
	got := defaultReadable(userHome)
	if !slices.Contains(got, goroot) {
		t.Errorf("default read roots %v lack the Go root %s", got, goroot)
	}
	if slices.Contains(got, filepath.Dir(other)) {
		t.Errorf("default read roots %v widened a plain bin to %s", got, filepath.Dir(other))
	}
}

// With HOME under /tmp, as in some containers, /tmp stops being writable
// rather than taking the home directory with it.
func TestTmpHoldingHomeIsNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /tmp")
	}
	userHome, err := os.MkdirTemp("/tmp", "malachi-home-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(userHome) })
	t.Setenv("HOME", userHome)
	project := filepath.Join(userHome, "proj")
	p := Build(Config{Cwd: project, Home: "/nonexistent/malachi-home"})
	if err := p.CheckWrite(filepath.Join(userHome, "elsewhere", "x")); err == nil {
		t.Fatal("home under /tmp is writable")
	}
	if err := p.CheckRead(filepath.Join(userHome, "elsewhere", "x")); err == nil {
		t.Fatal("home under /tmp is readable")
	}
}

func TestDarwinClangCaches(t *testing.T) {
	both := []string{"/var/folders/8h/abc/C/clang", "/var/folders/8h/abc/C/org.llvm.clang", "/var/folders/8h/abc/C/com.apple.DeveloperTools"}
	for tmp, want := range map[string][]string{
		"/var/folders/8h/abc/T/": both,
		"/var/folders/8h/abc/T":  both,
		"/tmp":                   nil,
		"/Users/u/T":             nil, // a TMPDIR someone set: no per-user cache beside it
	} {
		if runtime.GOOS != "darwin" {
			want = nil
		}
		if got := darwinClangCaches(tmp); !slices.Equal(got, want) {
			t.Errorf("%s: got %q want %q", tmp, got, want)
		}
	}
}
