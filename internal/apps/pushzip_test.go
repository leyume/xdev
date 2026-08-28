package apps

import (
	"os"
	"path/filepath"
	"testing"

	"xdev/internal/store"
)

// TestPushDeployTakesAZip: the settings page's upload button posts whatever the
// user picked, and for a static site that is far more often a .zip than a
// .tar.gz. The format is read from the archive, so both arrive at the same
// place.
func TestPushDeployTakesAZip(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := staticApp(t, st, proj, "dist")

	target, err := svc.PushDeploy(app.ID, codeZip(t, map[string]string{
		"index.html":  "<h1>new</h1>",
		"css/app.css": "body{}",
	}), store.DeployPush)
	if err != nil {
		t.Fatalf("PushDeploy: %v", err)
	}
	if got := laidOut(t, target); !equal(got, []string{"css/app.css", "index.html"}) {
		t.Errorf("published %v, want the zip's files", got)
	}
}

// TestPushDeployDropsTheWrapperDirectory: a zip made by right-clicking a folder
// wraps everything in that folder. Published verbatim the site would serve a
// directory listing, so the wrapper goes — the same rule the create-time upload
// follows.
func TestPushDeployDropsTheWrapperDirectory(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := staticApp(t, st, proj, "dist")

	target, err := svc.PushDeploy(app.ID, codeZip(t, map[string]string{
		"dist/index.html": "<h1>new</h1>",
	}), store.DeployPush)
	if err != nil {
		t.Fatalf("PushDeploy: %v", err)
	}
	if got := laidOut(t, target); !equal(got, []string{"index.html"}) {
		t.Errorf("published %v, want [index.html] with the wrapper dropped", got)
	}
}

// TestPushDeployKeepsTheGeneratedEnv is the hazard the upload button makes easy
// to hit.
//
// A push replaces its target wholesale, and an app with no separate build
// output has the app directory itself as that target — which is where xdev
// wrote the database name, user and password it created for this app. Losing
// them on every upload would leave the app unable to reach a database that
// still exists and still belongs to it.
func TestPushDeployKeepsTheGeneratedEnv(t *testing.T) {
	svc, st, proj := editFixture(t)
	app, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "api", Slug: "api", Type: store.TypeStatic,
		Domain: "api.demo.test", ServeMode: store.ServeStatic,
		DBMode: store.DBShared, Status: store.AppRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj.Dir, app.Slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const creds = "DB_NAME=demo_api\nDB_USER=demo_api\nDB_PASSWORD=s3cret\n"
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}

	target, err := svc.PushDeploy(app.ID, codeZip(t, map[string]string{"index.html": "hi"}), store.DeployPush)
	if err != nil {
		t.Fatalf("PushDeploy: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, ".env"))
	if err != nil {
		t.Fatalf("the generated .env did not survive the upload: %v", err)
	}
	if string(got) != creds {
		t.Errorf(".env = %q, want the credentials xdev generated", got)
	}
	// Still a secrets file afterwards.
	info, err := os.Stat(filepath.Join(target, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %v, want 0600", info.Mode().Perm())
	}
}

// TestPushDeployLetsTheArchiveReplaceTheEnv: supplying one is how you change
// those settings deliberately, as against losing them by not thinking about it.
func TestPushDeployLetsTheArchiveReplaceTheEnv(t *testing.T) {
	svc, st, proj := editFixture(t)
	app, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "api2", Slug: "api2", Type: store.TypeStatic,
		Domain: "api2.demo.test", ServeMode: store.ServeStatic,
		DBMode: store.DBShared, Status: store.AppRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(proj.Dir, app.Slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("DB_PASSWORD=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	target, err := svc.PushDeploy(app.ID, codeZip(t, map[string]string{
		"index.html": "hi",
		".env":       "DB_PASSWORD=mine\n",
	}), store.DeployPush)
	if err != nil {
		t.Fatalf("PushDeploy: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "DB_PASSWORD=mine\n" {
		t.Errorf(".env = %q, want the archive's own", got)
	}
}

// TestPushDeployLeavesEnvAloneWithoutADatabase: the rule is scoped to a file
// xdev generated. An app with no provisioned database gets the plain wholesale
// replace it always had.
func TestPushDeployLeavesEnvAloneWithoutADatabase(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := staticApp(t, st, proj, "")
	dir := filepath.Join(proj.Dir, app.Slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MINE=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	target, err := svc.PushDeploy(app.ID, codeZip(t, map[string]string{"index.html": "hi"}), store.DeployPush)
	if err != nil {
		t.Fatalf("PushDeploy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".env")); err == nil {
		t.Error(".env survived a push to an app with no provisioned database — the swap is meant to be wholesale")
	}
}
