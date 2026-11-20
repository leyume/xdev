package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteMailEnv: with the standard mail ports free the .env sets nothing (the
// compose defaults apply); with any one held — another mail server mid-migration
// — every port moves to its high alternative so the stack can start beside it.
func TestWriteMailEnv(t *testing.T) {
	orig := mailPortFree
	t.Cleanup(func() { mailPortFree = orig })

	dir := t.TempDir()
	mailPortFree = func(int) bool { return true }
	if err := writeMailEnv(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if strings.Contains(string(b), "\nMAIL_") {
		t.Errorf("free ports: .env must not override any port:\n%s", b)
	}

	mailPortFree = func(p int) bool { return p != 25 }
	if err := writeMailEnv(dir); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir, ".env"))
	for _, p := range mailPorts {
		if !strings.Contains(string(b), "\n"+p.Var+"=") {
			t.Errorf("port 25 busy: .env missing override %s:\n%s", p.Var, b)
		}
	}
	if !strings.Contains(string(b), "MAIL_SMTP_PORT=2525\n") || !strings.Contains(string(b), "MAIL_IMAPS_PORT=9930\n") {
		t.Errorf("unexpected alternates:\n%s", b)
	}
}
