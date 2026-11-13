package coding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/codemode"
	"github.com/ddombrow/malachi/sandbox"
)

const (
	// DefaultGrepLimit is how many matches grep returns unless asked otherwise.
	DefaultGrepLimit = 100
	// MaxGrepLimit caps the limit argument.
	MaxGrepLimit = 1000
	// MaxGrepFileBytes skips files too large to be source. Reading them whole
	// would cost more context than the answer is worth.
	MaxGrepFileBytes = 2 * 1024 * 1024
	// MaxGrepLineWidth clips a single matched line. Minified or generated files
	// put entire documents on one line, and one match should not be able to
	// fill the window.
	MaxGrepLineWidth = 300
	// MaxGrepContext caps the context argument. More than this is reading the
	// file, which read does with less noise.
	MaxGrepContext = 20
)

// Grep output modes.
const (
	grepLines = "lines" // matching lines, with any context
	grepFiles = "files" // paths of files with a match
	grepCount = "count" // matches per file
)

// binarySniffBytes is how much of a file is checked for a NUL byte.
const binarySniffBytes = 8000

// NewGrepTool returns the grep tool rooted at cwd.
func NewGrepTool(cwd string, opts ToolOptions) *agent.Tool {
	return &agent.Tool{
		Name:  "grep",
		Label: "Grep",
		Description: fmt.Sprintf("Search file contents with a regular expression and return matching lines "+
			"as path:line: text. Searches the working directory by default; pass path to target a file or "+
			"subdirectory, and glob to restrict by filename (e.g. \"*_test.go\"). Set context to also show "+
			"that many lines around each match (shown as path-line- text, with -- between separate groups). "+
			"Set output to \"files\" to list only matching paths, or \"count\" for matches per file. "+
			"Returns at most %d results; narrow the pattern or add a glob to see more.", DefaultGrepLimit),
		PromptSnippet: "Search file contents by regular expression",
		PromptGuidelines: []string{
			"Use grep to find where something is defined or used, instead of reading whole files or shelling out to grep/rg.",
			"Search for a distinctive identifier rather than reading a file to see whether it contains something.",
			"Use grep's context option to see the code around a match instead of grep -A/-B/-C in bash.",
		},
		Parallel: true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "Regular expression to search for"},
				"path":        map[string]any{"type": "string", "description": "File or directory to search (default: working directory)"},
				"glob":        map[string]any{"type": "string", "description": "Only search files matching this pattern, e.g. \"*.go\" or \"src/**/*.ts\""},
				"ignore_case": map[string]any{"type": "boolean", "description": "Case-insensitive match"},
				"context":     map[string]any{"type": "integer", "description": fmt.Sprintf("Lines of context to show before and after each match (default 0, max %d)", MaxGrepContext)},
				"output": map[string]any{
					"type":        "string",
					"enum":        []string{grepLines, grepFiles, grepCount},
					"description": "lines (default): matching lines; files: only paths of matching files; count: number of matches per file",
				},
				"limit": map[string]any{"type": "integer", "description": fmt.Sprintf("Maximum results to return: matches, or files for output files/count (default %d, max %d)", DefaultGrepLimit, MaxGrepLimit)},
			},
			"required": []string{"pattern"},
		},
		Execute: func(ctx context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return executeGrep(cwd, opts, args, codemode.InScript(ctx))
		},
	}
}

