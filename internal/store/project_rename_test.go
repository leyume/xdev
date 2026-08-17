package store

import "testing"

// Renaming changes the label and nothing else. The slug names the project's
// directory, its container network, every container in every app's stack and
// the databases those apps authenticate against — so if a rename touched it,
// the row would stop describing the infrastructure on disk.
func TestRenameProjectLeavesIdentityAlone(t *testing.T) {
	st := testStore(t)
	p, err := st.CreateProject(Project{
		Name: "Servorien", Slug: "servorien", BaseDomain: "servorien.test",
		Environment: "prod", NetworkName: "xdev_servorien", Engine: "docker",
		Dir: "/var/lib/xdev/projects/servorien",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := st.RenameProject(p.ID, "Servorien Group"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := st.ProjectByID(p.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Name != "Servorien Group" {
		t.Errorf("name = %q, want %q", got.Name, "Servorien Group")
	}
	for _, tc := range []struct{ field, got, want string }{
		{"slug", got.Slug, p.Slug},
		{"base domain", got.BaseDomain, p.BaseDomain},
		{"network", got.NetworkName, p.NetworkName},
		{"dir", got.Dir, p.Dir},
		{"engine", got.Engine, p.Engine},
		{"environment", got.Environment, p.Environment},
	} {
		if tc.got != tc.want {
			t.Errorf("rename changed the %s: %q, want %q", tc.field, tc.got, tc.want)
		}
	}

	// The old slug still resolves, because it never moved.
	if _, err := st.ProjectBySlug("servorien"); err != nil {
		t.Errorf("the project is no longer reachable at its slug: %v", err)
	}
}

// A rename must not disturb the project's apps.
func TestRenameProjectKeepsItsApps(t *testing.T) {
	st := testStore(t)
	pid, ids := seedProject(t, st, "demo", "api", "web")

	if err := st.SetAppOrder(pid, []int64{ids[1], ids[0]}); err != nil {
		t.Fatalf("SetAppOrder: %v", err)
	}
	if err := st.RenameProject(pid, "A Better Name"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := listIDs(t, st, pid); !sameIDs(got, []int64{ids[1], ids[0]}) {
		t.Errorf("apps after a rename = %v, want %v", got, []int64{ids[1], ids[0]})
	}
}
