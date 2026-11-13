package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
)

// tau_session.jsonl is written by tau's own entry_to_json_line.
func TestGoldenTauSessionRoundTrip(t *testing.T) {
	f, err := os.Open("testdata/tau_session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("decode %s: %v", sc.Bytes(), err)
		}
		out, _ := json.Marshal(&e)
		var want, got any
		_ = json.Unmarshal(sc.Bytes(), &want)
		_ = json.Unmarshal(out, &got)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("mismatch\nwant %s\n got %s", sc.Bytes(), out)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAndReplayTauSession(t *testing.T) {
	f, err := Load("testdata/tau_session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// Tip skips the legacy leaf entry: e10 (branch summary off e3).
	if f.TipID() != "e10" {
		t.Fatalf("tip %s", f.TipID())
	}
	st := Replay(BranchPath(f.Entries(), "e9"))
	if st.Model != "kimi-k2.7-code" || st.Provider != "opencode-go" || st.ThinkingLevel != "medium" || st.Title != "demo" {
		t.Fatalf("state: %+v", st)
	}
	// Compaction at e9 keeps from e5: summary, toolResult(e5), custom message(e8).
	if len(st.Messages) != 3 || st.Messages[0].Role() != "compactionSummary" || st.Messages[1].Role() != "toolResult" || st.Messages[2].Role() != "custom" {
		roles := []string{}
		for _, m := range st.Messages {
			roles = append(roles, m.Role())
		}
		t.Fatalf("roles: %v", roles)
	}
	branch := Replay(BranchPath(f.Entries(), "e10"))
	if len(branch.Messages) != 2 || branch.Messages[1].Role() != "branchSummary" {
		t.Fatalf("branch: %d", len(branch.Messages))
	}
}

func TestFileLazyCreateAndAppendChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s", "x.jsonl")
	f := NewFile(path)
	if f.Exists() {
		t.Fatal("created too early")
	}
	info := NewSessionInfo("/tmp", "")
	_ = f.Append(info)
	msg := NewMessageEntry(agent.NewUserText("hi"))
	_ = f.Append(msg)
	if msg.ParentID != info.ID {
		t.Fatal("append must parent on the tip")
	}
	g, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st := Replay(BranchPath(g.Entries(), g.TipID()))
	if len(st.Messages) != 1 || agent.MessageText(st.Messages[0]) != "hi" || st.Cwd != "/tmp" {
		t.Fatalf("replay: %+v", st)
	}
	infos, _ := List(filepath.Dir(path))
	if len(infos) != 1 || infos[0].Preview != "hi" {
		t.Fatalf("list: %+v", infos)
	}
}

func TestReplayReportsEntryIDsPerMessage(t *testing.T) {
	f, err := Load("testdata/tau_session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	st := Replay(BranchPath(f.Entries(), "e9"))
	if len(st.EntryIDs) != len(st.Messages) {
		t.Fatalf("%d ids for %d messages", len(st.EntryIDs), len(st.Messages))
	}
	// The compaction summary is synthesized; the rest came from entries.
	if want := []string{"", "e5", "e8"}; strings.Join(st.EntryIDs, ",") != strings.Join(want, ",") {
		t.Fatalf("entry ids = %v, want %v", st.EntryIDs, want)
	}
}

func TestLatestIsTheNewestSessionThatLoads(t *testing.T) {
	dir := t.TempDir()
	if _, err := Latest(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty directory: err = %v, want fs.ErrNotExist", err)
	}
	now := time.Now()
	write := func(name, body string, age time.Duration) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	entry := `{"id":"a","parent_id":null,"timestamp":1,"type":"session_info","cwd":"/x"}` + "\n"
	write("old.jsonl", entry, 3*time.Hour)
	newest := write("new.jsonl", entry, 2*time.Hour)
	write("broken.jsonl", "{not json\n", time.Hour)
	write("notes.txt", "not a session", 0)

	got, err := Latest(dir)
	if err != nil || got != newest {
		t.Fatalf("Latest = %q, %v; want %q (the newest that loads)", got, err, newest)
	}
	infos, err := List(dir)
	if err != nil || len(infos) == 0 || infos[0].Path != got {
		t.Fatalf("Latest must agree with List's first entry: %v, %v", infos, err)
	}
}
