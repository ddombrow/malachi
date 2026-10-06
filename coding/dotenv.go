package coding

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
)

// dotEnvKeys records the variables LoadDotEnv set. They are malachi's own
// secrets (API keys), so commands the model runs do not inherit them.
var (
	dotEnvMu   sync.Mutex
	dotEnvKeys []string
)

// DotEnvKeys returns the names of the variables LoadDotEnv has set.
func DotEnvKeys() []string {
	dotEnvMu.Lock()
	defer dotEnvMu.Unlock()
	return slices.Clone(dotEnvKeys)
}

// LoadDotEnv sets environment variables from a KEY=VALUE file. Non-empty
// variables already in the environment win, so a shell export always
// overrides the file. A missing file is not an error.
//
// Supported syntax: blank lines, # comments, an optional "export " prefix,
// and values that are bare, 'single-quoted' (literal), or "double-quoted"
// (with \n, \t, \", \\ escapes). Unquoted values may end in a " #" comment.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, n)
		}
		value, err := parseDotEnvValue(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if os.Getenv(key) == "" {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
			dotEnvMu.Lock()
			dotEnvKeys = append(dotEnvKeys, key)
			dotEnvMu.Unlock()
		}
	}
	return sc.Err()
}

func parseDotEnvValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", errors.New("unterminated single quote")
		}
		return v[1 : end+1], nil
	case '"':
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			switch c := v[i]; c {
			case '"':
				return b.String(), nil
			case '\\':
				if i+1 < len(v) {
					i++
					switch v[i] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					default:
						b.WriteByte(v[i])
					}
				}
			default:
				b.WriteByte(c)
			}
		}
		return "", errors.New("unterminated double quote")
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v, nil
}
