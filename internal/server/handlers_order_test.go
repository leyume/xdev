package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"xdev/internal/config"
	"xdev/internal/projects"
	"xdev/internal/store"
)

// sid renders an app id the way the page's form does.
func sid(n int64) string { return strconv.FormatInt(n, 10) }

// orderServer builds the smallest Server these two handlers actually use — the
// store and the projects service — plus a project with three apps.
//
// No engine, no reconciler: renaming a project and reordering its cards touch
// neither, and a nil that would panic is a better guard against this test
// quietly starting to exercise container code than a stub that would not.
func orderServer(t *testing.T) (*Server, store.Project, []int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	proj, err := st.CreateProject(store.Project{
		Name: "Demo", Slug: "demo", BaseDomain: "demo.test",
		Environment: "local", NetworkName: "xdev_demo", Dir: "/tmp/demo",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	var ids []int64
	for _, n := range []string{"api", "web", "admin"} {
		a, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: n, Slug: n, Type: store.TypeStatic})
		if err != nil {
			t.Fatalf("create app %s: %v", n, err)
		}
		ids = append(ids, a.ID)
	}
	return &Server{store: st, projects: projects.New(st, config.Config{}, nil)}, proj, ids
}

// post builds a form-encoded request with the slug already resolved, the way
// the router would hand it to the handler.
func post(t *testing.T, path, slug string, form url.Values, json bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if json {
		r.Header.Set("Accept", "application/json")
	}
	r.SetPathValue("slug", slug)
	return r
}

func orderOf(t *testing.T, st *store.Store, projectID int64) []int64 {
	t.Helper()
	list, err := st.ListAppsByProject(projectID)
	if err != nil {
		t.Fatalf("list apps: %v", err)
	}
	out := make([]int64, 0, len(list))
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

func eq(a, b []int64) bool {
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

// The order the ids arrive in *is* the new order of the cards, so this is the
// contract the whole feature rests on.
func TestAppOrderSavesTheSubmittedOrder(t *testing.T) {
	s, proj, ids := orderServer(t)

	form := url.Values{}
	for _, id := range []int64{ids[2], ids[0], ids[1]} {
		form.Add("id", sid(id))
	}
	w := httptest.NewRecorder()
	s.handleAppOrder(w, post(t, "/projects/demo/apps/order", "demo", form, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	want := []int64{ids[2], ids[0], ids[1]}
	if got := orderOf(t, s.store, proj.ID); !eq(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// A page that has been open since before an app was added or deleted submits a
// list that no longer describes the project. That is a 409 — the fix is to
// reload, not to retry — and nothing may be saved from it.
func TestAppOrderRejectsAStaleListWithConflict(t *testing.T) {
	s, proj, ids := orderServer(t)
	before := orderOf(t, s.store, proj.ID)

	form := url.Values{}
	form.Add("id", sid(ids[0]))
	form.Add("id", sid(ids[1])) // one short: the page predates "admin"

	w := httptest.NewRecorder()
	s.handleAppOrder(w, post(t, "/projects/demo/apps/order", "demo", form, true))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if got := orderOf(t, s.store, proj.ID); !eq(got, before) {
		t.Errorf("a rejected reorder changed the order to %v, want %v", got, before)
	}
}

// An app id belonging to someone else's project must not be reorderable through
// this project's URL.
func TestAppOrderWillNotAdoptAnotherProjectsApp(t *testing.T) {
	s, proj, ids := orderServer(t)

	other, err := s.store.CreateProject(store.Project{
		Name: "Other", Slug: "other", BaseDomain: "other.test",
		Environment: "local", NetworkName: "xdev_other", Dir: "/tmp/other",
	})
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := s.store.CreateApp(store.App{ProjectID: other.ID, Name: "x", Slug: "x", Type: store.TypeStatic})
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{}
	form.Add("id", sid(ids[0]))
	form.Add("id", sid(ids[1]))
	form.Add("id", sid(stranger.ID)) // right count, wrong project

	w := httptest.NewRecorder()
	s.handleAppOrder(w, post(t, "/projects/demo/apps/order", "demo", form, true))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", w.Code)
	}
	if got := orderOf(t, s.store, proj.ID); !eq(got, ids) {
		t.Errorf("order = %v, want the original %v", got, ids)
	}
	if got := orderOf(t, s.store, other.ID); !eq(got, []int64{stranger.ID}) {
		t.Errorf("the other project was disturbed: %v", got)
	}
}

func TestAppOrderRejectsNonNumericIDs(t *testing.T) {
	s, proj, ids := orderServer(t)

	form := url.Values{}
	form.Add("id", sid(ids[0]))
	form.Add("id", "not-a-number")
	form.Add("id", sid(ids[2]))

	w := httptest.NewRecorder()
	s.handleAppOrder(w, post(t, "/projects/demo/apps/order", "demo", form, true))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if got := orderOf(t, s.store, proj.ID); !eq(got, ids) {
		t.Errorf("order changed to %v on a malformed request", got)
	}
}

func TestAppOrderUnknownProject(t *testing.T) {
	s, _, _ := orderServer(t)

	w := httptest.NewRecorder()
	s.handleAppOrder(w, post(t, "/projects/nope/apps/order", "nope", url.Values{}, true))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// Renaming changes the name and redirects back to the same URL, because the
// slug — which the URL is built from — deliberately does not move.
func TestProjectRenameKeepsTheURL(t *testing.T) {
	s, proj, _ := orderServer(t)

	form := url.Values{}
	form.Set("name", "Demo Group")
	w := httptest.NewRecorder()
	s.handleProjectRename(w, post(t, "/projects/demo/rename", "demo", form, false))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/projects/demo" {
		t.Errorf("redirected to %q, want /projects/demo", loc)
	}
	got, err := s.store.ProjectByID(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Demo Group" {
		t.Errorf("name = %q, want %q", got.Name, "Demo Group")
	}
	if got.Slug != "demo" {
		t.Errorf("slug moved to %q — the project's directory and network are named after it", got.Slug)
	}
}

// An empty name is refused, and the stored name is left alone.
func TestProjectRenameRejectsAnEmptyName(t *testing.T) {
	s, proj, _ := orderServer(t)

	form := url.Values{}
	form.Set("name", "   ")
	w := httptest.NewRecorder()
	s.handleProjectRename(w, post(t, "/projects/demo/rename", "demo", form, true))

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	got, _ := s.store.ProjectByID(proj.ID)
	if got.Name != "Demo" {
		t.Errorf("name = %q, want it unchanged", got.Name)
	}
}

func TestProjectRenameUnknownProject(t *testing.T) {
	s, _, _ := orderServer(t)

	form := url.Values{}
	form.Set("name", "Whatever")
	w := httptest.NewRecorder()
	s.handleProjectRename(w, post(t, "/projects/nope/rename", "nope", form, false))

	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
