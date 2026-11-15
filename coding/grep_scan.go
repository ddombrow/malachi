package coding

import (
	"bytes"
	"io/fs"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"runtime"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/ddombrow/malachi/sandbox"
)

// A search is two jobs of very different shape. Finding which lines of which
// files match is most of the work and is the same whatever the output: it
// is done here, on every core. Turning matches into results (the limit, the
// context around a match, the order files come in) is cheap and depends on
// everything before it: executeGrep does that, one file at a time in the
// order the walk found them, from what this hands it.

// grepSkip is why a file was not searched.
type grepSkip int

const (
	grepSearched   grepSkip = iota
	grepTooBig              // over MaxGrepFileBytes
	grepUnreadable          // gone, unreadable, or swapped for a link to a hidden file
	grepBinary
)

// grepFile is what was found in one file.
type grepFile struct {
	rel  string
	skip grepSkip
	// matches are the numbers of the lines that match, in order, up to the
	// scanner's cap. data is the file's text, kept only when there are any.
	matches []int
	data    []byte
	buf     *[]byte // data's buffer, to give back with release
}

// grepScanner finds a pattern's matches in files.
type grepScanner struct {
	re   *regexp.Regexp
	lit  literal
	pol  sandbox.Policy
	root string
	// cap is the most matching lines wanted from one file; 0 is all of them.
	cap  int
	pool sync.Pool
}

func (s *grepScanner) release(f *grepFile) {
	if f.buf != nil {
		s.pool.Put(f.buf)
		f.buf, f.data = nil, nil
	}
}

// scan reads rel and finds its matching lines.
func (s *grepScanner) scan(rel string, d fs.DirEntry) *grepFile {
	out := &grepFile{rel: rel}
	if info, err := d.Info(); err == nil && info.Size() > MaxGrepFileBytes {
		out.skip = grepTooBig
		return out
	}
	f, err := s.pol.OpenRead(filepath.Join(s.root, rel))
	if err != nil {
		out.skip = grepUnreadable
		return out
	}
	buf, _ := s.pool.Get().(*[]byte)
	if buf == nil {
		buf = new([]byte)
	}
	data, err := readInto(buf, f, MaxGrepFileBytes+1)
	f.Close()
	switch {
	case err != nil:
		out.skip = grepUnreadable
	case len(data) > MaxGrepFileBytes: // grew since it was listed
		out.skip = grepTooBig
	case bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0:
		out.skip = grepBinary
	default:
		out.matches = s.matchingLines(data)
	}
	if len(out.matches) == 0 {
		s.pool.Put(buf)
		return out
	}
	out.data, out.buf = data, buf
	return out
}

// matchingLines returns the numbers of data's lines that match, split as
// nextLine splits them.
//
// A pattern that cannot match without some literal text (most can't) is only
// tried on the lines where that text occurs: the regexp engine is far slower
// than a search for a string, and nearly every line of nearly every file has
// nothing to try.
func (s *grepScanner) matchingLines(data []byte) []int {
	var matches []int
	if !s.lit.usable(data) {
		for n := 1; len(data) > 0; n++ {
			var line []byte
			if line, data = nextLine(data); s.re.Match(line) {
				if matches = append(matches, n); len(matches) == s.cap {
					break
				}
			}
		}
		return matches
	}
	// start is where the line being looked at begins, and n its number.
	for start, n := 0, 1; start < len(data); {
		at := s.lit.index(data[start:])
		if at < 0 {
			break
		}
		at += start
		// The line holding the occurrence.
		if nl := bytes.LastIndexByte(data[start:at], '\n'); nl >= 0 {
			n += bytes.Count(data[start:start+nl+1], []byte{'\n'})
			start += nl + 1
		}
		line, rest := nextLine(data[start:])
		if s.re.Match(line) {
			if matches = append(matches, n); len(matches) == s.cap {
				break
			}
		}
		start, n = len(data)-len(rest), n+1
	}
	return matches
}

