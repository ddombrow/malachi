package coding

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkingTreeDiff(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "one\ntwo\n", ".gitignore": "*.log\n"})
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "one\nTWO\n")
	write(t, dir, "staged.txt", "s\n")
	testGit(t, dir, "add", "staged.txt")
	write(t, dir, "new.txt", "fresh\nno newline")
	write(t, dir, "skip.log", "ignored\n")
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{1, 0, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	// From a subdirectory, the diff is still the whole repository's.
	g := &gitTool{cwd: filepath.Join(dir, "sub")}
	d, err := g.workingTreeDiff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if d.Truncated || d.NoRepo {
		t.Fatalf("%+v", d)
	}
	for _, want := range []string{
		"diff --git a/a.txt b/a.txt", "-two\n+TWO\n",
		"diff --git a/staged.txt b/staged.txt", "+s\n",
		"diff --git a/new.txt b/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+fresh\n+no newline\n\\ No newline at end of file\n",
		"Binary files /dev/null and b/blob.bin differ",
	} {
		if !strings.Contains(d.Patch, want) {
			t.Errorf("missing %q in:\n%s", want, d.Patch)
		}
	}
	if got := strings.Join(d.Untracked, " "); got != "blob.bin new.txt" {
		t.Errorf("untracked: %q", got)
	}
	if strings.Contains(d.Patch, "skip.log") {
		t.Errorf("ignored file shown:\n%s", d.Patch)
	}
}

func TestWorkingTreeDiffBeforeFirstCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	testGit(t, dir, "init", "-q")
	write(t, dir, "added.txt", "a\n")
	testGit(t, dir, "add", "added.txt")
	write(t, dir, "loose.txt", "l\n")
	d, err := (&gitTool{cwd: dir}).workingTreeDiff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Patch, "b/added.txt") || !strings.Contains(d.Patch, "b/loose.txt") {
		t.Fatalf("%s", d.Patch)
	}
}

func TestWorkingTreeDiffClean(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "a\n"})
	d, err := (&gitTool{cwd: dir}).workingTreeDiff(context.Background(), dir)
	if err != nil || d.Patch != "" {
		t.Fatalf("%q %v", d.Patch, err)
	}
}

func TestCutPatch(t *testing.T) {
	two := "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/y b/y\n@@ -1 +1 @@\n-c\n+"
	if got := cutPatch(two); got != "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\n" {
		t.Fatalf("%q", got)
	}
	if got := cutPatch("diff --git a/x b/x\n+a\n+b"); got != "diff --git a/x b/x\n+a\n" {
		t.Fatalf("%q", got)
	}
}
