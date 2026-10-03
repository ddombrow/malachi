package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

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
