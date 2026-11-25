package coding

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ddombrow/malachi/agent"
)

// gitLine is one line of a hunk, with its number on each side it is on.
type gitLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
	Old  int    `json:"old,omitempty"`
	New  int    `json:"new,omitempty"`
}

type gitHunk struct {
	// Header is the context git prints after the second @@, usually the
	// enclosing function.
	Header   string    `json:"header"`
	OldStart int       `json:"old_start"`
	OldLines int       `json:"old_lines"`
	NewStart int       `json:"new_start"`
	NewLines int       `json:"new_lines"`
	Text     string    `json:"text"`
	Lines    []gitLine `json:"lines"`
}

type gitDiffFile struct {
	Path      string    `json:"path"`
	OldPath   string    `json:"old_path,omitempty"`
	Status    string    `json:"status"`
	Binary    bool      `json:"binary"`
	Additions int       `json:"additions"`
	Deletions int       `json:"deletions"`
	Hunks     []gitHunk `json:"hunks"`
	// HunksOmitted marks a file whose hunks are missing or incomplete
	// because the whole patch was too long.
	HunksOmitted bool `json:"hunks_omitted,omitempty"`
}

// gitDiffOptions make a diff's output the same whatever the configuration
// says, and keep it from running the repository's diff programs.
var gitDiffOptions = []string{
	"--no-ext-diff", "--no-textconv", "--no-color", "--no-relative", "--find-renames",
	"--submodule=short", "--inter-hunk-context=0",
}

func (g *gitTool) diff(ctx context.Context, top string, args map[string]any) (agent.ToolResult, error) {
	var revs [3]string
	for i, name := range []string{"base", "head", "commit"} {
		rev, err := singleRevArg(args, name)
		if err != nil {
			return agent.ToolResult{}, err
		}
		revs[i] = rev
	}
	base, head, commit := revs[0], revs[1], revs[2]
	staged, _, err := optBool(args, "staged")
	if err != nil {
		return agent.ToolResult{}, err
	}
	exact, _, err := optBool(args, "exact")
	if err != nil {
		return agent.ToolResult{}, err
	}
	lines, hasLines, err := optInt(args, "context")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !hasLines {
		lines = 3
	}
	if lines < 0 {
		return agent.ToolResult{}, errors.New("context must not be negative")
	}
	specs, err := pathspecs(args)
	if err != nil {
		return agent.ToolResult{}, err
	}
	switch {
	case commit != "" && (base != "" || head != "" || staged):
		return agent.ToolResult{}, errors.New("commit is one commit's own change; it does not go with base, head or staged")
	case staged && head != "":
		return agent.ToolResult{}, errors.New("staged compares to the index, so it does not go with head")
	}

	details := map[string]any{"action": "diff"}
	var cmd, ends []string
	if commit != "" {
		hash, err := g.commit(ctx, commit)
		if err != nil {
			return agent.ToolResult{}, err
		}
		// A first commit has no parent, and is all additions.
		if parent, err := g.commit(ctx, hash+"^"); err == nil {
			details["base"] = parent
		}
		cmd, ends = []string{"show", "--format=", "--diff-merges=first-parent"}, []string{hash}
	} else {
		from, err := g.diffBase(ctx, base, head, exact)
		if err != nil {
			return agent.ToolResult{}, err
		}
		details["base"] = from
		cmd, ends = []string{"diff"}, []string{from}
		if staged {
			cmd = append(cmd, "--cached")
		}
		if head != "" {
			hash, err := g.commit(ctx, head)
			if err != nil {
				return agent.ToolResult{}, err
			}
			ends = append(ends, hash)
		}
	}
	cmd = append(cmd, gitDiffOptions...)
	ends = append(ends, specs...)

	// The list of files and the patch are asked for apart: a list whose
	// names need no quoting, whole however long the patch, and a patch whose
	// names are quoted, so that a newline in one cannot pass for a line.
	list, err := g.list(ctx, "", append(append(cmd, "--raw", "--numstat", "-z"), ends...)...)
	if err != nil {
		return agent.ToolResult{}, err
	}
	files, err := parseGitDiffList(list)
	if err != nil {
		return agent.ToolResult{}, err
	}
	hitLimit := len(files) > MaxGitFiles
	if hitLimit {
		files = files[:MaxGitFiles]
	}
	patch, cut, err := g.run(ctx, "", MaxScriptOutputBytes,
		append(append(cmd, "--patch", fmt.Sprintf("--unified=%d", lines), "--src-prefix=a/", "--dst-prefix=b/"), ends...)...)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if cut { // a whole number of lines
		patch = patch[:strings.LastIndexByte(patch, '\n')+1]
	}
	hunks, last := parseGitPatch(patch)
	for i := range files {
		f := &files[i]
		f.Hunks = hunks[f.Path]
		if f.Hunks == nil {
			f.Hunks = []gitHunk{}
		}
		if cut {
			f.HunksOmitted = f.Path == last || len(f.Hunks) == 0 && f.Additions+f.Deletions > 0
		}
		f.Path = scriptPath(g.cwd, top, f.Path)
		if f.OldPath != "" {
			f.OldPath = scriptPath(g.cwd, top, f.OldPath)
		}
	}
	details["files"], details["hit_limit"], details["truncated"] = files, hitLimit, cut
	if patch == "" {
		patch = "No changes."
	}
	return gitResult(patch, details), nil
}

