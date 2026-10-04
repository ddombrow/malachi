package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
)

// project lays out a home and a working directory with instruction files at
// several levels, the shape a real checkout has.
func project(t *testing.T) (home, cwd string) {
	t.Helper()
	home = t.TempDir()
	cwd = t.TempDir()
	write(t, home, "AGENTS.md", "home rules")
	write(t, cwd, "AGENTS.md", "project rules")
	return home, cwd
}

func TestDetectProjectResourcesExcludesHome(t *testing.T) {
	home, cwd := project(t)
	got := DetectProjectResources(home, cwd)
	if got.Total != 1 {
		t.Fatalf("want 1 project file, got %d: %v", got.Total, got.Files)
	}
	if !strings.Contains(got.Files[0], "AGENTS.md") {
		t.Errorf("unexpected file %q", got.Files[0])
	}
}

func TestResolveTrustDefaultsToWithholding(t *testing.T) {
	home, cwd := project(t)
	state := ResolveTrust(home, cwd, TrustAsk, "")
	if state.Trusted() {
		t.Fatal("an undecided project must not be trusted")
	}
	if !state.Pending {
		t.Error("state should be pending until the user answers")
	}
	if state.Source != "default" {
		t.Errorf("source = %q", state.Source)
	}
}

func TestResolveTrustTrustsProjectWithNoInstructions(t *testing.T) {
	home, cwd := project(t)
	cwd = t.TempDir() // no AGENTS.md here
	state := ResolveTrust(home, cwd, TrustAsk, "")
	if !state.Trusted() {
		t.Error("a project with no instruction files is not a question worth asking")
	}
}

func TestResolveTrustOverrideWinsOverPolicy(t *testing.T) {
	home, cwd := project(t)
	if state := ResolveTrust(home, cwd, TrustNever, "yes"); !state.Trusted() {
		t.Error("an explicit run override should beat a configured policy")
	}
	if state := ResolveTrust(home, cwd, TrustAlways, "no"); state.Trusted() {
		t.Error("a run decline should beat a configured policy")
	}
}

func TestResolveTrustPolicies(t *testing.T) {
	home, cwd := project(t)
	if state := ResolveTrust(home, cwd, TrustAlways, ""); !state.Trusted() {
		t.Error(`"always" should trust without asking`)
	}
	if state := ResolveTrust(home, cwd, TrustNever, ""); state.Trusted() {
		t.Error(`"never" should withhold without asking`)
	}
}

func TestSavedDecisionAppliesOnReopen(t *testing.T) {
	home, cwd := project(t)
	if err := SetTrust(home, cwd, TrustTrusted); err != nil {
		t.Fatal(err)
	}
	state := ResolveTrust(home, cwd, TrustAsk, "")
	if !state.Trusted() {
		t.Fatal("a saved decision should be honored")
	}
	if state.Source != "saved" {
		t.Errorf("source = %q, want saved", state.Source)
	}
}

func TestSavedDecisionIsExactToTheDirectory(t *testing.T) {
	home, cwd := project(t)
	if err := SetTrust(home, cwd, TrustTrusted); err != nil {
		t.Fatal(err)
	}
	// Trusting one checkout must not extend to another.
	other := filepath.Join(filepath.Dir(cwd), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, other, "AGENTS.md", "a different project's rules")
	if state := ResolveTrust(home, other, TrustAsk, ""); state.Trusted() {
		t.Error("a decision about one directory leaked to a sibling")
	}
}

func TestTrustNoticeNamesFilesWithoutReadingThem(t *testing.T) {
	home, cwd := project(t)
	notice := ResolveTrust(home, cwd, TrustAsk, "").TrustNotice()
	if !strings.Contains(notice, "AGENTS.md") {
		t.Errorf("notice should name the withheld file:\n%s", notice)
	}
	// The whole point of the summary is to show what was withheld without
	// printing instructions nobody has agreed to follow.
	if strings.Contains(notice, "project rules") {
		t.Errorf("notice leaked file contents:\n%s", notice)
	}
	if !strings.Contains(notice, "/trust") {
		t.Errorf("notice should say how to change the decision:\n%s", notice)
	}
}

