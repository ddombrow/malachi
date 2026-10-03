package coding

import (
	"fmt"
	"strings"
)

type diffOp struct {
	kind byte // ' ', '-', '+'
	text string
}

func diffLines(a, b []string) []diffOp {
	// Trim the common prefix and suffix; edits usually touch a small middle.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []diffOp
	for _, l := range a[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, lcsDiff(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

// lcsDiff is a quadratic LCS diff for the (small) changed middle. Very large
// middles degrade to delete-all/insert-all rather than allocating a huge table.
func lcsDiff(a, b []string) []diffOp {
	var ops []diffOp
	if len(a)*len(b) > 4_000_000 {
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i, j = i+1, j+1
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

func splitForDiff(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// UnifiedDiff returns a unified patch between old and new (3 lines of
// context) and the 1-based first changed line in new, or 0 if identical.
func UnifiedDiff(path, oldText, newText string) (patch string, firstChanged int) {
	ops := diffLines(splitForDiff(oldText), splitForDiff(newText))
	const ctx = 3
	var b strings.Builder
	oldLine, newLine := 1, 1
	// Positions (old, new line numbers) of every op.
	type pos struct{ o, n int }
	at := make([]pos, len(ops))
	for i, op := range ops {
		at[i] = pos{oldLine, newLine}
		switch op.kind {
		case ' ':
			oldLine++
			newLine++
		case '-':
			oldLine++
			if firstChanged == 0 {
				firstChanged = max(newLine, 1)
			}
		case '+':
			newLine++
			if firstChanged == 0 {
				firstChanged = newLine - 1
			}
		}
	}
	if firstChanged == 0 {
		return "", 0
	}
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", path, path)
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(0, i-ctx)
		end := i
		// Extend the hunk while changes are within 2*ctx of each other.
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run < len(ops) && run-end <= 2*ctx {
				end = run
				continue
			}
			end = min(len(ops), end+ctx)
			break
		}
		var oldN, newN int
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				oldN++
			}
			if op.kind != '-' {
				newN++
			}
		}
		oStart, nStart := at[start].o, at[start].n
		if oldN == 0 {
			oStart--
		}
		if newN == 0 {
			nStart--
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", oStart, oldN, nStart, newN)
		for _, op := range ops[start:end] {
			b.WriteByte(op.kind)
			b.WriteString(op.text)
			b.WriteByte('\n')
		}
		i = end
	}
	return b.String(), firstChanged
}
