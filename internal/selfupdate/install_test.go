package selfupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInstallSwapsAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	dest := write(t, filepath.Join(dir, "xdev"), "old binary", 0o755)
	src := write(t, filepath.Join(dir, "new"), "new binary", 0o644)

	backup, err := Install(src, dest)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got, _ := os.ReadFile(dest); string(got) != "new binary" {
		t.Errorf("dest = %q, want the new binary", got)
	}
	if got, _ := os.ReadFile(backup); string(got) != "old binary" {
		t.Errorf("backup = %q, want the old binary", got)
	}

	// The installed file has to be executable regardless of the source's mode:
	// a downloaded asset arrives 0600 so it is not runnable before it is
	// verified.
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: %v", info.Mode())
	}

	// Nothing may be left staged beside the target.
	if _, err := os.Stat(dest + ".new"); !os.IsNotExist(err) {
		t.Error("the staging file survived the install")
	}
}

// Rollback has to restore exactly what was replaced, or an update that fails is
// worse than one that never ran.
func TestRestorePutsTheOldBinaryBack(t *testing.T) {
	dir := t.TempDir()
	dest := write(t, filepath.Join(dir, "xdev"), "good", 0o755)
	src := write(t, filepath.Join(dir, "new"), "bad", 0o755)

	backup, err := Install(src, dest)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := Restore(backup, dest); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "good" {
		t.Errorf("after rollback dest = %q, want the original", got)
	}
	if info, _ := os.Stat(dest); info.Mode().Perm()&0o111 == 0 {
		t.Error("the restored binary is not executable")
	}
}

// Install must not destroy the current binary when the new one cannot be
// staged — the machine has to be left exactly as it was found.
func TestInstallLeavesTheBinaryAloneOnFailure(t *testing.T) {
	dir := t.TempDir()
	dest := write(t, filepath.Join(dir, "xdev"), "old binary", 0o755)

	if _, err := Install(filepath.Join(dir, "does-not-exist"), dest); err == nil {
		t.Fatal("Install accepted a missing source")
	}
	if got, _ := os.ReadFile(dest); string(got) != "old binary" {
		t.Errorf("dest = %q, want the original untouched", got)
	}
	// A failed install must not leave a backup either, or the pruner counts it
	// as one of the three good ones.
	if matches, _ := filepath.Glob(dest + ".*" + backupSuffix); len(matches) != 0 {
		t.Errorf("failed install left backups behind: %v", matches)
	}
}

func TestPruneBackupsKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "xdev")
	write(t, dest, "current", 0o755)

	// Timestamps are sortable, so lexical order is chronological order.
	stamps := []string{"20260101000000", "20260201000000", "20260301000000", "20260401000000", "20260501000000"}
	for _, s := range stamps {
		write(t, dest+"."+s+backupSuffix, s, 0o755)
	}

	removed := PruneBackups(dest, 3)
	if len(removed) != 2 {
		t.Fatalf("removed %d backups, want 2: %v", len(removed), removed)
	}
	for _, r := range removed {
		if strings.Contains(r, "20260501") || strings.Contains(r, "20260401") || strings.Contains(r, "20260301") {
			t.Errorf("pruned a backup that should have been kept: %s", r)
		}
	}
	left, _ := filepath.Glob(dest + ".*" + backupSuffix)
	if len(left) != 3 {
		t.Errorf("%d backups left, want 3", len(left))
	}
	// The binary itself is not a backup and must never be swept up.
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("the pruner deleted the binary: %v", err)
	}
}

func TestPruneBackupsDoesNothingUnderTheLimit(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "xdev")
	write(t, dest, "current", 0o755)
	write(t, dest+".20260101000000"+backupSuffix, "one", 0o755)

	if removed := PruneBackups(dest, 3); len(removed) != 0 {
		t.Errorf("pruned %v with fewer backups than the limit", removed)
	}
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a"), "identical", 0o644)
	b := write(t, filepath.Join(dir, "b"), "identical", 0o644)
	c := write(t, filepath.Join(dir, "c"), "different", 0o644)

	if !SameFile(a, b) {
		t.Error("identical files reported as different")
	}
	if SameFile(a, c) {
		t.Error("different files reported as identical")
	}
	if SameFile(a, filepath.Join(dir, "missing")) {
		t.Error("a missing file was reported identical")
	}
}

// The writability check gates on the directory, because the swap is a rename.
// Testing the file's own mode would pass for a root-owned binary in a directory
// the user cannot write — and then fail at the very last step.
func TestWritableChecksTheDirectory(t *testing.T) {
	dir := t.TempDir()
	dest := write(t, filepath.Join(dir, "xdev"), "binary", 0o755)

	if !Writable(dest) {
		t.Error("a writable directory reported as not writable")
	}

	// A read-only binary in a writable directory can still be replaced.
	if err := os.Chmod(dest, 0o555); err != nil {
		t.Fatal(err)
	}
	if !Writable(dest) {
		t.Error("a read-only file in a writable directory reported as not writable")
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not constrain this process")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	if Writable(filepath.Join(locked, "xdev")) {
		t.Error("a read-only directory reported as writable")
	}
}

// TargetPath must resolve symlinks: renaming over a link would replace the link
// with a file and detach every other path pointing at the real binary.
func TestTargetPathResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := write(t, filepath.Join(dir, "xdev-real"), "binary", 0o755)
	link := filepath.Join(dir, "xdev")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if got != want {
		t.Errorf("EvalSymlinks(%s) = %s, want %s", link, got, want)
	}
}
