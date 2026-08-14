package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/hostproc"
	"xdev/internal/store"
)

// TestValidSourceDir covers the one guard on a user-supplied app folder: it has
// to be an absolute path to a directory that already exists. xdev writes inside
// this path but never creates or deletes it, so a typo has to fail here.
func TestValidSourceDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Blank is the default (the managed <project.dir>/<slug>), not an error.
	if got, err := validSourceDir("  "); err != nil || got != "" {
		t.Errorf(`validSourceDir("  ") = %q, %v; want "", nil`, got, err)
	}
	if got, err := validSourceDir("  " + dir + "/  "); err != nil || got != dir {
		t.Errorf("validSourceDir(%q) = %q, %v; want %q trimmed and cleaned", dir, got, err, dir)
	}
	for name, in := range map[string]string{
		"relative":  "ui/xyz",
		"missing":   filepath.Join(dir, "nope"),
		"is a file": file,
		"root":      string(filepath.Separator),
	} {
		if _, err := validSourceDir(in); err == nil {
			t.Errorf("%s: validSourceDir(%q) should be rejected", name, in)
		}
	}
}

// TestValidSourceDirExpandsHome accepts "~/..." because that is what people
// type into the field.
func TestValidSourceDirExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := validSourceDir("~")
	if err != nil {
		t.Fatalf(`validSourceDir("~"): %v`, err)
	}
	if got != filepath.Clean(home) {
		t.Errorf(`validSourceDir("~") = %q, want %q`, got, home)
	}
}

// hostAppIn lays a static app out the way Create would, pointed at sourceDir
// ("" = the managed folder under the project), and persists it.
func hostAppIn(t *testing.T, svc *Service, st *store.Store, proj store.Project, slug, sourceDir, mode string) store.App {
	t.Helper()
	app := store.App{ProjectID: proj.ID, Name: slug, Slug: slug, Type: store.TypeStatic,
		Domain: slug + ".demo.test", Status: store.AppStopped}
	appDir := filepath.Join(proj.Dir, slug)
	if sourceDir != "" {
		var err error
		if app.SourceDir, err = validSourceDir(sourceDir); err != nil {
			t.Fatalf("validSourceDir(%q): %v", sourceDir, err)
		}
		appDir = app.SourceDir
	}
	if err := svc.layoutStatic(&app, &CreateOpts{Type: store.TypeStatic, ServeMode: mode}, appDir); err != nil {
		t.Fatalf("layoutStatic: %v", err)
	}
	saved, err := st.CreateApp(app)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := st.ReplaceAppDomains(saved.ID, []string{app.Domain}, nil, true, "internal"); err != nil {
		t.Fatalf("attach domain: %v", err)
	}
	return saved
}

