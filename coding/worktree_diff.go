package coding

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ddombrow/malachi/sandbox"
)

const (
	// MaxWorkingDiffBytes caps the patch WorkingTreeDiff returns.
	MaxWorkingDiffBytes = 8 << 20
	// maxUntrackedFiles caps the untracked files WorkingTreeDiff shows, and
	// maxUntrackedBytes the size of one: past it a file is listed, not shown.
	maxUntrackedFiles = 500
	maxUntrackedBytes = 512 << 10
)

// WorkingTreeDiff is what changed in the repository holding the session's
// working directory since its last commit, for a frontend to show.
type WorkingTreeDiff struct {
	// Patch is a git-style unified patch: tracked files' changes, staged or
	// not, then untracked files as new ones. Paths are the repository's.
	Patch string
	// Truncated says the patch was cut short (at a file's boundary where it
	// could be) to stay within MaxWorkingDiffBytes, or that untracked files
	// were left out past maxUntrackedFiles.
	Truncated bool
	// NoRepo says the working directory is not in a git repository, or
	// there is no git: there is nothing to diff, and that is not an error.
	NoRepo bool
	// Untracked are the paths in Patch that git does not track yet, which
	// it otherwise shows as new files.
	Untracked []string
}

// WorkingTreeDiff diffs the working tree against HEAD. git runs as the git
// tool runs it: in the sandbox, without the programs a repository's
// configuration can name, and without writing to the repository. The
// patch is for the user, not the model.
func (s *Session) WorkingTreeDiff(ctx context.Context) (WorkingTreeDiff, error) {
	repo, ok := findRepo(s.cwd)
	if !ok {
		return WorkingTreeDiff{NoRepo: true}, nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return WorkingTreeDiff{NoRepo: true}, nil
	}
	g := &gitTool{cwd: sandbox.Canonical(s.cwd), opts: s.toolOpts}
	return g.workingTreeDiff(ctx, repo.Top)
}

func (g *gitTool) workingTreeDiff(ctx context.Context, top string) (WorkingTreeDiff, error) {
	base, err := g.commit(ctx, "HEAD")
	if err != nil {
		// No commit yet: everything is new, compared with the empty tree.
		out, _, terr := g.run(ctx, "", 4096, "hash-object", "-t", "tree", "--stdin")
		if terr != nil {
			return WorkingTreeDiff{}, err // why HEAD could not be read
		}
		base = strings.TrimSpace(out)
	}
	args := append([]string{"diff"}, gitDiffOptions...)
	args = append(args, "--patch", "--src-prefix=a/", "--dst-prefix=b/", base, "--", ":/")
	patch, cut, err := g.run(ctx, "", MaxWorkingDiffBytes, args...)
	if err != nil {
		return WorkingTreeDiff{}, err
	}
	d := WorkingTreeDiff{Patch: patch}
	if cut {
		d.Patch, d.Truncated = cutPatch(patch), true
		return d, nil
	}
	list, err := g.list(ctx, "", "ls-files", "--others", "--exclude-standard", "--full-name", "-z", "--", ":/")
	if err != nil {
		return WorkingTreeDiff{}, err
	}
	var b strings.Builder
	b.WriteString(d.Patch)
	n := 0
	for path := range strings.SplitSeq(strings.TrimSuffix(list, "\x00"), "\x00") {
		if path == "" {
			continue
		}
		if n == maxUntrackedFiles {
			d.Truncated = true
			break
		}
		n++
		p := untrackedPatch(top, path)
		if b.Len()+len(p) > MaxWorkingDiffBytes {
			d.Truncated = true
			break
		}
		b.WriteString(p)
		d.Untracked = append(d.Untracked, path)
	}
	d.Patch = b.String()
	return d, nil
}

// cutPatch ends a patch cut off mid-way at the last file that started
// before the cut, so no file shows half a hunk; with a single file, at the
// last whole line.
func cutPatch(patch string) string {
	if i := strings.LastIndex(patch, "\ndiff --git "); i > 0 {
		return patch[:i+1]
	}
	return patch[:strings.LastIndexByte(patch, '\n')+1]
}

// untrackedPatch shows an untracked file as git shows a new one. The file
// is read here rather than by git diff --no-index, whose exit status says
// whether files differ rather than whether it failed.
func untrackedPatch(top, path string) string {
	header := fmt.Sprintf("diff --git a/%s b/%s\nnew file mode 100644\n", path, path)
	full := filepath.Join(top, filepath.FromSlash(path))
	info, err := os.Lstat(full)
	switch {
	case err != nil:
		return header
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return header
		}
		return strings.Replace(header, "100644", "120000", 1) + newFileHunk(path, target)
	case !info.Mode().IsRegular():
		return header
	case info.Size() > maxUntrackedBytes:
		return header + fmt.Sprintf("Binary files /dev/null and b/%s differ\n", path)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return header
	}
	if len(data) == 0 {
		return header
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 || !utf8.Valid(data) {
		return header + fmt.Sprintf("Binary files /dev/null and b/%s differ\n", path)
	}
	if info.Mode()&0o111 != 0 {
		header = strings.Replace(header, "100644", "100755", 1)
	}
	return header + newFileHunk(path, string(data))
}

// newFileHunk is the ---/+++ lines and the one hunk of a file all of
// whose lines are new. A symlink's content is its target, without a newline.
func newFileHunk(path, content string) string {
	trailing := strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n", path, len(lines))
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	if !trailing {
		b.WriteString("\\ No newline at end of file\n")
	}
	return b.String()
}
