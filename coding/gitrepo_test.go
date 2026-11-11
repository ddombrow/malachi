package coding

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ddombrow/malachi/sandbox"
)

// mkfile writes content to path, creating its directories.
func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindRepoPlainCheckout(t *testing.T) {
	root := sandbox.Canonical(t.TempDir())
	repo := filepath.Join(root, "repo")
	mkdirs(t, filepath.Join(repo, ".git"), filepath.Join(repo, "pkg", "sub"))
	got, ok := findRepo(filepath.Join(repo, "pkg", "sub"))
	if !ok || got.Top != repo || !slices.Equal(got.GitDirs, []string{filepath.Join(repo, ".git")}) {
		t.Fatalf("findRepo = %+v, %v", got, ok)
	}
	if _, ok := findRepo(filepath.Join(root)); ok {
		t.Fatal("found a repository above a plain directory") // assumes no .git above the temp dir
	}
}

// worktree lays out a main checkout and a linked worktree as git does.
func worktree(t *testing.T) (main, wt, gitDir string) {
	t.Helper()
	root := sandbox.Canonical(t.TempDir())
	main, wt = filepath.Join(root, "main"), filepath.Join(root, "wt")
	gitDir = filepath.Join(main, ".git", "worktrees", "wt")
	mkdirs(t, filepath.Join(main, ".git", "objects"), wt)
	mkfile(t, filepath.Join(wt, ".git"), "gitdir: "+gitDir+"\n")
	mkfile(t, filepath.Join(gitDir, "commondir"), "../..\n")
	mkfile(t, filepath.Join(gitDir, "gitdir"), filepath.Join(wt, ".git")+"\n")
	return main, wt, gitDir
}

func TestFindRepoWorktree(t *testing.T) {
	main, wt, gitDir := worktree(t)
	got, ok := findRepo(wt)
	if !ok || got.Top != wt || !slices.Equal(got.GitDirs, []string{gitDir, filepath.Join(main, ".git")}) {
		t.Fatalf("findRepo = %+v, %v", got, ok)
	}
}

// A .git file the model could have written must not grant a directory that
// does not link back to it.
func TestFindRepoRefusesForgedLinks(t *testing.T) {
	t.Run("worktree without back-link", func(t *testing.T) {
		_, wt, gitDir := worktree(t)
		mkfile(t, filepath.Join(gitDir, "gitdir"), "/somewhere/else/.git\n")
		if got, _ := findRepo(wt); len(got.GitDirs) != 0 {
			t.Fatalf("granted %v", got.GitDirs)
		}
	})
	t.Run("rewritten commondir", func(t *testing.T) {
		main, wt, gitDir := worktree(t)
		other := filepath.Join(filepath.Dir(main), "other", ".git")
		mkdirs(t, other)
		mkfile(t, filepath.Join(gitDir, "commondir"), other+"\n")
		if got, _ := findRepo(wt); len(got.GitDirs) != 0 {
			t.Fatalf("granted %v", got.GitDirs)
		}
	})
	t.Run("gitdir into another repository", func(t *testing.T) {
		root := sandbox.Canonical(t.TempDir())
		other := filepath.Join(root, "other", ".git")
		mkdirs(t, other)
		mkfile(t, filepath.Join(root, "proj", ".git"), "gitdir: "+other+"\n")
		if got, _ := findRepo(filepath.Join(root, "proj")); len(got.GitDirs) != 0 {
			t.Fatalf("granted %v", got.GitDirs)
		}
	})
	t.Run("another submodule's directory", func(t *testing.T) {
		root := sandbox.Canonical(t.TempDir())
		mkdirs(t, filepath.Join(root, ".git", "modules", "lib", "b"))
		mkfile(t, filepath.Join(root, "lib", "a", ".git"), "gitdir: ../../.git/modules/lib/b\n")
		if got, _ := findRepo(filepath.Join(root, "lib", "a")); len(got.GitDirs) != 0 {
			t.Fatalf("granted %v", got.GitDirs)
		}
	})
}

func TestFindRepoSubmodule(t *testing.T) {
	root := sandbox.Canonical(t.TempDir())
	gitDir := filepath.Join(root, ".git", "modules", "lib", "a")
	mkdirs(t, gitDir)
	mkfile(t, filepath.Join(root, "lib", "a", ".git"), "gitdir: ../../.git/modules/lib/a\n")
	got, ok := findRepo(filepath.Join(root, "lib", "a"))
	if !ok || !slices.Equal(got.GitDirs, []string{gitDir}) {
		t.Fatalf("findRepo = %+v, %v", got, ok)
	}
}

func TestAddRepoRoots(t *testing.T) {
	root := sandbox.Canonical(t.TempDir())
	userHome := filepath.Join(root, "home")
	repo := filepath.Join(userHome, "src", "repo")
	mkdirs(t, filepath.Join(repo, ".git"), filepath.Join(repo, "cmd"))
	t.Setenv("HOME", userHome)

	var c sandbox.Config
	addRepoRoots(&c, filepath.Join(repo, "cmd"))
	if !slices.Equal(c.Readable, []string{repo}) || !slices.Equal(c.Writable, []string{filepath.Join(repo, ".git")}) {
		t.Fatalf("readable %v writable %v", c.Readable, c.Writable)
	}

	// A home directory under version control must not make the whole home
	// readable, nor its .git writable.
	mkdirs(t, filepath.Join(userHome, ".git"), filepath.Join(userHome, "notes"))
	c = sandbox.Config{}
	addRepoRoots(&c, filepath.Join(userHome, "notes"))
	if len(c.Readable) != 0 || len(c.Writable) != 0 {
		t.Fatalf("home repository granted readable %v writable %v", c.Readable, c.Writable)
	}
}
