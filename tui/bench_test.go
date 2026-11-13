package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
)

// benchModel is a model whose transcript already holds blocks replies, the
// state a long session is in when the next token or keystroke arrives.
func benchModel(b *testing.B, blocks int) *model {
	b.Helper()
	s, err := coding.Open(coding.Options{
		Cwd: b.TempDir(), Home: b.TempDir(), Settings: &coding.Settings{},
		Provider: fake.New(fake.Text("hi")), NoSession: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(s.Close)
	m := newModel(s, "")
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	para := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 12)
	for i := range blocks {
		msg := agent.NewAssistantMessage("m")
		msg.Content = []agent.Content{&agent.TextContent{Text: fmt.Sprintf("## Step %d\n\n%s\n\n```go\nfunc f() int { return %d }\n```\n", i, para, i)}}
		m.handleEvent(&agent.MessageEndEvent{Message: msg})
	}
	m.refresh()
	return m
}

// The cost of one update must not grow with the transcript above it.
func BenchmarkStreamedToken(b *testing.B) {
	for _, blocks := range []int{10, 300} {
		b.Run(fmt.Sprintf("blocks=%d", blocks), func(b *testing.B) {
			m := benchModel(b, blocks)
			partial := agent.NewAssistantMessage("m")
			partial.Content = []agent.Content{&agent.TextContent{Text: strings.Repeat("Streaming some markdown text with words in it.\n", 60)}}
			for b.Loop() {
				m.Update(sessionEventsMsg{&agent.MessageUpdateEvent{Message: partial}})
				_ = m.View()
			}
		})
	}
}

func BenchmarkKeypress(b *testing.B) {
	m := benchModel(b, 300)
	for b.Loop() {
		m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
		_ = m.View()
	}
}
