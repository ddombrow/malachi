// Command malachi is a terminal coding agent.
//
//	malachi                      interactive TUI
//	malachi -p "fix the tests"   print mode: run one prompt and exit
//	echo "..." | malachi -p      prompt from stdin
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/coding"
	"github.com/ddombrow/malachi/rpc"
	"github.com/ddombrow/malachi/tui"
)

func main() {
	os.Exit(recoverAndRun(run))
}

// recoverAndRun turns a panic anywhere outside the TUI's own handler into a
// logged crash and a non-zero exit, instead of a trace that scrolls away.
func recoverAndRun(fn func() int) (code int) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		path := coding.NewDiagnostics(coding.Home()).LogPanic("main", r, debug.Stack())
		fmt.Fprintf(os.Stderr, "malachi: internal error (logged to %s): %v\n", path, r)
		code = 1
	}()
	return fn()
}

func run() int {
	var (
		printMode  = flag.Bool("p", false, "print mode: run the prompt non-interactively and exit")
		model      = flag.String("model", "", `model as "provider/model" or "model" (default from settings, else opencode-go/kimi-k2.7-code)`)
		thinking   = flag.String("thinking", "", "thinking level: off, minimal, low, medium, high, xhigh")
		cont       = flag.Bool("c", false, "continue the most recent session in this directory")
		resume     = flag.String("resume", "", "resume a session by file path or name prefix")
		noSession  = flag.Bool("no-session", false, "do not save the session to disk")
		mode       = flag.String("mode", "text", `"text" or "json" (one Pi-compatible event per line) for -p output, or "rpc" to run headless: JSON commands on stdin, responses and events on stdout`)
		cwd        = flag.String("cwd", "", "working directory (default: current directory)")
		listModels = flag.Bool("list-models", false, "list the models the provider currently serves and exit")
		version    = flag.Bool("version", false, "print the version and exit")
		trustFlag  = flag.String("trust", "", `project trust for this run: "yes" to load this directory's instruction files, "no" to withhold them`)
	)
	flag.BoolVar(cont, "continue", false, "same as -c")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: malachi [flags] [prompt]\n\n")
		flag.PrintDefaults()
	}
	positional := parseInterspersed(os.Args[1:])
	if *version {
		fmt.Println("malachi", coding.Version)
		return 0
	}
	switch *mode {
	case "text", "json", "rpc":
	default:
		fmt.Fprintf(os.Stderr, "malachi: unknown -mode %q (want text, json or rpc)\n", *mode)
		return 2
	}

	// Secrets such as OPENCODE_API_KEY may live in ~/.malachi/.env.
	if err := coding.LoadDotEnv(filepath.Join(coding.Home(), ".env")); err != nil {
		fmt.Fprintln(os.Stderr, "malachi:", err)
		return 1
	}

	s, err := coding.Open(coding.Options{
		Cwd:           *cwd,
		Model:         *model,
		ThinkingLevel: *thinking,
		Continue:      *cont,
		Resume:        *resume,
		NoSession:     *noSession,
		Trust:         *trustFlag,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "malachi:", err)
		return 1
	}
	defer s.Close()

	if *mode == "rpc" {
		return rpcRun(s)
	}

	if *printMode {
		// Print mode cannot ask a question, so a withheld project file is
		// stated here. Failing quietly would look like the agent ignoring
		// AGENTS.md. The TUI announces it in its own banner instead.
		if notice := s.TrustState().TrustNotice(); notice != "" {
			fmt.Fprintln(os.Stderr, notice)
			fmt.Fprintln(os.Stderr, "  pass -trust yes to load them, or set projectTrust in settings.json")
		}
	}

	if *listModels {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ids, err := s.Models(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "malachi: live model list unavailable, showing built-in list:", err)
		}
		for _, id := range ids {
			fmt.Println(id)
		}
		return 0
	}

	prompt := strings.Join(positional, " ")
	if *printMode {
		if prompt == "" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintln(os.Stderr, "malachi:", err)
				return 1
			}
			prompt = strings.TrimSpace(string(data))
		}
		if prompt == "" {
			fmt.Fprintln(os.Stderr, "malachi: -p needs a prompt argument or stdin")
			return 2
		}
		return printRun(s, prompt, *mode)
	}

	if err := tui.Run(s, prompt); err != nil {
		fmt.Fprintln(os.Stderr, "malachi:", err)
		return 1
	}
	return 0
}

