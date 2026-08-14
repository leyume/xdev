package apps

import (
	"strings"
	"testing"

	"xdev/internal/store"
)

// TestParseHosts pins the domain field's grammar: one field, several hostnames,
// separators people actually type. Order matters — the first is the app's own
// domain, the one the UI links to and every other hostname is checked against.
func TestParseHosts(t *testing.T) {
	for in, want := range map[string]string{
		"test.com":                     "test.com",
		"test.com,www.test.com":        "test.com www.test.com",
		"test.com, www.test.com":       "test.com www.test.com",
		" TEST.com ,  WWW.test.com\n":  "test.com www.test.com", // normalized + trimmed
		"test.com www.test.com":        "test.com www.test.com", // spaces separate too
		"a.com,b.com,c.com,d.com":      "a.com b.com c.com d.com",
		"www.test.com, test.com":       "www.test.com test.com", // order is the user's
		"":                             "",                      // callers decide what blank means
		"   ,  ":                       "",
		"test.com.ng, www.test.com.ng": "test.com.ng www.test.com.ng",
	} {
		got, err := parseHosts(in)
		if err != nil {
			t.Errorf("parseHosts(%q): %v", in, err)
			continue
		}
		if strings.Join(got, " ") != want {
			t.Errorf("parseHosts(%q) = %v, want %q", in, got, want)
		}
	}

	for _, in := range []string{
		"test.com, www.test.com, test.com", // the same name twice: a typo, not intent
		"test.com, WWW.TEST.COM, www.test.com",
		"test.com, http://www.test.com", // a URL, not a hostname
		"test.com, www.test.com:8080",   // a port
		"test.com, .www.test.com",
	} {
		if _, err := parseHosts(in); err == nil {
			t.Errorf("parseHosts(%q) should be rejected", in)
		}
	}
}

// TestUpdateMultipleDomains is the end-to-end shape of the feature: several
// hostnames in one field all become routes for the same app, the first is the
// app's domain, and a name dropped from the list stops routing.
func TestUpdateMultipleDomains(t *testing.T) {
	svc, st, proj := editFixture(t)
	saved, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "site", Slug: "site",
		Type: store.TypeStatic, Domain: "test.com", ServeMode: store.ServeStatic, RootDir: "dist"})
	if err != nil {
		t.Fatal(err)
	}
	st.ReplaceAppDomains(saved.ID, []string{"test.com"}, nil, false, "letsencrypt")

	got, err := svc.Update(saved.ID, EditOpts{
		Name: "site", Domain: "test.com, www.test.com, test.com.ng",
		ServeMode: store.ServeStatic, RootDir: "dist",
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Domain != "test.com" {
		t.Errorf("app domain = %q, want the first of the list", got.Domain)
	}
	hosts, err := st.AppHostnames(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(hosts, " ") != "test.com www.test.com test.com.ng" {
		t.Fatalf("hostnames = %v, want all three in entry order", hosts)
	}
	// Every one of them routes, and to the same place.
	routes, err := st.ProxyRoutes()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, r := range routes {
		seen[r.Host] = r.Root
	}
	for _, h := range hosts {
		if seen[h] == "" {
			t.Errorf("no route for %s (got %v)", h, seen)
		}
	}
	if seen["test.com"] != seen["www.test.com"] {
		t.Errorf("the two names serve different roots: %q vs %q", seen["test.com"], seen["www.test.com"])
	}

	// Dropping one from the field is how it stops routing.
	if _, err := svc.Update(saved.ID, EditOpts{
		Name: "site", Domain: "test.com, www.test.com",
		ServeMode: store.ServeStatic, RootDir: "dist",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if owner := st.DomainOwner("test.com.ng"); owner != 0 {
		t.Errorf("test.com.ng still owned by app %d after being removed from the list", owner)
	}
}

// TestCreateWithMultipleDomains covers the same field on the add-app form, plus
// the two things create decides on its own: the app's stored domain is the first
// name, and a list containing a hostname another app owns is refused outright
// rather than partly applied.
func TestCreateWithMultipleDomains(t *testing.T) {
	svc, st, proj := editFixture(t)

	app, err := svc.Create(proj.ID, CreateOpts{
		Name: "site", Type: store.TypeStatic, ServeMode: store.ServeStatic,
		Domain: "test.com, www.test.com, test.com.ng",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if app.Domain != "test.com" {
		t.Errorf("app domain = %q, want the first of the list", app.Domain)
	}
	hosts, err := st.AppHostnames(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(hosts, " ") != "test.com www.test.com test.com.ng" {
		t.Errorf("hostnames = %v, want all three in entry order", hosts)
	}

	// A list that collides anywhere is rejected whole — including the names in it
	// that were free, which must not be left attached to a half-made app.
	if _, err := svc.Create(proj.ID, CreateOpts{
		Name: "other", Type: store.TypeStatic, ServeMode: store.ServeStatic,
		Domain: "free.example.com, www.test.com",
	}); err == nil {
		t.Fatal("a list containing a taken hostname should be rejected")
	}
	if owner := st.DomainOwner("free.example.com"); owner != 0 {
		t.Errorf("free.example.com was attached (app %d) despite the create failing", owner)
	}
}

// TestUpdateRejectsATakenExtraDomain checks the collision check covers every
// name in the list, not just the first — otherwise the second one would be
// silently stolen from the app that has it.
func TestUpdateRejectsATakenExtraDomain(t *testing.T) {
	svc, st, proj := editFixture(t)
	mine, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "a", Slug: "a",
		Type: store.TypeStatic, Domain: "test.com", ServeMode: store.ServeStatic})
	if err != nil {
		t.Fatal(err)
	}
	st.ReplaceAppDomains(mine.ID, []string{"test.com"}, nil, false, "letsencrypt")
	theirs, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "b", Slug: "b",
		Type: store.TypeStatic, Domain: "www.test.com", ServeMode: store.ServeStatic})
	if err != nil {
		t.Fatal(err)
	}
	st.ReplaceAppDomains(theirs.ID, []string{"www.test.com"}, nil, false, "letsencrypt")

	_, err = svc.Update(mine.ID, EditOpts{
		Name: "a", Domain: "test.com, www.test.com", ServeMode: store.ServeStatic,
	})
	if err == nil {
		t.Fatal("claiming another app's hostname should be rejected")
	}
	// And nothing moved: the rejection happens before any write.
	if owner := st.DomainOwner("www.test.com"); owner != theirs.ID {
		t.Errorf("www.test.com owner = %d, want %d — a rejected save must change nothing", owner, theirs.ID)
	}
	hosts, _ := st.AppHostnames(mine.ID)
	if strings.Join(hosts, " ") != "test.com" {
		t.Errorf("hostnames = %v, want the original single one", hosts)
	}
}
