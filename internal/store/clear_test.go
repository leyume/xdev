package store

import (
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedApp creates the project + app rows a deployment needs to hang off.
func seedApp(t *testing.T, st *Store, name string) (projectID, appID int64) {
	t.Helper()
	proj, err := st.CreateProject(Project{
		Name: name, Slug: name, BaseDomain: name + ".test",
		Environment: "dev", NetworkName: name + "_net", Dir: "/tmp/" + name,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	app, err := st.CreateApp(App{
		ProjectID: proj.ID, Name: name, Slug: name, Type: "static",
		Domain: name + "." + proj.BaseDomain, Status: AppStopped,
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	return proj.ID, app.ID
}

// TestClearDeploymentsKeepsARunningOne is the whole reason ClearDeployments has
// a WHERE clause. A running row is not just history: the goroutine building
// right now will write its result there, and its presence is how a second
// deploy is refused. Clearing it would let another deploy start on top of the
// one still going.
func TestClearDeploymentsKeepsARunningOne(t *testing.T) {
	st := testStore(t)
	_, appID := seedApp(t, st, "web")

	done, err := st.StartDeployment(appID, DeployManual)
	if err != nil {
		t.Fatalf("start deployment: %v", err)
	}
	if err := st.FinishDeployment(done, DeployOK, "abc1234", "deployed", "$ build\nok"); err != nil {
		t.Fatalf("finish deployment: %v", err)
	}
	running, err := st.StartDeployment(appID, DeployWebhook)
	if err != nil {
		t.Fatalf("start second deployment: %v", err)
	}

	n, err := st.ClearDeployments(appID)
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n != 1 {
		t.Errorf("cleared %d rows, want 1 (the finished one)", n)
	}

	left, err := st.AppDeployments(appID, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(left) != 1 || left[0].ID != running {
		t.Fatalf("after clear, deployments = %+v; want only the running one (id %d)", left, running)
	}
	// And it is still the row the running build will finish into.
	if err := st.FinishDeployment(running, DeployOK, "def5678", "deployed", ""); err != nil {
		t.Fatalf("the running deploy could not finish into its row: %v", err)
	}
}

// TestClearDeploymentsIsPerApp: one app's Clear must not empty another's.
func TestClearDeploymentsIsPerApp(t *testing.T) {
	st := testStore(t)
	_, mine := seedApp(t, st, "mine")
	_, theirs := seedApp(t, st, "theirs")

	for _, id := range []int64{mine, theirs} {
		d, err := st.StartDeployment(id, DeployManual)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		st.FinishDeployment(d, DeployOK, "", "done", "")
	}
	if _, err := st.ClearDeployments(mine); err != nil {
		t.Fatalf("clear: %v", err)
	}
	left, _ := st.AppDeployments(theirs, 10)
	if len(left) != 1 {
		t.Errorf("clearing one app's history left %d rows on the other, want 1", len(left))
	}
}

// TestClearEventsScoping: a project's Clear takes that project's rows and
// nothing else; an unscoped Clear takes everything, including rows with no
// project, which are otherwise unreachable from any per-project view.
func TestClearEventsScoping(t *testing.T) {
	st := testStore(t)
	a, _ := seedApp(t, st, "alpha")
	b, _ := seedApp(t, st, "beta")

	st.AddEvent(a, 0, "info", "alpha one")
	st.AddEvent(a, 0, "info", "alpha two")
	st.AddEvent(b, 0, "info", "beta one")
	st.AddEvent(0, 0, "info", "no project at all")

	n, err := st.ClearEvents(a)
	if err != nil {
		t.Fatalf("clear project: %v", err)
	}
	if n != 2 {
		t.Errorf("cleared %d project rows, want 2", n)
	}
	if left, _ := st.ListEventsByProject(b, 10); len(left) != 1 {
		t.Errorf("other project has %d events, want its 1 untouched", len(left))
	}

	// Two left: beta's, and the one belonging to no project.
	if all, _ := st.ListEvents(10); len(all) != 2 {
		t.Fatalf("before the unscoped clear there are %d events, want 2", len(all))
	}
	if _, err := st.ClearEvents(0); err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if all, _ := st.ListEvents(10); len(all) != 0 {
		t.Errorf("after clearing everything, %d events remain: %+v", len(all), all)
	}
}
