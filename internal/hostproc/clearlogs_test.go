package hostproc

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestClearLogsWhileRunning is the regression test for the O_APPEND in Start.
//
// Truncating a file that a process still holds open only works if that process
// writes at the end of the file. Without O_APPEND the writer keeps the offset
// it had reached, so the first line after a clear lands at that offset and the
// filesystem fills everything before it with NUL bytes — the log comes back
// *longer* than before it was cleared, as a run of nulls followed by one line.
//
// The test writes enough to make that hole unmistakable, clears, then waits for
// a later line and checks the file for nulls.
func TestClearLogsWhileRunning(t *testing.T) {
	dir := t.TempDir()
	s := NewSupervisor(dir)

	// A shell loop is the honest shape here: a long-lived process writing on its
	// own schedule, which is what a dev server does.
	const name = "proj_app"
	err := s.Start(1, name, dir,
		`i=0; while [ $i -lt 400 ]; do echo "line-$i-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"; i=$((i+1)); sleep 0.005; done`,
		os.Environ())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop(1)

	waitFor(t, s, name, "line-20-")

	before, err := s.Logs(name, 0)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(before) < 500 {
		t.Fatalf("only %d bytes written before the clear; the test needs a bigger head start", len(before))
	}

	if err := s.ClearLogs(name); err != nil {
		t.Fatalf("clear: %v", err)
	}

	// Something written *after* the clear is what proves the writer kept going.
	waitFor(t, s, name, "line-")

	raw, err := os.ReadFile(s.logPath(name))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if i := strings.IndexByte(string(raw), 0); i >= 0 {
		t.Fatalf("log has a NUL hole at byte %d of %d — the writer is not appending, so the clear "+
			"left a sparse gap instead of an empty file", i, len(raw))
	}
	if len(raw) >= len(before) {
		t.Errorf("log is %d bytes after clearing %d bytes — it did not shrink", len(raw), len(before))
	}
}

// TestClearLogsWithNoLogFile: an app that never started has nothing to clear,
// and that is success, not a missing-file error the UI would have to explain.
func TestClearLogsWithNoLogFile(t *testing.T) {
	s := NewSupervisor(t.TempDir())
	if err := s.ClearLogs("never_started"); err != nil {
		t.Errorf("clearing a log that was never written: %v, want nil", err)
	}
}

func waitFor(t *testing.T, s *Supervisor, name, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := s.Logs(name, 0); strings.Contains(out, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in the log", want)
}
