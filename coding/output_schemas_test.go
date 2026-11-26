package coding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/mcp"
)

// checkSchema reports where value departs from schema: a missing required
// field, a field the schema does not declare, or a value of the wrong type.
// It knows the parts of JSON Schema the output schemas use.
func checkSchema(path string, schema map[string]any, value any) []string {
	var errs []string
	if kind, ok := schema["type"].(string); ok && !hasType(kind, value) {
		return []string{fmt.Sprintf("%s: %T is not %s", path, value, kind)}
	}
	if enum, ok := schema["enum"].([]string); ok {
		if s, _ := value.(string); !slices.Contains(enum, s) {
			errs = append(errs, fmt.Sprintf("%s: %q is not one of %v", path, value, enum))
		}
	}
	switch v := value.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]string)
		for _, name := range required {
			if _, ok := v[name]; !ok {
				errs = append(errs, fmt.Sprintf("%s.%s: required but missing", path, name))
			}
		}
		for name, field := range v {
			sub, declared := props[name].(map[string]any)
			if !declared {
				if schema["additionalProperties"] == false {
					errs = append(errs, fmt.Sprintf("%s.%s: not in the schema", path, name))
				}
				continue
			}
			errs = append(errs, checkSchema(path+"."+name, sub, field)...)
		}
	case []any:
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range v {
				errs = append(errs, checkSchema(fmt.Sprintf("%s[%d]", path, i), items, item)...)
			}
		}
	}
	return errs
}

func hasType(kind string, v any) bool {
	switch kind {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	}
	return true
}

// Every built-in tool's details, as a script gets them, match the tool's
// output schema, in each of the shapes a tool returns: the schemas are the
// script's contract, so they must not drift from what the tools send.
func TestOutputSchemasMatchDetails(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("alpha\nbeta\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "dot.png"), img.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := testServer(t)

	jobs := newJobManager(t.TempDir())
	t.Cleanup(jobs.Close)
	opts := ToolOptions{jobs: jobs, SpillDir: t.TempDir()}
	var tools []*agent.Tool
	for _, tool := range CodingTools(cwd, opts) {
		if tool.Name == "fetch" {
			tool = newFetchTool(ToolOptions{Sandbox: networkOn, SpillDir: opts.SpillDir}, loopbackToo)
		}
		tools = append(tools, tool)
	}
	script := NewScriptTool(tools, opts, nil, nil)

	res, isError := runScriptTool(t, script, `
		const out = [];
		const call = async (label, tool, args) => {
			const r = await tools[tool](args);
			// A command's whole output would push this script's own past the
			// display limit; a few bytes keep its type for the check.
			const details = r.details && typeof r.details.output === "string" ? {...r.details, output: r.details.output.slice(0, 8)} : r.details;
			out.push({label, tool, details, isError: r.isError});
			return r;
		};
		await call("read text", "read", {path: "a.txt"});
		await call("read window past the end", "read", {path: "a.txt", offset: 50});
		await call("read image", "read", {path: "dot.png"});
		await call("grep lines", "grep", {pattern: "alpha"});
		await call("grep files", "grep", {pattern: "alpha", output: "files"});
		await call("grep count", "grep", {pattern: "a", output: "count"});
		await call("grep no match", "grep", {pattern: "nowhere-to-be-found"});
		await call("glob", "glob", {pattern: "*.txt"});
		await call("bash", "bash", {command: "echo hi"});
		await call("bash failing", "bash", {command: "echo no >&2; exit 3"});
		await call("bash long output", "bash", {command: "seq 1 20000"});
		const job = await call("bash background", "bash", {command: "echo up; sleep 30", background: true});
		await call("jobs list", "jobs", {action: "list"});
		await call("jobs output", "jobs", {action: "output", id: job.details.job});
		await call("jobs stop", "jobs", {action: "stop", id: job.details.job});
		await call("edit", "edit", {path: "a.txt", edits: [{oldText: "beta", newText: "gamma"}]});
		await call("edit changing nothing", "edit", {path: "a.txt", edits: [{oldText: "alpha", newText: "alpha"}]});
		await call("write", "write", {path: "b.txt", content: "new\n"});
		await call("fetch", "fetch", {url: `+"`"+srv.URL+"/page`"+`});
		await call("fetch long", "fetch", {url: `+"`"+srv.URL+"/big`"+`});
		return JSON.stringify(out);`)
	if isError {
		t.Fatalf("script failed: %s", res.Text())
	}
	var calls []struct {
		Label, Tool string
		Details     any
		IsError     bool
	}
	if err := json.Unmarshal([]byte(res.Text()), &calls); err != nil {
		t.Fatalf("%v: %s", err, res.Text())
	}
	schemas := map[string]map[string]any{}
	for _, tool := range tools {
		if tool.OutputSchema == nil {
			t.Errorf("%s declares no output schema", tool.Name)
		}
		schemas[tool.Name] = tool.OutputSchema
	}
	covered := map[string]bool{}
	for _, c := range calls {
		if c.IsError {
			t.Errorf("%s: the call failed, so its details were not checked", c.Label)
			continue
		}
		covered[c.Tool] = true
		for _, e := range checkSchema(c.Tool, schemas[c.Tool], c.Details) {
			t.Errorf("%s: %s", c.Label, e)
		}
	}
	for name := range schemas {
		if !covered[name] {
			t.Errorf("no call checked %s's details", name)
		}
	}
}

// The checker itself catches each kind of drift it is there for.
func TestCheckSchemaCatchesDrift(t *testing.T) {
	for _, tc := range []struct {
		details map[string]any
		want    string
	}{
		{map[string]any{"path": "a", "characters": 1, "mode": "x"}, "write.mode: not in the schema"},
		{map[string]any{"path": "a"}, "write.characters: required but missing"},
		{map[string]any{"path": "a", "characters": "1"}, "write.characters: string is not integer"},
	} {
		var v any
		b, _ := json.Marshal(tc.details)
		_ = json.Unmarshal(b, &v)
		errs := checkSchema("write", writeOutputSchema, v)
		if !slices.ContainsFunc(errs, func(e string) bool { return strings.Contains(e, tc.want) }) {
			t.Errorf("%v: got %v, want %q", tc.details, errs, tc.want)
		}
	}
}

// An MCP tool's output schema reaches scripts as the schema of its
// structured content, its descriptions labelled as the server's.
func TestMCPToolsPassOnTheirOutputSchema(t *testing.T) {
	m := &MCPManager{}
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"temperature": map[string]any{"type": "number", "description": "Degrees"},
	}}
	tool := m.agentTool("weather", "mcp__weather__now", mcp.Tool{Name: "now", OutputSchema: schema})
	sc, _ := tool.OutputSchema["properties"].(map[string]any)["structuredContent"].(map[string]any)
	field, _ := sc["properties"].(map[string]any)["temperature"].(map[string]any)
	if desc, _ := field["description"].(string); !strings.HasPrefix(desc, "Untrusted MCP schema description") || !strings.HasSuffix(desc, "Degrees") {
		t.Fatalf("structuredContent schema = %v", sc)
	}
	if errs := checkSchema("mcp", tool.OutputSchema, map[string]any{"structuredContent": map[string]any{"temperature": 21.5}, "trust": "untrusted MCP output"}); len(errs) > 0 {
		t.Fatal(errs)
	}
}