// printRun streams assistant text to stdout and tool activity to stderr.
func printRun(s *coding.Session, prompt, mode string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var last *agent.AssistantMessage
	enc := json.NewEncoder(os.Stdout)
	s.Harness.Subscribe(func(e agent.Event) {
		if end, ok := e.(*agent.MessageEndEvent); ok {
			if a, ok := end.Message.(*agent.AssistantMessage); ok {
				last = a
			}
		}
		if mode == "json" {
			_ = enc.Encode(e)
			return
		}
		switch ev := e.(type) {
		case *agent.MessageUpdateEvent:
			if d, ok := ev.AssistantMessageEvent.(*agent.TextDelta); ok {
				fmt.Print(d.Delta)
			}
		case *agent.MessageEndEvent:
			if a, ok := ev.Message.(*agent.AssistantMessage); ok && a.Text() != "" {
				fmt.Println()
			}
		case *agent.ToolExecutionStartEvent:
			fmt.Fprintf(os.Stderr, "→ %s\n", coding.SummarizeToolCall(ev.ToolName, ev.Args))
		case *agent.ToolExecutionEndEvent:
			if ev.IsError {
				fmt.Fprintf(os.Stderr, "  ✗ %s\n", firstLine(ev.Result.Text()))
			}
		}
	})

	// A conversation past the configured window is compacted before it is
	// sent; Prompt does that. The one-shot path says so plainly, since there
	// is no status line to show progress on.
	_, _, compacting := s.NeedsCompaction()
	compacting = compacting && s.AutoCompactionEnabled()
	if compacting {
		estimated, threshold, _ := s.NeedsCompaction()
		fmt.Fprintf(os.Stderr, "malachi: compacting %d tokens over %d before sending\n", estimated, threshold)
	}
	if err := s.Prompt(ctx, prompt); err != nil {
		fmt.Fprintln(os.Stderr, "malachi:", err)
		return 1
	}
	if msgs := s.Harness.Messages(); compacting && len(msgs) > 0 {
		if _, ok := msgs[0].(*agent.CompactionSummaryMessage); !ok {
			fmt.Fprintln(os.Stderr, "malachi: auto-compaction failed; the prompt was sent anyway (see /session for the log)")
		}
	}
	if err := s.PersistError(); err != nil {
		fmt.Fprintln(os.Stderr, "malachi:", err)
	}
	if last != nil && (last.StopReason == agent.StopError || last.StopReason == agent.StopAborted) {
		fmt.Fprintf(os.Stderr, "malachi: %s: %s\n", last.StopReason, last.ErrorMessage)
		return 1
	}
	return 0
}

// rpcRun serves the session headless until stdin closes. Project trust is
// reported in get_state and answered with set_trust, so nothing is printed:
// stdout belongs to the protocol.
func rpcRun(s *coding.Session) int {
	sv := rpc.New(s, os.Stdin, os.Stdout)
	err := sv.Run(context.Background())
	// new_session and switch_session replace the session; the one opened
	// here is closed by run's defer, the last one here.
	if last := sv.Session(); last != s {
		last.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "malachi:", err)
		return 1
	}
	return 0
}

// parseInterspersed parses flags that appear before, between, or after
// positional arguments (the standard flag package stops at the first
// positional). Everything after "--" is positional.
func parseInterspersed(args []string) []string {
	var positional []string
	for {
		_ = flag.CommandLine.Parse(args) // ExitOnError: exits on bad flags
		rest := flag.Args()
		if len(rest) == 0 {
			return positional
		}
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positional, rest...)
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
