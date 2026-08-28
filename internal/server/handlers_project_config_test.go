package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"xdev/internal/store"
)

// configForm posts the settings form the project page renders.
func configForm(t *testing.T, srv *Server, slug, domain, env string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"base_domain": {domain}, "environment": {env}}
	r := httptest.NewRequest("POST", "/projects/"+slug+"/settings", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetPathValue("slug", slug)
	w := httptest.NewRecorder()
	srv.handleProjectSettings(w, r)
	return w
}

func TestProjectSettingsSaves(t *testing.T) {
	srv, proj, _ := orderServer(t)

	w := configForm(t, srv, proj.Slug, "example.com", "prod")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); loc != "/projects/"+proj.Slug {
		t.Errorf("Location = %q, want the project page", loc)
	}
	got, err := srv.store.ProjectByID(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseDomain != "example.com" || got.Environment != "prod" {
		t.Errorf("project = %q/%q, want example.com/prod", got.BaseDomain, got.Environment)
	}
}

// The base domain can be cleared. A form field left empty has to mean "remove
// it" rather than "leave it alone", or there is no way to take one off.
func TestProjectSettingsRemovesTheBaseDomain(t *testing.T) {
	srv, proj, _ := orderServer(t)

	if w := configForm(t, srv, proj.Slug, "", "local"); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	got, err := srv.store.ProjectByID(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseDomain != "" {
		t.Errorf("BaseDomain = %q, want it cleared", got.BaseDomain)
	}
}

// A rejected edit leaves the project alone and says why.
func TestProjectSettingsRejectsABadDomain(t *testing.T) {
	srv, proj, _ := orderServer(t)

	w := configForm(t, srv, proj.Slug, "https://example.com:8080", "local")
	if w.Code == http.StatusSeeOther && !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Fatal("a URL was accepted as a base domain")
	}
	got, err := srv.store.ProjectByID(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseDomain != proj.BaseDomain {
		t.Errorf("BaseDomain = %q after a rejected edit, want %q", got.BaseDomain, proj.BaseDomain)
	}
}

// TestProjectSettingsLogsTheSwitch: changing where a project's certificates
// come from is worth a line in the activity feed, and submitting the form
// unchanged is not.
func TestProjectSettingsLogsTheSwitch(t *testing.T) {
	srv, proj, _ := orderServer(t)

	if w := configForm(t, srv, proj.Slug, proj.BaseDomain, proj.Environment); w.Code != http.StatusSeeOther {
		t.Fatalf("unchanged submit: status %d", w.Code)
	}
	if n := len(eventsFor(t, srv)); n != 0 {
		t.Errorf("%d events logged for an unchanged submit, want none", n)
	}

	if w := configForm(t, srv, proj.Slug, proj.BaseDomain, "prod"); w.Code != http.StatusSeeOther {
		t.Fatalf("switch: status %d", w.Code)
	}
	events := eventsFor(t, srv)
	if len(events) != 1 {
		t.Fatalf("%d events after switching environment, want 1: %v", len(events), events)
	}
	// The message has to name the consequence, not just the setting: "prod" on
	// its own does not tell anyone their certificates are about to be reissued.
	if !strings.Contains(events[0].Message, "Let's Encrypt") {
		t.Errorf("event %q does not say where certificates now come from", events[0].Message)
	}
}

func eventsFor(t *testing.T, srv *Server) []store.Event {
	t.Helper()
	events, err := srv.store.ListEvents(20)
	if err != nil {
		t.Fatal(err)
	}
	return events
}
