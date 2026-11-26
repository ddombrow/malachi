package coding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/sandbox"
)

const (
	// DefaultGitLogLimit is how many commits log returns unless asked otherwise.
	DefaultGitLogLimit = 50
	// MaxGitLogLimit caps log's limit argument.
	MaxGitLogLimit = 1000
	// MaxGitFiles caps the files status and diff list.
	MaxGitFiles = 5000
	// maxGitListBytes caps what is read of git's lists of files and commits.
	// Past it a list could not be trusted to be whole, so the call fails.
	maxGitListBytes = 16 << 20
)

var gitActions = []string{"status", "log", "diff", "show"}

// gitGuideline is the prompt's advice on the git tool, which the model is
// offered only through scripts.
const gitGuideline = "To review changes or look through history, call git in a script: its details are data (changed files, hunks with line numbers, commits), so one script can take a branch's diff and read around each hunk that matters. Commit, and change anything else in the repository, with bash."

// NewGitTool returns the git tool for the repository holding cwd: a script's
// view of its state and history as data. It only looks; commits stay with
// bash, where their hooks and output show.
//
// Reading is not a guardrail (bash can run any git command), but it has to
// stay reading, because scripts run calls together. And git runs commands a
// repository's configuration names (filters, fsmonitor, external diffs),
// which the model can write, so git runs in the sandbox as bash's commands
// do, with what it need not run turned off.
func NewGitTool(cwd string, opts ToolOptions) *agent.Tool {
	g := &gitTool{cwd: sandbox.Canonical(cwd), opts: opts}
	return &agent.Tool{
		Name:         "git",
		Label:        "Git",
		OutputSchema: gitOutputSchema(),
		Description: "Look at the git repository: what changed, its history, and files as they were. Read-only. Paths in details are relative to the working directory, so they can go straight to read or edit. By action:\n" +
			"- status: details.branch, head, upstream, ahead, behind and files [{path, old_path?, staged?, unstaged?, conflicted?}], where staged and unstaged say what changed: added, modified, deleted, renamed, copied, type_changed or untracked.\n" +
			fmt.Sprintf("- log: details.commits, newest first [{hash, parents, author, date, refs, subject, body, files?: [{path, old_path?, status}]}]. rev takes a range such as \"main..HEAD\". At most %d commits; raise limit for more.\n", DefaultGitLogLimit) +
			"- diff: details.files [{path, old_path?, status, binary, additions, deletions, hunks, hunks_omitted?}] and details.base, the commit compared with. A hunk is {header, old_start, old_lines, new_start, new_lines, text, lines: [{op: \"+\", \"-\" or \" \", text, old?, new?}]}, old and new being line numbers. " +
			"With no arguments: every uncommitted change to tracked files (untracked files are only in status). {base: \"main\"}: this branch's work since it left main, uncommitted changes included; add head: \"HEAD\" for commits only. {commit: rev}: what one commit changed. {staged: true}: what would be committed.\n" +
			"- show: details.content, the file at path as of rev (default HEAD). For a diff's old side, use rev details.base and old_path, or path when there is none.",
		Parallel: true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "enum": gitActions, "description": "What to look at"},
				"rev":     map[string]any{"type": "string", "description": "log: a revision or range; show: a revision (default HEAD)"},
				"base":    map[string]any{"type": "string", "description": "diff: compare from where the branch left this revision (default HEAD)"},
				"head":    map[string]any{"type": "string", "description": "diff: compare to this revision (default: the working tree)"},
				"exact":   map[string]any{"type": "boolean", "description": "diff: compare from base itself, not from where the branch left it"},
				"commit":  map[string]any{"type": "string", "description": "diff: one commit, against its first parent"},
				"staged":  map[string]any{"type": "boolean", "description": "diff: compare to the index instead of the working tree"},
				"context": map[string]any{"type": "integer", "description": "diff: unchanged lines around each change (default 3)"},
				"path":    map[string]any{"type": "string", "description": "show: the file"},
				"paths":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "status, log, diff: only these files, directories or pathspecs, relative to the working directory"},
				"search":  map[string]any{"type": "string", "description": "log: only commits that change how often this text occurs"},
				"files":   map[string]any{"type": "boolean", "description": "log: list each commit's changed files"},
				"limit":   map[string]any{"type": "integer", "description": fmt.Sprintf("log: maximum commits (default %d, max %d)", DefaultGitLogLimit, MaxGitLogLimit)},
			},
			"required": []string{"action"},
		},
		Execute: func(ctx context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return g.execute(ctx, args)
		},
	}
}

