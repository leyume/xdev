package store

import (
	"errors"
	"testing"
)

// seedProject makes a project with n apps and returns its id plus their ids in
// creation order, which is the order the project page shows before anyone
// drags anything.
func seedProject(t *testing.T, st *Store, slug string, names ...string) (int64, []int64) {
	t.Helper()
	p, err := st.CreateProject(Project{
		Name: slug, Slug: slug, BaseDomain: slug + ".test",
		Environment: "local", NetworkName: "xdev_" + slug, Dir: "/tmp/" + slug,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	var ids []int64
	for _, n := range names {
		a, err := st.CreateApp(App{ProjectID: p.ID, Name: n, Slug: n, Type: "static"})
		if err != nil {
			t.Fatalf("create app %s: %v", n, err)
		}
		ids = append(ids, a.ID)
	}
	return p.ID, ids
}

func listIDs(t *testing.T, st *Store, projectID int64) []int64 {
	t.Helper()
	apps, err := st.ListAppsByProject(projectID)
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	out := make([]int64, 0, len(apps))
	for _, a := range apps {
		out = append(out, a.ID)
	}
	return out
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// New apps go to the end of the list, so adding one never reshuffles an order
// somebody arranged by hand.
func TestCreateAppAppendsToTheEnd(t *testing.T) {
	st := testStore(t)
	pid, ids := seedProject(t, st, "demo", "api", "web", "admin")

	if got := listIDs(t, st, pid); !sameIDs(got, ids) {
		t.Fatalf("creation order = %v, want %v", got, ids)
	}

	// Reverse it, then add a fourth: the new app belongs last, not first.
	reversed := []int64{ids[2], ids[1], ids[0]}
	if err := st.SetAppOrder(pid, reversed); err != nil {
		t.Fatalf("SetAppOrder: %v", err)
	}
	extra, err := st.CreateApp(App{ProjectID: pid, Name: "worker", Slug: "worker", Type: "static"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	want := append(append([]int64{}, reversed...), extra.ID)
	if got := listIDs(t, st, pid); !sameIDs(got, want) {
		t.Errorf("after adding an app the order is %v, want %v", got, want)
	}
}

func TestSetAppOrderReorders(t *testing.T) {
	st := testStore(t)
	pid, ids := seedProject(t, st, "demo", "api", "web", "admin")

	want := []int64{ids[1], ids[2], ids[0]}
	if err := st.SetAppOrder(pid, want); err != nil {
		t.Fatalf("SetAppOrder: %v", err)
	}
	if got := listIDs(t, st, pid); !sameIDs(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}

	// And it survives a second reorder — positions are rewritten, not nudged.
	want = []int64{ids[2], ids[0], ids[1]}
	if err := st.SetAppOrder(pid, want); err != nil {
		t.Fatalf("SetAppOrder: %v", err)
	}
	if got := listIDs(t, st, pid); !sameIDs(got, want) {
		t.Errorf("second order = %v, want %v", got, want)
	}
}

// A list that doesn't describe the project is rejected whole. Accepting the
// parts that match would let a page open since before an app was added save an
// order that silently drops it.
func TestSetAppOrderRejectsAStaleList(t *testing.T) {
	st := testStore(t)
	pid, ids := seedProject(t, st, "demo", "api", "web", "admin")
	other, otherIDs := seedProject(t, st, "second", "lonely")

	for _, tc := range []struct {
		name string
		ids  []int64
	}{
		{"too few", []int64{ids[0], ids[1]}},
		{"too many", []int64{ids[0], ids[1], ids[2], otherIDs[0]}},
		{"a duplicate", []int64{ids[0], ids[0], ids[1]}},
		{"an app from another project", []int64{ids[0], ids[1], otherIDs[0]}},
		{"an id that does not exist", []int64{ids[0], ids[1], 99999}},
		{"empty", nil},
	} {
		err := st.SetAppOrder(pid, tc.ids)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !errors.Is(err, ErrStaleOrder) {
			t.Errorf("%s: err = %v, want ErrStaleOrder", tc.name, err)
		}
	}

	// Every rejection must leave the stored order exactly as it was.
	if got := listIDs(t, st, pid); !sameIDs(got, ids) {
		t.Errorf("a rejected reorder changed the order to %v, want %v", got, ids)
	}
	if got := listIDs(t, st, other); !sameIDs(got, otherIDs) {
		t.Errorf("a rejected reorder touched another project: %v", got)
	}
}

// Reordering one project must not renumber another's apps into its own
// positions — they share the column, and only project_id separates them.
func TestSetAppOrderIsScopedToItsProject(t *testing.T) {
	st := testStore(t)
	one, oneIDs := seedProject(t, st, "one", "a", "b")
	two, twoIDs := seedProject(t, st, "two", "c", "d")

	if err := st.SetAppOrder(one, []int64{oneIDs[1], oneIDs[0]}); err != nil {
		t.Fatalf("SetAppOrder: %v", err)
	}
	if got := listIDs(t, st, two); !sameIDs(got, twoIDs) {
		t.Errorf("project two's order changed to %v, want %v", got, twoIDs)
	}
}

// Deleting an app leaves a gap in the positions; the remaining order must be
// unaffected, since positions are only ever compared with each other.
func TestOrderSurvivesADeletedApp(t *testing.T) {
	st := testStore(t)
	pid, ids := seedProject(t, st, "demo", "api", "web", "admin")

	if err := st.SetAppOrder(pid, []int64{ids[2], ids[0], ids[1]}); err != nil {
		t.Fatalf("SetAppOrder: %v", err)
	}
	if err := st.DeleteApp(ids[0]); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if got := listIDs(t, st, pid); !sameIDs(got, []int64{ids[2], ids[1]}) {
		t.Errorf("order after delete = %v, want %v", got, []int64{ids[2], ids[1]})
	}
}

// UpdateApp saves the settings page's fields; position is not one of them, and
// an app edit must not fling the card back to where it started.
func TestUpdateAppKeepsPosition(t *testing.T) {
	st := testStore(t)
	pid, ids := seedProject(t, st, "demo", "api", "web")

	if err := st.SetAppOrder(pid, []int64{ids[1], ids[0]}); err != nil {
		t.Fatalf("SetAppOrder: %v", err)
	}
	a, err := st.AppByID(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	a.Name = "renamed"
	if err := st.UpdateApp(a); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	if got := listIDs(t, st, pid); !sameIDs(got, []int64{ids[1], ids[0]}) {
		t.Errorf("order after an edit = %v, want %v", got, []int64{ids[1], ids[0]})
	}
}
