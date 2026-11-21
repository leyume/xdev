package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// failingEngine puts a `docker` on PATH that fails every command, so a test
// can drive the error paths of a running app without a container engine.
func failingEngine(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// Hand-edited port lines still name Rail's port.
func TestRailPortLineVariants(t *testing.T) {
	for _, line := range []string{
		`- "20031:8080"`, `- 20031:8080`, `- '20031:8080'`, `- "127.0.0.1:20031:8080"`,
		`- "20031:8080/tcp"`, `- "20031:8080" # rail`, `- "[::1]:20031:8080"`,
	} {
		y := "services:\n  rail:\n    image: x\n    ports:\n      " + line + "\n"
		if got := composeRailPort(y); got != 20031 {
			t.Errorf("composeRailPort(%s) = %d, want 20031", line, got)
		}
	}
}

// Neither webmail depends_on mail: podman-compose ≤1.3 ignores --no-deps and
// would recreate Stalwart on every switch. Rail binds to loopback by default.
func TestWebmailPartialsLeaveMailAlone(t *testing.T) {
	d := templates.Data{ProjectSlug: "p", AppSlug: "a", HostPort: 20001, RailPort: 20031, RailOnAppPort: true, PublishIP: "127.0.0.1"}
	for _, name := range []string{"rail_service", "webmail_service"} {
		out, err := templates.RenderPartial("mail", name, d)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "depends_on") {
			t.Errorf("%s depends on another service:\n%s", name, out)
		}
	}
	out, _ := templates.RenderPartial("mail", "rail_service", d)
	for _, want := range []string{`"127.0.0.1:20031:8080"`, `"127.0.0.1:20001:8080"`} {
		if !strings.Contains(out, want) {
			t.Errorf("rail block missing %s:\n%s", want, out)
		}
	}
}

// With MAIL_TLS_HOSTS unset Stalwart gets the console's certificate only, not
// Rail's — copying Rail's would restart the mail server on every renewal.
func TestMailTLSHostsSkipsRail(t *testing.T) {
	svc, _, app, _ := mailFixture(t)
	if _, err := svc.SetRail(app.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(filepath.Dir(app.ComposePath), ".env"))
	if got := svc.mailTLSHosts(app); len(got) != 1 || got[0] != "zail.demo.test" {
		t.Errorf("mailTLSHosts = %v, want [zail.demo.test]", got)
	}
}

// Saving settings with Rail's hostname in either domain field is refused
// before anything is written.
func TestUpdateRefusesRailHost(t *testing.T) {
	svc, st, app, _ := mailFixture(t)
	if _, err := svc.SetRail(app.ID, true, "rail.demo.test"); err != nil {
		t.Fatal(err)
	}
	for _, o := range []EditOpts{
		{Name: "renamed", Domain: "rail.demo.test", ServiceDomain: "zail.demo.test"},
		{Name: "renamed", Domain: "sail.demo.test", ServiceDomain: "rail.demo.test"},
	} {
		if _, err := svc.Update(app.ID, o); err == nil || strings.Contains(err.Error(), "UNIQUE") {
			t.Errorf("Update(%+v) = %v, want a plain refusal", o, err)
		}
		got, _ := st.AppByID(app.ID)
		if got.Name != "post" || got.Domain != "sail.demo.test" {
			t.Errorf("refused save still wrote the app: %q %q", got.Name, got.Domain)
		}
	}
}

// Without an admin-console row Rail's row would be proxied as the console.
func TestSetRailNeedsConsoleRow(t *testing.T) {
	svc, st, app, _ := mailFixture(t)
	if err := st.ReplaceAppDomains(app.ID, []string{app.Domain}, nil, true, "internal"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetRail(app.ID, true, ""); err == nil {
		t.Error("Rail went on with no admin console row")
	}
	if strings.Contains(composeOf(t, app), "  rail:") {
		t.Error("refused Rail still wrote its block")
	}
}

// A rail block edited past recognition still leaves Rail's hostname
// manageable: the card shows it on, and Turn off removes its route.
func TestRailRowSurvivesHandEdit(t *testing.T) {
	svc, st, app, _ := mailFixture(t)
	if _, err := svc.SetRail(app.ID, true, "rail.demo.test"); err != nil {
		t.Fatal(err)
	}
	y := composeOf(t, app)
	lines := strings.Split(y, "\n")
	for i, l := range lines {
		if railPortRE.MatchString(l) {
			lines[i] = "      - target: 8080"
		}
	}
	os.WriteFile(app.ComposePath, []byte(strings.Join(lines, "\n")), 0o644)

	w := svc.WebmailState(app)
	if !w.Rail || w.RailHost != "rail.demo.test" {
		t.Fatalf("state after hand edit = %+v, want Rail on at rail.demo.test", w)
	}
	if _, err := svc.SetRail(app.ID, true, "rail.demo.test"); err != nil {
		t.Fatalf("re-enabling on its own host: %v", err)
	}
	if _, err := svc.SetRail(app.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if ds, _ := st.AppServiceDomains(app.ID); len(ds) != 1 {
		t.Errorf("service domains after off = %v, want the console only", ds)
	}
}

// If Rail can't start, nothing routes to it and the compose file is as before.
func TestSetRailRollsBackOnFailedStart(t *testing.T) {
	svc, st, app, _ := mailFixture(t)
	if err := st.SetAppStatus(app.ID, store.AppRunning); err != nil {
		t.Fatal(err)
	}
	before := composeOf(t, app)
	failingEngine(t)
	if _, err := svc.SetRail(app.ID, true, ""); err == nil {
		t.Fatal("SetRail succeeded with an engine that fails")
	}
	if ds, _ := st.AppServiceDomains(app.ID); len(ds) != 1 {
		t.Errorf("service domains = %v, want only the console", ds)
	}
	if composeOf(t, app) != before {
		t.Error("compose file not restored after Rail failed to start")
	}
}

// A running app whose webmail container can't be removed keeps its file.
func TestSetSnappyMailOffStopsOnFailedRemove(t *testing.T) {
	svc, st, app, _ := mailFixture(t)
	if _, err := svc.SetRail(app.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppStatus(app.ID, store.AppRunning); err != nil {
		t.Fatal(err)
	}
	before := composeOf(t, app)
	failingEngine(t)
	if err := svc.SetSnappyMail(app.ID, false); err == nil {
		t.Fatal("SetSnappyMail(off) succeeded though its container could not be removed")
	}
	if composeOf(t, app) != before {
		t.Error("compose file changed though the container is still there")
	}
}
