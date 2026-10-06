package coding

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/ddombrow/malachi/sandbox"
)

// errStopWalk aborts a walkSearch early without reporting an error, so tools
// that stop at a match limit don't pay to scan the rest of a repository.
var errStopWalk = errors.New("stop walk")

// skipDirs are never searched. They hold dependency trees, build output, and
// version-control data: large, generated, and never where the answer is.
var skipDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	"dist":          true,
	"build":         true,
	"out":           true,
	"target":        true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	".mypy_cache":   true,
	".pytest_cache": true,
	".next":         true,
	".gradle":       true,
}

// walkSearch visits every searchable file under root in lexical order,
// skipping ignored directories and paths the sandbox hides. fn receives the
// slash-separated path relative to root. Returning errStopWalk from fn ends
// the walk successfully. It reports what it left out.
//
// Symlinks are not followed: a link pointing at an ancestor would otherwise
// make the walk unbounded.
func walkSearch(root string, pol sandbox.Policy, fn func(rel string, d fs.DirEntry) error) (st walkStats, err error) {
	ign := loadIgnores(root)
	// The walk follows no symlinks, so below the canonical root every
	// entry's canonical path is just root/rel: no per-file resolving.
	canonRoot := ""
	if pol.Enabled {
		canonRoot = sandbox.Canonical(root)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		if canonRoot != "" && pol.HiddenAtCanonical(filepath.Join(canonRoot, rel)) != "" {
			st.hidden++
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] || ign.match(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || ign.match(rel, false) {
			return nil
		}
		return fn(rel, d)
	})
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	st.skippedRules = ign.skipped
	return st, err
}

// walkStats are what a search left out, for its footer.
type walkStats struct {
	skippedRules int // .gitignore rules beyond the limits
	hidden       int // files and directories hidden by the sandbox
}

// note is the footer remark for what was left out, or "".
func (st walkStats) note() string {
	out := skippedRulesNote(st.skippedRules)
	if st.hidden > 0 {
		out += fmt.Sprintf(" (%d path(s) hidden by the sandbox were not searched)", st.hidden)
	}
	return out
}

// skippedRulesNote is the footer remark for ignore rules that were not
// applied, or "".
func skippedRulesNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d .gitignore rule(s) beyond the first %d, or over %d bytes, were not applied)", n, maxIgnoreRules, maxPatternBytes)
}

// Limits on patterns, which come from the model (glob, grep's glob) and from
// a repository's own .gitignore. Matching is polynomial, but unbounded input
// would still let a hostile repository make every search slow.
const (
	maxPatternBytes    = 1024
	maxPatternSegments = 64
	maxIgnoreRules     = 1000
)

// checkPattern rejects a glob too large to be a reasonable path pattern.
func checkPattern(pattern string) error {
	if len(pattern) > maxPatternBytes {
		return fmt.Errorf("pattern is %d bytes; the limit is %d", len(pattern), maxPatternBytes)
	}
	if n := strings.Count(pattern, "/") + 1; n > maxPatternSegments {
		return fmt.Errorf("pattern has %d path segments; the limit is %d", n, maxPatternSegments)
	}
	return nil
}

// matchGlob matches a slash-separated path against a glob supporting "**",
// which matches any number of path segments including none. Every other
// segment follows path.Match, so "*.go" does not cross a "/".
//
// Matching is memoized over (pattern segment, path segment), so it costs at
// most their product. Without that, each "**" retried every split and a
// pattern like "**/**/**/…/x" took exponential time against a deep path.
func matchGlob(pattern, name string) bool {
	pat, segs := strings.Split(pattern, "/"), strings.Split(name, "/")
	width := len(segs) + 1
	memo := make([]int8, (len(pat)+1)*width) // 0 unknown, 1 match, 2 no match
	var match func(i, j int) bool
	match = func(i, j int) bool {
		k := i*width + j
		if memo[k] != 0 {
			return memo[k] == 1
		}
		var ok bool
		switch {
		case i == len(pat):
			ok = j == len(segs)
		case pat[i] == "**":
			// Consume no segment, or one and stay on "**".
			ok = match(i+1, j) || (j < len(segs) && match(i, j+1))
		case j == len(segs):
			ok = false
		default:
			m, err := path.Match(pat[i], segs[j])
			ok = err == nil && m && match(i+1, j+1)
		}
		memo[k] = 2
		if ok {
			memo[k] = 1
		}
		return ok
	}
	return match(0, 0)
}

// matchFilter applies gitignore-style matching to one relative path: a pattern
// containing a slash is anchored to the search root, otherwise it matches the
// base name at any depth.
func matchFilter(pattern, rel string) bool {
	if pattern == "" {
		return true
	}
	if !strings.Contains(pattern, "/") {
		ok, err := path.Match(pattern, path.Base(rel))
		return err == nil && ok
	}
	return matchGlob(pattern, rel)
}

// ignoreRule is one parsed .gitignore line.
type ignoreRule struct {
	pattern  string
	negate   bool // "!pattern": re-include a previously ignored path
	dirOnly  bool // "pattern/": match directories only
	anchored bool // matches from the search root, not by base name
}

// ignores holds the root .gitignore. Nested .gitignore files are not read;
// honoring them per-directory would change results depending on where the
// search started. skipped counts rules beyond the limits, which are ignored.
type ignores struct {
	rules   []ignoreRule
	skipped int
}

func loadIgnores(root string) *ignores {
	ig := &ignores{}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return ig
	}
	for _, line := range strings.Split(string(data), "\n") {
		r, ok := parseIgnoreRule(line)
		if !ok {
			continue
		}
		if len(ig.rules) >= maxIgnoreRules || checkPattern(r.pattern) != nil {
			ig.skipped++
			continue
		}
		ig.rules = append(ig.rules, r)
	}
	return ig
}

func parseIgnoreRule(line string) (ignoreRule, bool) {
	line = strings.TrimSpace(strings.TrimRight(line, "\r"))
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	var r ignoreRule
	if rest, ok := strings.CutPrefix(line, "!"); ok {
		r.negate, line = true, rest
	}
	if rest, ok := strings.CutSuffix(line, "/"); ok {
		r.dirOnly, line = true, rest
	}
	// A leading slash is stripped, but still marks the rule as anchored.
	if rest, ok := strings.CutPrefix(line, "/"); ok {
		r.anchored, line = true, rest
	}
	if line == "" {
		return ignoreRule{}, false
	}
	if strings.Contains(line, "/") {
		r.anchored = true
	}
	r.pattern = line
	return r, true
}

// match reports whether rel is ignored. Later rules win, so a negation can
// re-include something an earlier rule excluded.
func (ig *ignores) match(rel string, isDir bool) bool {
	if ig == nil {
		return false
	}
	ignored := false
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		var hit bool
		if r.anchored {
			hit = matchGlob(r.pattern, rel)
		} else {
			ok, err := path.Match(r.pattern, path.Base(rel))
			hit = err == nil && ok
		}
		if hit {
			ignored = !r.negate
		}
	}
	return ignored
}
