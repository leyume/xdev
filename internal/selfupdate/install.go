package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupSuffix marks the timestamped copies kept beside the binary. Matching
// what deploy/update.sh writes, so the two updaters leave one recognisable
// trail rather than two.
const backupSuffix = ".bak"

// TargetPath resolves the binary to replace: the one this process is running
// from, with symlinks followed.
//
// Following the link matters on macOS (Homebrew's bin is a tree of symlinks)
// and anywhere an admin has put xdev behind /usr/local/bin/xdev -> /opt/…:
// renaming over the link would replace the link with a file and quietly detach
// every other path that pointed at the real binary.
func TargetPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the running binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// A binary that has been deleted out from under us still has a path
		// worth reporting; the writability check below is what actually gates
		// the update.
		return exe, nil
	}
	return resolved, nil
}

// Writable reports whether this process can replace the binary at path.
//
// The test is on the *directory*, not the file: the swap is a rename, which
// needs write permission on the containing directory and cares nothing about
// the file's own mode. Checking the file instead would pass for a root-owned
// 0755 binary in a directory the user cannot write, and then fail at the last
// step of an update that has already downloaded everything.
func Writable(path string) bool {
	dir := filepath.Dir(path)
	probe, err := os.CreateTemp(dir, ".xdev-update-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return true
}

// Install replaces dest with src, keeping the previous binary as a timestamped
// backup whose path is returned.
//
// The new file is written beside the target and renamed into place, so the
// running process keeps its own inode (no ETXTBSY, which is what you get for
// writing to a running executable on Linux) and there is no instant where dest
// is missing or half-written. Anything watching the path sees the old binary,
// then the new one, and nothing in between.
func Install(src, dest string) (backup string, err error) {
	info, err := os.Stat(dest)
	if err != nil {
		return "", fmt.Errorf("stat the installed binary: %w", err)
	}
	mode := info.Mode().Perm()

	backup = fmt.Sprintf("%s.%s%s", dest, time.Now().Format("20060102150405"), backupSuffix)
	if err := copyFile(dest, backup, mode); err != nil {
		return "", fmt.Errorf("back up the current binary: %w", err)
	}

	staged := dest + ".new"
	if err := copyFile(src, staged, mode|0o111); err != nil {
		os.Remove(backup)
		return "", fmt.Errorf("stage the new binary: %w", err)
	}
	if err := os.Rename(staged, dest); err != nil {
		os.Remove(staged)
		os.Remove(backup)
		return "", fmt.Errorf("swap in the new binary: %w", err)
	}
	return backup, nil
}

// Restore puts a backup back, used when the service does not come back after an
// update. It is deliberately the same rename dance as Install: a rollback that
// works differently from the thing it is undoing is a rollback nobody has
// tested.
func Restore(backup, dest string) error {
	staged := dest + ".new"
	if err := copyFile(backup, staged, 0o755); err != nil {
		return err
	}
	if err := os.Rename(staged, dest); err != nil {
		os.Remove(staged)
		return err
	}
	return nil
}

// PruneBackups deletes all but the newest keep backups of dest, returning what
// it removed. Backups are named with a sortable timestamp, so lexical order is
// chronological order.
func PruneBackups(dest string, keep int) []string {
	matches, err := filepath.Glob(dest + ".*" + backupSuffix)
	if err != nil || len(matches) <= keep {
		return nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))

	var removed []string
	for _, old := range matches[keep:] {
		if os.Remove(old) == nil {
			removed = append(removed, old)
		}
	}
	return removed
}

// copyFile writes src to dst with the given mode, replacing dst if it exists.
// Contents are copied rather than hard-linked or renamed because src is
// routinely on a different filesystem (a temp dir under /tmp, the target under
// /usr/local/bin), where rename fails with EXDEV.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	// OpenFile's mode is masked by umask, so set it explicitly: a binary
	// installed under a 077 umask would otherwise be unreadable by the service
	// user, and the failure looks like a missing file.
	return os.Chmod(dst, mode)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SameFile reports whether two paths hold identical bytes, used to answer
// "there is nothing to do" for a rebuild that carries the same version string.
func SameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	if sa.Size() != sb.Size() {
		return false
	}
	ha, err := hashFile(a)
	if err != nil {
		return false
	}
	hb, err := hashFile(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(ha, hb)
}
