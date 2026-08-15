package apps

import (
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
)

// A create that fails must not have published the app's hostname.
//
// A domain row is live routing: the next reconcile points Caddy at the app and,
// in a prod project, asks Let's Encrypt for a certificate. If the row went in
// before the stack came up, a failed create left the hostname resolving to a
// 502 for an app that does not work, and burned an issuance on it. So the rows
// go on last, and this test is what keeps them there — the ordering is invisible
// in the code once it is right, and easy to undo by accident.
//
// The project is pinned to an engine binary that cannot exist, so the start
// fails the same way on a machine with a container engine and on one without.
func TestCreateDoesNotPublishDomainsWhenStartFails(t *testing.T) {
	svc, st, _ := editFixture(t)
	proj, err := st.CreateProject(store.Project{
		Name: "Broken", Slug: "broken", BaseDomain: "broken.test", Environment: "prod",
		NetworkName: "xdev_broken", Engine: "xdev-no-such-engine", Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	app, err := svc.Create(proj.ID, CreateOpts{
		Name: "web", Type: store.TypeCompose, Domain: "web.broken.test",
		ComposeFile: "services:\n  web:\n    image: nginx:alpine\n    ports:\n      - \"${PORT}:80\"\n",
	})
	if err == nil {
		t.Fatal("expected the create to fail: its engine does not exist")
	}
	if app.ID == 0 {
		t.Fatal("the app row should survive a failed start, so the UI can show it in an error state")
	}

	// Neither the app's own hostname nor any service hostname may be routing.
	hosts, err := st.AppHostnames(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Errorf("failed create published hostnames %v", hosts)
	}
	svcDomains, err := st.AppServiceDomains(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(svcDomains) != 0 {
		t.Errorf("failed create published service hostnames %v", svcDomains)
	}
	if owner := st.DomainOwner("web.broken.test"); owner != 0 {
		t.Errorf("web.broken.test is claimed by app %d after a failed create", owner)
	}
}

// The mirror of the above: a create that works does publish them, so the test
// above cannot be satisfied by never attaching a domain at all.
func TestCreatePublishesDomainsOnSuccess(t *testing.T) {
	svc, st, proj := editFixture(t)
	app, err := svc.Create(proj.ID, CreateOpts{
		Name: "site", Type: store.TypeStatic, Domain: "site.demo.test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	hosts, err := st.AppHostnames(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0] != "site.demo.test" {
		t.Errorf("hostnames = %v, want [site.demo.test]", hosts)
	}
	if _, err := filepath.Rel(proj.Dir, filepath.Join(proj.Dir, app.Slug)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(app.Slug, "site") {
		t.Errorf("slug = %q", app.Slug)
	}
}
