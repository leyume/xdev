package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
