package coding

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// gitBlameLine is one line of a file and the commit that last changed it.
type gitBlameLine struct {
	Line     int    `json:"line"`
	Text     string `json:"text"`
	Commit   string `json:"commit,omitempty"` // absent: not committed yet
	OrigLine int    `json:"orig_line"`
	OrigPath string `json:"orig_path,omitempty"`
}

// gitBlameCommit is what blame says of a commit, which is less than log.
type gitBlameCommit struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`

	unix int64
	path string // the file's path in it, from the top level
}

// uncommitted is the hash blame gives lines changed in the working tree.
const uncommitted = "0000000000000000000000000000000000000000"

func (g *gitTool) blame(ctx context.Context, top string, args map[string]any) (agent.ToolResult, error) {
	rev, err := singleRevArg(args, "rev")
	if err != nil {
		return agent.ToolResult{}, err
	}
	rel, err := g.repoPath(top, args)
	if err != nil {
		return agent.ToolResult{}, err
	}
	start, hasStart, err := optInt(args, "start")
	if err != nil {
		return agent.ToolResult{}, err
	}
	end, hasEnd, err := optInt(args, "end")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if (hasStart && start < 1) || (hasEnd && end < 1) || (hasStart && hasEnd && end < start) {
		return agent.ToolResult{}, errors.New("start and end are line numbers from 1, with end not before start")
	}

	cmd := []string{"blame", "--porcelain", "--no-textconv"}
	if hasStart || hasEnd {
		r := ""
		if hasStart {
			r = strconv.Itoa(start)
		} else {
			r = "1"
		}
		r += ","
		if hasEnd {
			r += strconv.Itoa(end)
		}
		cmd = append(cmd, "-L", r)
	}
	if rev != "" {
		cmd = append(cmd, rev)
	}
	out, err := g.list(ctx, "", append(cmd, "--", filepath.Join(top, rel))...)
	if err != nil {
		return agent.ToolResult{}, err
	}
	lines, commits, err := parseBlame(out)
	if err != nil {
		return agent.ToolResult{}, err
	}
	for i := range lines {
		if c := commits[lines[i].Commit]; c != nil && c.path != filepath.ToSlash(rel) {
			lines[i].OrigPath = scriptPath(g.cwd, top, c.path)
		}
	}
	list := make([]gitBlameCommit, 0, len(commits))
	for _, c := range commits {
		list = append(list, *c)
	}
	slices.SortFunc(list, func(a, b gitBlameCommit) int {
		if a.unix != b.unix {
			return int(b.unix - a.unix)
		}
		return strings.Compare(a.Hash, b.Hash)
	})

	byHash := map[string]gitBlameCommit{}
	width := 0
	for _, c := range list {
		byHash[c.Hash] = c
		width = max(width, len([]rune(c.Author)))
	}
	width = min(width, 16)
	var b strings.Builder
	for _, l := range lines {
		who := "(uncommitted)"
		if c, ok := byHash[l.Commit]; ok {
			author := []rune(c.Author)
			who = fmt.Sprintf("%.8s %.10s %-*s", c.Hash, c.Date, width, string(author[:min(len(author), width)]))
		}
		fmt.Fprintf(&b, "%s %5d  %s\n", who, l.Line, l.Text)
	}
	if len(lines) == 0 {
		b.WriteString("No lines.")
	}
	details := map[string]any{"action": "blame", "path": scriptPath(g.cwd, top, rel), "lines": lines, "commits": list}
	if rev != "" {
		details["rev"] = rev
	}
	return gitResult(b.String(), details), nil
}

// parseBlame reads git blame --porcelain: for each line a header naming its
// commit and line numbers, the commit's fields the first time it appears,
// and the line itself after a tab.
func parseBlame(out string) ([]gitBlameLine, map[string]*gitBlameCommit, error) {
	lines := []gitBlameLine{}
	commits := map[string]*gitBlameCommit{}
	var cur *gitBlameLine
	var c *gitBlameCommit
	var tz string
	for _, row := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if row == "" && cur == nil {
			continue
		}
		if cur == nil {
			// <hash> <orig line> <final line> [<lines in group>]
			f := strings.Fields(row)
			if len(f) < 3 || len(f[0]) < 40 {
				return nil, nil, fmt.Errorf("unexpected git blame line %q", row)
			}
			orig, err1 := strconv.Atoi(f[1])
			final, err2 := strconv.Atoi(f[2])
			if err1 != nil || err2 != nil {
				return nil, nil, fmt.Errorf("unexpected git blame line %q", row)
			}
			cur = &gitBlameLine{Line: final, OrigLine: orig}
			if f[0] != uncommitted {
				cur.Commit = f[0]
				if commits[f[0]] == nil {
					commits[f[0]] = &gitBlameCommit{Hash: f[0]}
				}
				c = commits[f[0]]
			} else {
				c = nil
			}
			continue
		}
		if text, ok := strings.CutPrefix(row, "\t"); ok {
			cur.Text = text
			lines = append(lines, *cur)
			cur = nil
			continue
		}
		if c == nil {
			continue // the fields of the working tree's pseudo-commit
		}
		key, value, _ := strings.Cut(row, " ")
		switch key {
		case "author":
			c.Author = value
		case "author-time":
			c.unix, _ = strconv.ParseInt(value, 10, 64)
			c.Date = blameDate(c.unix, tz)
		case "author-tz":
			tz = value
			c.Date = blameDate(c.unix, tz)
		case "summary":
			c.Subject = value
		case "filename":
			c.path = value
		}
	}
	if cur != nil {
		return nil, nil, errors.New("git blame ended inside a line")
	}
	return lines, commits, nil
}

// blameDate is an author date as log gives it, ISO 8601 in the author's
// zone (tz is git's +hhmm).
func blameDate(unix int64, tz string) string {
	zone := time.UTC
	if len(tz) == 5 {
		h, err1 := strconv.Atoi(tz[1:3])
		m, err2 := strconv.Atoi(tz[3:5])
		if err1 == nil && err2 == nil {
			off := (h*60 + m) * 60
			if tz[0] == '-' {
				off = -off
			}
			zone = time.FixedZone("", off)
		}
	}
	return time.Unix(unix, 0).In(zone).Format(time.RFC3339)
}
