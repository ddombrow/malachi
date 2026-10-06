package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
)

// The golden tests replay the command scripts in testdata against malachi and
// compare the structure of the output with tau's recording of the same
// scripts (testdata/gen_tau.py). Values that legitimately differ (ids, paths,
// timestamps, model metadata, wording of provider-specific errors) are not
// compared; the sequence of records, their types and outcomes, and the keys
// they carry are.

// extraKeys are keys malachi adds to a response's data, by command.
var extraKeys = map[string][]string{
	"get_state": {"projectTrust", "sandbox"},
	"compact":   {"replaced", "kept", "usage", "warnings"},
}

// malachiOnlyEvents are events tau does not send in these scenarios. They are
// additions a Pi client ignores, filtered before the sequences are compared.
// tau defines thinking_level_changed too, but its RPC mode only forwards
// events while a prompt is streaming; malachi reports it whenever it happens.
var malachiOnlyEvents = map[string]bool{
	"compaction_start": true, "compaction_progress": true, "compaction_end": true,
	"thinking_level_changed": true,
}

// asyncCommands are answered from a goroutine, so a slow one (a compaction, a
// model list fetched over the network) does not hold up abort. Their
// responses can arrive after later commands' responses; clients correlate by
// id. tau answers strictly in order because its read loop blocks on them.
var asyncCommands = map[string]bool{"compact": true, "get_available_models": true}

func goldenScenarios() map[string]func(t *testing.T) *coding.Session {
	reply := func(text string) fake.Script { return fake.Text(text) }
	slow := func(text string, d time.Duration) fake.Script {
		return func(ctx context.Context, _ agent.Request, b *ai.Builder) {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return
			}
			b.Text(text)
			b.Done(agent.StopStop)
		}
	}
	plain := func(scripts ...fake.Script) func(t *testing.T) *coding.Session {
		return func(t *testing.T) *coding.Session { return openSession(t, fake.New(scripts...)) }
	}
	return map[string]func(t *testing.T) *coding.Session{
		"prompt":          plain(reply("hello")),
		"state":           plain(),
		"bad_records":     plain(),
		"streaming":       plain(slow("first", 600*time.Millisecond), reply("second"), reply("third")),
		"inspect":         plain(reply("the answer")),
		"set_model_fails": plain(),
		"compact": func(t *testing.T) *coding.Session {
			s := openSession(t, fake.New(reply("real summary")))
			var history []agent.Message
			for i := 0; i < 15; i++ {
				a := agent.NewAssistantMessage("fake")
				a.Content = []agent.Content{&agent.TextContent{Text: strings.Repeat("y", 8000)}}
				history = append(history, agent.NewUserText(fmt.Sprintf("question-%d-", i)+strings.Repeat("x", 8000)), a)
			}
			s.Harness.ReplaceMessages(history)
			return s
		},
	}
}

func readJSONL(t *testing.T, path string) []record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var r record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, r)
	}
	return out
}

func TestGoldenAgainstTau(t *testing.T) {
	for name, setup := range goldenScenarios() {
		t.Run(name, func(t *testing.T) {
			cmds := readJSONL(t, filepath.Join("testdata", name+".cmds.jsonl"))
			want := readJSONL(t, filepath.Join("testdata", "tau_"+name+".jsonl"))

			c := serve(t, setup(t))
			for _, cmd := range cmds {
				switch {
				case cmd["__wait"] != nil:
					time.Sleep(time.Duration(cmd["__wait"].(float64) * float64(time.Second)))
				case cmd["__raw"] != nil:
					c.send(cmd["__raw"].(string))
				default:
					raw, _ := json.Marshal(cmd)
					c.send(string(raw))
				}
			}
			c.close()
			compareToTau(t, want, c.seen)
		})
	}
}

// normalize collapses runs of message_update (streaming granularity differs)
// and separates queue_update events and asynchronous responses, which are
// compared on their own.
func normalize(rs []record) (main []record, queues []string, async []record) {
	for _, r := range rs {
		typ, _ := r["type"].(string)
		switch {
		case malachiOnlyEvents[typ]:
			continue
		case typ == "response" && asyncCommands[fmt.Sprint(r["command"])]:
			async = append(async, r)
			continue
		case typ == "queue_update":
			queues = append(queues, fmt.Sprint(r["steering"], r["followUp"]))
			continue
		case typ == "message_update" && len(main) > 0 && main[len(main)-1]["type"] == "message_update":
			continue
		}
		main = append(main, r)
	}
	return main, queues, async
}

func signature(r record) string {
	if r["type"] == "response" {
		return fmt.Sprintf("response:%v:%v", r["command"], r["success"])
	}
	return fmt.Sprint(r["type"])
}