// each scans the files walk reports, on several goroutines, and hands each
// file's result to consume in the order walk reported them. consume returning
// an error ends the search with it; walk's own callback contract (returning
// errStopWalk to stop) is kept by passing consume's error back to it.
//
// Results wait in a window a few files deep per worker, so the walk never
// runs far ahead of what has been consumed: a search that stops at its limit
// has read little it did not need.
func (s *grepScanner) each(walk func(visit func(rel string, d fs.DirEntry) error) error, consume func(*grepFile) error) error {
	workers := min(runtime.GOMAXPROCS(0), 16)
	type job struct {
		rel  string
		d    fs.DirEntry
		done chan *grepFile
	}
	jobs := make(chan job)
	var stopped atomic.Bool
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for j := range jobs {
				if stopped.Load() {
					j.done <- &grepFile{rel: j.rel, skip: grepUnreadable}
					continue
				}
				j.done <- s.scan(j.rel, j.d)
			}
		})
	}
	var window []chan *grepFile
	// take consumes the oldest result still waiting.
	take := func() error {
		f := <-window[0]
		window = window[1:]
		err := consume(f)
		s.release(f)
		return err
	}
	err := walk(func(rel string, d fs.DirEntry) error {
		done := make(chan *grepFile, 1)
		jobs <- job{rel, d, done}
		if window = append(window, done); len(window) < 4*workers {
			return nil
		}
		return take()
	})
	for err == nil && len(window) > 0 {
		err = take()
	}
	// Whatever is still in flight is finished and dropped.
	stopped.Store(true)
	close(jobs)
	wg.Wait()
	for _, done := range window {
		s.release(<-done)
	}
	return err
}

// literal is text that every match of a pattern contains.
type literal struct {
	text []byte // lower case when fold is set
	// fold says the text matches whatever its letters' case.
	fold bool
}

// The two characters outside ASCII that match an ASCII letter when case is
// ignored: the Kelvin sign matches k, and the long s matches s. Text holding
// either may match a folded literal without containing it byte for byte.
var (
	kelvinSign = []byte("K")
	longS      = []byte("ſ")
)

// usable reports whether data can be searched for the literal in place of
// trying the pattern on every line.
func (l literal) usable(data []byte) bool {
	if len(l.text) == 0 {
		return false
	}
	if l.fold && bytes.ContainsAny(l.text, "ks") && (bytes.Contains(data, kelvinSign) || bytes.Contains(data, longS)) {
		return false
	}
	return true
}

// index returns where the literal first occurs in data, or -1.
func (l literal) index(data []byte) int {
	if !l.fold {
		return bytes.Index(data, l.text)
	}
	// Candidates are found by the literal's first byte in either case, and
	// the rest compared letter by letter.
	first := l.text[0]
	either := string([]byte{first, upperASCII(first)})
	for off := 0; ; {
		i := bytes.IndexAny(data[off:], either)
		if i < 0 || off+i+len(l.text) > len(data) {
			return -1
		}
		i += off
		if equalFoldASCII(data[i:i+len(l.text)], l.text) {
			return i
		}
		off = i + 1
	}
}

func upperASCII(b byte) byte {
	if 'a' <= b && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}

// equalFoldASCII reports whether s is lower, which is lower case, but for
// the case of its ASCII letters.
func equalFoldASCII(s, lower []byte) bool {
	for i, b := range s {
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if b != lower[i] {
			return false
		}
	}
	return true
}

// requiredLiteral returns the longest text that every match of expr must
// contain, or none. It reads the pattern's syntax tree: a literal is
// required where it stands, a sequence requires what any of its parts does,
// and a group or a repetition of at least once requires what its body does.
// An alternative or anything optional requires nothing.
//
// A literal that ignores case is used only if it is ASCII, where ignoring
// case is a matter of twenty-six letters (and see kelvinSign).
func requiredLiteral(expr string) literal {
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return literal{}
	}
	var best literal
	var walk func(*syntax.Regexp)
	walk = func(re *syntax.Regexp) {
		switch re.Op {
		case syntax.OpLiteral:
			l := literal{text: []byte(string(re.Rune)), fold: re.Flags&syntax.FoldCase != 0}
			if l.fold {
				if len(l.text) != utf8.RuneCount(l.text) {
					return // not ASCII
				}
				l.text = bytes.ToLower(l.text)
			}
			if len(l.text) > len(best.text) {
				best = l
			}
		case syntax.OpConcat:
			for _, sub := range re.Sub {
				walk(sub)
			}
		case syntax.OpCapture, syntax.OpPlus:
			walk(re.Sub[0])
		case syntax.OpRepeat:
			if re.Min >= 1 {
				walk(re.Sub[0])
			}
		}
	}
	walk(re)
	return best
}
