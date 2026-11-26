package coding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/codemode"
	"github.com/ddombrow/malachi/sandbox"
)

// testGit runs git in dir as a user would, away from the developer's own
// configuration.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Ann", "GIT_AUTHOR_EMAIL=ann@example.com", "GIT_COMMITTER_NAME=Ann", "GIT_COMMITTER_EMAIL=ann@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// testRepo is a repository with one commit on main holding files.
func testRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	testGit(t, dir, "init", "-q", "-b", "main")
	for path, body := range files {
		write(t, dir, path, body)
	}
	testGit(t, dir, "add", "-A")
	testGit(t, dir, "commit", "-q", "--allow-empty", "-m", "first", "-m", "why it was\nneeded")
	return dir
}

// writeSub writes sub/new.txt in dir.
func writeSub(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "sub/new.txt", "n\n")
}

// gitDetails calls the tool as a script would and decodes its details, which
// must match the tool's schema.
func gitDetails(t *testing.T, tool *agent.Tool, args map[string]any) gitOut {
	t.Helper()
	r, err := run(t, tool, args)
	if err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	raw, err := json.Marshal(r.Details)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	_ = json.Unmarshal(raw, &value)
	for _, e := range checkSchema("git", tool.OutputSchema, value) {
		t.Errorf("%v: %s", args, e)
	}
	out := gitOut{text: r.Text()}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type gitOut struct {
	text                         string
	Branch, Head, Upstream, Base string
	Ahead, Behind                int
	Files                        []gitAnyFile
	Commits                      []gitCommit
	Path, Content                string
	Binary, Truncated            bool
	HitLimit                     bool `json:"hit_limit"`
}

type gitAnyFile struct {
	gitDiffFile
	Staged, Unstaged string
	Conflicted       bool
}

func (o gitOut) file(t *testing.T, path string) gitAnyFile {
	t.Helper()
	for _, f := range o.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no file %q in %+v", path, o.Files)
	return gitAnyFile{}
}

func TestGitStatus(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "a\n", "old name.txt": "one\ntwo\nthree\n", "gone.txt": "x\n"})
	testGit(t, dir, "mv", "old name.txt", "new name.txt")
	testGit(t, dir, "rm", "-q", "gone.txt")
	write(t, dir, "a.txt", "b\n")
	writeSub(t, dir)
	tool := NewGitTool(dir, ToolOptions{})

	out := gitDetails(t, tool, map[string]any{"action": "status"})
	if out.Branch != "main" || len(out.Head) < 40 || out.Upstream != "" {
		t.Errorf("branch: %+v", out)
	}
	if f := out.file(t, "new name.txt"); f.Staged != "renamed" || f.OldPath != "old name.txt" || f.Unstaged != "" {
		t.Errorf("rename: %+v", f)
	}
	if f := out.file(t, "a.txt"); f.Unstaged != "modified" || f.Staged != "" {
		t.Errorf("modified: %+v", f)
	}
	if f := out.file(t, "gone.txt"); f.Staged != "deleted" {
		t.Errorf("deleted: %+v", f)
	}
	if f := out.file(t, "sub/new.txt"); f.Unstaged != "untracked" {
		t.Errorf("untracked: %+v", f)
	}
	if len(out.Files) != 4 {
		t.Errorf("files: %+v", out.Files)
	}

	// From a subdirectory paths still work as they are: relative inside it,
	// absolute outside.
	out = gitDetails(t, NewGitTool(filepath.Join(dir, "sub"), ToolOptions{}), map[string]any{"action": "status"})
	out.file(t, "new.txt")
	out.file(t, filepath.Join(sandbox.Canonical(dir), "a.txt"))
	if out = gitDetails(t, tool, map[string]any{"action": "status", "paths": []any{"sub"}}); len(out.Files) != 1 {
		t.Errorf("paths: %+v", out.Files)
	}
}

