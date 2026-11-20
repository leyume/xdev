package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xdev/internal/store"
)

func writeFile(t *testing.T, path, body string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// TestNewestNgxCert: a host's file is matched exactly (not a longer name that
// starts with it), needs its key, and the newest wins.
func TestNewestNgxCert(t *testing.T) {
	dir := t.TempDir()
	old, now := time.Now().Add(-48*time.Hour), time.Now()
	writeFile(t, filepath.Join(dir, "mail.a.com-aaaa.crt"), "old", old)
	writeFile(t, filepath.Join(dir, "mail.a.com-aaaa.key"), "oldkey", old)
	writeFile(t, filepath.Join(dir, "mail.a.com-bbbb.crt"), "new", now)
	writeFile(t, filepath.Join(dir, "mail.a.com-bbbb.key"), "newkey", now)
	writeFile(t, filepath.Join(dir, "mail.a.com-cccc.crt"), "nokey", now.Add(time.Hour))
	writeFile(t, filepath.Join(dir, "mail.a.com.ng-dddd.crt"), "other", now.Add(time.Hour))
	writeFile(t, filepath.Join(dir, "mail.a.com.ng-dddd.key"), "other", now.Add(time.Hour))

	crt, key := newestNgxCert(dir, "mail.a.com")
	if filepath.Base(crt) != "mail.a.com-bbbb.crt" || filepath.Base(key) != "mail.a.com-bbbb.key" {
		t.Errorf("got %q %q", crt, key)
	}
	if crt, _ := newestNgxCert(dir, "zail.a.com"); crt != "" {
		t.Errorf("no cert for host: got %q", crt)
	}
}

// TestSyncMailCerts: MAIL_TLS_HOSTS picks the hosts; files land as <host>.crt
// /.key; a second sync with nothing renewed reports no change; a renewal does.
func TestSyncMailCerts(t *testing.T) {
	acme, appDir := t.TempDir(), t.TempDir()
	us := filepath.Join(appDir, "_")
	os.MkdirAll(us, 0o755)
	os.WriteFile(filepath.Join(us, ".env"), []byte("MAIL_TLS_HOSTS=zail.a.com, mail.a.com\n"), 0o644)
	now := time.Now()
	for _, h := range []string{"zail.a.com", "mail.a.com"} {
		writeFile(t, filepath.Join(acme, h+"-01.crt"), h+" cert", now)
		writeFile(t, filepath.Join(acme, h+"-01.key"), h+" key", now)
	}
	s := &Service{acmeDir: acme}
	app := store.App{Type: "mail", Slug: "m", ComposePath: filepath.Join(us, "compose.yml")}

	changed, err := s.syncMailCerts(app)
	if err != nil || !changed {
		t.Fatalf("first sync: changed=%v err=%v", changed, err)
	}
	for _, h := range []string{"zail.a.com", "mail.a.com"} {
		b, _ := os.ReadFile(filepath.Join(us, "tls", h+".key"))
		if string(b) != h+" key" {
			t.Errorf("%s.key = %q", h, b)
		}
	}
	if changed, _ := s.syncMailCerts(app); changed {
		t.Error("second sync with nothing renewed reported a change")
	}
	writeFile(t, filepath.Join(acme, "mail.a.com-01.crt"), "renewed", now.Add(time.Minute))
	if changed, _ := s.syncMailCerts(app); !changed {
		t.Error("renewal not detected")
	}
}

// TestEnsureMailPublicURL: written from the app's domain when absent, never
// over a value already in _/.env.
func TestEnsureMailPublicURL(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	us := filepath.Join(t.TempDir(), "_")
	os.MkdirAll(us, 0o755)
	env := filepath.Join(us, ".env")
	os.WriteFile(env, []byte("MAIL_SMTP_PORT=2525"), 0o644) // no trailing newline
	s := &Service{store: st}
	app := store.App{Type: "mail", Domain: "sail.a.com", ComposePath: filepath.Join(us, "compose.yml")}

	if err := s.ensureMailPublicURL(app); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(env); string(b) != "MAIL_SMTP_PORT=2525\nMAIL_PUBLIC_URL=https://sail.a.com\n" {
		t.Errorf("got %q", b)
	}
	os.WriteFile(env, []byte("MAIL_PUBLIC_URL=https://mine.a.com\n"), 0o644)
	s.ensureMailPublicURL(app)
	if b, _ := os.ReadFile(env); string(b) != "MAIL_PUBLIC_URL=https://mine.a.com\n" {
		t.Errorf("overwrote an explicit value: %q", b)
	}
}

// TestSyncMailCertsChown: the tls dir and every copied file are handed to the
// mail server's user on each sync, even when nothing was recopied.
func TestSyncMailCertsChown(t *testing.T) {
	var owned []string
	orig := chownMail
	chownMail = func(p string) error { owned = append(owned, filepath.Base(p)); return nil }
	t.Cleanup(func() { chownMail = orig })

	acme, us := t.TempDir(), filepath.Join(t.TempDir(), "_")
	os.MkdirAll(us, 0o755)
	os.WriteFile(filepath.Join(us, ".env"), []byte("MAIL_TLS_HOSTS=mail.a.com\n"), 0o644)
	writeFile(t, filepath.Join(acme, "mail.a.com-01.crt"), "c", time.Now())
	writeFile(t, filepath.Join(acme, "mail.a.com-01.key"), "k", time.Now())
	s := &Service{acmeDir: acme}
	app := store.App{Type: "mail", ComposePath: filepath.Join(us, "compose.yml")}
	s.syncMailCerts(app)
	owned = nil
	s.syncMailCerts(app) // unchanged: ownership still enforced
	got := strings.Join(owned, " ")
	for _, want := range []string{"tls", "mail.a.com.crt", "mail.a.com.key"} {
		if !strings.Contains(got, want) {
			t.Errorf("not chowned: %s (got %s)", want, got)
		}
	}
}
