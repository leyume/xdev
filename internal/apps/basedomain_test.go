package apps

// A project's base domain can be edited or removed from the project page, so
// "this project has no base domain" is a state apps have to be created in.

import (
	"testing"

	"xdev/internal/store"
)

// TestCreateWithNoBaseDomainGivesNoHostname pins the behaviour that letting a
// project's base domain be removed depends on.
//
// A blank domain field falls back to the project's base domain, and with no
// base domain there is nothing to fall back to. The app must end up with no
// hostname at all — the port-only answer — rather than one derived from an
// empty string. Which layer enforces that is not the point of the test: the
// point is that no route and no certificate are ever asked for.
func TestCreateWithNoBaseDomainGivesNoHostname(t *testing.T) {
	s, st, _ := editFixture(t)
	proj, err := st.CreateProject(store.Project{
		Name: "Bare", Slug: "bare", BaseDomain: "", Environment: "local",
		NetworkName: "xdev_bare", Engine: "docker", Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	app, err := s.Create(proj.ID, CreateOpts{Name: "site", Type: store.TypeStatic, Domain: ""})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if app.Domain != "" {
		t.Errorf("app.Domain = %q, want empty — there was no base domain to derive one from", app.Domain)
	}
	hosts, err := st.AppHostnames(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Errorf("domain rows = %#v, want none — a blank hostname is not a route", hosts)
	}
}

// TestCreateWithNoBaseDomainStillTakesAGivenOne: removing the base domain drops
// the default, not the ability to name an app.
func TestCreateWithNoBaseDomainStillTakesAGivenOne(t *testing.T) {
	s, st, _ := editFixture(t)
	proj, err := st.CreateProject(store.Project{
		Name: "Bare2", Slug: "bare2", BaseDomain: "", Environment: "local",
		NetworkName: "xdev_bare2", Engine: "docker", Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	app, err := s.Create(proj.ID, CreateOpts{Name: "site", Type: store.TypeStatic, Domain: "chosen.test"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if app.Domain != "chosen.test" {
		t.Errorf("app.Domain = %q, want chosen.test", app.Domain)
	}
}
