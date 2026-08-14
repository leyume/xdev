package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
)

// editFixture builds a store with a project and returns the service for it.
func editFixture(t *testing.T) (*Service, *store.Store, store.Project) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	proj, err := st.CreateProject(store.Project{
		Name: "Demo", Slug: "demo", BaseDomain: "demo.test", Environment: "local",
		NetworkName: "xdev_demo", Engine: "docker", Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return New(st, nil, nil, ""), st, proj
}

// newComposeApp lays out a compose app the way Create would (files, ports,
// domain rows) without needing a container engine.
func newComposeApp(t *testing.T, svc *Service, st *store.Store, proj store.Project, slug, yaml string, extras ...string) store.App {
	t.Helper()
	app := store.App{ProjectID: proj.ID, Name: slug, Slug: slug, Type: store.TypeCompose,
		Domain: slug + ".demo.test", Status: store.AppStopped}
	routes, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: yaml, ExtraDomains: extras},
		proj, filepath.Join(proj.Dir, slug), []string{app.Domain})
	if err != nil {
		t.Fatalf("layoutCompose: %v", err)
	}
	saved, err := st.CreateApp(app)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := st.ReplaceAppDomains(saved.ID, []string{app.Domain}, nil, true, "internal"); err != nil {
		t.Fatalf("attach domain: %v", err)
	}
	for _, r := range routes {
		if err := st.CreateDomain(saved.ID, r.Host, true, "internal", r.Port); err != nil {
			t.Fatalf("attach %s: %v", r.Host, err)
		}
	}
	return saved
}

const oneSlot = "services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"${PORT}:80\"\n"
const twoSlots = "services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"${PORT}:80\"\n" +
	"  api:\n    image: api:1\n    ports:\n      - \"${PORT_2}:3000\"\n"

// TestUpdateRenamesAndMovesDomain covers the two edits every type shares.
func TestUpdateRenamesAndMovesDomain(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := newComposeApp(t, svc, st, proj, "byo", oneSlot)
	port := app.Port

	got, err := svc.Update(app.ID, EditOpts{Name: "Renamed", Domain: "  NEW.demo.test  "})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "Renamed" {
		t.Errorf("name = %q", got.Name)
	}
	if got.Domain != "new.demo.test" {
		t.Errorf("domain = %q, want it normalized", got.Domain)
	}
	if got.Slug != app.Slug {
		t.Errorf("slug moved to %q — it names the app's directory and containers", got.Slug)
	}
	if got.Port != port {
		t.Errorf("port moved to %d, want the allocated %d to stay put", got.Port, port)
	}
	// The proxy routes off the domains table, so the old hostname must be gone.
	if owner := st.DomainOwner("byo.demo.test"); owner != 0 {
		t.Errorf("old hostname still routes to app %d", owner)
	}
	if owner := st.DomainOwner("new.demo.test"); owner != app.ID {
		t.Errorf("new hostname routes to app %d, want %d", owner, app.ID)
	}
}

