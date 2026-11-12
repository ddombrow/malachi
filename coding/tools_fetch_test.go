package coding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/sandbox"
)

var networkOn = func() sandbox.Policy { return sandbox.Policy{Enabled: true, Network: true} }

// loopbackToo lets tests reach their own servers; every other rule holds.
func loopbackToo(a netip.Addr) bool { return a.Unmap().IsLoopback() || publicAddr(a) }

func fetchVia(t *testing.T, tool *agent.Tool, url string) (agent.ToolResult, bool) {
	t.Helper()
	call := &agent.ToolCall{ID: "f", Name: "fetch", Arguments: map[string]any{"url": url}}
	return agent.ExecuteTool(context.Background(), tool, call, func(agent.ToolResult) {})
}

func testServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!doctype html><html><head><title>Docs  page</title><style>p{}</style>
			<script>alert("x")</script></head><body><nav hidden>menu</nav>
			<h1>Install</h1><p>Run   the <a href="https://example.com/dl">installer</a> and
			<code>go</code>.</p><ul><li>one</li><li>two</li></ul><pre>a  b
c</pre><div style="display: none">secret</div></body></html>`))
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok": true}`))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0, 1, 2, 0xff})
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such page", http.StatusNotFound)
	})
	mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/to-page", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/page", http.StatusFound)
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		for i := range 5000 {
			w.Write([]byte(strings.Repeat("x", 20) + " " + string(rune('a'+i%26)) + "\n"))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchHTMLAsText(t *testing.T) {
	srv := testServer(t)
	tool := newFetchTool(ToolOptions{Sandbox: networkOn}, loopbackToo)
	res, isError := fetchVia(t, tool, srv.URL+"/page")
	if isError {
		t.Fatal(res.Text())
	}
	want := srv.URL + "/page 200\n\nTitle: Docs page\n\n# Install\n\nRun the [installer](https://example.com/dl) and `go`.\n\n- one\n- two\n\n```\na  b\nc\n```"
	if res.Text() != want {
		t.Errorf("got\n%s\nwant\n%s", res.Text(), want)
	}
	for _, leaked := range []string{"alert", "menu", "secret", "p{}"} {
		if strings.Contains(res.Text(), leaked) {
			t.Errorf("%q leaked into the text", leaked)
		}
	}
	d := res.Details.(FetchDetails)
	if d.Method != "GET" || d.Status != 200 || !strings.HasPrefix(d.ContentType, "text/html") {
		t.Errorf("details %+v", d)
	}
}

func TestFetchContentTypes(t *testing.T) {
	srv := testServer(t)
	tool := newFetchTool(ToolOptions{Sandbox: networkOn}, loopbackToo)
	if res, isError := fetchVia(t, tool, srv.URL+"/json"); isError || !strings.HasSuffix(res.Text(), "\n\n{\"ok\": true}") {
		t.Errorf("json: %v %q", isError, res.Text())
	}
	if res, isError := fetchVia(t, tool, srv.URL+"/bin"); !isError || !strings.Contains(res.Text(), "not text") {
		t.Errorf("binary: %v %q", isError, res.Text())
	}
	res, isError := fetchVia(t, tool, srv.URL+"/missing")
	if !isError || !strings.Contains(res.Text(), " 404\n\nno such page") || res.Details.(FetchDetails).Status != 404 {
		t.Errorf("404: %v %q", isError, res.Text())
	}
	res, isError = fetchVia(t, tool, srv.URL+"/to-page")
	if isError || !strings.HasPrefix(res.Text(), srv.URL+"/to-page → "+srv.URL+"/page 200") || len(res.Details.(FetchDetails).Redirects) != 1 {
		t.Errorf("redirect: %v %q", isError, res.Text())
	}
}

// The user's network toggle governs fetch: off, it is refused as the
// sandbox refusing it, which the diagnostics log records.
func TestFetchRefusedWithTheNetworkOff(t *testing.T) {
	srv := testServer(t)
	off := func() sandbox.Policy { return sandbox.Policy{Enabled: true} }
	res, isError := fetchVia(t, newFetchTool(ToolOptions{Sandbox: off}, loopbackToo), srv.URL+"/json")
	if !isError || !strings.Contains(res.Text(), sandbox.ErrDenied.Error()) || !strings.Contains(res.Text(), "network is off") {
		t.Fatalf("%v %q", isError, res.Text())
	}
	if source, _ := sandboxDenialOf(res, isError); source != "policy" {
		t.Errorf("not recognized as a sandbox denial: %q", source)
	}
	// With the sandbox off, nothing is confined, fetch included.
	if res, isError := fetchVia(t, newFetchTool(ToolOptions{}, loopbackToo), srv.URL+"/json"); isError {
		t.Errorf("sandbox off: %q", res.Text())
	}
}

// Local addresses are refused where the connection is made, so neither a
// URL, a DNS name, nor a redirect reaches them.
func TestFetchRefusesLocalAddresses(t *testing.T) {
	srv := testServer(t)
	res, isError := fetchVia(t, NewFetchTool(ToolOptions{Sandbox: networkOn}), srv.URL+"/json")
	if !isError || !strings.Contains(res.Text(), "does not connect to loopback") {
		t.Errorf("loopback: %v %q", isError, res.Text())
	}
	res, isError = fetchVia(t, newFetchTool(ToolOptions{Sandbox: networkOn}, loopbackToo), srv.URL+"/to-metadata")
	if !isError || !strings.Contains(res.Text(), "169.254.169.254") || !strings.Contains(res.Text(), "does not connect") {
		t.Errorf("redirect to metadata: %v %q", isError, res.Text())
	}
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fd00::1": false, "fe80::1": false, "::ffff:127.0.0.1": false, "224.0.0.1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("publicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestFetchURLAndHostRules(t *testing.T) {
	srv := testServer(t)
	tool := newFetchTool(ToolOptions{Sandbox: networkOn, FetchHosts: []string{"docs.example.com", "*.go.dev"}}, loopbackToo)
	for url, want := range map[string]string{
		"file:///etc/passwd":            "takes http:// and https://",
		"https://user:pw@go.dev/":       "credentials",
		srv.URL + "/json":               "not in the hosts fetch may reach",
		"https://evil.example.com/":     "not in the hosts fetch may reach",
		"https://go.dev.evil.com/":      "not in the hosts fetch may reach",
		"ftp://docs.example.com/readme": "takes http:// and https://",
	} {
		if res, isError := fetchVia(t, tool, url); !isError || !strings.Contains(res.Text(), want) {
			t.Errorf("%s: %v %q", url, isError, res.Text())
		}
	}
	for host, want := range map[string]bool{
		"docs.example.com": true, "DOCS.example.com.": true, "pkg.go.dev": true, "go.dev": false, "example.com": false,
	} {
		if got := hostAllowed([]string{"docs.example.com", "*.go.dev"}, host); got != want {
			t.Errorf("hostAllowed(%s) = %v", host, got)
		}
	}
	// A redirect is held to the same list.
	open := newFetchTool(ToolOptions{Sandbox: networkOn, FetchHosts: []string{"127.0.0.1"}}, loopbackToo)
	if res, isError := fetchVia(t, open, srv.URL+"/to-metadata"); !isError || !strings.Contains(res.Text(), "redirect to http://169.254.169.254") {
		t.Errorf("redirect off the list: %v %q", isError, res.Text())
	}
}

func TestFetchSpillsLongText(t *testing.T) {
	srv := testServer(t)
	spill := t.TempDir()
	res, isError := fetchVia(t, newFetchTool(ToolOptions{Sandbox: networkOn, SpillDir: spill}, loopbackToo), srv.URL+"/big")
	if isError {
		t.Fatal(res.Text())
	}
	d := res.Details.(FetchDetails)
	if d.SpillPath == "" || !strings.Contains(res.Text(), "Full text: "+d.SpillPath) {
		t.Fatalf("no spill: %q", res.Text()[max(0, len(res.Text())-200):])
	}
	full, err := os.ReadFile(d.SpillPath)
	if err != nil || len(full) != 5000*23 {
		t.Errorf("spill %d bytes, %v", len(full), err)
	}
}

// Scripts fetch through the same tool, several at once.
func TestFetchFromScript(t *testing.T) {
	srv := testServer(t)
	fetch := newFetchTool(ToolOptions{Sandbox: networkOn}, loopbackToo)
	tool := NewScriptTool([]*agent.Tool{fetch}, ToolOptions{}, nil)
	res, isError := runScriptTool(t, tool, `
		const [a, b] = await Promise.all([tools.fetch({url: "`+srv.URL+`/json"}), tools.fetch({url: "`+srv.URL+`/missing"})]);
		return [a.text.split("\n\n")[1], b.isError, b.details.status];`)
	if isError || res.Text() != "[\n  \"{\\\"ok\\\": true}\",\n  true,\n  404\n]" {
		t.Errorf("%v %q", isError, res.Text())
	}
}

// A page's marked main content is kept and its frame dropped; a page
// without one loses its navigation, header and footer.
func TestHTMLMainContent(t *testing.T) {
	for _, c := range []struct{ name, html, want string }{
		{"main", `<header>Site</header><nav><ul><li><a href="/a">A</a></li></ul></nav>
			<main><h1>Doc</h1><p>Body.</p></main><footer>(c)</footer>`, "# Doc\n\nBody."},
		{"role main", `<div role="navigation">menu</div><div role="main"><p>Body.</p></div>`, "Body."},
		{"one article", `<nav>menu</nav><article><p>Story.</p></article><aside>ads</aside>`, "Story."},
		{"no landmark", `<header>Site</header><div role="banner">b</div><nav>menu</nav><p>Body.</p><footer>(c)</footer>`, "Body."},
		{"blocks in list items", `<main><ul><li><div><a href="/x">X</a></div><div>About x</div></li><li><p>Y</p></li></ul></main>`,
			"- [X](/x) About x\n- Y"},
		{"links resolved", `<main><a href="/doc/go1.27">notes</a> <a href="../up">up</a></main>`,
			"[notes](https://go.dev/doc/go1.27) [up](https://go.dev/doc/up)"},
	} {
		base, _ := url.Parse("https://go.dev/doc/devel/release")
		if c.name != "links resolved" {
			base = nil
		}
		if got := htmlText([]byte(c.html), base); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