// gitToolFor is the git tool for a session in cwd, or none when cwd is not
// in a repository or there is no git.
func gitToolFor(cwd string, opts ToolOptions) []*agent.Tool {
	if _, ok := findRepo(cwd); !ok {
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	return []*agent.Tool{NewGitTool(cwd, opts)}
}

type gitTool struct {
	cwd  string // canonical, as git reports paths
	opts ToolOptions
}

func (g *gitTool) execute(ctx context.Context, args map[string]any) (agent.ToolResult, error) {
	action, err := strArg(args, "action")
	if err != nil {
		return agent.ToolResult{}, err
	}
	repo, ok := findRepo(g.cwd)
	if !ok {
		return agent.ToolResult{}, errors.New("the working directory is not in a git repository")
	}
	switch action {
	case "status":
		return g.status(ctx, repo.Top, args)
	case "log":
		return g.log(ctx, repo.Top, args)
	case "diff":
		return g.diff(ctx, repo.Top, args)
	case "show":
		return g.show(ctx, repo.Top, args)
	}
	return agent.ToolResult{}, fmt.Errorf("action must be one of %s, not %q", strings.Join(gitActions, ", "), action)
}

// capped keeps the first max bytes written to it and notes whether there
// were more. The buffer is not embedded: its ReadFrom would let io.Copy past
// the cap.
type capped struct {
	buf bytes.Buffer
	max int
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	keep := p
	if room := c.max - c.buf.Len(); len(keep) > room {
		keep, c.cut = keep[:room], true
	}
	_, _ = c.buf.Write(keep)
	return len(p), nil
}

// gitOptions come before every subcommand. They keep git from running the
// programs a repository's configuration can name where it is not needed to
// read, and undo settings that change the output parsed here.
var gitOptions = []string{
	"--no-pager",
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.quotePath=false",
	"-c", "log.showSignature=false",
	"-c", "log.showRoot=true",
	"-c", "diff.suppressBlankEmpty=false",
}

// run runs git in the sandbox and returns the first max bytes it printed,
// and whether there were more. A failure is git's own message.
func (g *gitTool) run(ctx context.Context, stdin string, max int, args ...string) (string, bool, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return "", false, errors.New("git is not installed")
	}
	pol := g.opts.policy()
	cmd, err := g.opts.command(ctx, pol, bin, append(slices.Clone(gitOptions), args...)...)
	if err != nil {
		return "", false, fmt.Errorf("%v. Commands cannot run until that is fixed or the user turns the sandbox off (-sandbox off)", err)
	}
	out, errOut := &capped{max: max}, &capped{max: 8 << 10}
	cmd.Dir = g.cwd
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Stdout, cmd.Stderr = out, errOut
	// No index refresh written back, nothing fetched for a partial clone, no
	// prompt: looking must not change anything or wait for anyone.
	cmd.Env = append(commandEnv(g.opts.AllowEnv, g.opts.HideEnv),
		"GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
	configureProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Start()
	sandbox.Started(cmd)
	if err == nil {
		err = cmd.Wait()
	}
	if ctx.Err() != nil {
		return "", false, errors.New("Operation aborted")
	}
	if err != nil {
		msg := strings.TrimSpace(errOut.buf.String())
		if msg == "" {
			msg = err.Error()
		}
		if _, hint := failureHint(pol, "", msg); hint != "" {
			msg += "\n" + hint
		}
		return "", false, &gitError{msg: msg, quiet: errOut.buf.Len() == 0}
	}
	return out.buf.String(), out.cut, nil
}

// gitError is a git command that failed; quiet, when it did not say why.
type gitError struct {
	msg   string
	quiet bool
}

func (e *gitError) Error() string { return e.msg }

// list runs git for output that is only of use whole.
func (g *gitTool) list(ctx context.Context, stdin string, args ...string) (string, error) {
	out, cut, err := g.run(ctx, stdin, maxGitListBytes, args...)
	if err == nil && cut {
		err = fmt.Errorf("git printed more than %s; narrow it with paths, limit or a smaller range", FormatSize(maxGitListBytes))
	}
	return out, err
}

// revArg is a revision argument, or "" when absent. One that git would take
// for an option is refused.
func revArg(args map[string]any, name string) (string, error) {
	rev, _, err := optStr(args, name)
	if err != nil {
		return "", err
	}
	rev = strings.TrimSpace(rev)
	if strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, "\x00\n") {
		return "", fmt.Errorf("argument %q must be a revision, not %q", name, rev)
	}
	return rev, nil
}