// executeGrep runs a search. structured adds the results to the details as
// data, for scripts: see scriptResults.
func executeGrep(cwd string, opts ToolOptions, args map[string]any, structured bool) (agent.ToolResult, error) {
	pattern, err := strArg(args, "pattern")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if strings.TrimSpace(pattern) == "" {
		return agent.ToolResult{}, fmt.Errorf("argument %q must not be empty", "pattern")
	}
	filter, _, err := optStr(args, "glob")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if err := checkPattern(filter); err != nil {
		return agent.ToolResult{}, fmt.Errorf("glob: %w", err)
	}
	ignoreCase, _, err := optBool(args, "ignore_case")
	if err != nil {
		return agent.ToolResult{}, err
	}
	limit, hasLimit, err := optInt(args, "limit")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !hasLimit {
		limit = DefaultGrepLimit
	}
	if limit < 1 {
		return agent.ToolResult{}, fmt.Errorf("limit must be at least 1")
	}
	limit = min(limit, MaxGrepLimit)
	contextLines, _, err := optInt(args, "context")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if contextLines < 0 {
		return agent.ToolResult{}, fmt.Errorf("context must be at least 0")
	}
	contextLines = min(contextLines, MaxGrepContext)
	mode, _, err := optStr(args, "output")
	if err != nil {
		return agent.ToolResult{}, err
	}
	switch mode {
	case "", "content": // "content" is what other harnesses call it
		mode = grepLines
	case grepLines, grepFiles, grepCount:
	default:
		return agent.ToolResult{}, fmt.Errorf("output must be %q, %q or %q", grepLines, grepFiles, grepCount)
	}

	expr := pattern
	if ignoreCase {
		expr = "(?i)" + pattern
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}

	pol := opts.policy()
	root, single, err := searchRoot(cwd, pol, args)
	if err != nil {
		return agent.ToolResult{}, err
	}

	var (
		lines    []string
		matches  int
		files    int
		searched int
		// hitLimit records that output stopped at the cap, which is not the
		// same as having found everything.
		hitLimit   bool
		skippedBig int
		skippedBin int
		// results is what structured adds: one entry per match, file or
		// count, as the output mode lists them.
		results []map[string]any
	)
	// Any match starts with the pattern's literal prefix, when it has one, so
	// a file without it is skipped without running the regexp over its lines.
	var literal []byte
	if prefix, _ := re.LiteralPrefix(); prefix != "" {
		literal = []byte(prefix)
	}
	// One buffer serves every file: a search reads thousands of them.
	var buf []byte
	scan := func(rel string, d fs.DirEntry) error {
		if hitLimit {
			return errStopWalk // the last result's trailing context ended with its file
		}
		if info, err := d.Info(); err == nil && info.Size() > MaxGrepFileBytes {
			skippedBig++
			return nil
		}
		f, err := pol.OpenRead(filepath.Join(root, rel))
		if err != nil {
			return nil // gone, unreadable, or swapped for a link to a hidden file
		}
		data, err := readInto(&buf, f, MaxGrepFileBytes+1)
		f.Close()
		if err != nil {
			return nil
		}
		if len(data) > MaxGrepFileBytes { // grew since it was listed
			skippedBig++
			return nil
		}
		if bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0 {
			skippedBin++
			return nil
		}
		searched++
		found := 0
		// Context state for this file: the lines held back in case a match
		// follows, how many lines after the last match are still owed, and
		// the last line printed, which decides where a -- separator goes.
		type held struct {
			n    int
			text string
		}
		var before []held
		after, printed := 0, 0
		emit := func(n int, sep byte, text string) {
			lines = append(lines, fmt.Sprintf("%s%c%d%c %s", rel, sep, n, sep, clipLine(text, MaxGrepLineWidth)))
			printed = n
		}
		if len(literal) > 0 && !bytes.Contains(data, literal) {
			data = nil
		}
		for n := 1; len(data) > 0; n++ {
			var line []byte
			line, data = nextLine(data)
			if !re.Match(line) {
				switch {
				case after > 0:
					emit(n, '-', string(line))
					after--
				case hitLimit:
					return errStopWalk
				case contextLines > 0:
					if before = append(before, held{n, string(line)}); len(before) > contextLines {
						before = before[1:]
					}
				}
				continue
			}
			text := string(line)
			if hitLimit {
				// A match inside the trailing context of the last result
				// would be a result past the limit; stop before it.
				return errStopWalk
			}
			found++
			matches++
			if mode != grepLines {
				if mode == grepFiles {
					break // one match is enough to list the file
				}
				continue
			}
			if contextLines > 0 {
				first := n
				if len(before) > 0 {
					first = before[0].n
				}
				if len(lines) > 0 && (printed == 0 || first > printed+1) {
					lines = append(lines, "--")
				}
				for _, h := range before {
					emit(h.n, '-', h.text)
				}
				before, after = before[:0], contextLines
			}
			emit(n, ':', text)
			if structured {
				results = append(results, map[string]any{
					"path": scriptPath(cwd, root, rel), "line": n, "text": clipLine(text, MaxGrepLineWidth),
				})
			}
			if matches >= limit {
				hitLimit = true
				if after == 0 {
					return errStopWalk
				}
			}
		}
		if found == 0 {
			return nil
		}
		files++
		switch mode {
		case grepFiles:
			lines = append(lines, rel)
			if structured {
				results = append(results, map[string]any{"path": scriptPath(cwd, root, rel)})
			}
		case grepCount:
			lines = append(lines, fmt.Sprintf("%s: %d", rel, found))
			if structured {
				results = append(results, map[string]any{"path": scriptPath(cwd, root, rel), "count": found})
			}
		}
		if mode != grepLines && files >= limit {
			hitLimit = true
			return errStopWalk
		}
		return nil
	}

	stats, err := searchEach(root, single, filter, pol, scan)
	if err != nil {
		return agent.ToolResult{}, err
	}

	scope := grepFooter(mode, matches, files, searched, hitLimit, skippedBig, skippedBin, limit) + stats.note()
	var out string
	if matches == 0 {
		// A negative result is only trustworthy if it says what was searched:
		// a run that skipped everything also reports no matches.
		out = fmt.Sprintf("No matches for %q.\n\n%s", pattern, scope)
	} else {
		body := TruncateHead(strings.Join(lines, "\n"), MaxOutputLines, MaxOutputBytes)
		out = body.Content
		if body.Truncated {
			out += fmt.Sprintf("\n\n[Output truncated at %s. Narrow the pattern or add a glob.]",
				FormatSize(MaxOutputBytes))
		}
		out += "\n\n" + scope
	}
	details := map[string]any{
		"pattern": pattern, "path": root, "matches": matches, "output": mode,
		"files": files, "searched": searched, "hit_limit": hitLimit,
		"skipped_big": skippedBig, "skipped_binary": skippedBin,
	}
	if structured {
		details["results"] = nonNil(results)
	}
	return agent.ToolResult{
		Content: []agent.Content{&agent.TextContent{Text: out}},
		Details: details,
	}, nil
}

