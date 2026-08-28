package projects

import (
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/config"
	"xdev/internal/store"
)

func configService(t *testing.T) (*Service, store.Project) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	p, err := st.CreateProject(store.Project{
		Name: "Demo", Slug: "demo", BaseDomain: "demo.test", Environment: "local",
		NetworkName: "xdev_demo", Dir: "/tmp/demo",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return New(st, config.Config{}, nil), p
}

// TestConfigureSetsBoth covers the ordinary edit.
func TestConfigureSetsBoth(t *testing.T) {
	s, p := configService(t)

	got, err := s.Configure(p.ID, "  Example.COM  ", "prod")
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// Trimmed and lowercased, because it becomes part of a hostname and
	// hostnames are case-insensitive — storing "Example.COM" would make the
	// collision check against an existing "example.com" miss.
	if got.BaseDomain != "example.com" {
		t.Errorf("BaseDomain = %q, want %q", got.BaseDomain, "example.com")
	}
	if got.Environment != "prod" {
		t.Errorf("Environment = %q, want prod", got.Environment)
	}
}

// TestConfigureRemovesTheBaseDomain: blank is a real answer, not a mistake. It
// means new apps get no hostname unless given one — the shape of an install
// reached by IP.
func TestConfigureRemovesTheBaseDomain(t *testing.T) {
	s, p := configService(t)

	got, err := s.Configure(p.ID, "   ", "local")
	if err != nil {
		t.Fatalf("Configure with a blank domain: %v", err)
	}
	if got.BaseDomain != "" {
		t.Errorf("BaseDomain = %q, want it cleared", got.BaseDomain)
	}
}

// TestConfigureRejectsBadInput: whatever goes in the base domain becomes part
// of an app hostname later, so it has to survive that.
func TestConfigureRejectsBadInput(t *testing.T) {
	s, p := configService(t)

	for name, tc := range map[string]struct{ domain, env string }{
		"a URL":            {"https://example.com", "local"},
		"a port":           {"example.com:8080", "local"},
		"a path":           {"example.com/app", "local"},
		"a leading dot":    {".example.com", "local"},
		"a trailing dot":   {"example.com.", "local"},
		"a doubled dot":    {"example..com", "local"},
		"a space inside":   {"exa mple.com", "local"},
		"an underscore":    {"my_app.com", "local"},
		"too long":         {strings.Repeat("a", 201), "local"},
		"a made-up env":    {"example.com", "staging"},
		"a blank env":      {"example.com", ""},
		"an env with case": {"example.com", "Prod"},
	} {
		if _, err := s.Configure(p.ID, tc.domain, tc.env); err == nil {
			t.Errorf("%s: %q/%q was accepted", name, tc.domain, tc.env)
		}
	}
	// Nothing was written by any of them.
	after, err := s.store.ProjectByID(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.BaseDomain != "demo.test" || after.Environment != "local" {
		t.Errorf("project = %q/%q after the rejected edits, want it untouched", after.BaseDomain, after.Environment)
	}
}

// TestConfigureRejectsAMissingProject — the id comes from a URL, so it has to
// be checked rather than trusted into an UPDATE that silently matches no rows.
func TestConfigureRejectsAMissingProject(t *testing.T) {
	s, _ := configService(t)
	if _, err := s.Configure(9999, "example.com", "prod"); err == nil {
		t.Error("configuring a project that does not exist was accepted")
	}
}
