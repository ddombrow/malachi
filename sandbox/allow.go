package sandbox

import (
	"os"
	"path/filepath"
	"slices"
)

// carve returns paths that together cover root minus every hidden path under
// it, and the directories it had to split to do so. Landlock rules grant
// access to whole subtrees and cannot deny inside one, so excluding ~/.ssh
// from a read grant on / means granting each entry of /, of /home, and of the
// home directory individually, except the one leading to the hidden path.
//
// Only entries that exist are granted. Paths must be canonical. It is rebuilt
// for every command, so directories created since are picked up next time.
//
// A split directory cannot gain new entries, since Landlock could only allow
// that for its whole subtree, hidden paths included. By default no hidden
// path lies under a writable root, so this matters only for a project that
// contains one (say, malachi run in the home directory).
func carve(root string, hidden []string) (grant, split []string) {
	var under []string
	for _, h := range hidden {
		if within(root, h) {
			return nil, nil // root itself is hidden
		}
		// A hidden path that does not exist holds nothing to protect, and
		// splitting around it would cost root the ability to create entries.
		if _, err := os.Lstat(h); err == nil && within(h, root) {
			under = append(under, h)
		}
	}
	if len(under) == 0 {
		return []string{root}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil // unreadable or not a directory: grant nothing
	}
	split = []string{root}
	for _, e := range entries {
		child := filepath.Join(root, e.Name())
		g, s := carve(child, under)
		grant = append(grant, g...)
		split = append(split, s...)
	}
	return grant, split
}

// partition splits paths into directories and everything else, dropping
// paths that do not exist.
func partition(paths []string) (dirs, files []string) {
	for _, p := range paths {
		st, err := os.Stat(p)
		switch {
		case err != nil:
		case st.IsDir():
			dirs = append(dirs, p)
		default:
			files = append(files, p)
		}
	}
	slices.Sort(dirs)
	slices.Sort(files)
	return dirs, files
}
