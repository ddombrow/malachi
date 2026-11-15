package coding

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func TestRequiredLiteral(t *testing.T) {
	for _, c := range []struct {
		expr, want string
		fold       bool
	}{
		{"func NewReader", "func NewReader", false},
		{`^func .*Reader\(`, "Reader(", false},
		{`\bTODO\b`, "TODO", false},
		{`(?i)NewReader`, "newreader", true},
		{`(foo)+bar`, "foo", false},
		{`x{3,}`, "x", false},
		// Nothing is required where any part is optional or alternative.
		{`foo|bar`, "", false},
		{`(foo)?`, "", false},
		{`x{0,3}`, "", false},
		{`.`, "", false},
		// Case is ignored only for ASCII text.
		{`(?i)größe`, "", false},
		{`(?i)gr`, "gr", true},
	} {
		got := requiredLiteral(c.expr)
		if string(got.text) != c.want || (c.want != "" && got.fold != c.fold) {
			t.Errorf("requiredLiteral(%q) = %q fold=%v, want %q fold=%v", c.expr, got.text, got.fold, c.want, c.fold)
		}
	}
}

// Whatever the literal, the lines found must be the ones the regexp alone
// finds: the literal only saves work.
func TestMatchingLinesAgreeWithTheRegexp(t *testing.T) {
	text := strings.Join([]string{
		"func NewReader(r io.Reader) *Reader {",
		"",
		"\treturn NEWREADER",
		"no match here",
		"a newreader at the end",
		"Kelvin and ſs", // match k and s when case is ignored
		"KS ks",
		"crlf line\r",
		"last line without newline newreader",
	}, "\n")
	for _, expr := range []string{
		"NewReader", "(?i)newreader", "(?i)k", "(?i)s", "(?i)ks", `^func .*Reader\(`, "^$", `line\r?$`, "(?i)elvin", "x",
	} {
		re := regexp.MustCompile(expr)
		var want []int
		data := []byte(text)
		for n := 1; len(data) > 0; n++ {
			var line []byte
			if line, data = nextLine(data); re.Match(line) {
				want = append(want, n)
			}
		}
		s := &grepScanner{re: re, lit: requiredLiteral(expr)}
		if got := s.matchingLines([]byte(text)); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q: lines %v, want %v (literal %q)", expr, got, want, s.lit.text)
		}
		s.cap = 1
		if got := s.matchingLines([]byte(text)); len(want) > 0 && (len(got) != 1 || got[0] != want[0]) {
			t.Errorf("%q with a cap of 1: lines %v, want [%d]", expr, got, want[0])
		}
	}
}

// Files are consumed in the order the walk found them, and a consumer that
// stops the search ends it without scanning what is left.
func TestScannerKeepsTheWalksOrderAndStops(t *testing.T) {
	dir := t.TempDir()
	const files = 200
	var names []string
	for i := range files {
		name := fmt.Sprintf("f%03d.txt", i)
		write(t, dir, name, "needle\n")
		names = append(names, name)
	}
	s := &grepScanner{re: regexp.MustCompile("needle"), lit: requiredLiteral("needle"), pol: ToolOptions{}.policy(), root: dir}
	walk := func(visit func(string, fs.DirEntry) error) error {
		for _, n := range names {
			if err := visit(n, fakeEntry{}); err != nil {
				return err
			}
		}
		return nil
	}

	var seen []string
	if err := s.each(walk, func(f *grepFile) error {
		seen = append(seen, f.rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != strings.Join(names, ",") {
		t.Fatalf("consumed out of order: %v", seen)
	}

	seen = nil
	err := s.each(walk, func(f *grepFile) error {
		if seen = append(seen, f.rel); len(seen) == 3 {
			return errStopWalk
		}
		return nil
	})
	if err != errStopWalk || len(seen) != 3 {
		t.Fatalf("stopping: err %v after %d files", err, len(seen))
	}
}

type fakeEntry struct{ fs.DirEntry }

func (fakeEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }
