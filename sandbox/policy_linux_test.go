package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// The cache directory is where shells load completions from and browsers
// keep what they fetched, so commands get the build caches in it by name and
// not the directory.
func TestLinuxCachesAreGrantedByName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", "")
	cache := filepath.Join(Canonical(home), ".cache")
	p := Build(Config{Cwd: t.TempDir(), Home: filepath.Join(home, ".malachi")})

	for _, name := range []string{"go-build", "pip", "uv", "huggingface/hub", "ms-playwright"} {
		if err := p.CheckWrite(filepath.Join(cache, name, "x")); err != nil {
			t.Errorf("%s should be writable: %v", name, err)
		}
	}
	for _, name := range []string{"", "oh-my-zsh/completions/_git", "fish/generated_completions/git.fish",
		"p10k-instant-prompt-user.zsh", "mozilla/firefox", "malachi/codemode/x", "huggingface/token", "some-new-tool"} {
		path := filepath.Join(cache, name)
		if err := p.CheckWrite(path); err == nil {
			t.Errorf("%s must not be writable", path)
		}
		if err := p.CheckRead(path); err == nil {
			t.Errorf("%s must not be readable", path)
		}
	}
}

func TestLinuxDefaultsBeyondTheHome(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/elsewhere")
	p := Build(Config{Cwd: t.TempDir(), Home: t.TempDir()})
	if !slices.Contains(p.Writable, "/dev/shm") {
		t.Errorf("/dev/shm should be writable (semaphores and shared memory live there): %v", p.Writable)
	}
	uid := strconv.Itoa(os.Getuid())
	for _, path := range []string{"/run/user/" + uid + "/containers/auth.json", "/run/user/elsewhere/xauth_abc", "/var/log/journal/x/user-1000.journal"} {
		if p.HiddenAtCanonical(path) == "" {
			t.Errorf("%s should be hidden; hidden paths: %v", path, p.Hidden)
		}
	}
	// A default, so the user can lift it.
	lifted := Build(Config{Cwd: t.TempDir(), Home: t.TempDir(), Unhidden: []string{"/var/log/journal"}})
	if lifted.HiddenAtCanonical("/var/log/journal/x") != "" {
		t.Error("unhiddenPaths should be able to show the journal again")
	}
}