// TestCreateInExternalDir goes through the real add-app entry point: the folder
// is adopted as-is, no folder is made under the project, and a path that isn't
// there is refused before an app row exists.
func TestCreateInExternalDir(t *testing.T) {
	svc, st, proj := editFixture(t)
	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(ext, "index.html"), []byte("mine"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	app, err := svc.Create(proj.ID, CreateOpts{
		Name: "xyz", Type: store.TypeStatic, Domain: "xyz.demo.test",
		ServeMode: store.ServeStatic, SourceDir: ext,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if app.SourceDir != ext {
		t.Errorf("SourceDir = %q, want %q", app.SourceDir, ext)
	}
	if _, err := os.Stat(filepath.Join(proj.Dir, app.Slug)); !os.IsNotExist(err) {
		t.Errorf("a folder was still created under the project (err = %v)", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ext, "index.html")); string(b) != "mine" {
		t.Errorf("existing file was overwritten: %q", b)
	}

	// A path that doesn't exist is refused, and nothing survives the attempt.
	if _, err := svc.Create(proj.ID, CreateOpts{
		Name: "nope", Type: store.TypeStatic, Domain: "nope.demo.test",
		ServeMode: store.ServeStatic, SourceDir: filepath.Join(ext, "missing"),
	}); err == nil {
		t.Fatal("a missing folder should be rejected")
	}
	if owner := st.DomainOwner("nope.demo.test"); owner != 0 {
		t.Errorf("the rejected app still owns its domain (app %d)", owner)
	}
}

// TestExternalDirIsNotScaffolded is the property that makes pointing an app at
// existing code safe: xdev adds nothing to a directory it did not create. A
// managed folder still gets its starter files, so the default path is unchanged.
func TestExternalDirIsNotScaffolded(t *testing.T) {
	svc, st, proj := editFixture(t)

	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(ext, "index.html"), []byte("mine"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	hostAppIn(t, svc, st, proj, "ext", ext, store.ServeCommand)
	entries, err := os.ReadDir(ext)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "index.html" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("xdev wrote into the user's folder: %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(ext, "index.html")); string(b) != "mine" {
		t.Errorf("existing file was overwritten: %q", b)
	}

	// The managed path keeps its scaffold — this is only about foreign folders.
	hostAppIn(t, svc, st, proj, "managed", "", store.ServeCommand)
	if _, err := os.Stat(filepath.Join(proj.Dir, "managed", "package.json")); err != nil {
		t.Errorf("managed app lost its scaffold: %v", err)
	}
}

// TestServeModeSkipsPlaceholderInExternalDir guards the other write layoutStatic
// makes: a serve-mode app drops an index.html placeholder, which in someone
// else's folder would be xdev inventing content.
func TestServeModeSkipsPlaceholderInExternalDir(t *testing.T) {
	svc, st, proj := editFixture(t)
	ext := t.TempDir()
	hostAppIn(t, svc, st, proj, "site", ext, store.ServeStatic)
	if _, err := os.Stat(filepath.Join(ext, "index.html")); !os.IsNotExist(err) {
		t.Errorf("placeholder written into the user's folder (err = %v)", err)
	}
}

// TestDeleteKeepsExternalDir is the guarantee the field's help text makes:
// deleting the app unhooks it, it does not delete the user's code. The managed
// case still removes the folder xdev made.
func TestDeleteKeepsExternalDir(t *testing.T) {
	svc, st, proj := editFixture(t)
	// Delete stops the app's host process first, so this one needs a supervisor
	// (nothing is running — Stop on an unknown id is a no-op).
	svc.sup = hostproc.NewSupervisor(t.TempDir())

	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(ext, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	app := hostAppIn(t, svc, st, proj, "ext", ext, store.ServeStatic)
	if err := svc.Delete(app.ID, t.TempDir()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ext, "keep.txt")); err != nil {
		t.Fatalf("delete removed the user's files: %v", err)
	}

	managed := hostAppIn(t, svc, st, proj, "managed", "", store.ServeStatic)
	if err := svc.Delete(managed.ID, t.TempDir()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj.Dir, "managed")); !os.IsNotExist(err) {
		t.Errorf("managed app dir survived delete (err = %v)", err)
	}
}

// TestAppDirFollowsSourceDir checks the one function every file operation goes
// through — logs, backups, imports, the .env editor, the build's working
// directory — resolves to the folder the app was pointed at.
func TestAppDirFollowsSourceDir(t *testing.T) {
	svc, st, proj := editFixture(t)
	ext := t.TempDir()
	app := hostAppIn(t, svc, st, proj, "ext", ext, store.ServeCommand)

	if got := svc.appDir(app); got != ext {
		t.Errorf("appDir = %q, want %q", got, ext)
	}
	env, err := svc.envPath(app.ID)
	if err != nil {
		t.Fatalf("envPath: %v", err)
	}
	if want := filepath.Join(ext, ".env"); env != want {
		t.Errorf("envPath = %q, want %q", env, want)
	}
}

// TestProxyRouteServesExternalDir checks the reverse-proxy root: a serve-mode
// app rooted outside the project must be file-served from there, with root_dir
// still applied as a subdir of it.
func TestProxyRouteServesExternalDir(t *testing.T) {
	svc, st, proj := editFixture(t)
	ext := t.TempDir()
	app := hostAppIn(t, svc, st, proj, "site", ext, store.ServeStatic)
	if _, err := svc.Update(app.ID, EditOpts{
		Name: "site", Domain: app.Domain, ServeMode: store.ServeStatic,
		SourceDir: ext, RootDir: "dist",
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	routes, err := st.ProxyRoutes()
	if err != nil {
		t.Fatalf("ProxyRoutes: %v", err)
	}
	want := filepath.Join(ext, "dist")
	for _, r := range routes {
		if r.Host != app.Domain {
			continue
		}
		if r.Root != want {
			t.Fatalf("route root = %q, want %q", r.Root, want)
		}
		return
	}
	t.Fatalf("no route for %s in %v", app.Domain, routes)
}

// TestUpdateRepointsHostApp covers editing the folder after the fact, in both
// directions. Re-pointing never touches either directory: the one being left
// keeps its files, and the one being adopted gains none.
func TestUpdateRepointsHostApp(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := hostAppIn(t, svc, st, proj, "site", "", store.ServeCommand)
	managedDir := filepath.Join(proj.Dir, "site")

	ext := t.TempDir()
	got, err := svc.Update(app.ID, EditOpts{
		Name: "site", Domain: app.Domain, ServeMode: store.ServeCommand, SourceDir: ext,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.SourceDir != ext {
		t.Errorf("SourceDir = %q, want %q", got.SourceDir, ext)
	}
	if _, err := os.Stat(filepath.Join(managedDir, "package.json")); err != nil {
		t.Errorf("the folder it moved off was disturbed: %v", err)
	}
	if entries, _ := os.ReadDir(ext); len(entries) != 0 {
		t.Errorf("the folder it moved onto was written into: %v", entries)
	}

	// …and back to the managed path.
	got, err = svc.Update(app.ID, EditOpts{
		Name: "site", Domain: app.Domain, ServeMode: store.ServeCommand,
	})
	if err != nil {
		t.Fatalf("Update back to managed: %v", err)
	}
	if got.SourceDir != "" {
		t.Errorf("SourceDir = %q, want it back on the managed path", got.SourceDir)
	}
	if svc.appDir(got) != managedDir {
		t.Errorf("appDir = %q, want %q", svc.appDir(got), managedDir)
	}
}

// TestUpdateRejectsBadSourceDirWithoutChanging holds the settings page's
// validate-everything-first rule for the new field.
func TestUpdateRejectsBadSourceDirWithoutChanging(t *testing.T) {
	svc, st, proj := editFixture(t)
	ext := t.TempDir()
	app := hostAppIn(t, svc, st, proj, "site", ext, store.ServeCommand)

	_, err := svc.Update(app.ID, EditOpts{
		Name: "renamed", Domain: "moved.demo.test", ServeMode: store.ServeCommand,
		SourceDir: filepath.Join(ext, "does-not-exist"),
	})
	if err == nil {
		t.Fatal("a missing folder should be rejected")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error %q should say the folder is missing", err)
	}
	after, err := st.AppByID(app.ID)
	if err != nil {
		t.Fatalf("AppByID: %v", err)
	}
	if after.Name != app.Name || after.Domain != app.Domain || after.SourceDir != ext {
		t.Errorf("rejected save changed the app: %+v", after)
	}
}

// TestWriteDBEnvAppendsToExisting checks the last write xdev makes into a
// user's folder: a shared-database app's credentials go *after* whatever .env is
// already there rather than replacing it.
func TestWriteDBEnvAppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	block := dbEnvFile("demo_app", "pw", 20001)

	// No file yet: written as-is, and readable only by the owner (it has a
	// password in it).
	if err := writeDBEnv(path, block); err != nil {
		t.Fatalf("writeDBEnv: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != block {
		t.Errorf("fresh .env = %q, want %q", b, block)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %v, want 0600 — it holds a password", info.Mode().Perm())
	}

	// An existing file (no trailing newline, the awkward case) is kept whole.
	if err := os.WriteFile(path, []byte("API_KEY=mine"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writeDBEnv(path, block); err != nil {
		t.Fatalf("writeDBEnv: %v", err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "API_KEY=mine\n") {
		t.Errorf("existing .env was not preserved: %q", b)
	}
	if !strings.Contains(string(b), "DB_NAME=demo_app") {
		t.Errorf("DB block not appended: %q", b)
	}
}
