package server

import (
	"strings"
	"testing"

	"xdev/internal/store"
)

// projectCardData mirrors the anonymous struct handleProjectsList builds, which
// is what the template is written against.
type projectCardData struct {
	store.Project
	Apps    []store.App
	Stopped int
}

func renderProjects(t *testing.T, cards []projectCardData) string {
	t.Helper()
	return renderNamed(t, "projects", viewData{
		"Title": "Projects · xdev", "Projects": cards,
		"TotalApps": 2, "Running": 1, "Events": []store.Event{},
	})
}

func twoProjects() []projectCardData {
	return []projectCardData{
		{
			Project: store.Project{ID: 7, Name: "Servorien", Slug: "servorien",
				BaseDomain: "servorien.test", Environment: "prod", NetworkName: "xdev_servorien"},
			Apps: []store.App{{ID: 11, Name: "api", Type: "laravel", Domain: "api.servorien.test", Status: store.AppRunning}},
		},
		{
			Project: store.Project{ID: 9, Name: "Bizepp", Slug: "bizepp",
				BaseDomain: "bizepp.test", Environment: "local", NetworkName: "xdev_bizepp"},
			Apps:    []store.App{{ID: 12, Name: "web", Type: "static", Domain: "bizepp.test", Status: store.AppStopped}},
			Stopped: 1,
		},
	}
}

// TestProjectCardsAreDraggable: every card carries the id the save posts back,
// and starts undraggable.
//
// draggable="false" is the part worth pinning. A project card is an <a>, which
// browsers make draggable by default — without turning that off, grabbing a
// card starts a native link-drag and the reorder never runs.
func TestProjectCardsAreDraggable(t *testing.T) {
	out := renderProjects(t, twoProjects())

	for _, want := range []string{`data-project-id="7"`, `data-project-id="9"`} {
		if !strings.Contains(out, want) {
			t.Errorf("card id %s missing — the saved order would be blank", want)
		}
	}
	if n := strings.Count(out, `draggable="false"`); n < 3 {
		t.Errorf("%d cards start undraggable, want all three (two projects + the new-project tile) — an <a> drags its own link otherwise", n)
	}
	for _, want := range []string{`@dragstart="onDragStart($event)"`, `@dragend="onDragEnd($event)"`, `@dragover.prevent="onDragOver($event)"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing drag handler %s", want)
		}
	}
	if !strings.Contains(out, `setAttribute('draggable','true')`) {
		t.Error("nothing makes a card draggable — the handle never arms the drag")
	}
	if !strings.Contains(out, "/projects/order") {
		t.Error("the page never posts the new order anywhere")
	}
}

// TestProjectsViewSwitchRenders: both layouts are offered, and the grid is
// bound to the choice.
func TestProjectsViewSwitchRenders(t *testing.T) {
	out := renderProjects(t, twoProjects())

	if !strings.Contains(out, `@click="setView('grid')"`) || !strings.Contains(out, `@click="setView('list')"`) {
		t.Error("the view switch is missing one of its two buttons")
	}
	if !strings.Contains(out, `:class="{'is-list': view==='list'}"`) {
		t.Error("the grid is not bound to the chosen view — the buttons would do nothing")
	}
	if !strings.Contains(out, "localStorage.setItem('xdev.projects.view'") ||
		!strings.Contains(out, "localStorage.getItem('xdev.projects.view')") {
		t.Error("the view choice is not persisted, so it resets on every navigation")
	}
}

// TestProjectsEmptyStateHasNoSwitch: with nothing to arrange, a view switch is
// two buttons that change nothing.
func TestProjectsEmptyStateHasNoSwitch(t *testing.T) {
	out := renderProjects(t, nil)

	// The buttons, not the class name: the stylesheet is inlined into every
	// page, so .viewswitch appears in the markup either way.
	if strings.Contains(out, "setView('grid')") {
		t.Error("the view switch is shown with no projects to switch the view of")
	}
	if !strings.Contains(out, "No projects yet") {
		t.Error("the empty state is missing")
	}
}

// TestListViewIsStyled: the switch only sets a class, so the class has to mean
// something. A row is one per line and drops the per-app breakdown that is the
// whole reason a card is tall.
func TestListViewIsStyled(t *testing.T) {
	css := renderProjects(t, twoProjects())

	for _, want := range []string{
		".project-grid.is-list { grid-template-columns: 1fr;",
		".project-grid.is-list .project-card-apps { display: none; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("missing list-view rule %q — switching to list would look identical", want)
		}
	}
	// Out of the flow, and only then hidden. As a flex child the handle
	// reserved a slot in every card and pushed each project name permanently to
	// the right; switching it to display:none instead would move the name
	// sideways every time the pointer crossed a card. Absolute positioning is
	// what makes the name sit still in both states.
	grip := between(css, ".grip {", "}")
	if grip == "" {
		t.Fatal("no .grip rule — the drag handle is unstyled")
	}
	for _, want := range []string{"position: absolute", "opacity: 0"} {
		if !strings.Contains(grip, want) {
			t.Errorf(".grip is missing %q (rule: %s)", want, grip)
		}
	}
	if !strings.Contains(css, ".project-card:hover .grip") {
		t.Error("nothing reveals the drag handle on hover")
	}
}
