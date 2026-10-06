package sandbox

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// flipper swaps link between pointing at a and at b until stopped, as a
// sandboxed command racing a file tool would.
func flipper(t *testing.T, link, a, b string) (stop func()) {
	t.Helper()
	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tmp := link + ".tmp"
		for i := 0; !done.Load(); i++ {
			target := a
			if i%2 == 1 {
				target = b
			}
			_ = os.Remove(tmp)
			if os.Symlink(target, tmp) == nil {
				_ = os.Rename(tmp, link) // atomic replace
			}
		}
	}()
	return func() { done.Store(true); wg.Wait() }
}

func TestOpenReadJudgesTheOpenedFile(t *testing.T) {
	p, project, _, home, _ := fixture(t)
	secret := filepath.Join(home, ".env")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	benign := filepath.Join(project, "benign.txt")
	if err := os.WriteFile(benign, []byte("benign"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(project, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	// No pre-check here: the open itself must refuse.
	if f, err := p.OpenRead(link); err == nil || !errors.Is(err, ErrDenied) {
		f.Close()
		t.Fatalf("OpenRead through a link to a hidden file: %v", err)
	}

	stop := flipper(t, link, benign, secret)
	defer stop()
	opened := 0
	for i := 0; i < 3000; i++ {
		f, err := p.OpenRead(link)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		if string(data) == "secret" {
			t.Fatal("read a hidden file through a swapped link")
		}
		opened++
	}
	if opened == 0 {
		t.Fatal("the benign file was never readable; the race test proved nothing")
	}
}

func TestOpenWriteStaysInsideWritableRoots(t *testing.T) {
	p, project, outside, _, _ := fixture(t)
	p = projectOnly(p, project)
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(project, "dir")
	if err := os.Mkdir(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(project, "d")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := p.OpenWrite(filepath.Join(link, "victim.txt"), os.O_WRONLY|os.O_TRUNC, 0); err == nil {
		t.Fatal("OpenWrite through a link to an outside directory")
	}

	stop := flipper(t, link, inside, outside)
	defer stop()
	wrote := 0
	for i := 0; i < 3000; i++ {
		for _, name := range []string{"victim.txt", "new.txt"} {
			f, err := p.OpenWrite(filepath.Join(link, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				continue
			}
			_, _ = f.WriteString("pwned")
			f.Close()
			wrote++
		}
	}
	stop()
	if data, _ := os.ReadFile(victim); string(data) != "intact" {
		t.Fatalf("outside file changed to %q", data)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Fatal("a file was created outside the writable roots")
	}
	if wrote == 0 {
		t.Fatal("never wrote inside; the race test proved nothing")
	}
}

func TestOpenWriteCreatesParentsAndKeepsMode(t *testing.T) {
	p, project, _, _, _ := fixture(t)
	path := filepath.Join(project, "a", "b", "c.txt")
	f, err := p.OpenWrite(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("x")
	f.Close()
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err = p.OpenWrite(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o755 || st.Size() != 0 {
		t.Fatalf("mode %v size %d", st.Mode().Perm(), st.Size())
	}
}
