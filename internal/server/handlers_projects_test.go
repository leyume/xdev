package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// TestExtraDomains covers the contract the compose add-app form depends on: the
// hostnames for domains 2..N arrive in the order the rows were shown, because
// that order *is* the ${PORT_n} numbering. Blanks are passed through rather than
// dropped — the apps service rejects them by slot, which is a far better error
// than silently renumbering the ports behind the domains that follow.
func TestExtraDomains(t *testing.T) {
	form := url.Values{}
	form.Add("extra_domain", " api.demo.test ")
	form.Add("extra_domain", "")
	form.Add("extra_domain", "admin.demo.test")

	r := httptest.NewRequest("POST", "/projects/demo/apps", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}

	got := extraDomains(r)
	want := []string{"api.demo.test", "", "admin.demo.test"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("domain %d: got %q, want %q", i+2, got[i], want[i])
		}
	}

	// No rows at all (a single-domain app) is not an error, just no extras.
	r = httptest.NewRequest("POST", "/projects/demo/apps", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatalf("ParseForm: %v", err)
	}
	if got := extraDomains(r); len(got) != 0 {
		t.Errorf("got %q, want none", got)
	}
}

// TestWriteJSONError covers the reply the add-app dialog reads when a create is
// rejected: a non-2xx carrying the message, so the modal can stay open with
// everything still typed into it instead of reloading the page empty.
func TestWriteJSONError(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSONError(w, errors.New("domain \"api.demo.test\" is already in use"), http.StatusBadRequest)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	var body struct{ Error string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	if body.Error != `domain "api.demo.test" is already in use` {
		t.Errorf("error = %q", body.Error)
	}
}

// TestClampError checks the bound put on a message before it reaches the
// dialog: a failing `compose up` can return screens of engine output, but the
// whole first part of it — the part that says what went wrong — is kept.
func TestClampError(t *testing.T) {
	short := "compose file has no top-level \"services:\" block"
	if got := clampError(short); got != short {
		t.Errorf("a short message was altered: %q", got)
	}

	long := strings.Repeat("engine noise\n", 200)
	got := clampError(long)
	if lines := strings.Count(got, "\n"); lines > 13 {
		t.Errorf("clamped message still has %d lines", lines)
	}
	if !strings.HasPrefix(got, "engine noise\n") {
		t.Errorf("the start of the message was lost: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation not marked: %q", got)
	}

	// One enormous line (no newlines to cut on) is bounded by bytes instead.
	if got := clampError(strings.Repeat("x", 10000)); len(got) > 4100 {
		t.Errorf("byte cap not applied: %d bytes", len(got))
	}
}

// TestWantsJSON: the dialog's fetch asks for JSON, a native form submit does
// not — and only the latter should be answered with a redirect.
func TestWantsJSON(t *testing.T) {
	r := httptest.NewRequest("POST", "/projects/demo/apps", nil)
	if wantsJSON(r) {
		t.Error("a plain form post should not want JSON")
	}
	r.Header.Set("Accept", "application/json")
	if !wantsJSON(r) {
		t.Error("Accept: application/json should want JSON")
	}
}

// TestAddAppFormHasOneDomainField: every type is served at a hostname, so the
// add-app dialog offers the Domain field in the same place for all of them —
// including compose, whose own domain used to be hidden among the port rows and
// so read as uneditable. The rows are for its *extra* domains only, and there is
// exactly one field named "domain" so a submit can't carry two.
func TestAddAppFormHasOneDomainField(t *testing.T) {
	out := renderNamed(t, "project", viewData{
		"Project":    store.Project{Name: "Demo", Slug: "demo", BaseDomain: "demo.test"},
		"Apps":       []store.App{},
		"Activity":   []store.Event{},
		"Catalog":    templates.Catalog(),
		"MaxDomains": maxComposeDomains,
	})
	if n := strings.Count(out, `name="domain"`); n != 1 {
		t.Errorf("add-app form has %d domain fields, want exactly 1", n)
	}
	// The field must not be gated on the type: that gate is what hid it.
	if strings.Contains(out, `x-if="type!=='compose'"`) {
		t.Error("the Domain field is still hidden for compose apps")
	}
	for _, want := range []string{
		`name="extra_domain"`,   // the compose rows, extras only
		"Domain 1 (this app)",   // slot 1 echoes the field above
		"from Domain above",     //   ... and says where it comes from
		`x-text="portVar(i+1)"`, // so a row's position is ${PORT_(i+2)}
		`'Domain ' + (i+2)`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compose port map missing %q", want)
		}
	}
}

// TestAddAppFormOffersAnAppFolder: host apps can be pointed at code that
// already exists on disk. The field is optional and belongs only to the types
// that run from a directory — one per type block, so a submit carries one value.
func TestAddAppFormOffersAnAppFolder(t *testing.T) {
	out := renderNamed(t, "project", viewData{
		"Project":    store.Project{Name: "Demo", Slug: "demo", BaseDomain: "demo.test"},
		"Apps":       []store.App{},
		"Activity":   []store.Event{},
		"Catalog":    templates.Catalog(),
		"MaxDomains": maxComposeDomains,
	})
	// One in the static block, one in the go block — both inside x-if templates,
	// so only the chosen type's field is ever in the DOM to submit.
	if n := strings.Count(out, `name="source_dir"`); n != 2 {
		t.Errorf("add-app form has %d app-folder fields, want one for static and one for go", n)
	}
	for _, want := range []string{"/home/li/ui/xyz", "must already exist"} {
		if !strings.Contains(out, want) {
			t.Errorf("app-folder field missing %q", want)
		}
	}
}
