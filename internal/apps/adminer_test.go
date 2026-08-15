package apps

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// laravelFixture lays out a Laravel app the way Create would, without starting
// anything. noAdminer decides whether it is created with the bundled Adminer.
func laravelFixture(t *testing.T, noAdminer bool) (*Service, *store.Store, store.App) {
	t.Helper()
	svc, st, proj := editFixture(t)
	app := store.App{ProjectID: proj.ID, Name: "shop", Slug: "shop", Type: "laravel",
		Domain: "shop.demo.test", DBMode: "dedicated", Runtime: "docker"}
	opts := CreateOpts{Name: "shop", Type: "laravel", Domain: "shop.demo.test",
		DBMode: "dedicated", NoAdminer: noAdminer}
	adminerPort, err := svc.layoutContainer(&app, &opts, proj, filepath.Join(proj.Dir, app.Slug))
	if err != nil {
		t.Fatalf("layoutContainer: %v", err)
	}
	saved, err := st.CreateApp(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAppDomains(saved.ID, []string{app.Domain}, nil, true, "internal"); err != nil {
		t.Fatal(err)
	}
	if adminerPort > 0 {
		if err := st.CreateDomain(saved.ID, "adminer."+app.Domain, true, "internal", adminerPort); err != nil {
			t.Fatal(err)
		}
	}
	return svc, st, saved
}

func composeOf(t *testing.T, app store.App) string {
	t.Helper()
	b, err := os.ReadFile(app.ComposePath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Creating a Laravel app with Adminer declined allocates no second port, writes
// no adminer service, and claims no second hostname — the three things that
// together are what "having Adminer" means.
func TestCreateLaravelWithoutAdminer(t *testing.T) {
	_, st, app := laravelFixture(t, true)
	if strings.Contains(composeOf(t, app), "adminer:") {
		t.Error("the compose file has an adminer service")
	}
	svc, err := st.AppServiceDomains(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(svc) != 0 {
		t.Errorf("service domains = %v, want none", svc)
	}
}

// The toggle is reversible: off then on leaves the app with an Adminer again,
// on a port of its own and a hostname that routes.
func TestSetAdminerRoundTrip(t *testing.T) {
	svc, st, app := laravelFixture(t, false)
	if !strings.Contains(composeOf(t, app), "adminer:") {
		t.Fatal("the fixture was created without an adminer service")
	}

	if host, err := svc.SetAdminer(app.ID, false); err != nil || host != "" {
		t.Fatalf("SetAdminer(off) = %q, %v", host, err)
	}
	if strings.Contains(composeOf(t, app), "adminer:") {
		t.Error("the adminer service survived being switched off")
	}
	if d, _ := st.AppServiceDomains(app.ID); len(d) != 0 {
		t.Errorf("adminer hostname still routes after switching it off: %v", d)
	}
	// The app's own hostname is untouched — only the Adminer one goes.
	if hosts, _ := st.AppHostnames(app.ID); len(hosts) != 1 || hosts[0] != app.Domain {
		t.Errorf("app hostnames = %v, want [%s]", hosts, app.Domain)
	}
	// The previous file is kept, the same way a hand edit keeps one.
	if _, err := os.Stat(app.ComposePath + ".bak"); err != nil {
		t.Errorf("no compose.yml.bak beside the rewritten file: %v", err)
	}

	host, err := svc.SetAdminer(app.ID, true)
	if err != nil {
		t.Fatalf("SetAdminer(on): %v", err)
	}
	if host != "adminer.shop.demo.test" {
		t.Errorf("published %q, want adminer.shop.demo.test", host)
	}
	yaml := composeOf(t, app)
	if !strings.Contains(yaml, "adminer:") || !strings.Contains(yaml, ":8080") {
		t.Errorf("the adminer service did not come back:\n%s", yaml)
	}
	d, _ := st.AppServiceDomains(app.ID)
	if len(d) != 1 || d[0].Host != host || d[0].Port == 0 {
		t.Errorf("service domains = %v, want one routed hostname with a port", d)
	}
	// The port in the file has to be the one the route points at, or the
	// hostname answers nothing.
	if !strings.Contains(yaml, `"`+strconv.Itoa(d[0].Port)+`:8080"`) {
		t.Errorf("the file publishes a different port than the route uses (%d):\n%s", d[0].Port, yaml)
	}
	// And it must not have taken the app's own port.
	if d[0].Port == app.Port {
		t.Error("adminer was given the app's own host port")
	}
}

// Switching to the state it is already in changes nothing at all — no rewrite,
// no port allocation, no backup file.
func TestSetAdminerIsIdempotent(t *testing.T) {
	svc, _, app := laravelFixture(t, false)
	before := composeOf(t, app)
	if host, err := svc.SetAdminer(app.ID, true); err != nil || host != "" {
		t.Fatalf("SetAdminer(on) on an app that has it = %q, %v", host, err)
	}
	if composeOf(t, app) != before {
		t.Error("the compose file was rewritten for a no-op")
	}
	if _, err := os.Stat(app.ComposePath + ".bak"); err == nil {
		t.Error("a no-op left a backup file behind")
	}
}

// Only Laravel bundles an Adminer, so only Laravel is offered the switch.
func TestSetAdminerRefusesOtherTypes(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := newComposeApp(t, svc, st, proj, "byo",
		"services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"${PORT}:80\"\n")
	if _, err := svc.SetAdminer(app.ID, false); err == nil {
		t.Error("a compose app was allowed to switch an Adminer it does not have")
	}
}

// laravelCompose renders a real Laravel stack, which is what the surgery below
// has to survive — a hand-written fixture would only prove the functions agree
// with the fixture.
func laravelCompose(t *testing.T, adminerPort int, env string) string {
	t.Helper()
	yaml, err := templates.RenderCompose("laravel", templates.Data{
		ProjectSlug: "demo", AppSlug: "api", AppType: "laravel", Env: env,
		AppImage: "example/swoole:1", HostPort: 20000, AdminerPort: adminerPort,
	})
	if err != nil {
		t.Fatal(err)
	}
	return yaml
}

// A Laravel app created without Adminer gets a compose file with no adminer
// service and nothing published on its port — the port is never allocated, so a
// stray reference would name one belonging to another app.
func TestLaravelComposeWithoutAdminer(t *testing.T) {
	for _, env := range []string{"local", "prod"} {
		yaml := laravelCompose(t, 0, env)
		if strings.Contains(yaml, "adminer:") || strings.Contains(yaml, "8080") {
			t.Errorf("%s: adminer service still rendered with AdminerPort 0:\n%s", env, yaml)
		}
		// The rest of the stack has to be intact, not just adminer-free.
		for _, want := range []string{"services:", "  app:", "  redis:", "networks:"} {
			if !strings.Contains(yaml, want) {
				t.Errorf("%s: %q missing from a stack rendered without adminer", env, want)
			}
		}
	}
}

// Removing the adminer service must leave a file that still reads as the same
// file: every other service present, and no double blank line where the block
// used to be.
func TestComposeRemoveService(t *testing.T) {
	for _, env := range []string{"local", "prod"} {
		with := laravelCompose(t, 20001, env)
		without := laravelCompose(t, 0, env)

		got, found := composeRemoveService(with, "adminer")
		if !found {
			t.Fatalf("%s: adminer service not found in a stack that has one", env)
		}
		if got != without {
			t.Errorf("%s: removing adminer did not reproduce the stack rendered without it\n--- got ---\n%s\n--- want ---\n%s",
				env, got, without)
		}
	}
}

// And putting it back has to reproduce the file it was removed from, so the two
// directions of the toggle are inverses rather than two different files.
func TestComposeAddService(t *testing.T) {
	for _, env := range []string{"local", "prod"} {
		with := laravelCompose(t, 20001, env)
		without := laravelCompose(t, 0, env)

		block, err := templates.RenderPartial("laravel", "adminer_service", templates.Data{
			ProjectSlug: "demo", AppSlug: "api", AppType: "laravel", Env: env, AdminerPort: 20001,
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := composeAddService(without, block)
		if err != nil {
			t.Fatal(err)
		}
		if got != with {
			t.Errorf("%s: adding adminer back did not reproduce the rendered stack\n--- got ---\n%s\n--- want ---\n%s",
				env, got, with)
		}
	}
}

// A file that has been edited by hand keeps its edits: the toggle changes the
// adminer block and nothing else, which is the whole reason it edits the file
// instead of re-rendering it.
func TestComposeRemoveServiceKeepsEdits(t *testing.T) {
	yaml := laravelCompose(t, 20001, "local")
	edited := strings.Replace(yaml,
		"  redis:\n", "  # a note somebody added\n  redis:\n", 1)
	if edited == yaml {
		t.Fatal("fixture did not apply")
	}
	got, found := composeRemoveService(edited, "adminer")
	if !found {
		t.Fatal("adminer not found")
	}
	if !strings.Contains(got, "# a note somebody added") {
		t.Error("a hand-written comment did not survive removing the adminer service")
	}
	if strings.Contains(got, "adminer:\n") {
		t.Error("the adminer service survived")
	}
}

// A service that isn't there is reported as not there rather than removing
// something else — the toggle logs that case instead of writing a mangled file.
func TestComposeRemoveServiceMissing(t *testing.T) {
	yaml := laravelCompose(t, 0, "local")
	got, found := composeRemoveService(yaml, "adminer")
	if found {
		t.Error("found an adminer service in a stack rendered without one")
	}
	if got != yaml {
		t.Error("the file changed even though nothing was found to remove")
	}
}

// The insertion point is the end of the services section — not the end of the
// file, where it would land under `networks:` and belong to it.
func TestComposeAddServiceRefusesWithoutServices(t *testing.T) {
	if _, err := composeAddService("networks:\n  internal:\n", "  x:\n    image: y\n"); err == nil {
		t.Error("expected an error adding a service to a file with no services: section")
	}
}
