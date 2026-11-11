package coding

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ddombrow/malachi/sandbox"
)

// gitRepo is the git repository holding a project directory: its top-level
// directory, and the git directories git reads and writes for it. In an
// ordinary checkout that is top/.git; a linked worktree adds the shared
// directory of its main checkout, and a submodule's lives under its parent's
// .git/modules, both possibly outside top.
type gitRepo struct {
	Top     string
	GitDirs []string
}

// findRepo finds the repository holding dir by walking up to the nearest
// .git, as git does. ok is false when there is none. Paths are canonical.
//
// It reads .git itself rather than asking git, and follows a .git file (a
// worktree's or submodule's link) only when the far side links back: the
// project can be written by the model, so a planted "gitdir: ~/other/.git"
// must not make another repository's directory writable. A link that does
// not check out leaves GitDirs empty.
func findRepo(dir string) (repo gitRepo, ok bool) {
	for d := sandbox.Canonical(dir); ; d = filepath.Dir(d) {
		dotgit := filepath.Join(d, ".git")
		if st, err := os.Lstat(dotgit); err == nil {
			repo.Top = d
			switch {
			case st.IsDir():
				repo.GitDirs = []string{dotgit}
			case st.Mode().IsRegular():
				repo.GitDirs = linkedGitDirs(d, dotgit)
			}
			return repo, true
		}
		if filepath.Dir(d) == d {
			return gitRepo{}, false
		}
	}
}

// linkedGitDirs returns the git directories a .git file at dotgit (in top)
// leads to, if they link back to it.
func linkedGitDirs(top, dotgit string) []string {
	gitDir, ok := readGitPath(dotgit, "gitdir: ", top)
	if !ok {
		return nil
	}
	if common, ok := readGitPath(filepath.Join(gitDir, "commondir"), "", gitDir); ok {
		// A linked worktree. Its directory is <common>/worktrees/<name>, and
		// git records the worktree's .git file in <gitdir>/gitdir; only
		// `git worktree` writes either. Requiring the layout matters once
		// common is writable: a rewritten commondir must not lead the next
		// session to another repository.
		back, ok := readGitPath(filepath.Join(gitDir, "gitdir"), "", gitDir)
		if !ok || back != dotgit || filepath.Dir(gitDir) != filepath.Join(common, "worktrees") || !isDir(common) {
			return nil
		}
		return []string{gitDir, common}
	}
	// A submodule: git keeps its directory at .git/modules/<path> of the
	// repository above it, which the model cannot create.
	for a := filepath.Dir(top); ; a = filepath.Dir(a) {
		if rel, err := filepath.Rel(a, top); err == nil && gitDir == filepath.Join(a, ".git", "modules", rel) {
			return []string{gitDir}
		}
		if filepath.Dir(a) == a {
			return nil
		}
	}
}

// readGitPath reads the one-line file at path, which must start with prefix,
// and returns the path it names, resolved against base and canonical.
func readGitPath(path, prefix, base string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 4096 {
		return "", false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix)
	if !ok || rest == "" {
		return "", false
	}
	if !filepath.IsAbs(rest) {
		rest = filepath.Join(base, rest)
	}
	return sandbox.Canonical(rest), true
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// addRepoRoots extends c with the repository holding the project in cwd, so
// git works from a subdirectory or a linked worktree: its top level becomes
// readable and its git directories writable. Nothing is added that would
// cover the user's whole home directory, such as a home directory kept under
// version control.
func addRepoRoots(c *sandbox.Config, cwd string) {
	repo, ok := findRepo(cwd)
	if !ok {
		return
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return
	}
	home := sandbox.Canonical(userHome)
	if withinPath(home, repo.Top) {
		return
	}
	c.Readable = append(c.Readable, repo.Top)
	for _, g := range repo.GitDirs {
		if !withinPath(home, g) {
			c.Writable = append(c.Writable, g)
		}
	}
}
