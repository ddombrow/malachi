package coding

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
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
// skipping ignored directories. fn receives the slash-separated path relative
// to root. Returning errStopWalk from fn ends the walk successfully.
//
// Symlinks are not followed: a link pointing at an ancestor would otherwise
// make the walk unbounded.
func walkSearch(root string, fn func(rel string, d fs.DirEntry) error) error {
	ign := loadIgnores(root)
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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
		return nil
	}
	return err
}

// matchGlob matches a slash-separated path against a glob supporting "**",
// which matches any number of path segments including none. Every other
// segment follows path.Match, so "*.go" does not cross a "/".
func matchGlob(pattern, name string) bool {
	return globSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func globSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			// Try consuming every remaining segment count, including zero.
			for i := 0; i <= len(name); i++ {
				if globSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
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
// search started.
type ignores struct{ rules []ignoreRule }

func loadIgnores(root string) *ignores {
	ig := &ignores{}
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return ig
	}
	for _, line := range strings.Split(string(data), "\n") {
		if r, ok := parseIgnoreRule(line); ok {
			ig.rules = append(ig.rules, r)
		}
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