func TestGitStatusUpstreamAndDetached(t *testing.T) {
	dir := testRepo(t, nil)
	clone := filepath.Join(t.TempDir(), "clone")
	testGit(t, dir, "clone", "-q", dir, clone)
	testGit(t, clone, "commit", "-q", "--allow-empty", "-m", "ahead")
	out := gitDetails(t, NewGitTool(clone, ToolOptions{}), map[string]any{"action": "status"})
	if out.Upstream != "origin/main" || out.Ahead != 1 || out.Behind != 0 || len(out.Files) != 0 {
		t.Errorf("upstream: %+v", out)
	}
	testGit(t, dir, "checkout", "-q", "--detach")
	if out = gitDetails(t, NewGitTool(dir, ToolOptions{}), map[string]any{"action": "status"}); out.Branch != "" || out.Head == "" {
		t.Errorf("detached: %+v", out)
	}
	// A repository with no commits yet has a status.
	empty := t.TempDir()
	testGit(t, empty, "init", "-q", "-b", "main")
	if out = gitDetails(t, NewGitTool(empty, ToolOptions{}), map[string]any{"action": "status"}); out.Branch != "main" || out.Head != "" {
		t.Errorf("empty: %+v", out)
	}
}

func TestGitLog(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	testGit(t, dir, "tag", "v1")
	testGit(t, dir, "checkout", "-q", "-b", "feature")
	testGit(t, dir, "mv", "a.txt", "c.txt")
	testGit(t, dir, "commit", "-q", "-m", "move")
	write(t, dir, "b.txt", "b\nneedle\n")
	testGit(t, dir, "commit", "-q", "-am", "add needle")
	testGit(t, dir, "commit", "-q", "--allow-empty", "-m", "nothing")
	testGit(t, dir, "checkout", "-q", "main")
	testGit(t, dir, "commit", "-q", "--allow-empty", "-m", "main moves")
	testGit(t, dir, "merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	tool := NewGitTool(dir, ToolOptions{})

	out := gitDetails(t, tool, map[string]any{"action": "log", "files": true})
	bySubject := map[string]gitCommit{}
	for _, c := range out.Commits {
		bySubject[c.Subject] = c
	}
	if len(out.Commits) != 6 || out.HitLimit || out.Commits[0].Subject != "merge feature" {
		t.Fatalf("commits: %+v", out.Commits)
	}
	first := bySubject["first"]
	if first.Body != "why it was\nneeded" || first.Author != "Ann" || len(first.Parents) != 0 || !slices.Contains(first.Refs, "tag: v1") {
		t.Errorf("first: %+v", first)
	}
	if len(*first.Files) != 2 || (*first.Files)[0] != (gitChange{Path: "a.txt", Status: "added"}) {
		t.Errorf("first's files: %+v", *first.Files)
	}
	if got := *bySubject["move"].Files; len(got) != 1 || got[0] != (gitChange{Path: "c.txt", OldPath: "a.txt", Status: "renamed"}) {
		t.Errorf("rename: %+v", got)
	}
	if got := bySubject["nothing"].Files; got == nil || len(*got) != 0 {
		t.Errorf("an empty commit's files: %+v", got)
	}
	merge := bySubject["merge feature"]
	if len(merge.Parents) != 2 || len(*merge.Files) != 2 {
		t.Errorf("merge: %+v %+v", merge, *merge.Files)
	}

	out = gitDetails(t, tool, map[string]any{"action": "log", "limit": 2.0})
	if len(out.Commits) != 2 || !out.HitLimit || out.Commits[0].Files != nil {
		t.Errorf("limit: %+v", out)
	}
	out = gitDetails(t, tool, map[string]any{"action": "log", "search": "needle"})
	if len(out.Commits) != 1 || out.Commits[0].Subject != "add needle" {
		t.Errorf("search: %+v", out.Commits)
	}
	out = gitDetails(t, tool, map[string]any{"action": "log", "rev": "v1..feature", "paths": []any{"b.txt"}})
	if len(out.Commits) != 1 || out.Commits[0].Subject != "add needle" {
		t.Errorf("range and path: %+v", out.Commits)
	}
	if out = gitDetails(t, tool, map[string]any{"action": "log", "rev": "main..main"}); len(out.Commits) != 0 || out.text != "No commits." {
		t.Errorf("empty range: %+v", out)
	}
}

// A branch's diff is its own work, not what its base did since, and hunks
// say where each line is on both sides.
func TestGitDiffOfABranch(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "1\n2\n3\n4\n5\n6\n7\n8\n9\n", "b.txt": "b\n", "gone.txt": "-- dash\n++ plus\n"})
	testGit(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "a.txt", "1\n2\n3\n4\nfive\nextra\n6\n7\n8\n9\n")
	testGit(t, dir, "rm", "-q", "gone.txt")
	testGit(t, dir, "commit", "-q", "-am", "work")
	base := testGit(t, dir, "rev-parse", "main")
	testGit(t, dir, "checkout", "-q", "main")
	write(t, dir, "b.txt", "main moved on\n")
	testGit(t, dir, "commit", "-q", "-am", "main moves")
	testGit(t, dir, "checkout", "-q", "feature")
	write(t, dir, "new.txt", "uncommitted\n")
	testGit(t, dir, "add", "new.txt")
	tool := NewGitTool(dir, ToolOptions{})

	out := gitDetails(t, tool, map[string]any{"action": "diff", "base": "main"})
	if out.Base != base || len(out.Files) != 3 {
		t.Fatalf("base %s, files %+v", out.Base, out.Files)
	}
	a := out.file(t, "a.txt")
	if a.Status != "modified" || a.Additions != 2 || a.Deletions != 1 || len(a.Hunks) != 1 {
		t.Fatalf("a.txt: %+v", a)
	}
	h := a.Hunks[0]
	if h.OldStart != 2 || h.OldLines != 7 || h.NewStart != 2 || h.NewLines != 8 || !strings.HasPrefix(h.Text, "@@ -2,7 +2,8 @@") {
		t.Errorf("hunk: %+v", h)
	}
	for _, want := range []gitLine{{"-", "5", 5, 0}, {"+", "five", 0, 5}, {"+", "extra", 0, 6}, {" ", "6", 6, 7}} {
		if !slices.Contains(h.Lines, want) {
			t.Errorf("no line %+v in %+v", want, h.Lines)
		}
	}
	// Removed lines that look like a patch's headers are lines.
	gone := out.file(t, "gone.txt")
	if gone.Status != "deleted" || len(gone.Hunks) != 1 || gone.Hunks[0].Lines[0].Text != "-- dash" || gone.Deletions != 2 {
		t.Errorf("gone.txt: %+v", gone)
	}
	if !strings.Contains(out.text, "+uncommitted") {
		t.Errorf("the text is the patch: %q", out.text)
	}

	out = gitDetails(t, tool, map[string]any{"action": "diff", "base": "main", "head": "HEAD"})
	if len(out.Files) != 2 {
		t.Errorf("commits only: %+v", out.Files)
	}
	out = gitDetails(t, tool, map[string]any{"action": "diff", "base": "main", "head": "HEAD", "exact": true})
	if len(out.Files) != 3 {
		t.Errorf("exact: %+v", out.Files)
	}
	out = gitDetails(t, tool, map[string]any{"action": "diff"})
	if len(out.Files) != 1 || out.Files[0].Path != "new.txt" || out.Files[0].Status != "added" {
		t.Errorf("uncommitted: %+v", out.Files)
	}
	write(t, dir, "b.txt", "unstaged\n")
	out = gitDetails(t, tool, map[string]any{"action": "diff", "staged": true})
	if len(out.Files) != 1 || out.Files[0].Path != "new.txt" {
		t.Errorf("staged: %+v", out.Files)
	}
	out = gitDetails(t, tool, map[string]any{"action": "diff", "commit": "feature", "context": 0.0, "paths": []any{"a.txt"}})
	if len(out.Files) != 1 || out.Base != base || len(out.Files[0].Hunks[0].Lines) != 3 {
		t.Errorf("commit: %+v", out)
	}
	out = gitDetails(t, tool, map[string]any{"action": "diff", "commit": base})
	if out.Base != "" || len(out.Files) != 3 || out.file(t, "b.txt").Status != "added" {
		t.Errorf("first commit: %+v", out)
	}

	// The old side of a file, as the branch found it.
	old := gitDetails(t, tool, map[string]any{"action": "show", "rev": base, "path": "gone.txt"})
	if old.Content != "-- dash\n++ plus\n" || old.Path != "gone.txt" {
		t.Errorf("show: %+v", old)
	}
}

