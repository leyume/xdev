package server

import (
	"strings"
	"testing"

	"xdev/internal/store"
)

func twoApps() []store.App {
	return []store.App{
		{ID: 7, ProjectID: 3, Name: "l2", Slug: "l2", Type: "laravel", Status: store.AppRunning, Domain: "l2.demo.test", Port: 20001},
		{ID: 9, ProjectID: 3, Name: "site", Slug: "site", Type: store.TypeStatic, Status: store.AppStopped, Domain: "site.demo.test", Port: 20002},
	}
}

// The rename control posts to the rename route with a CSRF token, and does not
// offer to change the slug — which names the project's directory, network and
// containers, and so is not a form field.
func TestProjectPageOffersRename(t *testing.T) {
	out := renderProject(t, twoApps())

	if !strings.Contains(out, `action="/projects/demo/rename"`) {
		t.Error("no rename form on the project page")
	}
	form := between(out, `action="/projects/demo/rename"`, "</form>")
	if form == "" {
		t.Fatal("could not isolate the rename form")
	}
	if !strings.Contains(form, `name="csrf_token"`) {
		t.Error("the rename form has no CSRF token")
	}
	if !strings.Contains(form, `name="name"`) {
		t.Error("the rename form has no name field")
	}
	for _, forbidden := range []string{`name="slug"`, `name="base_domain"`, `name="network_name"`} {
		if strings.Contains(form, forbidden) {
			t.Errorf("the rename form offers to change identity: %s", forbidden)
		}
	}
}

// Each card carries its app id and a drag handle, which is what the reorder
// script reads and what the user grabs. Without the id on the card the saved
// order would be a list of empty strings.
func TestProjectPageCardsAreDraggable(t *testing.T) {
	out := renderProject(t, twoApps())

	for _, id := range []string{`data-app-id="7"`, `data-app-id="9"`} {
		if !strings.Contains(out, id) {
			t.Errorf("no card carries %s", id)
		}
	}
	if n := strings.Count(out, `class="drag-handle`); n != 2 {
		t.Errorf("found %d drag handles, want one per app card", n)
	}
	if !strings.Contains(out, `appOrder('demo'`) {
		t.Error("the app list is not wired to the reorder component")
	}
	if !strings.Contains(out, `@dragover.prevent="onDragOver($event)"`) {
		t.Error("the list has no dragover handler, so cards would never move")
	}
	// The handle must not toggle the card open on click — it sits inside the
	// header, whose click handler collapses the card.
	if !strings.Contains(out, `class="drag-handle tip" data-tip="Drag to reorder" aria-hidden="true"`) {
		t.Error("the drag handle is missing its label")
	}
}

// An empty project still renders, and still wires up the list — otherwise the
// first app added to it would land in a container with no drag behaviour until
// the next full reload.
func TestProjectPageWithNoApps(t *testing.T) {
	out := renderProject(t, nil)

	if !strings.Contains(out, "No apps yet") {
		t.Error("the empty state is gone")
	}
	if !strings.Contains(out, `appOrder('demo'`) {
		t.Error("an empty project's list is not wired to the reorder component")
	}
	if strings.Contains(out, `class="drag-handle`) {
		t.Error("a project with no apps rendered a drag handle")
	}
}

// between returns the text between the first occurrence of start and the next
// end after it, so an assertion about one form cannot match another's markup.
func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
