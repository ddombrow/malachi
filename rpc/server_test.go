package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
	"github.com/ddombrow/malachi/sandbox"
)

func TestMain(m *testing.M) {
	sandbox.MaybeRunHelper() // this binary is the helper for sandboxed commands on Linux
	// Sessions opened without an explicit Home land in a throwaway directory,
	// never the developer's ~/.malachi.
	home, err := os.MkdirTemp("", "malachi-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("MALACHI_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

type record map[string]any

// conn is a running server with a client on the other end of two pipes.
type conn struct {
	t    *testing.T
	sv   *Server
	in   *io.PipeWriter
	recs chan record
	done chan error
	seen []record
}

func serve(t *testing.T, s *coding.Session) *conn {
	t.Helper()
	return serveCtx(t, s, context.Background())
}

func serveCtx(t *testing.T, s *coding.Session, ctx context.Context) *conn {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &conn{t: t, sv: New(s, inR, outW), in: inW, recs: make(chan record, 4096), done: make(chan error, 1)}
	go func() {
		c.done <- c.sv.Run(ctx)
		outW.Close()
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var r record
			if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
				t.Errorf("server wrote a line that is not JSON: %q", sc.Text())
				continue
			}
			c.recs <- r
		}
		close(c.recs)
	}()
	t.Cleanup(func() { c.in.Close() })
	return c
}

func (c *conn) send(lines ...string) {
	c.t.Helper()
	for _, l := range lines {
		if _, err := io.WriteString(c.in, l+"\n"); err != nil {
			c.t.Fatal(err)
		}
	}
}

// next returns the next record matching want, failing after a timeout.
func (c *conn) next(what string, want func(record) bool) record {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case r, ok := <-c.recs:
			if !ok {
				c.t.Fatalf("stream ended waiting for %s; saw %v", what, types(c.seen))
			}
			c.seen = append(c.seen, r)
			if want(r) {
				return r
			}
		case <-timeout:
			c.t.Fatalf("timed out waiting for %s; saw %v", what, types(c.seen))
		}
	}
}

func (c *conn) response(id any) record {
	c.t.Helper()
	return c.next(fmt.Sprint("response ", id), func(r record) bool {
		return r["type"] == "response" && fmt.Sprint(r["id"]) == fmt.Sprint(id)
	})
}

func (c *conn) event(typ string) record {
	c.t.Helper()
	return c.next(typ, func(r record) bool { return r["type"] == typ })
}

// close ends the input, as a client exiting does, and returns everything
// written after what was already read.
func (c *conn) close() []record {
	c.t.Helper()
	c.in.Close()
	var rest []record
	timeout := time.After(10 * time.Second)
	for {
		select {
		case r, ok := <-c.recs:
			if !ok {
				if err := <-c.done; err != nil {
					c.t.Fatal(err)
				}
				return rest
			}
			c.seen = append(c.seen, r)
			rest = append(rest, r)
		case <-timeout:
			c.t.Fatal("server did not exit after its input closed")
		}
	}
}

func types(rs []record) []string {
	var out []string
	for _, r := range rs {
		t, _ := r["type"].(string)
		if t == "response" {
			t += ":" + fmt.Sprint(r["command"])
		}
		out = append(out, t)
	}
	return out
}

