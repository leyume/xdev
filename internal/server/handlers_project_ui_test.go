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
	if !strings.Contains(out, `class="drag-handle tip" data-tip="Drag to reorder" aria-hidden="true"`) {
		t.Error("the drag handle is missing its label")
	}

	// The handle must sit in the card's gutter, not in the header row. Inside
	// the header it took a flex slot and pushed the chevron, status dot and
	// name along; as a sibling it is absolutely positioned and costs no layout,
	// so the header is laid out exactly as it was before the handle existed.
	head := between(out, `<div class="app-card-head"`, "</div>")
	if head == "" {
		t.Fatal("could not isolate the card header")
	}
	if strings.Contains(head, "drag-handle") {
		t.Error("the drag handle is inside the header row — it will shift the chevron and status dot")
	}
	if i, j := strings.Index(head, "chevron"), strings.Index(head, `class="status `); i < 0 || j < 0 || i > j {
		t.Error("the header no longer starts with the chevron then the status dot")
	}
	// And it is emitted as the card's own child, immediately before the header.
	if !strings.Contains(out, "</span>\n      <div class=\"app-card-head\"") {
		t.Error("the drag handle is not a direct child of the card, just before its header")
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

// The reorder component must read $el exactly once, in init().
//
// This is not style. Alpine rebinds $el to whichever element's expression is
// being evaluated, and the drag handlers are bound on the cards while dragover
// is bound on the list — so `this.$el` inside a method is a card in one call
// and the list in another. The first version of this component read $el in
// ids(), which meant the drag-start snapshot was taken from a card (containing
// no cards, so an empty list), matched the equally empty snapshot at drag end,
// and never saved anything. Dragging looked right and nothing persisted.
func TestReorderComponentReadsElOnlyInInit(t *testing.T) {
	out := renderProject(t, twoApps())

	start := strings.Index(out, "function appOrder(")
	if start < 0 {
		t.Fatal("the reorder component is gone")
	}
	end := strings.Index(out[start:], "\nfunction ")
	if end < 0 {
		end = len(out) - start
	}
	body := out[start : start+end]

	if !strings.Contains(body, "init() { this.list = this.$el; }") {
		t.Error("the component no longer captures the list element in init()")
	}
	if n := strings.Count(body, "this.$el"); n != 1 {
		t.Errorf("the component reads this.$el %d times, want exactly 1 (in init) — "+
			"every other read is rebound to whichever element fired the handler", n)
	}
	// The methods have to go through the captured element.
	for _, want := range []string{
		"this.list.querySelectorAll('.app-card')",
		"this.list.insertBefore(this.dragging, ref)",
		"this.list.appendChild(this.dragging)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("component does not use the captured list for %q", want)
		}
	}
}

// Auto-save with no confirmation is indistinguishable from a save that failed,
// and there is no Save button to have pressed.
func TestReorderConfirmsItSaved(t *testing.T) {
	out := renderProject(t, twoApps())

	if !strings.Contains(out, `x-show="saved"`) {
		t.Error("nothing tells the user the new order was saved")
	}
	if !strings.Contains(out, "this.saved = true;") {
		t.Error("the save path never sets the confirmation")
	}
	if !strings.Contains(out, "this.saved = false;") {
		t.Error("the confirmation is never cleared, so it would stick after one drag")
	}
}

