package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ddombrow/malachi/ai/fake"
	"github.com/ddombrow/malachi/coding"
)

func TestSidebarShowsOnWideTerminals(t *testing.T) {
	m := newTestModel(t) // 100 columns
	if !m.sidebarShown() || m.contentWidth() != m.width-sidebarWidth || m.vp.Width() != m.contentWidth() {
		t.Fatalf("shown=%v content=%d viewport=%d", m.sidebarShown(), m.contentWidth(), m.vp.Width())
	}
	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"sandbox", "network", "read", "write", "hidden"} {
		if !strings.Contains(view, want) {
			t.Errorf("sidebar lacks %q:\n%s", want, view)
		}
	}
	// Every row of the transcript area is the full terminal width, so the
	// sidebar lines up.
	for i, l := range strings.Split(m.View().Content, "\n")[:m.vp.Height()] {
		if w := lipgloss.Width(l); w != m.width {
			t.Fatalf("row %d is %d columns, want %d: %q", i, w, m.width, ansi.Strip(l))
		}
	}

	m.Update(tea.WindowSizeMsg{Width: sidebarMinWidth - 1, Height: 30})
	if m.sidebarShown() || m.contentWidth() != m.width {
		t.Fatal("sidebar shown on a narrow terminal")
	}
	m.command("/sidebar") // hides it
	m.command("/sidebar") // wants it back, but there is no room
	if out := m.tr.text(m.r); !strings.Contains(out, "at least") {
		t.Fatalf("no explanation on a narrow terminal: %s", out)
	}
}

func TestSidebarToggleKey(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if m.sidebarShown() || m.vp.Width() != m.width {
		t.Fatalf("ctrl+s left the sidebar shown (viewport %d)", m.vp.Width())
	}
	if strings.Contains(ansi.Strip(m.View().Content), sidebarBorder) {
		t.Fatal("sidebar still drawn")
	}
	m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if !m.sidebarShown() {
		t.Fatal("ctrl+s did not bring the sidebar back")
	}
}

func TestSidebarCloseButton(t *testing.T) {
	m := newTestModel(t)
	top := ansi.Strip(m.sidebarLines(m.vp.Height())[0])
	x := strings.Index(top, "×")
	if x < 0 {
		t.Fatalf("no close button: %q", top)
	}
	m.Update(tea.MouseClickMsg{X: m.width - 1, Y: 0, Button: tea.MouseLeft})
	if m.sidebarShown() || m.vp.Width() != m.width {
		t.Fatal("clicking × left the sidebar shown")
	}
}

func sidebarText(m *model) string {
	return ansi.Strip(strings.Join(m.sidebarLines(m.vp.Height()), "\n"))
}

func TestNetworkToggleKeyAndClick(t *testing.T) {
	m := newTestModel(t)
	if m.s.Sandbox().Network {
		t.Fatal("network on by default")
	}
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if !m.s.Sandbox().Network || !strings.Contains(sidebarText(m), "^o on\n") {
		t.Fatalf("ctrl+o: network=%v\n%s", m.s.Sandbox().Network, sidebarText(m))
	}
	// Clicking the network row switches it back, and starts no selection.
	row := slices.IndexFunc(m.sidebarLines(m.vp.Height()), func(l string) bool { return strings.Contains(ansi.Strip(l), "network") })
	if row < 0 {
		t.Fatal("no network row")
	}
	m.Update(tea.MouseClickMsg{X: m.contentWidth() + 3, Y: row, Button: tea.MouseLeft})
	if m.s.Sandbox().Network || m.sel.dragging {
		t.Fatalf("click: network=%v dragging=%v", m.s.Sandbox().Network, m.sel.dragging)
	}
	m.Update(tea.MouseClickMsg{X: m.contentWidth() + 3, Y: row + 2, Button: tea.MouseLeft})
	if m.s.Sandbox().Network || m.sel.dragging {
		t.Fatal("a click elsewhere in the sidebar did something")
	}
}

// The sidebar follows the session's events, so a change made elsewhere (an
// RPC client, say) shows up too.
func TestSidebarFollowsSandboxEvents(t *testing.T) {
	m := newTestModel(t)
	st := m.s.Sandbox()
	st.Readable = append(st.Readable, filepath.Join(t.TempDir(), "granted-elsewhere"))
	m.handleEvent(coding.SandboxChangedEvent{State: st})
	if !strings.Contains(sidebarText(m), "granted-elsewhere") {
		t.Fatalf("event not shown:\n%s", sidebarText(m))
	}
}

func TestSidebarClipsToHeight(t *testing.T) {
	m := newTestModel(t)
	lines := m.sidebarLines(4)
	if len(lines) != 4 || !strings.Contains(ansi.Strip(lines[3]), "…") {
		t.Fatalf("clipped sidebar: %q", lines)
	}
	for _, l := range lines {
		if lipgloss.Width(l) != sidebarWidth {
			t.Fatalf("line %q is %d columns", ansi.Strip(l), lipgloss.Width(l))
		}
	}
}

func TestSidebarShowsTrust(t *testing.T) {
	m := newTestModel(t)
	if text := sidebarText(m); !strings.Contains(text, "no project files") {
		t.Fatalf("trust without project files:\n%s", text)
	}

	home, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("project rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := coding.Open(coding.Options{
		Cwd: cwd, Home: home, Settings: &coding.Settings{}, Provider: fake.New(), NoSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	m = newModel(s, "")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	for _, step := range []struct{ cmd, want string }{
		{"", "undecided"},
		{"/trust yes", "for this session"},
		{"/trust no --save", "saved for this directory"},
	} {
		if step.cmd != "" {
			m.command(step.cmd)
		}
		if text := sidebarText(m); !strings.Contains(text, step.want) || !strings.Contains(text, "1 instruction file ") {
			t.Fatalf("after %q want %q:\n%s", step.cmd, step.want, text)
		}
	}
}

func TestSidebarShowsVersionOnTop(t *testing.T) {
	m := newTestModel(t)
	top := ansi.Strip(m.sidebarLines(m.vp.Height())[0])
	if !strings.Contains(top, "malachi "+coding.Version) || !strings.HasSuffix(strings.TrimRight(top, " "), "×") {
		t.Fatalf("top row is %q, want the version and the close button", top)
	}
	if divider := ansi.Strip(m.sidebarLines(m.vp.Height())[1]); !strings.Contains(divider, strings.Repeat("─", sidebarInner)) {
		t.Fatalf("no divider under the top row: %q", divider)
	}
}
