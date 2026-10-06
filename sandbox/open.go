package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The file tools run inside malachi, beyond the OS sandbox, so they must not
// be fooled by a sandboxed command that swaps a symlink between a check and
// an open. OpenRead and OpenWrite judge the file actually opened: the kernel
// names it (fdPath), and that name is checked, not the path asked for.
// Hard links cannot stand in for hidden files, since sandboxed commands
// cannot create links to them.

// OpenRead opens path for reading if the file it reaches is not hidden.
func (p Policy) OpenRead(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil || !p.Enabled {
		return f, err
	}
	if err := p.checkOpened(f, path, false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// OpenWrite opens path with flag (os.O_WRONLY or os.O_RDWR, optionally
// os.O_CREATE and os.O_TRUNC) if the file it reaches is under a writable root
// and not hidden. With os.O_CREATE, missing parent directories are created.
//
// It works beneath the writable root holding the path through os.Root, which
// refuses symlinks leading out of it however they change, and truncates only
// after checking the file it opened.
func (p Policy) OpenWrite(path string, flag int, perm fs.FileMode) (*os.File, error) {
	if !p.Enabled {
		if flag&os.O_CREATE != 0 {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return nil, err
			}
		}
		return os.OpenFile(path, flag, perm)
	}
	if err := p.CheckWrite(path); err != nil {
		return nil, err
	}
	c := Canonical(path)
	root := ""
	for _, w := range p.Writable {
		if within(c, w) && len(w) > len(root) {
			root = w
		}
	}
	rel, err := filepath.Rel(root, c)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if flag&os.O_CREATE != 0 {
		if err := r.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			return nil, err
		}
	}
	f, err := r.OpenFile(rel, flag&^os.O_TRUNC, perm)
	if err != nil {
		return nil, err
	}
	if err := p.checkOpened(f, path, true); err != nil {
		f.Close()
		return nil, err
	}
	if flag&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

// checkOpened checks the file f, opened from path, by the name the kernel
// gives it.
func (p Policy) checkOpened(f *os.File, path string, write bool) error {
	real, err := fdPath(f)
	if errors.Is(err, errors.ErrUnsupported) {
		// No way to ask here; such platforms have no sandbox backend, so no
		// sandboxed command runs alongside to race the check.
		real = Canonical(path)
	} else if err != nil {
		return fmt.Errorf("%w: cannot tell which file %s is: %v", ErrDenied, path, err)
	}
	if h := p.HiddenAtCanonical(real); h != "" {
		return fmt.Errorf("%w: %s leads to %s, which is hidden; ask the user if access is needed", ErrDenied, path, real)
	}
	if write {
		for _, w := range p.Writable {
			if within(real, w) {
				return nil
			}
		}
		return fmt.Errorf("%w: %s leads to %s, outside the writable directories; ask the user if access is needed", ErrDenied, path, real)
	}
	return nil
}