// singleRevArg is revArg for where a range has no meaning.
func singleRevArg(args map[string]any, name string) (string, error) {
	rev, err := revArg(args, name)
	if err == nil && strings.Contains(rev, "..") {
		return "", fmt.Errorf("argument %q must be one revision, not the range %q; give a diff's two ends as base and head", name, rev)
	}
	return rev, err
}

// pathspecs is the paths argument as the end of a git command line.
func pathspecs(args map[string]any) ([]string, error) {
	out := []string{"--"}
	switch v := args["paths"].(type) {
	case nil:
	case string:
		if v != "" {
			out = append(out, v)
		}
	case []any:
		for _, p := range v {
			s, ok := p.(string)
			if !ok || s == "" {
				return nil, errors.New(`argument "paths" must be an array of non-empty strings`)
			}
			out = append(out, s)
		}
	default:
		return nil, errors.New(`argument "paths" must be an array of strings`)
	}
	return out, nil
}

var gitStatusWords = map[byte]string{
	'A': "added", 'M': "modified", 'D': "deleted", 'R': "renamed",
	'C': "copied", 'T': "type_changed", 'U': "unmerged",
}

func gitStatusWord(c byte) string {
	if w := gitStatusWords[c]; w != "" {
		return w
	}
	return "unknown"
}

// gitStatusFile is one file in status.
type gitStatusFile struct {
	Path       string `json:"path"`
	OldPath    string `json:"old_path,omitempty"`
	Staged     string `json:"staged,omitempty"`
	Unstaged   string `json:"unstaged,omitempty"`
	Conflicted bool   `json:"conflicted,omitempty"`
}

func (g *gitTool) status(ctx context.Context, top string, args map[string]any) (agent.ToolResult, error) {
	specs, err := pathspecs(args)
	if err != nil {
		return agent.ToolResult{}, err
	}
	out, err := g.list(ctx, "", append([]string{"status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all", "--find-renames"}, specs...)...)
	if err != nil {
		return agent.ToolResult{}, err
	}
	details := map[string]any{"action": "status"}
	files := []gitStatusFile{}
	records := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		kind, rest, _ := strings.Cut(rec, " ")
		switch kind {
		case "#":
			key, value, _ := strings.Cut(rest, " ")
			switch {
			case key == "branch.oid" && value != "(initial)":
				details["head"] = value
			case key == "branch.head" && value != "(detached)":
				details["branch"] = value
			case key == "branch.upstream":
				details["upstream"] = value
			case key == "branch.ab":
				var ahead, behind int
				if _, err := fmt.Sscanf(value, "+%d -%d", &ahead, &behind); err == nil {
					details["ahead"], details["behind"] = ahead, behind
				}
			}
		case "?":
			files = append(files, gitStatusFile{Path: scriptPath(g.cwd, top, rest), Unstaged: "untracked"})
		case "1", "2", "u":
			// XY, then a fixed number of fields, then the path, which may
			// itself hold spaces.
			n := map[string]int{"1": 8, "2": 9, "u": 10}[kind]
			fields := strings.SplitN(rec, " ", n+1)
			if len(fields) != n+1 || len(fields[1]) != 2 {
				return agent.ToolResult{}, fmt.Errorf("unexpected git status record %q", rec)
			}
			f := gitStatusFile{Path: scriptPath(g.cwd, top, fields[n])}
			if kind == "2" && i+1 < len(records) {
				i++
				f.OldPath = scriptPath(g.cwd, top, records[i])
			}
			if kind == "u" {
				f.Conflicted = true
			} else {
				if c := fields[1][0]; c != '.' {
					f.Staged = gitStatusWord(c)
				}
				if c := fields[1][1]; c != '.' {
					f.Unstaged = gitStatusWord(c)
				}
			}
			files = append(files, f)
		}
	}
	hitLimit := len(files) > MaxGitFiles
	if hitLimit {
		files = files[:MaxGitFiles]
	}
	details["files"], details["hit_limit"] = files, hitLimit

	var b strings.Builder
	branch, _ := details["branch"].(string)
	if branch == "" {
		branch = "(detached)"
	}
	fmt.Fprintf(&b, "On %s", branch)
	if up, ok := details["upstream"]; ok {
		fmt.Fprintf(&b, ", upstream %s", up)
		if ahead, ok := details["ahead"]; ok {
			fmt.Fprintf(&b, " (ahead %d, behind %d)", ahead, details["behind"])
		}
	}
	for _, f := range files {
		var what []string
		if f.Conflicted {
			what = append(what, "conflicted")
		}
		if f.Staged != "" {
			what = append(what, "staged "+f.Staged)
		}
		if f.Unstaged != "" {
			what = append(what, f.Unstaged)
		}
		fmt.Fprintf(&b, "\n%s: %s", f.Path, strings.Join(what, ", "))
	}
	if len(files) == 0 {
		b.WriteString("\nNothing changed.")
	}
	return gitResult(b.String(), details), nil
}

