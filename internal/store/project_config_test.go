package store

import "testing"

// domainFlags reads back the cert settings of every domain row belonging to a
// project's apps, keyed by hostname.
func domainFlags(t *testing.T, st *Store, projectID int64) map[string][2]string {
	t.Helper()
	rows, err := st.db.Query(`
		SELECT d.hostname, d.is_local, d.ssl_mode FROM domains d
		JOIN apps a ON a.id = d.app_id WHERE a.project_id = ?`, projectID)
	if err != nil {
		t.Fatalf("read domains: %v", err)
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var host, isLocal, mode string
		if err := rows.Scan(&host, &isLocal, &mode); err != nil {
			t.Fatal(err)
		}
		out[host] = [2]string{isLocal, mode}
	}
	return out
}

// configFixture is a prod project with one app answering on two hostnames, one
// of which is a secondary-service route (an Adminer port).
func configFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	st := testStore(t)
	p, err := st.CreateProject(Project{
		Name: "Demo", Slug: "demo", BaseDomain: "demo.com", Environment: "prod",
		NetworkName: "xdev_demo", Dir: "/tmp/demo",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	a, err := st.CreateApp(App{ProjectID: p.ID, Name: "web", Slug: "web", Type: "static"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := st.ReplaceAppDomains(a.ID, []string{"demo.com", "www.demo.com"},
		[]ServiceDomain{{Host: "adminer.demo.com", Port: 20001}}, false, "letsencrypt"); err != nil {
		t.Fatalf("attach domains: %v", err)
	}
	return st, p.ID
}

// TestSetProjectConfigRestampsDomains is the point of the whole feature.
//
// environment is only ever read when an app is created or edited, and baked
// into per-domain is_local/ssl_mode — which is what actually decides internal
// certificate versus Let's Encrypt. Changing the project row alone would give a
// project labelled "local" whose apps still go to ACME: a switch that looks
// like it worked and changed nothing.
func TestSetProjectConfigRestampsDomains(t *testing.T) {
	st, pid := configFixture(t)

	before := domainFlags(t, st, pid)
	if len(before) != 3 {
		t.Fatalf("fixture has %d domains, want 3", len(before))
	}
	for host, got := range before {
		if got != [2]string{"0", "letsencrypt"} {
			t.Fatalf("%s starts at %v, want prod settings — the fixture is wrong", host, got)
		}
	}

	if err := st.SetProjectConfig(pid, "demo.test", "local", true, "internal"); err != nil {
		t.Fatalf("SetProjectConfig: %v", err)
	}

	// Every hostname, including the Adminer route: it needs a certificate too,
	// and leaving it on ACME would fail on a machine with no public DNS.
	for host, got := range domainFlags(t, st, pid) {
		if got != [2]string{"1", "internal"} {
			t.Errorf("%s = %v after switching to local, want is_local=1 ssl_mode=internal", host, got)
		}
	}
	p, err := st.ProjectByID(pid)
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseDomain != "demo.test" || p.Environment != "local" {
		t.Errorf("project = %q/%q, want demo.test/local", p.BaseDomain, p.Environment)
	}
}

// TestSetProjectConfigLeavesOtherProjectsAlone: the restamp is scoped by
// project, so switching one does not re-certificate somebody else's apps.
func TestSetProjectConfigLeavesOtherProjectsAlone(t *testing.T) {
	st, pid := configFixture(t)

	other, err := st.CreateProject(Project{
		Name: "Other", Slug: "other", BaseDomain: "other.com", Environment: "prod",
		NetworkName: "xdev_other", Dir: "/tmp/other",
	})
	if err != nil {
		t.Fatal(err)
	}
	oa, err := st.CreateApp(App{ProjectID: other.ID, Name: "site", Slug: "site", Type: "static"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAppDomains(oa.ID, []string{"other.com"}, nil, false, "letsencrypt"); err != nil {
		t.Fatal(err)
	}

	if err := st.SetProjectConfig(pid, "demo.test", "local", true, "internal"); err != nil {
		t.Fatalf("SetProjectConfig: %v", err)
	}

	if got := domainFlags(t, st, other.ID)["other.com"]; got != [2]string{"0", "letsencrypt"} {
		t.Errorf("other.com = %v, want its prod settings untouched", got)
	}
}

// TestSetProjectConfigKeepsHostnames: switching environment re-certificates,
// it does not rename. An app answering on demo.com keeps answering on demo.com
// even once the project's base domain says demo.test.
func TestSetProjectConfigKeepsHostnames(t *testing.T) {
	st, pid := configFixture(t)

	if err := st.SetProjectConfig(pid, "demo.test", "local", true, "internal"); err != nil {
		t.Fatalf("SetProjectConfig: %v", err)
	}
	got := domainFlags(t, st, pid)
	for _, host := range []string{"demo.com", "www.demo.com", "adminer.demo.com"} {
		if _, ok := got[host]; !ok {
			t.Errorf("%s disappeared — changing the base domain must not rewrite live hostnames", host)
		}
	}
}
