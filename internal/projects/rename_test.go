package projects

import (
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/config"
	"xdev/internal/store"
)

// renameService builds a Service with a real store. Rename touches nothing but
// the store, so the engine selector it never reaches is left nil rather than
// stubbed — a nil that would panic is a better guard against this test quietly
// starting to exercise container code than a stub that would not.
func renameService(t *testing.T) (*Service, store.Project) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	p, err := st.CreateProject(store.Project{
		Name: "Servorien", Slug: "servorien", BaseDomain: "servorien.test",
		Environment: "prod", NetworkName: "xdev_servorien", Dir: "/tmp/servorien",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return New(st, config.Config{}, nil), p
}

func TestRenameSetsTheName(t *testing.T) {
	s, p := renameService(t)

	got, err := s.Rename(p.ID, "Servorien Group")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got.Name != "Servorien Group" {
		t.Errorf("name = %q, want %q", got.Name, "Servorien Group")
	}
	if got.Slug != p.Slug {
		t.Errorf("rename moved the slug to %q — the directory, network and containers are named after it", got.Slug)
	}
}

// A trailing space is invisible in the input and would show up as a lopsided
// page title, so it is trimmed rather than stored.
func TestRenameTrimsSurroundingSpace(t *testing.T) {
	s, p := renameService(t)

	got, err := s.Rename(p.ID, "  Servorien Group  ")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got.Name != "Servorien Group" {
		t.Errorf("name = %q, want it trimmed", got.Name)
	}
}

// Whitespace is not a name. Accepting it would leave the page head, the
// breadcrumb and every project link blank with nothing to click.
func TestRenameRejectsAnEmptyName(t *testing.T) {
	s, p := renameService(t)

	for _, in := range []string{"", "   ", "\t\n"} {
		if _, err := s.Rename(p.ID, in); err == nil {
			t.Errorf("Rename(%q) was accepted", in)
		}
	}
	// And the stored name is untouched by the attempts.
	got, err := s.Rename(p.ID, p.Name)
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got.Name != "Servorien" {
		t.Errorf("name = %q after rejected renames, want the original", got.Name)
	}
}

// The cap counts runes, not bytes: a name in a non-Latin script would otherwise
// be rejected at a third of the length an English one is allowed.
func TestRenameLengthCapCountsRunes(t *testing.T) {
	s, p := renameService(t)

	// 80 runes, 240 bytes — over any byte-based limit, exactly on the rune one.
	name := strings.Repeat("プ", MaxProjectName)
	if len(name) <= MaxProjectName {
		t.Fatalf("fixture does not exercise the difference (bytes %d, limit %d)", len(name), MaxProjectName)
	}
	got, err := s.Rename(p.ID, name)
	if err != nil {
		t.Fatalf("a name of exactly %d runes was rejected: %v", MaxProjectName, err)
	}
	if got.Name != name {
		t.Errorf("name was not stored intact")
	}

	if _, err := s.Rename(p.ID, strings.Repeat("a", MaxProjectName+1)); err == nil {
		t.Error("a name over the limit was accepted")
	}
}

// Renaming to the name it already has is a no-op, not an error — it is what a
// user gets for opening the form and pressing Save.
func TestRenameToTheSameNameIsFine(t *testing.T) {
	s, p := renameService(t)

	got, err := s.Rename(p.ID, "Servorien")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got.Name != "Servorien" {
		t.Errorf("name = %q, want unchanged", got.Name)
	}
}

func TestRenameUnknownProject(t *testing.T) {
	s, _ := renameService(t)

	if _, err := s.Rename(99999, "Nowhere"); err == nil {
		t.Error("renaming a project that does not exist was accepted")
	}
}