// TestUpdateRejectsWithoutChanging is the property the whole settings page rests
// on: a rejected save leaves the app exactly as it was, on disk and in the DB.
func TestUpdateRejectsWithoutChanging(t *testing.T) {
	svc, st, proj := editFixture(t)
	other := newComposeApp(t, svc, st, proj, "other", oneSlot)
	app := newComposeApp(t, svc, st, proj, "byo", oneSlot)
	before := readFile(t, app.ComposePath)

	for name, opts := range map[string]EditOpts{
		"blank name":          {Name: "", Domain: "byo.demo.test", ComposeFile: twoSlots},
		"blank domain":        {Name: "byo", Domain: "", ComposeFile: twoSlots},
		"domain taken":        {Name: "byo", Domain: other.Domain, ComposeFile: twoSlots},
		"broken compose":      {Name: "byo", Domain: "byo.demo.test", ComposeFile: "web:\n  image: nginx\n"},
		"unpublished slot":    {Name: "byo", Domain: "byo.demo.test", ComposeFile: oneSlot, ExtraDomains: []string{"api.demo.test"}},
		"slot with no domain": {Name: "byo", Domain: "byo.demo.test", ComposeFile: twoSlots},
		"blank extra domain":  {Name: "byo", Domain: "byo.demo.test", ComposeFile: twoSlots, ExtraDomains: []string{""}},
	} {
		if _, err := svc.Update(app.ID, opts); err == nil {
			t.Errorf("%s: should have been rejected", name)
		}
		fresh, err := st.AppByID(app.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Name != app.Name || fresh.Domain != app.Domain {
			t.Fatalf("%s: the row changed anyway: %+v", name, fresh)
		}
		if got := readFile(t, app.ComposePath); got != before {
			t.Fatalf("%s: the compose file changed anyway:\n%s", name, got)
		}
	}
}

// TestUpdateComposeFileAndSlots covers a compose app growing a second domain:
// the new file and the new hostname are validated together, the existing slot
// keeps its port, and the new one is allocated and written into _/.env.
func TestUpdateComposeFileAndSlots(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := newComposeApp(t, svc, st, proj, "byo", oneSlot)

	got, err := svc.Update(app.ID, EditOpts{
		Name: "byo", Domain: app.Domain, ComposeFile: twoSlots, ExtraDomains: []string{"api.demo.test"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Port != app.Port {
		t.Errorf("the app's own port moved from %d to %d", app.Port, got.Port)
	}
	if body := readFile(t, app.ComposePath); !strings.Contains(body, "${PORT_2}") {
		t.Errorf("compose file not saved:\n%s", body)
	}
	if _, err := os.Stat(app.ComposePath + ".bak"); err != nil {
		t.Errorf("no backup of the replaced file: %v", err)
	}
	svcDoms, err := st.AppServiceDomains(app.ID)
	if err != nil || len(svcDoms) != 1 {
		t.Fatalf("service domains = %+v (%v), want one", svcDoms, err)
	}
	if svcDoms[0].Host != "api.demo.test" {
		t.Errorf("slot 2 hostname = %q", svcDoms[0].Host)
	}
	if svcDoms[0].Port == 0 || svcDoms[0].Port == app.Port {
		t.Errorf("slot 2 port = %d, want a fresh one", svcDoms[0].Port)
	}
	env := readFile(t, filepath.Join(filepath.Dir(app.ComposePath), ".env"))
	if !strings.Contains(env, "PORT_2=") {
		t.Errorf("_/.env has no PORT_2:\n%s", env)
	}

	// …and shrinking again: the file drops back to one slot, the domain row and
	// its port go with it.
	if _, err := svc.Update(app.ID, EditOpts{Name: "byo", Domain: app.Domain, ComposeFile: oneSlot}); err != nil {
		t.Fatalf("Update back to one domain: %v", err)
	}
	if svcDoms, _ := st.AppServiceDomains(app.ID); len(svcDoms) != 0 {
		t.Errorf("service domains = %+v, want none", svcDoms)
	}
	if owner := st.DomainOwner("api.demo.test"); owner != 0 {
		t.Errorf("the removed hostname still routes to app %d", owner)
	}
}

// TestUpdateRenamesASlotKeepingItsPort: renaming domain 2 must not move the port
// behind it — the service answering there hasn't changed.
func TestUpdateRenamesASlotKeepingItsPort(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := newComposeApp(t, svc, st, proj, "byo", twoSlots, "api.demo.test")
	before, _ := st.AppServiceDomains(app.ID)

	if _, err := svc.Update(app.ID, EditOpts{
		Name: "byo", Domain: app.Domain, ComposeFile: twoSlots, ExtraDomains: []string{"admin.demo.test"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, _ := st.AppServiceDomains(app.ID)
	if len(after) != 1 || after[0].Host != "admin.demo.test" {
		t.Fatalf("service domains = %+v", after)
	}
	if after[0].Port != before[0].Port {
		t.Errorf("port moved from %d to %d on a rename", before[0].Port, after[0].Port)
	}
}

// TestUpdateSwapsPrimaryAndExtraDomain: promoting a stack's second hostname to
// its first is one transaction — the unique hostname index must not see both
// states at once.
func TestUpdateSwapsPrimaryAndExtraDomain(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := newComposeApp(t, svc, st, proj, "byo", twoSlots, "api.demo.test")

	got, err := svc.Update(app.ID, EditOpts{
		Name: "byo", Domain: "api.demo.test", ComposeFile: twoSlots, ExtraDomains: []string{"byo.demo.test"},
	})
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	if got.Domain != "api.demo.test" {
		t.Errorf("primary domain = %q", got.Domain)
	}
	after, _ := st.AppServiceDomains(app.ID)
	if len(after) != 1 || after[0].Host != "byo.demo.test" {
		t.Errorf("service domains = %+v", after)
	}
}

// TestUpdateRenamesTheBundledUIDomain: the types that ship a second web UI
// (Laravel's Adminer, the mail console) can move its hostname, but not its port
// — the container behind it hasn't moved — and not onto the app's own domain.
func TestUpdateRenamesTheBundledUIDomain(t *testing.T) {
	svc, st, proj := editFixture(t)
	saved, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "app", Slug: "app", Type: "laravel",
		Domain: "app.demo.test", Port: 20030, DBMode: store.DBShared})
	if err != nil {
		t.Fatal(err)
	}
	st.ReplaceAppDomains(saved.ID, []string{"app.demo.test"}, nil, true, "internal")
	if err := st.CreateDomain(saved.ID, "adminer.app.demo.test", true, "internal", 20031); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Update(saved.ID, EditOpts{Name: "app", Domain: "app.demo.test",
		ServiceDomain: "db.app.demo.test"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, _ := st.AppServiceDomains(saved.ID)
	if len(after) != 1 || after[0].Host != "db.app.demo.test" {
		t.Fatalf("service domains = %+v", after)
	}
	if after[0].Port != 20031 {
		t.Errorf("Adminer port moved to %d, want 20031", after[0].Port)
	}

	// Blank means "leave it alone" — the form only sends this field for the
	// types that have one, and a missing field must not delete the route.
	if _, err := svc.Update(saved.ID, EditOpts{Name: "app", Domain: "app.demo.test"}); err != nil {
		t.Fatalf("Update without the field: %v", err)
	}
	if after, _ := st.AppServiceDomains(saved.ID); len(after) != 1 || after[0].Host != "db.app.demo.test" {
		t.Errorf("the bundled UI's route was lost: %+v", after)
	}

	if _, err := svc.Update(saved.ID, EditOpts{Name: "app", Domain: "app.demo.test",
		ServiceDomain: "app.demo.test"}); err == nil {
		t.Error("pointing the bundled UI at the app's own domain should have been rejected")
	}
}

// TestUpdateHostApp covers a static app switching from serving files to running
// a command: it needs a port and a start command, and gets the defaults.
func TestUpdateHostApp(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := store.App{ProjectID: proj.ID, Name: "site", Slug: "site", Type: store.TypeStatic,
		Domain: "site.demo.test", ServeMode: store.ServeStatic, RootDir: "dist"}
	saved, err := st.CreateApp(app)
	if err != nil {
		t.Fatal(err)
	}
	st.ReplaceAppDomains(saved.ID, []string{app.Domain}, nil, true, "internal")

	got, err := svc.Update(saved.ID, EditOpts{
		Name: "site", Domain: app.Domain, ServeMode: store.ServeCommand, BuildCmd: " npm ci ",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.ServeMode != store.ServeCommand {
		t.Errorf("serve mode = %q", got.ServeMode)
	}
	if got.Port < portMin || got.Port > portMax {
		t.Errorf("port = %d, want one allocated for the process", got.Port)
	}
	if got.StartCmd != defaultStartCmd {
		t.Errorf("start command = %q, want the default filled in", got.StartCmd)
	}
	if got.BuildCmd != "npm ci" {
		t.Errorf("build command = %q, want it trimmed", got.BuildCmd)
	}

	// Back to serving files: the folder applies again, the command is kept but
	// unused (nothing is spawned in serve mode).
	got, err = svc.Update(saved.ID, EditOpts{
		Name: "site", Domain: app.Domain, ServeMode: store.ServeStatic, RootDir: "/build/",
	})
	if err != nil {
		t.Fatalf("Update back: %v", err)
	}
	if got.ServeMode != store.ServeStatic || got.RootDir != "build" {
		t.Errorf("serve mode = %q, root dir = %q", got.ServeMode, got.RootDir)
	}
}

// TestUpdateProxyUpstream: the upstream is validated the same way create does.
func TestUpdateProxyUpstream(t *testing.T) {
	svc, st, proj := editFixture(t)
	saved, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "api", Slug: "api", Type: store.TypeProxy,
		Domain: "api.demo.test", Upstream: "http://10.0.0.5:3000"})
	if err != nil {
		t.Fatal(err)
	}
	st.ReplaceAppDomains(saved.ID, []string{"api.demo.test"}, nil, true, "internal")

	if _, err := svc.Update(saved.ID, EditOpts{Name: "api", Domain: "api.demo.test", Upstream: "not a url"}); err == nil {
		t.Error("a bad upstream should have been rejected")
	}
	got, err := svc.Update(saved.ID, EditOpts{Name: "api", Domain: "api.demo.test", Upstream: "https://elsewhere.example/"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Upstream != "https://elsewhere.example" {
		t.Errorf("upstream = %q, want it normalized", got.Upstream)
	}
}

// TestUpdateTemplatedComposeFile: a generated stack's file is the user's to edit
// too, but xdev doesn't hold it to the ${PORT_n} slot contract — its ports were
// written in at create.
func TestUpdateTemplatedComposeFile(t *testing.T) {
	svc, st, proj := editFixture(t)
	dir := filepath.Join(proj.Dir, "app", "_")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, []byte("services:\n  app:\n    ports:\n      - \"20050:80\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	saved, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "app", Slug: "app", Type: "laravel",
		Domain: "app.demo.test", Port: 20050, ComposePath: path})
	if err != nil {
		t.Fatal(err)
	}
	st.ReplaceAppDomains(saved.ID, []string{"app.demo.test"}, nil, true, "internal")

	edited := "services:\n  app:\n    ports:\n      - \"20050:80\"\n    deploy:\n      resources:\n        limits:\n          memory: 512m\n"
	if _, err := svc.Update(saved.ID, EditOpts{Name: "app", Domain: "app.demo.test", ComposeFile: edited}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := readFile(t, path); !strings.Contains(got, "memory: 512m") {
		t.Errorf("edit not saved:\n%s", got)
	}
	// Structurally broken is still refused, whoever wrote the file.
	if _, err := svc.Update(saved.ID, EditOpts{Name: "app", Domain: "app.demo.test", ComposeFile: "app:\n  image: x\n"}); err == nil {
		t.Error("a file with no services: block should have been rejected")
	}
}

// TestUpdateRejectsMoreDomainsThanTheCap keeps the settings page honest about
// the same limit the add-app form enforces.
func TestUpdateRejectsMoreDomainsThanTheCap(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := newComposeApp(t, svc, st, proj, "byo", oneSlot)

	extras := make([]string, MaxComposeSlots) // one too many with the app's own
	yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n"
	for i := range extras {
		extras[i] = "x" + itoa(i) + ".demo.test"
		yaml += "  s" + itoa(i) + ":\n    ports:\n      - \"${PORT_" + itoa(i+2) + "}:80\"\n"
	}
	if _, err := svc.Update(app.ID, EditOpts{Name: "byo", Domain: app.Domain, ComposeFile: yaml, ExtraDomains: extras}); err == nil {
		t.Errorf("%d domains should have been rejected (cap is %d)", len(extras)+1, MaxComposeSlots)
	}
}
