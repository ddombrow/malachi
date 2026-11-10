package coding

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// looseNormalize rewrites s into the form looseIndex compares, and returns
// pos, where pos[i] is the offset in s of the byte that produced out[i], and
// pos[len(out)] is len(s). It differs from s in ways a model copying text
// from memory commonly gets wrong, and that do not change what code means:
//
//   - whitespace at the end of a line is dropped;
//   - a run of spaces and tabs inside a line becomes one space (alignment
//     padding, such as gofmt's in struct fields, is the usual victim);
//   - curly quotes, dashes and non-breaking spaces become their ASCII forms.
//
// Indentation is kept exactly: it changes meaning in some languages, and
// newText's indentation could not be trusted after matching around it.
func looseNormalize(s string) (out string, pos []int) {
	var b strings.Builder
	b.Grow(len(s))
	pos = make([]int, 0, len(s)+1)
	put := func(c byte, at int) {
		b.WriteByte(c)
		pos = append(pos, at)
	}
	lineStart := true
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\n' {
			put(c, i)
			i++
			lineStart = true
			continue
		}
		if c == ' ' || c == '\t' {
			j := i
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			switch {
			case j == len(s) || s[j] == '\n':
				// Trailing whitespace: dropped.
			case lineStart:
				for k := i; k < j; k++ {
					put(s[k], k)
				}
			default:
				put(' ', i)
			}
			i = j
			continue
		}
		lineStart = false
		r, size := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '‘', '’', '‚', '‛':
			put('\'', i)
		case '“', '”', '„', '‟':
			put('"', i)
		case '‐', '‑', '‒', '–', '—', '−':
			put('-', i)
		case ' ', ' ', ' ':
			put(' ', i)
		default:
			for k := range size {
				put(s[i+k], i+k)
			}
		}
		i += size
	}
	pos = append(pos, len(s))
	return b.String(), pos
}

// looseIndex finds old in content after normalizing both with
// looseNormalize. It returns the span of the first match in content and how
// many non-overlapping matches there are.
func looseIndex(content, old string) (start, end, count int) {
	nc, pos := looseNormalize(content)
	no, _ := looseNormalize(old)
	if no == "" || no == old && nc == content {
		return 0, 0, 0 // nothing differs, so the exact search already answered
	}
	first := -1
	for at := 0; ; {
		k := strings.Index(nc[at:], no)
		if k < 0 {
			break
		}
		if first < 0 {
			first = at + k
		}
		count++
		at += k + len(no)
	}
	if count == 0 {
		return 0, 0, 0
	}
	return pos[first], pos[first+len(no)], count
}

// Limits on the closest-text hint, which runs only on a failed edit.
const (
	nearestMaxLines     = 8   // lines of file shown
	nearestCompareBytes = 200 // bytes of each line compared
	nearestScanLines    = 20_000
)

// nearestText finds where old most likely was meant to match, for an edit
// that matched nowhere: the file line most similar to old's first non-blank
// line. It returns that region with line numbers, or "" when nothing is
// similar enough to be worth showing.
func nearestText(content, old string) string {
	oldLines := strings.Split(old, "\n")
	anchor, offset := "", 0
	for i, l := range oldLines {
		if t := strings.TrimSpace(l); t != "" {
			anchor, offset = t, i
			break
		}
	}
	if anchor == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	if len(lines) > nearestScanLines {
		return ""
	}
	best, bestScore := -1, 0.0
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if s := similarity(anchor, t); s > bestScore {
			best, bestScore = i, s
		}
	}
	// Below this the "closest" line is a guess that would mislead.
	if best < 0 || bestScore < 0.6 {
		return ""
	}
	first := max(best-offset, 0)
	last := min(first+min(len(oldLines), nearestMaxLines), len(lines))
	var b strings.Builder
	for i := first; i < last; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, clipLine(lines[i], MaxGrepLineWidth))
	}
	return strings.TrimRight(b.String(), "\n")
}

// similarity is 1 minus the edit distance between a and b over the longer
// length, comparing at most nearestCompareBytes of each.
func similarity(a, b string) float64 {
	a, b = a[:min(len(a), nearestCompareBytes)], b[:min(len(b), nearestCompareBytes)]
	longest := max(len(a), len(b))
	if longest == 0 {
		return 1
	}
	// Lengths far apart cannot be similar; skip the quadratic work.
	if float64(min(len(a), len(b)))/float64(longest) < 0.5 {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[len(b)])/float64(longest)
}
