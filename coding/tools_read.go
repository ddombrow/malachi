package coding

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/ddombrow/malachi/agent"
)

// MaxImageBytes is the largest image read returns inline.
const MaxImageBytes = 5 * 1024 * 1024

var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// NewReadTool returns the read tool rooted at cwd.
func NewReadTool(cwd string, opts ToolOptions) *agent.Tool {
	return &agent.Tool{
		Name:  "read",
		Label: "Read",
		Description: fmt.Sprintf("Read the contents of a file. Supports text files and images (jpg, png, gif, webp). "+
			"Images are sent to vision-capable models as attachments. For text files, output is truncated to "+
			"%d lines or %dKB (whichever is hit first). Use offset/limit for large files. When you need the "+
			"full file, continue with offset until complete.", MaxOutputLines, MaxOutputBytes/1024),
		PromptSnippet:    "Read file contents",
		PromptGuidelines: []string{"Use read to examine files instead of cat or sed."},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Path to the file to read"},
				"offset": map[string]any{"type": "integer", "description": "Line number to start reading from"},
				"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to read"},
			},
			"required": []string{"path"},
		},
		Execute: func(_ context.Context, _ string, args map[string]any, _ func(agent.ToolResult)) (agent.ToolResult, error) {
			return executeRead(cwd, opts, args)
		},
	}
}

func executeRead(cwd string, opts ToolOptions, args map[string]any) (agent.ToolResult, error) {
	raw, path, err := pathArg(args, cwd)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if err := opts.policy().CheckRead(path); err != nil {
		return agent.ToolResult{}, err
	}
	offset, _, err := optInt(args, "offset")
	if err != nil {
		return agent.ToolResult{}, err
	}
	limit, hasLimit, err := optInt(args, "limit")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if offset < 0 {
		return agent.ToolResult{}, fmt.Errorf("offset must be at least 0")
	}
	if hasLimit && limit < 1 {
		return agent.ToolResult{}, fmt.Errorf("limit must be at least 1")
	}

	// OpenRead judges the file actually opened, so a symlink swapped after
	// the check above still cannot reach a hidden file.
	f, err := opts.policy().OpenRead(path)
	if err != nil {
		if os.IsNotExist(err) {
			return agent.ToolResult{}, fmt.Errorf("File not found: %s", path)
		}
		return agent.ToolResult{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return agent.ToolResult{}, err
	}
	if info.IsDir() {
		return agent.ToolResult{}, fmt.Errorf("Path is a directory: %s. Use bash with ls to list it", path)
	}

	// DetectContentType reads at most 512 bytes, so that is all an image
	// check needs; only a small enough image is then read whole.
	sniff := make([]byte, 512)
	n, err := io.ReadFull(f, sniff)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return agent.ToolResult{}, err
	}
	if mime := http.DetectContentType(sniff[:n]); imageTypes[mime] {
		size := info.Size()
		if size > MaxImageBytes {
			return agent.ToolResult{
				Content: []agent.Content{&agent.TextContent{Text: fmt.Sprintf(
					"Read image file [%s]\n[Image omitted: %s exceeds the %s inline limit.]",
					mime, FormatSize(int(size)), FormatSize(MaxImageBytes))}},
				Details: map[string]any{"path": path, "mime_type": mime, "bytes": size},
			}, nil
		}
		rest, err := io.ReadAll(io.LimitReader(f, MaxImageBytes))
		if err != nil {
			return agent.ToolResult{}, err
		}
		data := append(sniff[:n], rest...)
		return agent.ToolResult{
			Content: []agent.Content{
				&agent.TextContent{Text: fmt.Sprintf("Read image file [%s]", mime)},
				&agent.ImageContent{Data: base64.StdEncoding.EncodeToString(data), MimeType: mime},
			},
			Details: map[string]any{"path": path, "mime_type": mime, "bytes": len(data)},
		}, nil
	}

	// Text: stream the file, keeping only the requested window and only as
	// much of it as the display limits can use. Lines past those limits are
	// counted, not kept, so a huge file costs time, not memory.
	start := 0
	if offset > 0 {
		start = offset - 1
	}
	var (
		kept      []string
		keptBytes int
		window    int // lines in the requested window
		firstLen  int // full length of the window's first line
	)
	inWindow := func(i int) bool { return i >= start && (!hasLimit || i < start+limit) }
	total, valid, err := scanLines(io.MultiReader(bytes.NewReader(sniff[:n]), f), MaxOutputBytes+1,
		func(i int, line []byte, full int) {
			if !inWindow(i) {
				return
			}
			if window == 0 {
				firstLen = full
			}
			window++
			// One line over the line limit (two, in case the last is a
			// trailing empty line) or one line past the byte limit is enough
			// for TruncateHead to reach the same decision as on the whole.
			if len(kept) < MaxOutputLines+2 && keptBytes <= MaxOutputBytes {
				kept = append(kept, string(line))
				keptBytes += len(line) + 1
			}
		})
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !valid {
		return agent.ToolResult{}, fmt.Errorf("Cannot read %s: file is not UTF-8 text or a supported image", path)
	}
	if start >= total {
		return agent.ToolResult{}, fmt.Errorf("Offset %d is beyond end of file (%d lines total)", offset, total)
	}
	end := total
	if hasLimit {
		end = min(start+limit, total)
	}
	selected := strings.Join(kept, "\n")

	t := TruncateHead(selected, MaxOutputLines, MaxOutputBytes)
	startDisplay := start + 1
	var out string
	switch {
	case t.FirstLineTooBig:
		out = fmt.Sprintf("[Line %d is %s, exceeds %s limit. Use bash: sed -n '%dp' %s | head -c %d]",
			startDisplay, FormatSize(firstLen), FormatSize(MaxOutputBytes), startDisplay, raw, MaxOutputBytes)
	case t.Truncated:
		endDisplay := startDisplay + t.OutputLines - 1
		limitNote := ""
		if t.TruncatedBy == "bytes" {
			limitNote = fmt.Sprintf(" (%s limit)", FormatSize(MaxOutputBytes))
		}
		out = fmt.Sprintf("%s\n\n[Showing lines %d-%d of %d%s. Use offset=%d to continue.]",
			t.Content, startDisplay, endDisplay, total, limitNote, endDisplay+1)
	case hasLimit && end < total:
		out = fmt.Sprintf("%s\n\n[%d more lines in file. Use offset=%d to continue.]", t.Content, total-end, end+1)
	default:
		out = t.Content
	}
	return agent.ToolResult{
		Content: []agent.Content{&agent.TextContent{Text: out}},
		Details: map[string]any{"path": path, "truncation": t},
	}, nil
}