// gitChange is one file a commit changed.
type gitChange struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Status  string `json:"status"`
}

type gitCommit struct {
	Hash    string   `json:"hash"`
	Parents []string `json:"parents"`
	Author  string   `json:"author"`
	Date    string   `json:"date"`
	Refs    []string `json:"refs"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
	// Files is set only when asked for; a commit that changed none has an
	// empty list then.
	Files *[]gitChange `json:"files,omitempty"`
}

func (g *gitTool) log(ctx context.Context, top string, args map[string]any) (agent.ToolResult, error) {
	rev, err := revArg(args, "rev")
	if err != nil {
		return agent.ToolResult{}, err
	}
	specs, err := pathspecs(args)
	if err != nil {
		return agent.ToolResult{}, err
	}
	search, _, err := optStr(args, "search")
	if err != nil {
		return agent.ToolResult{}, err
	}
	withFiles, _, err := optBool(args, "files")
	if err != nil {
		return agent.ToolResult{}, err
	}
	limit, hasLimit, err := optInt(args, "limit")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !hasLimit {
		limit = DefaultGitLogLimit
	}
	if limit < 1 {
		return agent.ToolResult{}, errors.New("limit must be at least 1")
	}
	limit = min(limit, MaxGitLogLimit)
	if rev == "" {
		rev = "HEAD"
	}

	// NUL separates both the fields and the commits, and no field can hold one.
	const fields = 8
	cmd := []string{"log", "-z", "--no-color", "--no-notes", "--no-ext-diff", "--no-textconv",
		fmt.Sprintf("--max-count=%d", limit+1),
		"--format=%H%x00%P%x00%an%x00%aI%x00%D%x00%s%x00%b%x00"}
	if search != "" {
		cmd = append(cmd, "-S"+search)
	}
	out, err := g.list(ctx, "", append(append(cmd, rev), specs...)...)
	if err != nil {
		return agent.ToolResult{}, err
	}
	commits := []gitCommit{}
	tokens := strings.Split(out, "\x00")
	for ; len(tokens) >= fields; tokens = tokens[fields:] {
		c := gitCommit{
			Hash: tokens[0], Parents: strings.Fields(tokens[1]), Author: tokens[2], Date: tokens[3],
			Refs: []string{}, Subject: tokens[5], Body: strings.TrimSpace(tokens[6]),
		}
		if tokens[4] != "" {
			c.Refs = strings.Split(tokens[4], ", ")
		}
		commits = append(commits, c)
	}
	hitLimit := len(commits) > limit
	if hitLimit {
		commits = commits[:limit]
	}
	if withFiles && len(commits) > 0 {
		if err := g.addFiles(ctx, top, commits); err != nil {
			return agent.ToolResult{}, err
		}
	}

	var b strings.Builder
	for _, c := range commits {
		refs := ""
		if len(c.Refs) > 0 {
			refs = " (" + strings.Join(c.Refs, ", ") + ")"
		}
		fmt.Fprintf(&b, "%.12s %.10s %s%s\n", c.Hash, c.Date, c.Subject, refs)
		if c.Files != nil {
			for _, f := range *c.Files {
				fmt.Fprintf(&b, "    %s: %s\n", f.Path, f.Status)
			}
		}
	}
	switch {
	case len(commits) == 0:
		b.WriteString("No commits.")
	case hitLimit:
		fmt.Fprintf(&b, "\n[Stopped at the limit of %d commits, so there are more.]", limit)
	}
	return gitResult(b.String(), map[string]any{"action": "log", "commits": commits, "hit_limit": hitLimit}), nil
}

// addFiles fills in what each commit changed, a merge against its first
// parent.
func (g *gitTool) addFiles(ctx context.Context, top string, commits []gitCommit) error {
	var hashes strings.Builder
	byHash := map[string]*gitCommit{}
	for i := range commits {
		hashes.WriteString(commits[i].Hash + "\n")
		commits[i].Files = &[]gitChange{}
		byHash[commits[i].Hash] = &commits[i]
	}
	out, err := g.list(ctx, hashes.String(), "diff-tree", "--stdin", "-r", "-z", "--root", "--find-renames",
		"--name-status", "--diff-merges=first-parent", "--no-ext-diff", "--no-textconv", "--no-relative")
	if err != nil {
		return err
	}
	// Each commit that changed something is its hash, then a status and one
	// path, or two for a rename or copy, for each file.
	var cur *gitCommit
	tokens := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	for i := 0; i < len(tokens); {
		if c := byHash[tokens[i]]; c != nil {
			cur = c
			i++
			continue
		}
		status := tokens[i]
		n := 1
		if status != "" && (status[0] == 'R' || status[0] == 'C') {
			n = 2
		}
		if status == "" || cur == nil || i+n >= len(tokens) {
			return fmt.Errorf("unexpected git diff-tree record %q", status)
		}
		f := gitChange{Path: scriptPath(g.cwd, top, tokens[i+n]), Status: gitStatusWord(status[0])}
		if n == 2 {
			f.OldPath = scriptPath(g.cwd, top, tokens[i+1])
		}
		*cur.Files = append(*cur.Files, f)
		i += n + 1
	}
	return nil
}

func (g *gitTool) show(ctx context.Context, top string, args map[string]any) (agent.ToolResult, error) {
	rev, err := singleRevArg(args, "rev")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if strings.Contains(rev, ":") {
		return agent.ToolResult{}, errors.New(`give the file as path, not as part of rev`)
	}
	if rev == "" {
		rev = "HEAD"
	}
	raw, err := strArg(args, "path")
	if err != nil {
		return agent.ToolResult{}, err
	}
	// The path is taken as it is spelled: the file need not exist now, and a
	// link in the working tree says nothing of the tree at rev. Only the
	// working directory is resolved, so an absolute path through a link to
	// it, as /tmp is on macOS, still lands inside the repository.
	path := resolvePath(g.cwd, raw)
	rel, err := filepath.Rel(top, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		rel, err = filepath.Rel(top, filepath.Join(sandbox.Canonical(filepath.Dir(path)), filepath.Base(path)))
	}
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return agent.ToolResult{}, fmt.Errorf("%s is not a file in the repository at %s", raw, top)
	}
	out, cut, err := g.run(ctx, "", MaxScriptOutputBytes, "cat-file", "blob", rev+":"+filepath.ToSlash(rel))
	if err != nil {
		return agent.ToolResult{}, err
	}
	details := map[string]any{"action": "show", "path": scriptPath(g.cwd, top, rel), "rev": rev, "truncated": cut}
	if strings.Contains(out[:min(len(out), 8000)], "\x00") {
		details["binary"] = true
		return gitResult("[binary file]", details), nil
	}
	details["content"] = out
	return gitResult(out, details), nil
}

// gitResult is a result whose text is cut to the display limits. Nothing is
// spilled: details have what was cut.
func gitResult(text string, details map[string]any) agent.ToolResult {
	text = strings.TrimRight(text, "\n")
	if t := TruncateHead(text, MaxOutputLines, MaxOutputBytes); t.Truncated {
		text = t.Content + fmt.Sprintf("\n\n[Showing lines 1-%d of %d; details have the rest]", t.OutputLines, t.TotalLines)
	}
	return agent.ToolResult{Content: []agent.Content{&agent.TextContent{Text: text}}, Details: details}
}
