package coding

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Output limits shared by read and bash, matching tau/Pi.
const (
	MaxOutputLines = 2000
	MaxOutputBytes = 50 * 1024
)

// Truncation describes how output was cut.
type Truncation struct {
	Content         string `json:"-"`
	Truncated       bool   `json:"truncated"`
	TruncatedBy     string `json:"truncated_by,omitempty"` // "lines" or "bytes"
	TotalLines      int    `json:"total_lines"`
	TotalBytes      int    `json:"total_bytes"`
	OutputLines     int    `json:"output_lines"`
	OutputBytes     int    `json:"output_bytes"`
	LastLinePartial bool   `json:"last_line_partial,omitempty"`
	FirstLineTooBig bool   `json:"first_line_exceeds_limit,omitempty"`
}

func splitLinesForCounting(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if strings.HasSuffix(s, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// TruncateHead keeps the beginning of s within the line and byte limits.
func TruncateHead(s string, maxLines, maxBytes int) Truncation {
	lines := splitLinesForCounting(s)
	t := Truncation{TotalLines: len(lines), TotalBytes: len(s)}
	if len(lines) <= maxLines && len(s) <= maxBytes {
		t.Content, t.OutputLines, t.OutputBytes = s, len(lines), len(s)
		return t
	}
	t.Truncated = true
	if len(lines) > 0 && len(lines[0]) > maxBytes {
		t.TruncatedBy, t.FirstLineTooBig = "bytes", true
		return t
	}
	t.TruncatedBy = "lines"
	var out []string
	n := 0
	for i, line := range lines[:min(len(lines), maxLines)] {
		size := len(line)
		if i > 0 {
			size++
		}
		if n+size > maxBytes {
			t.TruncatedBy = "bytes"
			break
		}
		out = append(out, line)
		n += size
	}
	t.Content = strings.Join(out, "\n")
	t.OutputLines, t.OutputBytes = len(out), len(t.Content)
	return t
}

// TruncateTail keeps the end of s within the line and byte limits.
func TruncateTail(s string, maxLines, maxBytes int) Truncation {
	lines := splitLinesForCounting(s)
	t := Truncation{TotalLines: len(lines), TotalBytes: len(s)}
	if len(lines) <= maxLines && len(s) <= maxBytes {
		t.Content, t.OutputLines, t.OutputBytes = s, len(lines), len(s)
		return t
	}
	t.Truncated, t.TruncatedBy = true, "lines"
	var out []string
	n := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if len(out) >= maxLines {
			break
		}
		size := len(lines[i])
		if len(out) > 0 {
			size++
		}
		if n+size > maxBytes {
			t.TruncatedBy = "bytes"
			if len(out) == 0 {
				clipped := tailBytes(lines[i], maxBytes)
				out = append(out, clipped)
				t.LastLinePartial = true
			}
			break
		}
		out = append(out, lines[i])
		n += size
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	t.Content = strings.Join(out, "\n")
	t.OutputLines, t.OutputBytes = len(out), len(t.Content)
	return t
}

// tailBytes returns at most n trailing bytes of s without splitting a rune.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// FormatSize renders a byte count like tau: 512B, 1.5KB, 2.0MB.
func FormatSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
}
