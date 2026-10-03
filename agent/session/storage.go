package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// File is one append-only session transcript. The file is created on the
// first Append, so a session that never gets a message leaves no trace.
type File struct {
	mu      sync.Mutex
	path    string
	entries []*Entry
}

// NewFile returns an empty, not-yet-created session at path.
func NewFile(path string) *File { return &File{path: path} }

// Load reads an existing session file.
func Load(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sf := &File{path: path}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 256*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("%s: invalid session entry on line %d: %w", path, n, err)
		}
		sf.entries = append(sf.entries, &e)
	}
	return sf, sc.Err()
}

func (f *File) Path() string { return f.path }

// Entries returns a copy of the entries in file order.
func (f *File) Entries() []*Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*Entry(nil), f.entries...)
}

// TipID returns the id of the active tip, or "".
func (f *File) TipID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := Tip(f.entries); t != nil {
		return t.ID
	}
	return ""
}

// Append parents e on the current tip (unless ParentID is already set)
// and writes it, making it the new tip.
func (f *File) Append(e *Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.ParentID == "" {
		if t := Tip(f.entries); t != nil {
			e.ParentID = t.ID
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	fh, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(append(line, '\n')); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	f.entries = append(f.entries, e)
	return nil
}

// Exists reports whether the file has been created on disk.
func (f *File) Exists() bool {
	_, err := os.Stat(f.path)
	return err == nil
}

// Info summarizes a session file for pickers.
type Info struct {
	Path     string
	Modified time.Time
	Title    string
	Preview  string // first user message text
	Messages int
}

// List returns the sessions in dir, newest first.
func List(dir string) ([]Info, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, p := range matches {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		f, err := Load(p)
		if err != nil {
			continue
		}
		info := Info{Path: p, Modified: st.ModTime()}
		for _, e := range f.entries {
			switch {
			case e.Type == TypeSessionInfo:
				info.Title = e.String("title")
			case e.Type == TypeMessage:
				info.Messages++
				if u, ok := e.Message.(*agent.UserMessage); ok && info.Preview == "" {
					info.Preview = u.Content.String()
				}
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// Latest returns the newest session in dir, or fs.ErrNotExist.
func Latest(dir string) (string, error) {
	infos, err := List(dir)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", fs.ErrNotExist
	}
	return infos[0].Path, nil
}

// Find resolves a session reference: a path, or a (prefix of a) file name
// within dir.
func Find(dir, ref string) (string, error) {
	if _, err := os.Stat(ref); err == nil {
		return ref, nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	var found []string
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasPrefix(base, ref) || strings.Contains(strings.TrimSuffix(base, ".jsonl"), ref) {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no session matching %q in %s", ref, dir)
	case 1:
		return found[0], nil
	}
	return "", errors.New("session reference " + ref + " is ambiguous")
}