func openSession(t *testing.T, p agent.Provider) *coding.Session {
	t.Helper()
	s, err := coding.Open(coding.Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &coding.Settings{}, Provider: p, NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func blocking(release <-chan struct{}, text string) fake.Script {
	return func(ctx context.Context, _ agent.Request, b *ai.Builder) {
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		b.Text(text)
		b.Done(agent.StopStop)
	}
}

func TestPromptResponsePrecedesItsEvents(t *testing.T) {
	c := serve(t, openSession(t, fake.New(fake.Text("hello"))))
	c.send(`{"id":"one","type":"prompt","message":"hi"}`)
	first := c.next("first record", func(record) bool { return true })
	if first["type"] != "response" || first["id"] != "one" || first["success"] != true || first["command"] != "prompt" {
		t.Fatalf("first record should be the prompt's response: %v", first)
	}
	end := c.event("agent_end")
	if end["willRetry"] != false {
		t.Fatalf("agent_end should carry willRetry like tau's: %v", end)
	}
	c.event("agent_settled")
	c.close()
}

func TestStreamingRequiresABehavior(t *testing.T) {
	release := make(chan struct{})
	s := openSession(t, fake.New(blocking(release, "first"), fake.Text("second"), fake.Text("third")))
	c := serve(t, s)
	c.send(`{"id":"p","type":"prompt","message":"start"}`)
	c.event("agent_start")
	c.send(`{"id":"again","type":"prompt","message":"no behavior"}`)
	if r := c.response("again"); r["success"] != false || r["error"] != "Agent is already streaming; set streamingBehavior to steer or followUp" {
		t.Fatalf("prompt while streaming: %v", r)
	}
	c.send(`{"id":"s","type":"steer","message":"steer this"}`)
	c.response("s")
	q := c.event("queue_update")
	if fmt.Sprint(q["steering"]) != "[steer this]" {
		t.Fatalf("queue: %v", q)
	}
	c.send(`{"id":"f","type":"prompt","message":"after","streamingBehavior":"followUp"}`)
	c.response("f")
	c.send(`{"id":"bad","type":"prompt","message":"x","streamingBehavior":"later"}`)
	if r := c.response("bad"); r["success"] != false {
		t.Fatalf("unknown streamingBehavior accepted: %v", r)
	}
	close(release)
	c.event("agent_settled")
	var users []string
	for _, m := range s.Harness.Messages() {
		if u, ok := m.(*agent.UserMessage); ok {
			users = append(users, u.Content.String())
		}
	}
	if strings.Join(users, "|") != "start|steer this|after" {
		t.Fatalf("user turns: %v", users)
	}
	c.close()
}

func TestBadRecordsAreAnsweredAndReadingContinues(t *testing.T) {
	c := serve(t, openSession(t, fake.New()))
	c.send("not-json", "[1]", `{"id":1}`, `{"id":2,"type":"nope"}`, `{"id":3,"type":"get_state"}`)
	want := []struct {
		id  any
		cmd string
		ok  bool
		err string
	}{
		{nil, "parse", false, "Failed to parse command: "},
		{nil, "parse", false, "Command must be a JSON object"},
		{1.0, "parse", false, "Command requires a string 'type'"},
		{2.0, "nope", false, "Unknown command: nope"},
		{3.0, "get_state", true, ""},
	}
	for i, w := range want {
		r := c.next(fmt.Sprint("record ", i), func(r record) bool { return r["type"] == "response" })
		if r["id"] != w.id || r["command"] != w.cmd || r["success"] != w.ok || !strings.HasPrefix(fmt.Sprint(r["error"]), w.err) && w.err != "" {
			t.Errorf("record %d = %v, want id=%v command=%s success=%v error=%q…", i, r, w.id, w.cmd, w.ok, w.err)
		}
	}
	c.close()
}

func TestFramingSplitsOnlyOnLFAndAcceptsCRLF(t *testing.T) {
	c := serve(t, openSession(t, fake.New()))
	id := "a b"
	raw, _ := json.Marshal(map[string]string{"id": id, "type": "get_state"})
	c.send(string(raw) + "\r")
	if r := c.response(id); r["success"] != true {
		t.Fatalf("CRLF record: %v", r)
	}
	c.close()
}

func TestOversizedRecordIsRejectedAndReadingContinues(t *testing.T) {
	c := serve(t, openSession(t, fake.New()))
	// 16 MiB fills the pipe, so write from a goroutine and report back here:
	// t.Fatal belongs to the test's own goroutine.
	written := make(chan error, 1)
	go func() {
		_, err := io.WriteString(c.in, `{"id":"huge","type":"get_state","pad":"`+strings.Repeat("x", maxRecordBytes)+"\"}\n"+`{"id":"after","type":"get_state"}`+"\n")
		written <- err
	}()
	r := c.next("the size error", func(r record) bool { return r["type"] == "response" })
	if r["success"] != false || !strings.Contains(fmt.Sprint(r["error"]), "16 MiB") {
		t.Fatalf("oversized record: %v", r)
	}
	if r := c.response("after"); r["success"] != true {
		t.Fatalf("record after the oversized one: %v", r)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	c.close()
}

func TestEOFStopsARunningPromptCleanly(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := openSession(t, fake.New(blocking(release, "never")))
	c := serve(t, s)
	c.send(`{"id":"p","type":"prompt","message":"long job"}`)
	c.event("agent_start")
	rest := c.close()
	got := types(rest)
	if len(got) == 0 || got[len(got)-1] != "agent_settled" {
		t.Fatalf("the run's last events were not delivered before exit: %v", got)
	}
	if st := s.State(); st.Running || st.Compacting {
		t.Fatalf("session still busy after exit: %+v", st)
	}
}

func TestNewSessionMovesTheEventStream(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	// new_session builds its provider from settings, as a real run does, so
	// configure one that needs no key; it is never called.
	settings := &coding.Settings{DefaultProvider: "local", Providers: map[string]coding.ProviderConfig{
		"local": {BaseURL: "http://127.0.0.1:1", DefaultModel: "m"},
	}}
	s, err := coding.Open(coding.Options{Cwd: cwd, Home: home, Settings: settings, Provider: fake.New(fake.Text("one"))})
	if err != nil {
		t.Fatal(err)
	}
	c := serve(t, s)
	c.send(`{"id":1,"type":"prompt","message":"first"}`)
	c.event("agent_settled")
	first := c.sv.Session().Path()

	c.send(`{"id":2,"type":"new_session"}`)
	if r := c.response(2); r["success"] != true || fmt.Sprint(r["data"]) != "map[cancelled:false]" {
		t.Fatalf("new_session: %v", r)
	}
	if c.sv.Session() == s || c.sv.Session().Path() == first {
		t.Fatal("new_session did not replace the session")
	}
	c.send(`{"id":3,"type":"get_state"}`)
	if r := c.response(3); r["data"].(map[string]any)["messageCount"] != 0.0 {
		t.Fatalf("new session should start empty: %v", r)
	}

	c.send(fmt.Sprintf(`{"id":4,"type":"switch_session","sessionPath":%q}`, first))
	c.response(4)
	c.send(`{"id":5,"type":"get_messages"}`)
	msgs := c.response(5)["data"].(map[string]any)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("switching back should restore the conversation, got %d messages", len(msgs))
	}
	c.close()
}

func TestSetTrustChangesState(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("project rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := coding.Open(coding.Options{Cwd: cwd, Home: home, Settings: &coding.Settings{}, Provider: fake.New(), NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	c := serve(t, s)
	c.send(`{"id":1,"type":"get_state"}`)
	trust := c.response(1)["data"].(map[string]any)["projectTrust"].(map[string]any)
	if trust["decision"] != "untrusted" || trust["pending"] != true || trust["total"] != 1.0 {
		t.Fatalf("an undecided project should be withheld and pending: %v", trust)
	}
	if strings.Contains(fmt.Sprint(trust), "project rules") {
		t.Fatal("trust state must not carry file contents")
	}
	c.send(`{"id":2,"type":"set_trust","decision":"trusted"}`)
	if r := c.response(2); r["data"].(map[string]any)["decision"] != "trusted" {
		t.Fatalf("set_trust: %v", r)
	}
	if !strings.Contains(s.Harness.Config().System, "project rules") {
		t.Fatal("trusting the project did not load its instructions")
	}
	c.send(`{"id":3,"type":"set_trust","decision":"maybe"}`)
	if r := c.response(3); r["success"] != false {
		t.Fatalf("invalid decision accepted: %v", r)
	}
	c.close()
}

func TestSetModelToUnknownProviderChangesNothing(t *testing.T) {
	s := openSession(t, fake.New())
	before := s.Provider().Name + "/" + s.Model()
	c := serve(t, s)
	c.send(`{"id":"model","type":"set_model","provider":"other","modelId":"missing"}`)
	if r := c.response("model"); r["success"] != false {
		t.Fatalf("set_model to an unknown provider: %v", r)
	}
	if after := s.Provider().Name + "/" + s.Model(); after != before {
		t.Fatalf("model changed to %s", after)
	}
	c.close()
}

func TestCompactAnswersWithTheResultAndReportsEvents(t *testing.T) {
	summary := "## Goal\nFix the parser in internal/scan.go.\n\n## Next Steps\nRun go test ./internal/... and wire the new case into scan_test.go."
	s := openSession(t, fake.New(fake.Text(summary)))
	var history []agent.Message
	for i := 0; i < 15; i++ {
		a := agent.NewAssistantMessage("m")
		a.Content = []agent.Content{&agent.TextContent{Text: "patched internal/scan.go"}}
		history = append(history, agent.NewUserText("fix internal/scan.go"), a)
	}
	s.Harness.ReplaceMessages(history)
	c := serve(t, s)
	c.send(`{"id":"compact","type":"compact"}`)
	r := c.response("compact")
	// The compaction's events come before the response that reports it.
	var before []string
	for _, rec := range c.seen[:len(c.seen)-1] {
		before = append(before, fmt.Sprint(rec["type"]))
	}
	if len(before) < 2 || before[0] != "compaction_start" || before[len(before)-1] != "compaction_end" {
		t.Fatalf("records before the compact response: %v", before)
	}
	data := r["data"].(map[string]any)
	for _, k := range []string{"summary", "firstKeptEntryId", "tokensBefore", "estimatedTokensAfter", "details"} {
		if _, ok := data[k]; !ok {
			t.Errorf("compact response missing %q: %v", k, data)
		}
	}
	if data["summary"] != summary {
		t.Fatalf("summary = %v", data["summary"])
	}
	c.close()
}

// drain reads everything left until the server closes its output, and
// returns Run's result.
func (c *conn) drain() ([]record, error) {
	c.t.Helper()
	var rest []record
	timeout := time.After(10 * time.Second)
	for {
		select {
		case r, ok := <-c.recs:
			if !ok {
				return rest, <-c.done
			}
			c.seen = append(c.seen, r)
			rest = append(rest, r)
		case <-timeout:
			c.t.Fatal("server did not stop")
		}
	}
}

// A record is exactly one JSON value. Trailing garbage or a second object
// makes it malformed, as a full parse (tau's json.loads) would.
func TestTrailingDataMakesARecordMalformed(t *testing.T) {
	c := serve(t, openSession(t, fake.New()))
	c.send(`{"id":1,"type":"get_state"} garbage`,
		`{"id":2,"type":"get_state"}{"id":3,"type":"get_state"}`,
		`{"id":4,"type":"get_state"}   `)
	for i := 0; i < 2; i++ {
		r := c.next("a parse error", func(r record) bool { return r["type"] == "response" })
		if r["success"] != false || r["command"] != "parse" || r["id"] != nil {
			t.Fatalf("record with trailing data was not rejected: %v", r)
		}
	}
	if r := c.response(4); r["success"] != true {
		t.Fatalf("trailing whitespace should be fine: %v", r)
	}
	rest := c.close()
	for _, r := range append(c.seen, rest...) {
		if id := fmt.Sprint(r["id"]); id == "1" || id == "2" || id == "3" {
			t.Fatalf("a malformed record was dispatched: %v", r)
		}
	}
}

// Cancelling Run's context ends it even while the input is open and idle,
// with the same cleanup as EOF: work stopped, events delivered.
func TestCancelEndsAnIdleServerCleanly(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := openSession(t, fake.New(blocking(release, "never")))
	ctx, cancel := context.WithCancel(context.Background())
	c := serveCtx(t, s, ctx)
	c.send(`{"id":"p","type":"prompt","message":"long job"}`)
	c.event("agent_start")

	cancel() // the input stays open
	rest, err := c.drain()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if got := types(rest); len(got) == 0 || got[len(got)-1] != "agent_settled" {
		t.Fatalf("the run's last events were not delivered: %v", got)
	}
	if st := s.State(); st.Running || st.Compacting {
		t.Fatalf("session still busy: %+v", st)
	}
}

// A failed read goes through the same shutdown as EOF, then is returned.
func TestReadErrorStillCleansUp(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := openSession(t, fake.New(blocking(release, "never")))
	c := serve(t, s)
	c.send(`{"id":"p","type":"prompt","message":"long job"}`)
	c.event("agent_start")

	boom := errors.New("disk read failed")
	c.in.CloseWithError(boom)
	rest, err := c.drain()
	if !errors.Is(err, boom) {
		t.Fatalf("Run returned %v, want the read error", err)
	}
	if got := types(rest); len(got) == 0 || got[len(got)-1] != "agent_settled" {
		t.Fatalf("the run's last events were not delivered: %v", got)
	}
	if st := s.State(); st.Running || st.Compacting {
		t.Fatalf("session still busy: %+v", st)
	}
}

// get_state reports the sandbox, a malachi extension.
func TestGetStateReportsTheSandbox(t *testing.T) {
	c := serve(t, openSession(t, fake.New()))
	c.send(`{"id":"s","type":"get_state"}`)
	r := c.response("s")
	sb, ok := r["data"].(map[string]any)["sandbox"].(map[string]any)
	if !ok {
		t.Fatalf("no sandbox in get_state: %v", r)
	}
	if sb["enabled"] != true || sb["network"] != false {
		t.Fatalf("sandbox: %v", sb)
	}
	if roots, _ := sb["writableRoots"].([]any); len(roots) == 0 {
		t.Fatalf("no writable roots: %v", sb)
	}
	if hidden, _ := sb["hiddenPaths"].([]any); len(hidden) == 0 {
		t.Fatalf("no hidden paths: %v", sb)
	}
	if avail := sandbox.Available(); (avail == nil) != (sb["available"] == true) {
		t.Fatalf("available=%v but Available() = %v", sb["available"], avail)
	}
	c.close()
}

func TestSetNetwork(t *testing.T) {
	s := openSession(t, fake.New())
	c := serve(t, s)
	c.send(`{"id":"n","type":"set_network","enabled":true}`)
	r := c.response("n")
	if r["success"] != true || r["data"].(map[string]any)["network"] != true || !s.Sandbox().Network {
		t.Fatalf("set_network: %v", r)
	}
	c.send(`{"id":"bad","type":"set_network","enabled":"no"}`)
	if r := c.response("bad"); r["success"] != false {
		t.Fatalf("non-boolean accepted: %v", r)
	}
	c.close()
}