// Names and contents that a patch spells in special ways come out as they
// are.
func TestGitDiffOddFiles(t *testing.T) {
	odd := "q\n\"é\t.txt"
	dir := testRepo(t, map[string]string{
		"with space.txt": "a\n", odd: "a\n", "mode.sh": "x\n", "bin": "\x00\x01", "empty": "",
		"nonl": "no newline", "move me.txt": "1\n2\n3\n4\n5\n6\n", "crlf": "a\r\n",
	})
	write(t, dir, "with space.txt", "b\n")
	write(t, dir, odd, "b\n\nafter a blank\n")
	write(t, dir, "bin", "\x00\x02")
	write(t, dir, "nonl", "still none")
	write(t, dir, "crlf", "b\r\n")
	if err := os.Chmod(filepath.Join(dir, "mode.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "mv", "move me.txt", "moved.txt")
	write(t, dir, "moved.txt", "1\n2\n3\n4\n5\nsix\n")
	tool := NewGitTool(dir, ToolOptions{})
	out := gitDetails(t, tool, map[string]any{"action": "diff"})

	for _, path := range []string{"with space.txt", odd} {
		if f := out.file(t, path); len(f.Hunks) != 1 || f.Hunks[0].Lines[0] != (gitLine{"-", "a", 1, 0}) {
			t.Errorf("%q: %+v", path, f)
		}
	}
	if f := out.file(t, odd); len(f.Hunks[0].Lines) != 4 || f.Hunks[0].Lines[2] != (gitLine{"+", "", 0, 2}) {
		t.Errorf("blank line: %+v", f.Hunks[0].Lines)
	}
	if f := out.file(t, "bin"); !f.Binary || len(f.Hunks) != 0 {
		t.Errorf("binary: %+v", f)
	}
	if f := out.file(t, "mode.sh"); f.Status != "modified" || len(f.Hunks) != 0 {
		t.Errorf("mode: %+v", f)
	}
	if f := out.file(t, "nonl"); len(f.Hunks) != 1 || len(f.Hunks[0].Lines) != 2 || strings.Count(f.Hunks[0].Text, "No newline") != 2 {
		t.Errorf("no newline: %+v", f)
	}
	if f := out.file(t, "crlf"); f.Hunks[0].Lines[1].Text != "b\r" {
		t.Errorf("crlf: %+v", f.Hunks[0].Lines)
	}
	if f := out.file(t, "moved.txt"); f.Status != "renamed" || f.OldPath != "move me.txt" || len(f.Hunks) != 1 || f.Additions != 1 {
		t.Errorf("rename: %+v", f)
	}
	if b := gitDetails(t, tool, map[string]any{"action": "show", "path": "bin"}); !b.Binary || b.Content != "" {
		t.Errorf("show binary: %+v", b)
	}
}

func TestGitConflict(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "a\n"})
	testGit(t, dir, "checkout", "-q", "-b", "other")
	write(t, dir, "a.txt", "theirs\n")
	testGit(t, dir, "commit", "-q", "-am", "theirs")
	testGit(t, dir, "checkout", "-q", "main")
	write(t, dir, "a.txt", "ours\n")
	testGit(t, dir, "commit", "-q", "-am", "ours")
	if out, err := exec.Command("git", "-C", dir, "-c", "user.name=a", "-c", "user.email=a@example.com", "merge", "-q", "other").CombinedOutput(); err == nil {
		t.Fatalf("the merge did not conflict: %s", out)
	}
	tool := NewGitTool(dir, ToolOptions{})
	if f := gitDetails(t, tool, map[string]any{"action": "status"}).file(t, "a.txt"); !f.Conflicted {
		t.Errorf("status: %+v", f)
	}
	out := gitDetails(t, tool, map[string]any{"action": "diff"})
	if f := out.file(t, "a.txt"); len(out.Files) != 1 || f.Status != "modified" || len(f.Hunks) != 1 {
		t.Errorf("diff: %+v", out.Files)
	}
	out = gitDetails(t, tool, map[string]any{"action": "diff", "staged": true})
	if f := out.file(t, "a.txt"); len(out.Files) != 1 || f.Status != "unmerged" {
		t.Errorf("staged diff: %+v", out.Files)
	}
}

