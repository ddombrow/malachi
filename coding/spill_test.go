package coding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ddombrow/malachi/agent"
	"github.com/ddombrow/malachi/ai/fake"
)

// A truncated command's full output lives in the session's own directory,
// which Close removes.
func TestSpillFilesAreRemovedOnClose(t *testing.T) {
	s, err := Open(Options{Cwd: t.TempDir(), Home: t.TempDir(), Settings: &Settings{}, Provider: fake.New(), NoSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(s.spillDir) != spillRoot() {
		t.Fatalf("spill dir %s is not under %s", s.spillDir, spillRoot())
	}
	var bash *agent.Tool
	for _, tl := range s.tools {
		if tl.Name == "bash" {
			bash = tl
		}
	}
	r, err := run(t, bash, map[string]any{"command": "seq 1 3000"})
	if err != nil {
		t.Fatal(err)
	}
	path := r.Details.(map[string]any)["full_output_path"].(string)
	if !strings.HasPrefix(path, s.spillDir+string(filepath.Separator)) {
		t.Fatalf("spilled to %s, not under %s", path, s.spillDir)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(s.spillDir); !os.IsNotExist(err) {
		t.Fatalf("spill dir survived Close: %v", err)
	}
}

func TestSweepRemovesOnlyStaleSpillDirs(t *testing.T) {
	root := t.TempDir()
	stale, fresh := filepath.Join(root, "stale"), filepath.Join(root, "fresh")
	for _, d := range []string{stale, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-spillRetention - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	sweepSpill(root, time.Now())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale spill dir kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh spill dir removed")
	}
}