func TestTrustNoticeEmptyWhenTrustedOrNothingToGate(t *testing.T) {
	home, cwd := project(t)
	if got := ResolveTrust(home, cwd, TrustAlways, "").TrustNotice(); got != "" {
		t.Errorf("trusted project should not warn: %q", got)
	}
	if got := ResolveTrust(home, t.TempDir(), TrustAsk, "").TrustNotice(); got != "" {
		t.Errorf("project with no files should not warn: %q", got)
	}
}

func TestSessionWithholdsProjectInstructionsByDefault(t *testing.T) {
	home, cwd := project(t)
	s, err := Open(testOpts(t, home, cwd, fake.New(fake.Text("ok"))))
	if err != nil {
		t.Fatal(err)
	}
	if s.TrustState().Trusted() {
		t.Fatal("session trusted an undecided project")
	}
	prompt := s.Harness.Config().System
	if strings.Contains(prompt, "project rules") {
		t.Error("an untrusted project's instructions reached the system prompt")
	}
	if !strings.Contains(prompt, "home rules") {
		t.Error("the user's own instructions should still be loaded")
	}
}

func TestSessionTrustOverrideLoadsInstructions(t *testing.T) {
	home, cwd := project(t)
	opts := testOpts(t, home, cwd, fake.New(fake.Text("ok")))
	opts.Trust = "yes"
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.Harness.Config().System, "project rules") {
		t.Error("-trust yes should load the project's instructions")
	}
}

func TestSessionTrustAppliesAndRebuildsThePrompt(t *testing.T) {
	home, cwd := project(t)
	s, err := Open(testOpts(t, home, cwd, fake.New(fake.Text("ok"))))
	if err != nil {
		t.Fatal(err)
	}
	before := s.Harness.Config().System
	if err := s.Trust(TrustTrusted, false); err != nil {
		t.Fatal(err)
	}
	after := s.Harness.Config().System
	if before == after {
		t.Fatal("trusting a project did not change the prompt")
	}
	if !strings.Contains(after, "project rules") {
		t.Error("trusted instructions missing from the rebuilt prompt")
	}
	// The decision is live on the harness, not just recorded: a session that
	// says it trusts the project but keeps sending the untrusted prompt would
	// be worse than either.
	if s.Harness.Config().System != after {
		t.Error("harness prompt diverged from the session's")
	}
}

func TestSessionTrustRememberedSurvivesReopen(t *testing.T) {
	home, cwd := project(t)
	opts := testOpts(t, home, cwd, fake.New(fake.Text("ok")))
	s, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trust(TrustTrusted, true); err != nil {
		t.Fatal(err)
	}
	next, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !next.TrustState().Trusted() {
		t.Error("a remembered decision did not apply to the next session")
	}
}

func TestTrustPolicySettingDefaultsToAsk(t *testing.T) {
	if (&Settings{}).TrustPolicyOrDefault() != TrustAsk {
		t.Error("unset policy should ask")
	}
	for _, v := range []string{"always", "never"} {
		s := &Settings{ProjectTrust: v}
		if s.TrustPolicyOrDefault() != TrustPolicy(v) {
			t.Errorf("policy %q not honored", v)
		}
	}
	// An unrecognized value must not silently widen access.
	if (&Settings{ProjectTrust: "yes please"}).TrustPolicyOrDefault() != TrustAsk {
		t.Error("an unrecognized policy should fall back to asking")
	}
}

func TestDetectProjectResourcesIsBounded(t *testing.T) {
	home := t.TempDir()
	// A deep chain outside home, since a home directory is never gated.
	dir := t.TempDir()
	for i := 0; i < maxListedResources*2; i++ {
		dir = filepath.Join(dir, "d")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, dir, "AGENTS.md", "rules")
	}
	got := DetectProjectResources(home, dir)
	if got.Total <= maxListedResources {
		t.Fatalf("expected more files than the listing bound, got %d", got.Total)
	}
	if len(got.Files) != maxListedResources {
		t.Errorf("listed %d files, want the bound %d", len(got.Files), maxListedResources)
	}
}

var _ agent.Provider = (*fake.Provider)(nil)
