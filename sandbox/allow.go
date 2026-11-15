package sandbox

import (
	"io/fs"
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
		// A link is never granted. Landlock puts a rule on what a path
		// leads to, so a link to the hidden path, or to a directory above
		// it (/var/run to /run, a home directory reached through a link),
		// would grant what is being carved around. Nothing is lost: a link
		// can be read and followed without a rule, and where it leads is
		// judged by the rules for that place.
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		child := filepath.Join(root, e.Name())
		g, s := carve(child, under)
		grant = append(grant, g...)
		split = append(split, s...)
	}
	return grant, split
}

// readGrants returns the paths Landlock grants read access to under p, and
// the directories that may still be listed: everything outside hidden and
// private directories, plus each readable root that lies in (or holds) a
// private directory, minus hidden paths. Writable roots are granted
// separately, with reading included.
//
// Directories split to carve the grants are listable, except those holding a
// private directory: Landlock grants listing for a whole tree, so listing /
// would list everything in the home directory too. That costs "ls /" and
// "ls /home" while the home directory is private; reading and walking
// through them still work.
func readGrants(p Policy) (grant, listable []string) {
	grant, split := readGrantsSplit(p)
	listable = slices.DeleteFunc(split, func(d string) bool {
		return slices.ContainsFunc(p.Private, func(priv string) bool {
			return within(priv, d) && p.PrivateAtCanonical(priv) != ""
		})
	})
	return grant, listable
}

func readGrantsSplit(p Policy) (grant, split []string) {
	grant, split = carve("/", slices.Concat(p.Hidden, p.Private))
	for _, r := range p.Readable {
		if !slices.ContainsFunc(p.Private, func(priv string) bool { return within(r, priv) || within(priv, r) }) {
			continue // readable already, unless hidden
		}
		g, s := carve(r, p.Hidden)
		grant = append(grant, g...)
		split = append(split, s...)
	}
	return grant, split
}