// A patch too long to keep still lists every file, and says which lack
// hunks.
func TestGitDiffTooLong(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "", "b.txt": "", "c.txt": ""})
	write(t, dir, "a.txt", "short\n")
	write(t, dir, "b.txt", strings.Repeat("a line of some length\n", 60000))
	write(t, dir, "c.txt", "short\n")
	out := gitDetails(t, NewGitTool(dir, ToolOptions{}), map[string]any{"action": "diff"})
	if !out.Truncated || len(out.Files) != 3 {
		t.Fatalf("truncated %v, %d files", out.Truncated, len(out.Files))
	}
	for path, want := range map[string]bool{"a.txt": false, "b.txt": true, "c.txt": true} {
		if f := out.file(t, path); f.HunksOmitted != want || f.Additions == 0 {
			t.Errorf("%s: omitted %v, additions %d", path, f.HunksOmitted, f.Additions)
		}
	}
	if len(out.text) > MaxOutputBytes+200 {
		t.Errorf("text of %d bytes", len(out.text))
	}
}

// The repository's configuration changes neither what comes back nor, where
// git can be told not to, what runs: the model can write it.
func TestGitIgnoresRepositoryConfiguration(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "1\n\n3\n", ".gitattributes": "*.txt diff=evil\n"})
	ran := filepath.Join(t.TempDir(), "ran")
	evil := filepath.Join(t.TempDir(), "evil.sh")
	if err := os.WriteFile(evil, []byte("#!/bin/sh\necho \"$0 $*\" >> "+ran+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"core.fsmonitor": evil, "core.pager": evil, "diff.external": evil, "diff.evil.textconv": evil,
		"diff.evil.command": evil, "gpg.program": evil, "log.showSignature": "true",
		"color.ui": "always", "diff.noprefix": "true", "diff.mnemonicPrefix": "true", "diff.renames": "false",
		"diff.context": "0", "diff.suppressBlankEmpty": "true", "diff.relative": "true", "core.quotePath": "true",
		"status.showUntrackedFiles": "no", "log.showRoot": "false", "log.date": "relative", "log.decorate": "full",
		"diff.statNameWidth": "3", "status.renames": "false", "status.relativePaths": "true",
	} {
		testGit(t, dir, "config", key, value)
	}
	hook := filepath.Join(dir, ".git", "hooks", "post-index-change")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho hook >> "+ran+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "1\n\nthree\n")
	writeSub(t, dir)
	tool := NewGitTool(dir, ToolOptions{})
	out := gitDetails(t, tool, map[string]any{"action": "diff"})
	f := out.file(t, "a.txt")
	if len(f.Hunks) != 1 || len(f.Hunks[0].Lines) != 4 || f.Hunks[0].Lines[1] != (gitLine{" ", "", 2, 2}) {
		t.Errorf("diff: %+v", f)
	}
	gitDetails(t, tool, map[string]any{"action": "status"}).file(t, "sub/new.txt")
	log := gitDetails(t, tool, map[string]any{"action": "log", "files": true})
	if c := log.Commits[0]; c.Subject != "first" || len(*c.Files) != 2 || !strings.HasPrefix(c.Date, "20") {
		t.Errorf("log: %+v", c)
	}
	if c := gitDetails(t, tool, map[string]any{"action": "diff", "commit": "HEAD"}); len(c.Files) != 2 {
		t.Errorf("first commit: %+v", c.Files)
	}
	gitDetails(t, tool, map[string]any{"action": "log", "search": "3"})
	gitDetails(t, tool, map[string]any{"action": "show", "path": "a.txt"})
	if data, err := os.ReadFile(ran); err == nil {
		t.Errorf("git ran the repository's programs:\n%s", data)
	}
}

