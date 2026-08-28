package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

// makeProject creates one project and returns its id.
func makeProject(t *testing.T, st *Store, slug string) int64 {
	t.Helper()
	p, err := st.CreateProject(Project{
		Name: slug, Slug: slug, BaseDomain: slug + ".test",
		Environment: "local", NetworkName: "xdev_" + slug, Dir: "/tmp/" + slug,
	})
	if err != nil {
		t.Fatalf("create project %s: %v", slug, err)
	}
	return p.ID
}

func projectIDs(t *testing.T, st *Store) []int64 {
	t.Helper()
	list, err := st.ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	out := make([]int64, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

// TestNewProjectsStillArriveFirst pins the behaviour the position column had to
// preserve. Before it existed the overview was ordered newest-first, and the
// point of adding a hand-arranged order was not to quietly change where a
// freshly created project turns up.
func TestNewProjectsStillArriveFirst(t *testing.T) {
	st := testStore(t)
	a := makeProject(t, st, "alpha")
	b := makeProject(t, st, "beta")
	c := makeProject(t, st, "gamma")

	if got := projectIDs(t, st); !sameIDs(got, []int64{c, b, a}) {
		t.Fatalf("order = %v, want the newest first (%v)", got, []int64{c, b, a})
	}
}

// TestSetProjectOrderRearranges is the feature itself, and the part that
// matters most: an order survives the round trip to the database.
func TestSetProjectOrderRearranges(t *testing.T) {
	st := testStore(t)
	a := makeProject(t, st, "alpha")
	b := makeProject(t, st, "beta")
	c := makeProject(t, st, "gamma")

	want := []int64{b, a, c}
	if err := st.SetProjectOrder(want); err != nil {
		t.Fatalf("SetProjectOrder: %v", err)
	}
	if got := projectIDs(t, st); !sameIDs(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestNewProjectJoinsAnArrangedList: creating a project after somebody has
// dragged the cards must not undo their arrangement — it goes to the front and
// everything else keeps its place.
func TestNewProjectJoinsAnArrangedList(t *testing.T) {
	st := testStore(t)
	a := makeProject(t, st, "alpha")
	b := makeProject(t, st, "beta")
	c := makeProject(t, st, "gamma")

	if err := st.SetProjectOrder([]int64{b, a, c}); err != nil {
		t.Fatalf("SetProjectOrder: %v", err)
	}
	d := makeProject(t, st, "delta")

	want := []int64{d, b, a, c}
	if got := projectIDs(t, st); !sameIDs(got, want) {
		t.Errorf("order = %v, want %v — the arranged order was disturbed", got, want)
	}
}

// TestSetProjectOrderRejectsAStaleList: a page open since before a project was
// created or deleted describes a set that no longer exists. Applying the part
// it recognises would silently drop the project it never knew about, so the
// whole submission is refused.
func TestSetProjectOrderRejectsAStaleList(t *testing.T) {
	st := testStore(t)
	a := makeProject(t, st, "alpha")
	b := makeProject(t, st, "beta")
	before := projectIDs(t, st)

	for name, ids := range map[string][]int64{
		"one short":       {a},
		"one too many":    {b, a, 999},
		"a foreign id":    {b, 999},
		"the same twice":  {a, a},
		"nothing at all":  {},
		"an unknown pair": {998, 999},
	} {
		err := st.SetProjectOrder(ids)
		if !errors.Is(err, ErrStaleProjectOrder) {
			t.Errorf("%s: err = %v, want ErrStaleProjectOrder", name, err)
		}
	}
	if got := projectIDs(t, st); !sameIDs(got, before) {
		t.Errorf("order = %v after the rejected saves, want it untouched (%v)", got, before)
	}
}

// TestProjectPositionBackfillKeepsTheExistingOrder covers what happens on an
// install that already has projects — the only interesting case, and the one
// every other test in this file misses, because they run the migration against
// an empty table where a backfill can do nothing wrong.
//
// The database is rewound (the column dropped, the migration forgotten) and
// reopened, so 0017 replays against populated rows exactly as it will on a
// machine being upgraded. What it must not do is reshuffle a page nobody asked
// to change: the overview was newest-first before, and has to stay that way.
func TestProjectPositionBackfillKeepsTheExistingOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "xdev.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// created_at is forced apart because it defaults to datetime('now') at
	// second resolution: three rows made in one test share a timestamp, and
	// then the backfill's tie-break would be all the test ever exercised.
	for i, slug := range []string{"alpha", "beta", "gamma"} {
		makeProject(t, st, slug)
		if _, err := st.db.Exec(`UPDATE projects SET created_at = ? WHERE slug = ?`,
			fmt.Sprintf("2026-01-0%d 10:00:00", i+1), slug); err != nil {
			t.Fatalf("backdate %s: %v", slug, err)
		}
	}
	before := projectIDs(t, st)
	if len(before) != 3 {
		t.Fatalf("seeded %d projects, want 3", len(before))
	}

	if _, err := st.db.Exec(`ALTER TABLE projects DROP COLUMN position`); err != nil {
		t.Fatalf("drop position: %v", err)
	}
	if _, err := st.db.Exec(`DELETE FROM schema_migrations WHERE name = '0017_project_position.sql'`); err != nil {
		t.Fatalf("forget the migration: %v", err)
	}
	st.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (the migration failed to replay): %v", err)
	}
	defer upgraded.Close()

	if got := projectIDs(t, upgraded); !sameIDs(got, before) {
		t.Errorf("order after upgrading = %v, want the order the page already showed (%v)", got, before)
	}
}
