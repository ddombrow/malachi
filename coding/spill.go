package coding

import (
	"os"
	"path/filepath"
	"time"
)

// spillRetention is how long a spill directory left by a session that never
// closed (a crash, a kill) survives before a later Open removes it.
const spillRetention = 7 * 24 * time.Hour

// spillRoot holds one directory per open session for the full output of
// truncated commands. It is under the system temp directory, not the malachi
// home: the model reads these files back, and the home holds secrets.
func spillRoot() string { return filepath.Join(os.TempDir(), "malachi-spill") }

// sweepSpill removes spill directories untouched for longer than
// spillRetention. Errors are ignored: this is housekeeping.
func sweepSpill(root string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || now.Sub(info.ModTime()) < spillRetention {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}
