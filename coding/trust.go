package coding

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Project trust governs whether a directory's instruction files are folded
// into the system prompt. An AGENTS.md is a file the project supplies, and
// supplying one is exactly what an attacker would do if they could get you to
// open a repository: its contents become the agent's instructions, not data.
//
// This is deliberately not a sandbox. It does not restrict the filesystem, the
// shell, the network, or any tool. It decides one thing only: whether files
// found on the way from the root down to the working directory are treated as
// instructions or ignored. Port of tau_coding/project_trust.py.

// TrustPolicy is what to do with a project that has not been decided.
type TrustPolicy string

const (
	// TrustAsk withholds project instructions and asks for a decision.
	TrustAsk TrustPolicy = "ask"
	// TrustAlways trusts every project without asking.
	TrustAlways TrustPolicy = "always"
	// TrustNever withholds project instructions everywhere, permanently.
	TrustNever TrustPolicy = "never"
)

// TrustDecision is a resolved answer for one directory.
type TrustDecision string

const (
	TrustTrusted   TrustDecision = "trusted"
	TrustUntrusted TrustDecision = "untrusted"
)

// maxListedResources bounds how many paths a summary names. A repository tree
// can contain thousands of instruction files, and the point of the summary is
// to show what was withheld, not to reproduce it.
const maxListedResources = 12

// ProjectResources is a bounded, metadata-only summary of the instruction
// files a working directory would contribute: paths and a count, never
// contents. Showing the text would mean printing unvetted instructions into
// the terminal, which is the thing being gated.
type ProjectResources struct {
	Files []string `json:"files"`
	Total int      `json:"total"`
}

// Empty reports whether the project contributes no instruction files, in which
// case there is nothing to decide.
func (r ProjectResources) Empty() bool { return r.Total == 0 }

// TrustState is why a session loaded or withheld project instructions.
type TrustState struct {
	Policy   TrustPolicy
	Decision TrustDecision
	// Path is the canonical directory the decision applies to.
	Path string
	// Source is where the decision came from: "run", "saved", "inherited",
	// "always", "never", or "default".
	Source string
	// InheritedFrom is the ancestor directory a saved decision was made
	// against, empty unless Source is "inherited".
	InheritedFrom string
	Resources     ProjectResources
	// Pending is set when instructions are being withheld only because nobody
	// has answered yet. It is the difference between "this project has not
	// been vouched for" and "you said no to this project".
	Pending bool
}

// Trusted reports whether project instructions were folded into the prompt.
func (t TrustState) Trusted() bool { return t.Decision == TrustTrusted }

// trustEntry is one saved decision. Only exact paths are saved: a decision
// about one directory should not silently extend to a sibling.
type trustEntry struct {
	Decision TrustDecision `json:"decision"`
	At       time.Time     `json:"at"`
}

// TrustStore holds saved per-directory decisions.
type TrustStore struct {
	// Entries maps a canonical directory to its decision.
	Entries map[string]trustEntry
}

// TrustStorePath is where saved decisions live.
func TrustStorePath(home string) string { return filepath.Join(home, "trust.json") }

// LoadTrustStore reads saved decisions. A missing file is an empty store, and
// an unreadable one is an error: silently discarding trust decisions would
// re-prompt for directories the user already answered for.
func LoadTrustStore(home string) (*TrustStore, error) {
	store := &TrustStore{Entries: map[string]trustEntry{}}
	data, err := os.ReadFile(TrustStorePath(home))
	if os.IsNotExist(err) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var entries map[string]trustEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("trust store %s: %w", TrustStorePath(home), err)
	}
	for path, entry := range entries {
		if entry.Decision == TrustTrusted || entry.Decision == TrustUntrusted {
			store.Entries[canonicalPath(path)] = entry
		}
	}
	return store, nil
}

// nearest returns the decision saved for path or the closest ancestor that has
// one, along with the directory it was saved against. Decisions are inherited
// so one answer about a repository covers its package directories; the nearest
// decision wins, so a package can override a parent without disturbing its
// siblings.
func (s *TrustStore) nearest(path string) (string, trustEntry, bool) {
	for dir := canonicalPath(path); ; dir = filepath.Dir(dir) {
		if entry, ok := s.Entries[dir]; ok {
			return dir, entry, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", trustEntry{}, false
		}
	}
}

