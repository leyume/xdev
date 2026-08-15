package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
)

func TestAddressPrefersARoutedHostname(t *testing.T) {
	app := store.App{Domain: "web.example.com", Port: 20001}

	r := Reach{ProxyEnabled: true, HTTPSPort: 443}
	if got := r.Address(app); got != "https://web.example.com" {
		t.Errorf("Address = %q, want the hostname over HTTPS", got)
	}
	// A non-standard HTTPS port has to be in the URL or the link opens nothing.
	r.HTTPSPort = 8444
	if got := r.Address(app); got != "https://web.example.com:8444" {
		t.Errorf("Address = %q, want the port included", got)
	}
}

// A hostname with nothing routing behind it is not an address. The app is still
// up, and its published port still answers — that is what to link to.
func TestAddressFallsBackToThePortWhenNothingRoutes(t *testing.T) {
	app := store.App{Domain: "web.example.com", Port: 20001}
	r := Reach{PublicHost: "203.0.113.10", ProxyEnabled: false}
	if got := r.Address(app); got != "http://203.0.113.10:20001" {
		t.Errorf("Address = %q, want the published port", got)
	}
}

func TestAddressOfAPortOnlyApp(t *testing.T) {
	app := store.App{Port: 20431} // no domain at all

	r := Reach{PublicHost: "203.0.113.10", ProxyEnabled: true}
	if got := r.Address(app); got != "http://203.0.113.10:20431" {
		t.Errorf("Address = %q, want the public host and port", got)
	}
	if !r.PortOnly() {
		t.Error("an install with a public host is not reporting port-only")
	}

	// No public host configured: loopback, which is exactly right for the ssh
	// tunnel the UI is normally read through, and honest about knowing nothing
	// else. Never a guess at the machine's external address.
	bare := Reach{ProxyEnabled: true}
	if got := bare.Address(app); got != "http://127.0.0.1:20431" {
		t.Errorf("Address = %q, want loopback", got)
	}
	if bare.PortOnly() {
		t.Error("an install with no public host should not be port-only — blank domains still get one")
	}

	// Nothing published and no hostname: no address, rather than a URL to
	// nowhere. A serve-mode static app with no domain is genuinely unreachable.
	if got := (Reach{}).Address(store.App{}); got != "" {
		t.Errorf("Address = %q, want empty", got)
	}
}

// The whole point of the mode: with a public host set, a blank domain field
// creates an app that is addressed by port — no hostname invented from the
// project's base domain, no domain row, nothing for Caddy to route.
func TestCreateWithoutADomainWhenPortOnly(t *testing.T) {
	s, _, proj := editFixture(t)
	s.SetReach(Reach{PublicHost: "203.0.113.10", ProxyEnabled: true})

	app, err := s.Create(proj.ID, CreateOpts{Name: "api", Type: "static", Domain: ""})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if app.Domain != "" {
		t.Errorf("app.Domain = %q, want empty — a port-only app must not be given a hostname", app.Domain)
	}
	hosts, _ := s.store.AppHostnames(app.ID)
	if len(hosts) != 0 {
		t.Errorf("domain rows = %v, want none: Caddy would route a name nobody asked for", hosts)
	}
}

// The limit of the mode, pinned so it is a known shape rather than a surprise:
// a *serve-mode* static app publishes no port — Caddy serves its files, keyed
// by hostname — so with no hostname there is nothing to reach it by. It has no
// address, and the UI says so instead of linking somewhere useless. Anything
// that runs a process (command-mode static, Go, and every container type) has a
// port and is fine.
func TestServeModeStaticHasNoPortToBeReachedBy(t *testing.T) {
	s, _, proj := editFixture(t)
	s.SetReach(Reach{PublicHost: "203.0.113.10", ProxyEnabled: true})

	app, err := s.Create(proj.ID, CreateOpts{Name: "site", Type: "static", Domain: ""})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if app.Port != 0 {
		t.Fatalf("a serve-mode static app has port %d — this test is out of date, and the mode now covers it", app.Port)
	}
	if got := s.reach.Address(app); got != "" {
		t.Errorf("address = %q, want empty — there is genuinely nowhere to send anyone", got)
	}
}

// And without a public host, nothing changes: a blank domain is still filled in
// from the project's base domain, exactly as before.
func TestCreateWithoutADomainStillDefaultsNormally(t *testing.T) {
	s, _, proj := editFixture(t)

	app, err := s.Create(proj.ID, CreateOpts{Name: "api", Type: "static", Domain: ""})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if app.Domain == "" {
		t.Fatal("app.Domain is empty on an install with no public host — the base-domain default was lost")
	}
	if !strings.HasSuffix(app.Domain, proj.BaseDomain) {
		t.Errorf("app.Domain = %q, want it derived from the project base domain %q", app.Domain, proj.BaseDomain)
	}
}

// APP_URL is the one that fails quietly: Laravel builds signed URLs, reset
// links and asset paths from it, so a hostname that resolves nowhere is not
// noticed until somebody clicks a link weeks later.
func TestLaravelEnvGetsTheRealAddress(t *testing.T) {
	dir := t.TempDir()
	if err := writeLaravelEnv(dir, "api", "http://203.0.113.10:20431", false, "", ""); err != nil {
		t.Fatalf("write env: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "laravel.env"))
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if !strings.Contains(string(b), "APP_URL=http://203.0.113.10:20431") {
		t.Errorf("APP_URL is not the address the app is reachable at:\n%s", firstLines(string(b), 20))
	}
}

// An app that already has a .env keeps it: APP_KEY lives there, and rewriting
// the file would invalidate every encrypted column and signed URL it issued.
func TestLaravelEnvIsNeverClobbered(t *testing.T) {
	dir := t.TempDir()
	if err := writeLaravelEnv(dir, "api", "https://web.example.com", false, "", ""); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeLaravelEnv(dir, "api", "http://203.0.113.10:20431", false, "", ""); err != nil {
		t.Fatalf("second write: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "laravel.env"))
	if !strings.Contains(string(b), "APP_URL=https://web.example.com") {
		t.Error("the existing .env was rewritten — APP_KEY would have changed with it")
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