func TestGitRefusals(t *testing.T) {
	dir := testRepo(t, map[string]string{"a.txt": "a\n"})
	tool := NewGitTool(dir, ToolOptions{})
	out := filepath.Join(t.TempDir(), "out")
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "log", "rev": "--output=" + out}, "must be a revision"},
		{map[string]any{"action": "diff", "base": "--output=" + out}, "must be a revision"},
		{map[string]any{"action": "diff", "head": "--output=" + out}, "must be a revision"},
		{map[string]any{"action": "diff", "commit": " --output=" + out}, "must be a revision"},
		{map[string]any{"action": "show", "rev": "--batch", "path": "a.txt"}, "must be a revision"},
		{map[string]any{"action": "diff", "base": "main..HEAD"}, "as base and head"},
		{map[string]any{"action": "diff", "base": "nope"}, `no commit "nope"`},
		{map[string]any{"action": "diff", "commit": "HEAD", "base": "HEAD"}, "does not go with"},
		{map[string]any{"action": "diff", "staged": true, "head": "HEAD"}, "does not go with"},
		{map[string]any{"action": "log", "rev": "nope"}, "nope"},
		{map[string]any{"action": "show", "path": "../elsewhere"}, "not a file in the repository"},
		{map[string]any{"action": "show", "path": "missing.txt"}, "missing.txt"},
		{map[string]any{"action": "push"}, "action must be one of"},
	} {
		r, err := run(t, tool, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v %v, want %q", tc.args, r, err, tc.want)
		}
	}
	// A pathspec or search that starts with a dash is not an option.
	gitDetails(t, tool, map[string]any{"action": "diff", "paths": []any{"--output=" + out}})
	gitDetails(t, tool, map[string]any{"action": "log", "search": "--output=" + out})
	if _, err := os.Stat(out); err == nil {
		t.Error("an argument was taken for git's --output option")
	}
	if _, err := run(t, NewGitTool(t.TempDir(), ToolOptions{}), map[string]any{"action": "status"}); err == nil {
		t.Error("git worked outside a repository")
	}
}

