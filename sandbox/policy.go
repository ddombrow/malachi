// Package sandbox confines what model-chosen commands and file tools may
// touch: writes only under a set of writable roots, reads inside the user's
// home only under a set of readable roots, no access at all to hidden paths
// (credentials, malachi's own home), and optionally no network.
//
// Commands are confined by the operating system (Seatbelt on macOS, Landlock
// and seccomp on Linux); the file tools, which run inside malachi, check the
// same Policy themselves. It is a fence against a prompt-injected command, not
// a virtual machine: anything readable and not hidden can still be read, and
// with the network on it can be sent somewhere.
//
// The package depends on nothing else in this module.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Policy is what one sandboxed command or file tool may do. Paths are
// absolute and canonical (symlinks resolved, as far as they exist).
type Policy struct {
	Enabled bool
	Network bool // outbound IP networking
	// UnixSockets allows connecting to any Unix socket. Off, commands cannot
	// reach local daemons such as Docker or ssh-agent, which would let them
	// act outside the sandbox (a container mounting the home directory, a
	// push with loaded keys). On macOS sockets under writable roots still
	// work; on Linux no Unix sockets can be created.
	UnixSockets bool
	Writable    []string // roots under which writes are allowed
	// Private directories (the user's home) are readable only under a
	// Readable or Writable root. Everything outside them stays readable:
	// commands need the system's files to run at all, and private data
	// lives in the home directory.
	Private  []string
	Readable []string // roots readable inside Private directories
	Hidden   []string // neither readable nor writable; wins over everything
}

// Config is everything a Policy is built from: settings, per-project grants
// and command-line flags all end up here, so there is one place that decides.
type Config struct {
	Disabled bool
	Cwd      string // the project; writable
	Home     string // malachi's home (sessions, .env, settings); hidden
	NoNet    bool
	// UnixSockets allows connecting to any Unix socket; see Policy.
	UnixSockets bool
	// ReadAnywhere lifts the read confinement: the user's home becomes
	// readable outside the readable roots too, hidden paths aside.
	ReadAnywhere bool
	// Writable, Readable and Hidden extend the defaults. Unhidden removes
	// defaults: a hidden path equal to or under one of these is dropped.
	Writable []string
	Readable []string
	Hidden   []string
	Unhidden []string
}

// Build turns c into a Policy with the default writable roots and hidden
// paths for this user and platform.
func Build(c Config) Policy {
	p := Policy{Enabled: !c.Disabled, Network: !c.NoNet, UnixSockets: c.UnixSockets}
	userHome, _ := os.UserHomeDir()
	inHome := func(rel string) string {
		if userHome == "" {
			return ""
		}
		return filepath.Join(userHome, rel)
	}

	// /tmp is checked like the derived roots: with HOME under it, as in some
	// containers, it would make the whole home directory writable.
	writable := []string{c.Cwd, safeDerivedRoot("/tmp", userHome), safeDerivedRoot(os.TempDir(), userHome)}
	if dir, err := os.UserCacheDir(); err == nil {
		writable = append(writable, safeDerivedRoot(dir, userHome)) // go-build, pip, and most other caches
	}
	writable = append(writable, safeDerivedRoot(os.Getenv("GOCACHE"), userHome), safeDerivedRoot(goModCache(userHome), userHome),
		inHome(".npm"), inHome(".cargo/registry"), inHome(".cargo/git"), inHome(".rustup"))
	writable = append(writable, c.Writable...)

	var private, readable []string
	if !c.ReadAnywhere && userHome != "" {
		private = []string{userHome}
		readable = append(defaultReadable(userHome), c.Cwd)
		readable = append(readable, c.Readable...)
	}

	hidden := []string{c.Home}
	for _, rel := range defaultHidden {
		hidden = append(hidden, inHome(rel))
	}
	hidden = append(hidden, c.Hidden...)
	unhidden := canonicalAll(c.Unhidden)
	hidden = slices.DeleteFunc(canonicalAll(hidden), func(h string) bool {
		return slices.ContainsFunc(unhidden, func(u string) bool { return within(h, u) })
	})

	p.Writable, p.Hidden = canonicalAll(writable), hidden
	p.Private, p.Readable = canonicalAll(private), canonicalAll(readable)
	return p
}

// defaultReadable are the places in the user's home that ordinary commands
// read to run: toolchains and package managers, binaries on PATH, git's
// configuration and the malachi binary itself (on Linux the sandbox helper
// is malachi). Writable roots are readable anyway.
func defaultReadable(userHome string) []string {
	var out []string
	for _, rel := range []string{
		".cargo", ".rustup", ".nvm", ".pyenv", ".rbenv", ".asdf", ".volta", ".bun", ".deno",
		".local/bin", ".local/share/mise", ".local/share/pnpm", ".local/share/uv",
		".sdkman", ".gradle", ".m2", ".nix-profile",
		".gitconfig", ".config/git", ".config/mise",
		"go/bin", "sdk", // sdk: Go toolchains from golang.org/dl
	} {
		out = append(out, filepath.Join(userHome, rel))
	}
	if dir, err := os.UserConfigDir(); err == nil {
		out = append(out, filepath.Join(dir, "go")) // go env -w settings
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		out = append(out, filepath.Join(filepath.SplitList(gopath)[0], "bin"))
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		// A PATH entry naming the home directory itself would make the
		// confinement pointless; relative entries such as "." are skipped.
		if filepath.IsAbs(dir) && Canonical(dir) != Canonical(userHome) {
			out = append(out, dir)
			if root := goRootOf(dir); root != "" {
				out = append(out, root)
			}
		}
	}
	if goroot := os.Getenv("GOROOT"); filepath.IsAbs(goroot) {
		out = append(out, goroot)
	}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Dir(exe))
	}
	// Anything not under the home directory is readable already; keeping
	// those out keeps the list short enough to show.
	home := Canonical(userHome)
	return slices.DeleteFunc(out, func(p string) bool {
		c := Canonical(p)
		return c == home || !within(c, home)
	})
}

