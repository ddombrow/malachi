package coding

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// MCPConfigFile is where MCP servers are configured, under malachi's home:
// the only place, so a repository cannot add servers. The home is hidden
// from the sandbox, so no tool reads or changes it.
const MCPConfigFile = "mcp.json"

// MCPServerConfig is one server in mcp.json, in the common mcpServers shape
// plus malachi's roots and tool allowlist.
type MCPServerConfig struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// Env adds variables to the minimal environment servers get. A value
	// may name malachi's own variables as $NAME or ${NAME}.
	Env map[string]string `json:"env,omitempty"`
	// Readable and Writable are roots for this server's process only, never
	// for bash or the file tools. Paths may start with ~.
	Readable []string `json:"readable,omitempty"`
	Writable []string `json:"writable,omitempty"`
	// Tools, when set, are the only tools of the server's offered.
	Tools []string `json:"tools,omitempty"`
	// Deferred false offers the server's tools directly rather than through
	// Code Mode; nil means deferred.
	Deferred *bool `json:"deferred,omitempty"`
	Disabled bool  `json:"disabled,omitempty"`

	// Type and URL belong to remote servers, which malachi refuses.
	Type string `json:"type,omitempty"`
	URL  string `json:"url,omitempty"`
}

// MCPConfig is mcp.json.
type MCPConfig struct {
	Servers map[string]MCPServerConfig `json:"mcpServers"`
}

// Names returns the configured servers' names, sorted.
func (c MCPConfig) Names() []string {
	names := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// LoadMCPConfig reads <home>/mcp.json. A missing file is no servers.
func LoadMCPConfig(home string) (MCPConfig, error) {
	b, err := os.ReadFile(filepath.Join(home, MCPConfigFile))
	if errors.Is(err, fs.ErrNotExist) {
		return MCPConfig{}, nil
	}
	if err != nil {
		return MCPConfig{}, err
	}
	var c MCPConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return MCPConfig{}, fmt.Errorf("%s: %w", MCPConfigFile, err)
	}
	return c, nil
}

// serverName is what a server may be called: it becomes part of tool names
// (mcp__name__tool), which providers restrict to these characters.
var serverName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// launchers fetch and run code when they start, so the code that runs is
// whatever a registry serves that day rather than what the user installed.
// A value lists the first arguments that make the command a launcher; nil
// means the command is one whatever its arguments.
var launchers = map[string][][]string{
	"npx": nil, "bunx": nil, "uvx": nil, "pnpx": nil,
	"pipx":   {{"run"}},
	"pnpm":   {{"dlx"}, {"exec"}},
	"yarn":   {{"dlx"}},
	"npm":    {{"exec"}, {"x"}},
	"bun":    {{"x"}},
	"uv":     {{"tool", "run"}, {"run", "--with"}},
	"docker": {{"run"}},
	"podman": {{"run"}},
}

// wrappers run another command; they would let a launcher through.
var wrappers = []string{"env", "nohup", "nice", "timeout", "sudo", "doas", "xargs", "exec", "command", "stdbuf"}

// shells and interpreters run code given inline with these flags.
var inlineCode = map[string][]string{
	"sh": {"-c"}, "bash": {"-c"}, "zsh": {"-c"}, "dash": {"-c"}, "ksh": {"-c"}, "fish": {"-c", "--command"},
	"python": {"-c"}, "python3": {"-c"}, "node": {"-e", "--eval", "-p", "--print"},
	"deno": {"eval"}, "bun": {"-e", "--eval"}, "ruby": {"-e"}, "perl": {"-e", "-E"},
}

// installHint is how to install a server instead of launching it.
const installHint = "install the server and point command at the installed program " +
	"(npm i -g <package>, uv tool install <package>, or a binary on disk)"

// checkServer validates a server's configuration and resolves its command
// to an installed executable. cwd resolves relative paths.
func checkServer(name string, c MCPServerConfig, cwd string) (string, error) {
	if !serverName.MatchString(name) {
		return "", fmt.Errorf("server name %q: use letters, digits, - and _ (at most 32)", name)
	}
	if c.URL != "" || (c.Type != "" && c.Type != "stdio") {
		return "", errors.New("malachi runs only local servers it starts itself (stdio); remote and HTTP servers are not supported")
	}
	if strings.TrimSpace(c.Command) == "" {
		return "", errors.New("no command")
	}
	if err := checkLaunch(c.Command, c.Args); err != nil {
		return "", err
	}
	path, err := resolveCommand(c.Command, cwd)
	if err != nil {
		return "", err
	}
	// The program a symlink names counts too: a link called "server"
	// pointing at npx is still npx.
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		if err := checkLaunch(real, c.Args); err != nil {
			return "", err
		}
	}
	for k, v := range c.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return "", fmt.Errorf("env: invalid variable name %q", k)
		}
		if strings.ContainsRune(v, 0) {
			return "", fmt.Errorf("env %s: value contains NUL", k)
		}
	}
	return path, nil
}