// git runs in the sandbox: a filter, which it cannot be told to skip, writes
// nowhere a command could not.
func TestGitRunsInTheSandbox(t *testing.T) {
	if err := sandbox.Available(); err != nil {
		t.Skip(err)
	}
	home := t.TempDir()
	dir := testRepo(t, map[string]string{"a.txt": "a\n", ".gitattributes": "*.txt filter=evil\n"})
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	escaped := filepath.Join(userHome, fmt.Sprintf(".malachi-git-test-%d", os.Getpid()))
	t.Cleanup(func() { os.Remove(escaped) })
	testGit(t, dir, "config", "filter.evil.clean", "touch '"+filepath.Join(dir, "filtered")+"'; touch '"+escaped+"'; cat")
	write(t, dir, "a.txt", "b\n")
	cfg := sandbox.Config{Cwd: dir, Home: home}
	addRepoRoots(&cfg, dir)
	pol := sandbox.Build(cfg)
	tool := NewGitTool(dir, ToolOptions{Sandbox: func() sandbox.Policy { return pol }})
	if out := gitDetails(t, tool, map[string]any{"action": "diff"}); len(out.file(t, "a.txt").Hunks) != 1 {
		t.Errorf("diff: %+v", out.Files)
	}
	if _, err := os.Stat(filepath.Join(dir, "filtered")); err != nil {
		t.Fatal("the filter did not run, so this shows nothing")
	}
	if _, err := os.Stat(escaped); err == nil {
		t.Fatal("a repository's filter wrote outside the sandbox")
	}
}

// Only scripts are offered git, and only in a repository.
func TestGitIsOfferedOnlyToScripts(t *testing.T) {
	repo := testRepo(t, map[string]string{"a.txt": "a\n"})
	write(t, repo, "a.txt", "b\n")
	for _, tc := range []struct {
		cwd, mode string
		want      bool
	}{{repo, CodeModeOn, true}, {repo, CodeModeExplore, true}, {repo, CodeModeOff, false}, {t.TempDir(), CodeModeOn, false}} {
		p := fake.New(fake.Text("ok"))
		s, err := Open(Options{Cwd: tc.cwd, Home: t.TempDir(), Settings: &Settings{}, Provider: p, NoSession: true, CodeMode: tc.mode})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		if err := s.Prompt(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
		var script *agent.Tool
		for _, tl := range p.Requests[0].Tools {
			switch tl.Name {
			case "git":
				t.Errorf("%s: git was offered directly", tc.mode)
			case codemode.ToolName:
				script = tl
			}
		}
		described := script != nil && strings.Contains(script.Description, "### tools.git(args)")
		if described != tc.want || strings.Contains(p.Requests[0].System, gitGuideline) != tc.want {
			t.Errorf("%s in a repository %v: described %v", tc.mode, tc.cwd == repo, described)
		}
		if !tc.want {
			continue
		}
		res, isError := runScriptTool(t, script, `
			const d = await tools.git({action: "diff"});
			if (d.isError) throw new Error(d.text);
			return d.details.files.flatMap(f => f.hunks.flatMap(h => h.lines.filter(l => l.op === "+").map(l => f.path + ":" + l.new + ":" + l.text)));`)
		if isError || res.Text() != "[\n  \"a.txt:1:b\"\n]" {
			t.Errorf("%s: %s", tc.mode, res.Text())
		}
	}
}
