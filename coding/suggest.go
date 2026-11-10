package coding

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ddombrow/malachi/sandbox"
)

// Limits on path suggestions, which run only after a path was not found.
const (
	maxPathSuggestions = 3
	maxDirListing      = 30
	// suggestWalkFiles caps the project walk for files of the same name, so
	// a huge tree cannot stall an error message.
	suggestWalkFiles = 20_000
	// minNameSimilarity is the least similar a file name in the same
	// directory may be and still be suggested.
	minNameSimilarity = 0.5
)

// notFoundHint is what to append to a "not found" error for requested: the
// paths the model most likely meant, or the directory's contents when
// nothing is close. It is "" when there is nothing useful to say. Every path
// it names is one the policy lets the model read, so a hint never reveals a
// hidden file.
func notFoundHint(cwd, requested string, pol sandbox.Policy) string {
	show := func(p string) string {
		if rel, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
		return p
	}
	if paths := suggestPaths(cwd, requested, pol); len(paths) > 0 {
		for i, p := range paths {
			paths[i] = show(p)
		}
		if len(paths) == 1 {
			return fmt.Sprintf(" Did you mean %s?", paths[0])
		}
		return fmt.Sprintf(" Did you mean one of: %s?", strings.Join(paths, ", "))
	}
	dir := filepath.Dir(requested)
	names, more := listDir(dir, pol)
	if len(names) == 0 {
		return ""
	}
	s := fmt.Sprintf(" %s contains: %s", show(dir)+"/", strings.Join(names, ", "))
	if more > 0 {
		s += fmt.Sprintf(" (and %d more)", more)
	}
	return s
}

// suggestPaths returns up to maxPathSuggestions existing, readable files the
// model may have meant by requested, best first:
//
//  1. requested with leading directories dropped, under cwd: a path written
//     from the wrong root, such as src/coding/x.go for src/app/coding/x.go;
//  2. files of the same name elsewhere under cwd, those sharing more of the
//     requested path's tail first;
//  3. files in the requested directory with a similar name.
func suggestPaths(cwd, requested string, pol sandbox.Policy) []string {
	readable := func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && info.Mode().IsRegular() && pol.CheckRead(p) == nil
	}

	parts := strings.Split(filepath.ToSlash(filepath.Clean(requested)), "/")
	for i := 1; i < len(parts); i++ {
		p := filepath.Join(cwd, filepath.FromSlash(path.Join(parts[i:]...)))
		if p != requested && readable(p) {
			return []string{p}
		}
	}

	var out []string
	add := func(p string) {
		if len(out) < maxPathSuggestions && p != requested && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}

	base := filepath.Base(requested)
	var same []string
	visited := 0
	_, _ = walkSearch(cwd, pol, func(rel string, _ fs.DirEntry) error {
		if visited++; visited > suggestWalkFiles {
			return errStopWalk
		}
		if path.Base(rel) == base {
			same = append(same, filepath.Join(cwd, filepath.FromSlash(rel)))
		}
		return nil
	})
	tail := func(p string) int {
		a := strings.Split(filepath.ToSlash(p), "/")
		n := 0
		for n < len(a) && n < len(parts) && a[len(a)-1-n] == parts[len(parts)-1-n] {
			n++
		}
		return n
	}
	sort.SliceStable(same, func(i, j int) bool {
		if ti, tj := tail(same[i]), tail(same[j]); ti != tj {
			return ti > tj
		}
		return len(same[i]) < len(same[j])
	})
	for _, p := range same {
		add(p)
	}

	dir := filepath.Dir(requested)
	entries, err := os.ReadDir(dir)
	if err != nil || pol.CheckRead(dir) != nil {
		return out
	}
	type scored struct {
		name  string
		score float64
	}
	var similar []scored
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if s := nameSimilarity(base, e.Name()); s >= minNameSimilarity {
			similar = append(similar, scored{e.Name(), s})
		}
	}
	sort.SliceStable(similar, func(i, j int) bool { return similar[i].score > similar[j].score })
	for _, s := range similar {
		if p := filepath.Join(dir, s.name); readable(p) {
			add(p)
		}
	}
	return out
}

// nameSimilarity scores how alike two file names are. A name that contains
// the other's stem (sandbox.go and sandbox_state.go) counts as close even
// when the edit distance is large.
func nameSimilarity(want, have string) float64 {
	stem := func(s string) string { return strings.TrimSuffix(s, filepath.Ext(s)) }
	ws, hs := strings.ToLower(stem(want)), strings.ToLower(stem(have))
	s := similarity(strings.ToLower(want), strings.ToLower(have))
	if len(ws) >= 3 && len(hs) >= 3 && (strings.Contains(hs, ws) || strings.Contains(ws, hs)) {
		s = max(s, 0.7)
	}
	return s
}

// listDir returns up to maxDirListing readable entries of dir, directories
// marked with a trailing slash, and how many more were left out.
func listDir(dir string, pol sandbox.Policy) (names []string, more int) {
	if pol.CheckRead(dir) != nil {
		return nil, 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0
	}
	for _, e := range entries {
		if pol.CheckRead(filepath.Join(dir, e.Name())) != nil {
			continue
		}
		if len(names) == maxDirListing {
			more++
			continue
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	return names, more
}