// The handle sits in the gutter, outside the card, so revealing it on
// .app-card:hover alone hides it again the moment the cursor sets off towards
// it — the card is behind, the handle is not yet under the pointer, and the
// thing being reached for fades away. The card's hover area is extended across
// the gutter to close that gap.
//
// Every assertion here keys on rules rather than on the comments above them:
// html/template strips CSS comments in a <style> context, so they never reach
// the page.
func TestDragHandleStaysVisibleOnTheWayToIt(t *testing.T) {
	css := renderProject(t, twoApps())

	if !strings.Contains(css, ".app-card::before") {
		t.Fatal("no hover extension across the gutter — the handle fades as the cursor approaches it")
	}
	if !strings.Contains(css, ".app-card:hover .drag-handle, .drag-handle:hover, .drag-handle:focus-visible") {
		t.Error("hovering the handle itself does not keep it visible")
	}

	// The strip has to reach at least as far as the handle, or it leaves
	// exactly the dead zone it exists to remove, and run the card's full height
	// so the handle is reachable from the bottom of an expanded card.
	block := between(css, ".app-card::before {", "}")
	for _, want := range []string{"left: -34px", "width: 34px", "top: 0", "bottom: 0"} {
		if !strings.Contains(block, want) {
			t.Errorf("the hover strip is missing %q:\n%s", want, block)
		}
	}

	// An absolutely positioned box reaching past the viewport edge is what puts
	// a horizontal scrollbar on the page, so the narrow breakpoint must pull the
	// handle and the strip in together.
	for _, want := range []string{
		".drag-handle { left: -18px; }",
		".app-card::before { left: -20px; width: 20px; }",
	} {
		i := strings.Index(css, want)
		if i < 0 {
			t.Errorf("small-screen rules missing %q", want)
			continue
		}
		if m := strings.LastIndex(css[:i], "@media"); m < 0 || !strings.HasPrefix(css[m:], "@media (max-width: 900px)") {
			t.Errorf("%q is not inside the max-width:900px block, so it would apply at every width", want)
		}
	}
}

// TestProjectSettingsFormRenders: the base domain and environment are editable
// from the project page, and the form warns about what the switch does.
func TestProjectSettingsFormRenders(t *testing.T) {
	out := renderProject(t, []store.App{{ID: 1, Name: "web", Slug: "web", Type: "static"}})

	if !strings.Contains(out, `action="/projects/demo/settings"`) {
		t.Fatal("no settings form on the project page")
	}
	for _, want := range []string{`name="base_domain"`, `name="environment"`,
		`<option value="local"`, `<option value="prod"`} {
		if !strings.Contains(out, want) {
			t.Errorf("settings form is missing %s", want)
		}
	}
	// The field must be optional in the markup too. A `required` here would
	// make "remove the base domain" impossible from the UI, whatever the
	// server accepts.
	form := between(out, `action="/projects/demo/settings"`, "</form>")
	if strings.Contains(form, `name="base_domain"`) && strings.Contains(
		between(form, `name="base_domain"`, ">"), "required") {
		t.Error("the base domain field is required, so it can never be cleared")
	}
	// The pencil that opens the form sits outside it, and Alpine's $refs only
	// reach up through ancestors. A nested x-data on the form would put
	// x-ref="baseDomain" in a scope the button cannot see, and focusing it
	// would throw instead of putting the cursor in the field.
	if strings.Contains(between(out, `class="proj-config"`, ">"), "x-data") {
		t.Error("the settings form declares its own x-data, so the button outside it cannot reach x-ref=\"baseDomain\"")
	}
	if !strings.Contains(out, "cfgEnv:") || !strings.Contains(out, "cfgDomain:") {
		t.Error("the form's state is not on the page root, so the pencil and the fields are in different scopes")
	}
	// Both consequences are stated where the decision is made. Neither is
	// visible from the field itself.
	if !strings.Contains(form, "re-issues certificates") {
		t.Error("nothing warns that switching environment reissues certificates")
	}
	if !strings.Contains(form, "apps added from now on") {
		t.Error("nothing says a new base domain does not rename existing apps")
	}
}

// TestProjectMetaShowsNoBaseDomain: with the base domain removed the meta line
// has to say so rather than render an empty gap between two separators.
func TestProjectMetaShowsNoBaseDomain(t *testing.T) {
	out := renderProjectWith(t, nil, viewData{
		"Project": store.Project{ID: 3, Name: "Demo", Slug: "demo", BaseDomain: "",
			Environment: "local", NetworkName: "xdev_demo", Dir: "/p/demo"},
	})
	if !strings.Contains(out, "no base domain") {
		t.Error("a project with no base domain renders a blank where the domain was")
	}
}