func compareToTau(t *testing.T, tauRecs, ours []record) {
	t.Helper()
	tauMain, tauQueues, tauAsync := normalize(tauRecs)
	ourMain, ourQueues, ourAsync := normalize(ours)
	// Asynchronous responses: same set, compared by id, in any order.
	byID := map[string]record{}
	for _, r := range ourAsync {
		byID[fmt.Sprint(r["id"])] = r
	}
	if len(tauAsync) != len(ourAsync) {
		t.Fatalf("%d asynchronous responses, tau %d", len(ourAsync), len(tauAsync))
	}
	for _, tr := range tauAsync {
		or, ok := byID[fmt.Sprint(tr["id"])]
		if !ok {
			t.Fatalf("no response for %v %v", tr["command"], tr["id"])
		}
		tauMain = append(tauMain, tr)
		ourMain = append(ourMain, or)
	}

	var tauSigs, ourSigs []string
	for _, r := range tauMain {
		tauSigs = append(tauSigs, signature(r))
	}
	for _, r := range ourMain {
		ourSigs = append(ourSigs, signature(r))
	}
	if !slices.Equal(tauSigs, ourSigs) {
		t.Fatalf("record sequence differs from tau\n tau: %v\nours: %v", tauSigs, ourSigs)
	}
	// tau's queue reports must appear, in order; malachi also reports the
	// queue draining, which tau does not.
	for i, j := 0, 0; i < len(tauQueues); j++ {
		if j == len(ourQueues) {
			t.Fatalf("queue reports %v missing from ours %v", tauQueues, ourQueues)
		}
		if ourQueues[j] == tauQueues[i] {
			i++
		}
	}

	for i := range tauMain {
		tr, or := tauMain[i], ourMain[i]
		where := fmt.Sprintf("record %d (%s)", i, signature(tr))
		compareKeys(t, where, keysOf(tr), keysOf(or), nil)
		if tr["type"] != "response" {
			continue
		}
		if fmt.Sprint(tr["id"]) != fmt.Sprint(or["id"]) {
			t.Errorf("%s: id %v, tau %v", where, or["id"], tr["id"])
		}
		compareError(t, where, tr, or)
		cmd := fmt.Sprint(tr["command"])
		td, tok := tr["data"].(map[string]any)
		od, ook := or["data"].(map[string]any)
		if tok != ook {
			t.Errorf("%s: data object present=%v, tau %v", where, ook, tok)
			continue
		}
		if !tok {
			continue
		}
		compareKeys(t, where+" data", sortedKeys(td), sortedKeys(od), extraKeys[cmd])
		// Lists of objects (models, messages) compare their first element.
		for k, tv := range td {
			tl, ok1 := tv.([]any)
			ol, ok2 := od[k].([]any)
			if ok1 && ok2 && len(tl) > 0 && len(ol) > 0 {
				if tm, ok := tl[0].(map[string]any); ok {
					if om, ok := ol[0].(map[string]any); ok {
						compareKeys(t, where+" data."+k+"[0]", sortedKeys(tm), sortedKeys(om), nil)
					}
				}
			}
			if tm, ok := tv.(map[string]any); ok {
				if om, ok := od[k].(map[string]any); ok {
					compareKeys(t, where+" data."+k, sortedKeys(tm), sortedKeys(om), nil)
				}
			}
		}
	}
}

// compareError requires the same error text, except where it is the JSON
// parser's own message or names provider configuration that differs between
// the two programs by design.
func compareError(t *testing.T, where string, tr, or record) {
	t.Helper()
	te, _ := tr["error"].(string)
	oe, _ := or["error"].(string)
	const parsePrefix = "Failed to parse command: "
	switch {
	case strings.HasPrefix(te, parsePrefix):
		if !strings.HasPrefix(oe, parsePrefix) {
			t.Errorf("%s: error %q, want prefix %q", where, oe, parsePrefix)
		}
	case tr["command"] == "set_model":
		if (te == "") != (oe == "") {
			t.Errorf("%s: error %q, tau %q", where, oe, te)
		}
	case te != oe:
		t.Errorf("%s: error %q, tau %q", where, oe, te)
	}
}

func keysOf(r record) []string { return sortedKeys(r) }

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// compareKeys requires every tau key, and allows only the listed extras.
func compareKeys(t *testing.T, where string, tau, ours, allowedExtra []string) {
	t.Helper()
	for _, k := range tau {
		if !slices.Contains(ours, k) {
			t.Errorf("%s: missing key %q (ours %v)", where, k, ours)
		}
	}
	for _, k := range ours {
		if !slices.Contains(tau, k) && !slices.Contains(allowedExtra, k) {
			t.Errorf("%s: extra key %q not in tau's output (tau %v)", where, k, tau)
		}
	}
}
