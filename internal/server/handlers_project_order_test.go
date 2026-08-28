package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
)

// boardServer builds the smallest Server handleProjectOrder uses — just the
// store — plus three projects, and returns their ids in the order the overview
// shows them (newest first).
func boardServer(t *testing.T) (*Server, []int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	for _, slug := range []string{"alpha", "beta", "gamma"} {
		if _, err := st.CreateProject(store.Project{
			Name: slug, Slug: slug, BaseDomain: slug + ".test",
			Environment: "local", NetworkName: "xdev_" + slug, Dir: "/tmp/" + slug,
		}); err != nil {
			t.Fatalf("create project %s: %v", slug, err)
		}
	}
	srv := &Server{store: st}
	return srv, boardOrder(t, srv)
}

func boardOrder(t *testing.T, srv *Server) []int64 {
	t.Helper()
	list, err := srv.store.ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	out := make([]int64, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

// postOrder builds the form-encoded request the page sends.
func postOrder(t *testing.T, ids []int64, json bool) *http.Request {
	t.Helper()
	form := url.Values{}
	for _, id := range ids {
		form.Add("id", sid(id))
	}
	r := httptest.NewRequest("POST", "/projects/order", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if json {
		r.Header.Set("Accept", "application/json")
	}
	return r
}

// The order the ids arrive in is the new order of the cards.
func TestProjectOrderSaves(t *testing.T) {
	srv, ids := boardServer(t)
	want := []int64{ids[2], ids[0], ids[1]}

	w := httptest.NewRecorder()
	srv.handleProjectOrder(w, postOrder(t, want, true))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if got := boardOrder(t, srv); !eq(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// A stale list gets a 409 rather than a 500: the fix is to reload the page, not
// to retry the request, and the client tells the user exactly that.
func TestProjectOrderRejectsAStaleList(t *testing.T) {
	srv, ids := boardServer(t)
	before := boardOrder(t, srv)

	w := httptest.NewRecorder()
	srv.handleProjectOrder(w, postOrder(t, []int64{ids[0], ids[1]}, true))

	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 so the page knows to reload", w.Code)
	}
	if got := boardOrder(t, srv); !eq(got, before) {
		t.Errorf("order = %v after a rejected save, want it untouched (%v)", got, before)
	}
}

// A non-numeric id is a malformed request, not a stale one — 400, and nothing
// written.
func TestProjectOrderRejectsAGarbageID(t *testing.T) {
	srv, _ := boardServer(t)
	before := boardOrder(t, srv)

	form := url.Values{}
	form.Add("id", "not-a-number")
	r := httptest.NewRequest("POST", "/projects/order", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	srv.handleProjectOrder(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if got := boardOrder(t, srv); !eq(got, before) {
		t.Errorf("order changed after a malformed request: %v, want %v", got, before)
	}
}

// Without JavaScript the same endpoint has to answer a plain form post, so the
// page it redirects back to is the overview rather than a JSON body nobody will
// render.
func TestProjectOrderRedirectsANativePost(t *testing.T) {
	srv, ids := boardServer(t)
	want := []int64{ids[1], ids[2], ids[0]}

	w := httptest.NewRecorder()
	srv.handleProjectOrder(w, postOrder(t, want, false))

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/projects" {
		t.Errorf("Location = %q, want /projects", loc)
	}
	if got := boardOrder(t, srv); !eq(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}