// checkLaunch refuses launchers, wrappers and inline code.
func checkLaunch(command string, args []string) error {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(command)), ".exe")
	// Versioned interpreters: python3.12, node22.
	family := strings.TrimRight(base, "0123456789.")
	if _, ok := launchers[base]; ok {
		forms := launchers[base]
		if forms == nil {
			return fmt.Errorf("%s fetches and runs packages each time it starts; %s", base, installHint)
		}
		for _, form := range forms {
			if len(args) >= len(form) && slices.Equal(args[:len(form)], form) {
				return fmt.Errorf("%s %s fetches and runs packages each time it starts; %s", base, strings.Join(form, " "), installHint)
			}
		}
	}
	if slices.Contains(wrappers, base) {
		return fmt.Errorf("%s runs another command; point command at the server itself", base)
	}
	for _, name := range []string{base, family} {
		flags, ok := inlineCode[name]
		if !ok {
			continue
		}
		if name == "deno" && len(args) > 0 {
			switch args[0] {
			case "eval":
				return fmt.Errorf("deno eval runs inline code; point command at an installed server")
			case "run":
				for _, a := range args[1:] {
					if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") || strings.HasPrefix(a, "npm:") || strings.HasPrefix(a, "jsr:") {
						return fmt.Errorf("deno run %s fetches code each time it starts; %s", a, installHint)
					}
				}
			}
			continue
		}
		// An interpreter's own flags come before its script; what follows
		// the script is the script's.
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				break
			}
			if inlineFlag(a, flags) {
				return fmt.Errorf("%s %s runs inline code; point command at an installed server", base, a)
			}
		}
	}
	return nil
}

// inlineFlag reports whether a is one of flags, or a cluster of short flags
// (bash -lc) containing a short one.
func inlineFlag(a string, flags []string) bool {
	if slices.Contains(flags, a) {
		return true
	}
	if strings.HasPrefix(a, "--") || len(a) < 3 {
		return false
	}
	for _, f := range flags {
		if len(f) == 2 && strings.Contains(a[1:], f[1:]) {
			return true
		}
	}
	return false
}

// resolveCommand finds the executable a command names: a path (~ expanded,
// relative to cwd) or a name on PATH. It must exist and be executable.
func resolveCommand(command, cwd string) (string, error) {
	path := command
	if strings.ContainsRune(command, filepath.Separator) || strings.HasPrefix(command, "~") {
		path = resolvePath(cwd, command)
	} else {
		found, err := exec.LookPath(command)
		if err != nil {
			return "", fmt.Errorf("%s is not installed (not found on PATH)", command)
		}
		path = found
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %w", abs, err)
	case info.IsDir():
		return "", fmt.Errorf("%s is a directory", abs)
	case info.Mode()&0o111 == 0:
		return "", fmt.Errorf("%s is not executable", abs)
	}
	return abs, nil
}

// serverEnv is the environment a server gets: the minimal command
// environment plus its entry's variables, never malachi's own keys.
func serverEnv(c MCPServerConfig, opts ToolOptions) ([]string, error) {
	env := commandEnv(nil, opts.HideEnv)
	hidden := map[string]bool{}
	for _, h := range opts.HideEnv {
		hidden[strings.ToUpper(h)] = true
	}
	keys := make([]string, 0, len(c.Env))
	for k := range c.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if hidden[strings.ToUpper(k)] {
			return nil, fmt.Errorf("env %s: malachi's own keys are never passed to servers", k)
		}
		var bad string
		v := os.Expand(c.Env[k], func(name string) string {
			if hidden[strings.ToUpper(name)] {
				bad = name
				return ""
			}
			return os.Getenv(name)
		})
		if bad != "" {
			return nil, fmt.Errorf("env %s: malachi's own key %s is never passed to servers", k, bad)
		}
		env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, k+"=") })
		env = append(env, k+"="+v)
	}
	return env, nil
}