// commit resolves rev to a commit's hash.
func (g *gitTool) commit(ctx context.Context, rev string) (string, error) {
	out, _, err := g.run(ctx, "", 4096, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	var failed *gitError
	if errors.As(err, &failed) && failed.quiet {
		return "", fmt.Errorf("no commit %q in this repository", rev)
	}
	return strings.TrimSpace(out), err
}

// diffBase is the commit a diff compares from: base, or unless exact, where
// the branch at head (the working tree's, when there is none) left it. The
// two are the same when base is an ancestor; when it is not, comparing from
// base itself would show its own later work undone.
func (g *gitTool) diffBase(ctx context.Context, base, head string, exact bool) (string, error) {
	if head == "" {
		head = "HEAD"
	}
	if base == "" {
		base = "HEAD"
	}
	from, err := g.commit(ctx, base)
	if err != nil || exact {
		return from, err
	}
	to, err := g.commit(ctx, head)
	if err != nil || from == to {
		return from, err
	}
	out, _, err := g.run(ctx, "", 4096, "merge-base", from, to)
	var failed *gitError
	if errors.As(err, &failed) && failed.quiet {
		return "", fmt.Errorf("%s and %s have no common ancestor; pass exact: true to compare them anyway", base, head)
	}
	return strings.TrimSpace(out), err
}

// parseGitDiffList reads the output of --raw --numstat -z: every file's
// status, then every file's line counts. Paths are the repository's.
func parseGitDiffList(out string) ([]gitDiffFile, error) {
	var files []gitDiffFile
	index := map[string]int{}
	tokens := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	next := func() string {
		if len(tokens) == 0 {
			return ""
		}
		t := tokens[0]
		tokens = tokens[1:]
		return t
	}
	for len(tokens) > 0 {
		rec := next()
		if rec == "" {
			continue
		}
		if strings.HasPrefix(rec, ":") {
			status := rec[strings.LastIndexByte(rec, ' ')+1:]
			if status == "" {
				return nil, fmt.Errorf("unexpected git diff record %q", rec)
			}
			f := gitDiffFile{Status: gitStatusWord(status[0]), Path: next()}
			if status[0] == 'R' || status[0] == 'C' {
				f.OldPath, f.Path = f.Path, next()
			}
			// A conflicted file is listed a second time for its contents.
			if i, seen := index[f.Path]; seen {
				if f.Status == "unmerged" {
					files[i].Status = f.Status
				}
				continue
			}
			index[f.Path] = len(files)
			files = append(files, f)
			continue
		}
		counts := strings.SplitN(rec, "\t", 3)
		if len(counts) != 3 {
			return nil, fmt.Errorf("unexpected git diff record %q", rec)
		}
		path := counts[2]
		if path == "" { // a rename's old and new paths follow
			next()
			path = next()
		}
		i, ok := index[path]
		if !ok {
			continue
		}
		if counts[0] == "-" {
			files[i].Binary = true
			continue
		}
		files[i].Additions, _ = strconv.Atoi(counts[0])
		files[i].Deletions, _ = strconv.Atoi(counts[1])
	}
	if files == nil {
		files = []gitDiffFile{}
	}
	return files, nil
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// parseGitPatch returns each file's hunks by its path, the new one unless it
// was deleted, and the path of the last file with any. A hunk cut short by
// the end of the patch is left out.
func parseGitPatch(patch string) (hunks map[string][]gitHunk, last string) {
	hunks = map[string][]gitHunk{}
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	path := ""
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "diff --git "):
			path = ""
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "+++ "):
			// The new name comes second and wins unless it is /dev/null.
			if p := patchPath(l[4:]); p != "" {
				path = p
			}
		case strings.HasPrefix(l, "@@ ") && path != "":
			m := hunkHeader.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			last = path
			h := gitHunk{OldStart: atoi(m[1], 0), OldLines: atoi(m[2], 1), NewStart: atoi(m[3], 0), NewLines: atoi(m[4], 1), Header: m[5]}
			end, ok := h.parse(lines, i)
			if !ok {
				return hunks, last
			}
			hunks[path] = append(hunks[path], h)
			i = end - 1
		}
	}
	return hunks, last
}

// parse reads the hunk whose header is lines[start], returning the index
// past it. It counts lines rather than looking for the next header, since a
// deleted line can look like one ("-- a" is "--- a" in a patch).
func (h *gitHunk) parse(lines []string, start int) (end int, ok bool) {
	oldNo, newNo := h.OldStart, h.NewStart
	oldEnd, newEnd := h.OldStart+h.OldLines, h.NewStart+h.NewLines
	h.Lines = []gitLine{}
	i := start + 1
	for oldNo < oldEnd || newNo < newEnd {
		if i >= len(lines) || lines[i] == "" {
			return i, false
		}
		gl := gitLine{Op: lines[i][:1], Text: lines[i][1:]}
		switch gl.Op {
		case " ":
			gl.Old, gl.New = oldNo, newNo
			oldNo++
			newNo++
		case "-":
			gl.Old = oldNo
			oldNo++
		case "+":
			gl.New = newNo
			newNo++
		case "\\": // "\ No newline at end of file"
			i++
			continue
		default:
			return i, false
		}
		h.Lines = append(h.Lines, gl)
		i++
	}
	if i < len(lines) && strings.HasPrefix(lines[i], "\\") {
		i++
	}
	h.Text = strings.Join(lines[start:i], "\n")
	return i, true
}

// patchPath is the path a --- or +++ line names, without its a/ or b/, or
// "" for /dev/null. git quotes a name with special characters as C does,
// and ends one that has a space in it with a tab.
func patchPath(s string) string {
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(strings.TrimSuffix(s, "\t")); err == nil {
			s = u
		}
	} else {
		s = strings.TrimSuffix(s, "\t")
	}
	if s == "/dev/null" || len(s) < 2 {
		return ""
	}
	return s[2:]
}

func atoi(s string, missing int) int {
	if s == "" {
		return missing
	}
	n, _ := strconv.Atoi(s)
	return n
}