// SaveTrust writes a decision for one directory. The file is rewritten whole,
// which is fine at the scale of directories a person works in.
func (s *TrustStore) Save(home, path string, decision TrustDecision) error {
	if s.Entries == nil {
		s.Entries = map[string]trustEntry{}
	}
	s.Entries[canonicalPath(path)] = trustEntry{Decision: decision, At: time.Now().UTC()}
	entries := make(map[string]trustEntry, len(s.Entries))
	for k, v := range s.Entries {
		entries[k] = v
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	path = TrustStorePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	// Rename is atomic, so an interrupted save leaves the old file intact
	// rather than a truncated one that would discard every decision.
	return os.Rename(tmp, path)
}

// SetTrust records a decision for dir and persists it.
func SetTrust(home, dir string, decision TrustDecision) error {
	store, err := LoadTrustStore(home)
	if err != nil {
		return err
	}
	return store.Save(home, dir, decision)
}

// canonicalPath resolves symlinks and cleans a directory so that decisions
// match regardless of how the path was spelled on the command line.
func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

// underHome reports whether path is home or inside it. A home directory is the
// user's own, so its instructions are not project input.
func underHome(home, path string) bool {
	if home == "" {
		return false
	}
	home = canonicalPath(home)
	rel, err := filepath.Rel(home, canonicalPath(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// DetectProjectResources reports the instruction files between the root and
// cwd, excluding the user's own home directory. It is what a decision would
// gate, summarized without reading any of it.
func DetectProjectResources(home, cwd string) ProjectResources {
	var all []string
	add := func(dir string) {
		for _, name := range contextFileNames {
			p := filepath.Join(dir, name)
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				all = append(all, p)
				return
			}
		}
	}
	// cwd first, then each parent up to the root: a checkout can carry an
	// AGENTS.md of its own as well as inherit one from a parent directory.
	if !underHome(home, cwd) {
		add(cwd)
	}
	for dir, ok := nextProjectDir(home, cwd); ok; dir, ok = nextProjectDir(home, dir) {
		add(dir)
	}
	sort.Strings(all)
	out := ProjectResources{Total: len(all)}
	if len(all) > maxListedResources {
		out.Files = append([]string(nil), all[:maxListedResources]...)
	} else {
		out.Files = all
	}
	return out
}

// nextProjectDir walks one step up from dir, stopping at the filesystem root
// and at the user's home, whose instructions are the user's own.
func nextProjectDir(home, dir string) (string, bool) {
	if dir == "" || dir == "/" {
		return "", false
	}
	parent := filepath.Dir(dir)
	if parent == dir {
		return "", false
	}
	if underHome(home, parent) {
		return "", false
	}
	return parent, true
}

// ResolveTrust decides whether cwd's instruction files may be used. override
// is a decision made for this run only ("yes"/"no"), and applies ahead of
// saved decisions.
func ResolveTrust(home, cwd string, policy TrustPolicy, override string) TrustState {
	state := TrustState{
		Policy:    policy,
		Path:      canonicalPath(cwd),
		Decision:  TrustUntrusted,
		Resources: DetectProjectResources(home, cwd),
	}
	if state.Resources.Empty() {
		// Nothing to gate. Trusting a project that ships no instructions is
		// not a question worth asking.
		state.Decision, state.Source = TrustTrusted, "default"
		return state
	}
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "yes", "y", "true":
		state.Decision, state.Source = TrustTrusted, "run"
		return state
	case "no", "n", "false":
		state.Decision, state.Source = TrustUntrusted, "run"
		return state
	}
	if policy == TrustAlways {
		state.Decision, state.Source = TrustTrusted, "always"
		return state
	}
	if policy == TrustNever {
		state.Decision, state.Source = TrustUntrusted, "never"
		return state
	}
	if store, err := LoadTrustStore(home); err == nil {
		if dir, entry, found := store.nearest(state.Path); found {
			state.Decision = entry.Decision
			if dir == state.Path {
				state.Source = "saved"
			} else {
				state.Source, state.InheritedFrom = "inherited", dir
			}
			return state
		}
	}
	state.Source = "default"
	state.Pending = true
	return state
}

// TrustNotice is what a frontend tells the user when project instructions were
// withheld. It states what was skipped and how to change the decision, because
// an agent quietly ignoring a repository's AGENTS.md looks like a bug in the
// agent rather than a decision the user did not make.
func (t TrustState) TrustNotice() string {
	if t.Trusted() || t.Resources.Empty() {
		return ""
	}
	var b strings.Builder
	b.WriteString("project instructions are NOT loaded for this directory\n")
	if len(t.Resources.Files) > 0 {
		extra := ""
		if t.Resources.Total > len(t.Resources.Files) {
			extra = fmt.Sprintf(" (and %d more)", t.Resources.Total-len(t.Resources.Files))
		}
		fmt.Fprintf(&b, "  %d file(s) withheld%s:\n", t.Resources.Total, extra)
		for _, f := range t.Resources.Files {
			fmt.Fprintf(&b, "    %s\n", f)
		}
	}
	switch {
	case t.Pending:
		b.WriteString("  /trust to load them, /trust no to decline and stop asking")
	case t.Source == "never":
		b.WriteString("  projectTrust is \"never\" in settings; change it to \"ask\" or \"always\"")
	case t.Source == "run":
		b.WriteString("  declined for this run; /trust yes loads them for the session")
	default:
		b.WriteString("  /trust to load them")
	}
	return strings.TrimRight(b.String(), "\n")
}