// goRootOf returns the Go installation whose bin directory is dir, if it is
// one. go reads its standard library and tools from there, so a Go installed
// under the home directory (a CI runner's tool cache, a tarball unpacked in
// ~/go1.x) needs the whole installation, not just the bin on PATH.
func goRootOf(dir string) string {
	if filepath.Base(dir) != "bin" {
		return ""
	}
	root := filepath.Dir(dir)
	if st, err := os.Stat(filepath.Join(root, "src", "runtime")); err != nil || !st.IsDir() {
		return ""
	}
	return root
}

// safeDerivedRoot validates writable roots inferred from process environment
// and platform defaults. A typo such as GOCACHE=/ or TMPDIR=$HOME must not
// silently turn a cache convenience into broad filesystem write access.
func safeDerivedRoot(path, userHome string) string {
	if path == "" || !filepath.IsAbs(path) {
		return ""
	}
	root := Canonical(path)
	volumeRoot := filepath.Clean(filepath.VolumeName(root) + string(filepath.Separator))
	if root == volumeRoot || userHome != "" && within(Canonical(userHome), root) {
		return ""
	}
	return root
}

// defaultHidden are credential stores under the user's home directory.
var defaultHidden = []string{
	".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker/config.json",
	".config/gh", ".config/gcloud", ".config/op", ".password-store",
	".netrc", ".git-credentials", ".npmrc", ".pypirc",
	".cargo/credentials", ".cargo/credentials.toml", ".terraform.d/credentials.tfrc.json",
	"Library/Keychains",
}

func goModCache(userHome string) string {
	if v := os.Getenv("GOMODCACHE"); v != "" {
		return v
	}
	gopath := os.Getenv("GOPATH")
	if gopath == "" && userHome != "" {
		gopath = filepath.Join(userHome, "go")
	}
	if gopath == "" {
		return ""
	}
	// GOPATH may list several directories; the module cache uses the first.
	return filepath.Join(filepath.SplitList(gopath)[0], "pkg", "mod")
}

// canonicalAll canonicalizes paths, dropping empty and relative ones and
// duplicates, and keeps them sorted.
func canonicalAll(paths []string) []string {
	var out []string
	for _, p := range paths {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		out = append(out, Canonical(p))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Canonical resolves symlinks in the longest existing prefix of the absolute
// path p and appends the rest, so a path that does not exist yet compares
// correctly with existing ones (on macOS /var is a link to /private/var).
func Canonical(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// within reports whether p is root or inside it. Both must be clean.
func within(p, root string) bool {
	if p == root || root == "/" {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}

// ErrDenied is wrapped by every error the checks return.
var ErrDenied = errors.New("blocked by the sandbox")

// HiddenAt reports the hidden path that covers the absolute path p, or "".
func (p Policy) HiddenAt(path string) string {
	if !p.Enabled {
		return ""
	}
	return p.HiddenAtCanonical(Canonical(path))
}

// HiddenAtCanonical is HiddenAt for a path already canonical, which saves
// resolving symlinks for every file of a directory walk.
func (p Policy) HiddenAtCanonical(c string) string {
	if !p.Enabled {
		return ""
	}
	for _, h := range p.Hidden {
		if within(c, h) {
			return h
		}
	}
	return ""
}

// PrivateAtCanonical reports the private directory that keeps the canonical
// path c from being read, or "" when c is outside every private directory or
// under a readable or writable root.
func (p Policy) PrivateAtCanonical(c string) string {
	if !p.Enabled {
		return ""
	}
	for _, priv := range p.Private {
		if !within(c, priv) {
			continue
		}
		for _, roots := range [][]string{p.Readable, p.Writable} {
			for _, r := range roots {
				if within(c, r) {
					return ""
				}
			}
		}
		return priv
	}
	return ""
}

// ReadableCanonical reports whether the canonical path c may be read.
func (p Policy) ReadableCanonical(c string) bool {
	return p.HiddenAtCanonical(c) == "" && p.PrivateAtCanonical(c) == ""
}

// CheckRead allows reading the absolute path unless it is hidden or in a
// private directory outside the readable roots. A symlink is judged by where
// it leads.
func (p Policy) CheckRead(path string) error {
	if !p.Enabled {
		return nil
	}
	return p.checkReadCanonical(path, Canonical(path))
}

// checkReadCanonical is CheckRead for path, whose canonical form is c.
func (p Policy) checkReadCanonical(path, c string) error {
	if h := p.HiddenAtCanonical(c); h != "" {
		return fmt.Errorf("%w: %s is hidden (%s); ask the user if access is needed", ErrDenied, path, h)
	}
	if p.PrivateAtCanonical(c) != "" {
		return fmt.Errorf("%w: %s is outside the readable directories; ask the user to add it as a read root if access is needed", ErrDenied, path)
	}
	return nil
}

// CheckWrite allows writing the absolute path if it is under a writable root
// and not hidden. A path that does not exist yet is judged by its nearest
// existing ancestor, so a symlinked directory cannot redirect a new file.
func (p Policy) CheckWrite(path string) error {
	if !p.Enabled {
		return nil
	}
	c := Canonical(path)
	if h := p.HiddenAtCanonical(c); h != "" {
		return fmt.Errorf("%w: %s is hidden (%s); ask the user if access is needed", ErrDenied, path, h)
	}
	for _, w := range p.Writable {
		if within(c, w) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is outside the writable directories (the project, temp and build caches); ask the user if access is needed", ErrDenied, path)
}
