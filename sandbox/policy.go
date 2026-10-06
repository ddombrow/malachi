// Package sandbox confines what model-chosen commands and file tools may
// touch: writes only under a set of writable roots, no access at all to
// hidden paths (credentials, malachi's own home), and optionally no network.
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
	Hidden      []string // neither readable nor writable; wins over Writable
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
	// Writable and Hidden extend the defaults. Unhidden removes defaults:
	// a hidden path equal to or under one of these is dropped.
	Writable []string
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

	writable := []string{c.Cwd, os.TempDir(), "/tmp"}
	if dir, err := os.UserCacheDir(); err == nil {
		writable = append(writable, dir) // go-build, pip, and most other caches
	}
	writable = append(writable, os.Getenv("GOCACHE"), goModCache(userHome),
		inHome(".npm"), inHome(".cargo/registry"), inHome(".cargo/git"), inHome(".rustup"))
	writable = append(writable, c.Writable...)

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
	return p
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

// CheckRead allows reading the absolute path unless it is hidden. A symlink
// is judged by where it leads.
func (p Policy) CheckRead(path string) error {
	if h := p.HiddenAt(path); h != "" {
		return fmt.Errorf("%w: %s is hidden (%s); ask the user if access is needed", ErrDenied, path, h)
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
	if err := p.CheckRead(path); err != nil {
		return err
	}
	c := Canonical(path)
	for _, w := range p.Writable {
		if within(c, w) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is outside the writable directories (the project, temp and build caches); ask the user if access is needed", ErrDenied, path)
}
