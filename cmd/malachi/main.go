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
		mode       = flag.String("mode", "text", "print mode output: text or json (one Pi-compatible event per line)")
		cwd        = flag.String("cwd", "", "working directory (default: current directory)")
		listModels = flag.Bool("list-models", false, "list the models the provider currently serves and exit")
		version    = flag.Bool("version", false, "print the version and exit")
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
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "malachi:", err)
		return 1
	}
	defer s.Close()

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
	// sent. The one-shot path reports it plainly rather than waiting in
	// silence, since there is no status line to show progress on.
	if estimated, threshold, needed := s.NeedsCompaction(); needed {
		fmt.Fprintf(os.Stderr, "malachi: compacting %d tokens over %d before sending\n", estimated, threshold)
		if _, err := s.AutoCompact(ctx, nil); err != nil {
			fmt.Fprintf(os.Stderr, "malachi: auto-compaction failed, sending anyway: %v\n", err)
		}
	}
	if err := s.Prompt(ctx, prompt); err != nil {
		fmt.Fprintln(os.Stderr, "malachi:", err)
		return 1
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