// grepFooter states what was searched and what was left out. A silent result
// is indistinguishable from a broken search unless the scope is reported.
func grepFooter(mode string, matches, files, searched int, hitLimit bool, skippedBig, skippedBin int, limit int) string {
	var s string
	if mode == grepFiles {
		// Scanning stops at a file's first match, so only files are counted.
		s = fmt.Sprintf("%d of %d searched file", files, searched)
		if searched != 1 {
			s += "s"
		}
		s += " match"
	} else {
		unit := "matches"
		if matches == 1 {
			unit = "match"
		}
		s = fmt.Sprintf("%d %s in %d of %d searched file", matches, unit, files, searched)
		if searched != 1 {
			s += "s"
		}
	}
	if hitLimit {
		s += fmt.Sprintf(" — stopped at the limit of %d, so results may continue", limit)
	}
	var notes []string
	if skippedBig > 0 {
		notes = append(notes, fmt.Sprintf("%d file(s) over %s skipped", skippedBig, FormatSize(MaxGrepFileBytes)))
	}
	if skippedBin > 0 {
		notes = append(notes, fmt.Sprintf("%d binary file(s) skipped", skippedBin))
	}
	if len(notes) > 0 {
		s += " (" + strings.Join(notes, ", ") + ")"
	}
	return s
}

// searchRoot resolves the path argument to a directory to walk. Naming a
// single file is allowed and returns that file's directory along with its path
// relative to it, so "grep pattern path=foo.go" works without a second rule.
// The working directory is the default.
func searchRoot(cwd string, pol sandbox.Policy, args map[string]any) (dir, single string, err error) {
	raw, has, err := optStr(args, "path")
	if err != nil {
		return "", "", err
	}
	if !has || strings.TrimSpace(raw) == "" {
		raw = "."
	}
	root := resolvePath(cwd, raw)
	// A single file is judged by where it leads; a directory's hidden
	// subtrees are skipped by the walk.
	if err := pol.CheckRead(root); err != nil {
		return "", "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("Path not found: %s.%s", root, notFoundHint(cwd, root, pol))
		}
		return "", "", err
	}
	if info.IsDir() {
		return root, "", nil
	}
	rel, err := filepath.Rel(filepath.Dir(root), root)
	if err != nil {
		return "", "", err
	}
	return filepath.Dir(root), filepath.ToSlash(rel), nil
}

// searchEach applies fn to every searchable file under dir, restricted to one
// file when single is set and to matching filenames when filter is set.
//
// A named file is visited directly. Walking its directory to find it was slow
// in a large tree, and ignore rules would silently skip a file the caller
// asked for by name; an explicit path is not subject to .gitignore.
func searchEach(dir, single, filter string, pol sandbox.Policy, fn func(rel string, d fs.DirEntry) error) (st walkStats, err error) {
	if single != "" {
		if !matchFilter(filter, single) {
			return st, nil
		}
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(single)))
		if err != nil {
			return st, err
		}
		if err := fn(single, fs.FileInfoToDirEntry(info)); err != nil && !errors.Is(err, errStopWalk) {
			return st, err
		}
		return st, nil
	}
	return walkSearch(dir, pol, func(rel string, d fs.DirEntry) error {
		if !matchFilter(filter, rel) {
			return nil
		}
		return fn(rel, d)
	})
}

// readInto reads up to limit bytes of f into *buf, growing it as needed, and
// returns what was read. The result is only valid until the next call.
func readInto(buf *[]byte, f *os.File, limit int) ([]byte, error) {
	b := (*buf)[:0]
	if cap(b) == 0 {
		b = make([]byte, 0, 64*1024)
	}
	for len(b) < limit {
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)]
		}
		n, err := f.Read(b[len(b):min(cap(b), limit)])
		b = b[:len(b)+n]
		if err == io.EOF {
			break
		}
		if err != nil {
			*buf = b
			return nil, err
		}
	}
	*buf = b
	return b, nil
}

// nextLine splits the first line off data, without its line ending (\n or
// \r\n), the way bufio.ScanLines does.
func nextLine(data []byte) (line, rest []byte) {
	i := bytes.IndexByte(data, '\n')
	if i < 0 {
		line, rest = data, nil
	} else {
		line, rest = data[:i], data[i+1:]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line, rest
}

// clipLine trims a matched line to width, marking the cut so the model knows
// it is not seeing the whole line.
func clipLine(text string, width int) string {
	text = strings.TrimRight(text, " \t")
	if len(text) <= width {
		return text
	}
	// Cut on a rune boundary so the clipped text stays valid UTF-8.
	cut := width
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + " …"
}
