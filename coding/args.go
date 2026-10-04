package coding

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func strArg(args map[string]any, name string) (string, error) {
	v, ok := args[name]
	if !ok {
		return "", fmt.Errorf("missing required argument %q", name)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", name)
	}
	return s, nil
}

// optNumber returns a numeric argument, accepting JSON numbers or numeric
// strings (some models quote numbers).
func optNumber(args map[string]any, name string) (float64, bool, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch n := v.(type) {
	case float64:
		return n, true, nil
	case int:
		return float64(n), true, nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err == nil {
			return f, true, nil
		}
	}
	return 0, false, fmt.Errorf("argument %q must be a number", name)
}

func optInt(args map[string]any, name string) (int, bool, error) {
	f, ok, err := optNumber(args, name)
	if err != nil || !ok {
		return 0, ok, err
	}
	if f != math.Trunc(f) {
		return 0, false, fmt.Errorf("argument %q must be an integer", name)
	}
	return int(f), true, nil
}

// optStr returns an optional string argument. A present but empty value is
// reported as absent, so "" and omission mean the same thing to a tool.
func optStr(args map[string]any, name string) (string, bool, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("argument %q must be a string", name)
	}
	if strings.TrimSpace(s) == "" {
		return "", false, nil
	}
	return s, true, nil
}

// optBool returns an optional boolean argument.
func optBool(args map[string]any, name string) (bool, bool, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return false, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, fmt.Errorf("argument %q must be a boolean", name)
	}
	return b, true, nil
}

// resolvePath expands ~ and resolves relative paths against cwd.
func resolvePath(cwd, p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

func pathArg(args map[string]any, cwd string) (raw, resolved string, err error) {
	raw, err = strArg(args, "path")
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(raw) == "" {
		return "", "", fmt.Errorf("argument %q must not be empty", "path")
	}
	return raw, resolvePath(cwd, raw), nil
}