// scanLines reads text from r and calls emit for every line, numbered from
// 0. CRLF and a lone CR are line breaks like LF, as when the whole file was
// normalized and split, and a final line is emitted even when empty, so the
// count matches strings.Split. emit receives at most keep bytes of each line
// (the slice is reused; copy it to keep it) and the line's full length. It
// returns the number of lines and whether the whole stream was valid UTF-8.
func scanLines(r io.Reader, keep int, emit func(i int, line []byte, full int)) (lines int, validUTF8 bool, err error) {
	buf := make([]byte, 64*1024)
	var (
		cur    []byte
		curLen int
		prevCR bool
		carry  []byte // an incomplete UTF-8 sequence split across reads
		idx    int
	)
	validUTF8 = true
	endLine := func() {
		emit(idx, cur, curLen)
		idx++
		cur, curLen = cur[:0], 0
	}
	for {
		n, rerr := r.Read(buf)
		chunk := buf[:n]
		if validUTF8 && n > 0 {
			data := append(carry, chunk...)
			cut := len(data)
			// Hold back a trailing partial rune for the next read.
			for back := 1; back <= utf8.UTFMax-1 && back <= len(data); back++ {
				if utf8.RuneStart(data[len(data)-back]) {
					if !utf8.FullRune(data[len(data)-back:]) {
						cut = len(data) - back
					}
					break
				}
			}
			if !utf8.Valid(data[:cut]) {
				validUTF8 = false
			}
			carry = append(carry[:0], data[cut:]...)
		}
		for len(chunk) > 0 {
			if prevCR {
				prevCR = false
				if chunk[0] == '\n' {
					chunk = chunk[1:]
					continue
				}
			}
			k := bytes.IndexAny(chunk, "\r\n")
			seg := chunk
			if k >= 0 {
				seg = chunk[:k]
			}
			if room := keep - len(cur); room > 0 {
				cur = append(cur, seg[:min(len(seg), room)]...)
			}
			curLen += len(seg)
			if k < 0 {
				break
			}
			prevCR = chunk[k] == '\r'
			endLine()
			chunk = chunk[k+1:]
		}
		if rerr == io.EOF {
			if len(carry) > 0 {
				validUTF8 = false
			}
			endLine()
			return idx, validUTF8, nil
		}
		if rerr != nil {
			return idx, validUTF8, rerr
		}
	}
}
