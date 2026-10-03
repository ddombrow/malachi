package coding

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := `# keys
OPENCODE_API_KEY=sk-plain # trailing comment
export MALACHI_T_EXPORTED="a \"quoted\" value\n"
MALACHI_T_SINGLE='lit $HOME \n'
MALACHI_T_EMPTY=
MALACHI_T_PRESET=from-file
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_API_KEY", "") // set but empty: the file fills it in
	for _, k := range []string{"MALACHI_T_EXPORTED", "MALACHI_T_SINGLE", "MALACHI_T_EMPTY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("MALACHI_T_PRESET", "from-shell")

	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"OPENCODE_API_KEY":   "sk-plain",
		"MALACHI_T_EXPORTED": "a \"quoted\" value\n",
		"MALACHI_T_SINGLE":   `lit $HOME \n`,
		"MALACHI_T_EMPTY":    "",
		"MALACHI_T_PRESET":   "from-shell", // the shell environment wins
	}
	for k, v := range want {
		if got, ok := os.LookupEnv(k); !ok || got != v {
			t.Errorf("%s = %q (set=%v), want %q", k, got, ok, v)
		}
	}
}

func TestLoadDotEnvMissingFileAndBadLine(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	bad := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(bad, []byte("just words\n"), 0o600)
	if err := LoadDotEnv(bad); err == nil {
		t.Fatal("want error for malformed line")
	}
}
