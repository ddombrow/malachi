package coding

import (
	"os"
	"path/filepath"
	"testing"
)

// contextWindow is the denominator for the context gauge and the derived
// tool-output ceiling, so an override that silently failed to merge left the
// gauge on its default and the /ctx warning telling the user to set a field
// that did nothing.
func TestProviderOverrideMergesContextWindow(t *testing.T) {
	home := t.TempDir()
	body := `{"providers":{"opencode-go":{"contextWindow":350000}}}`
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	pc := s.ProviderConfigs()["opencode-go"]
	if got := pc.ContextWindowTokens(); got != 350000 {
		t.Errorf("contextWindow = %d, want 350000", got)
	}
	// The override layers onto the preset rather than replacing it.
	if pc.BaseURL == "" || pc.SessionHeader != "x-opencode-session" {
		t.Errorf("override dropped preset fields: %+v", pc)
	}
}

// An override that says nothing about the window keeps the preset's, and an
// unset one still falls back to the conservative default.
func TestContextWindowFallsBackWhenUnset(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "settings.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ProviderConfigs()["opencode-go"].ContextWindowTokens(); got != defaultContextWindow {
		t.Errorf("contextWindow = %d, want the %d default", got, defaultContextWindow)
	}
}
