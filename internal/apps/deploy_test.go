package apps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
)

// tarGzOf builds a .tar.gz of name->contents, the shape a CI job uploads
// (`tar -czf build.tar.gz -C dist .`: the *contents*, not the folder).
func tarGzOf(t *testing.T, files map[string]string) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf.Bytes())
}

// staticApp creates a serve-mode static app with a served subdirectory.
func staticApp(t *testing.T, st *store.Store, proj store.Project, rootDir string) store.App {
	t.Helper()
	app, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "site", Slug: "site", Type: store.TypeStatic,
		Domain: "site.demo.test", ServeMode: store.ServeStatic, RootDir: rootDir,
		Status: store.AppRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// TestPushDeployReplacesTheServedFolder is the upload path end to end: what CI
// sends becomes what the app serves, and what was there before is gone rather
// than merged with it.
func TestPushDeployReplacesTheServedFolder(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := staticApp(t, st, proj, "dist")
	dist := filepath.Join(proj.Dir, app.Slug, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file from the previous build that the new one does not contain.
	if err := os.WriteFile(filepath.Join(dist, "old-chunk.js"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	target, err := svc.PushDeploy(app.ID, tarGzOf(t, map[string]string{
		"index.html":     "<h1>v2</h1>",
		"assets/app.css": "body{}",
	}), store.DeployPush)
	if err != nil {
		t.Fatalf("PushDeploy: %v", err)
	}
	if target != dist {
		t.Errorf("published to %q, want %q", target, dist)
	}
	if b, err := os.ReadFile(filepath.Join(dist, "index.html")); err != nil || string(b) != "<h1>v2</h1>" {
		t.Errorf("index.html = %q (%v)", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dist, "assets", "app.css")); err != nil || string(b) != "body{}" {
		t.Errorf("assets/app.css = %q (%v)", b, err)
	}
	if _, err := os.Stat(filepath.Join(dist, "old-chunk.js")); err == nil {
		t.Error("a file from the previous build survived — the folder was merged, not replaced")
	}
	// No staging or retiring directories left behind.
	entries, _ := os.ReadDir(filepath.Join(proj.Dir, app.Slug))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".xdev-push-") || strings.HasSuffix(e.Name(), ".retiring") {
			t.Errorf("left %s behind", e.Name())
		}
	}
	// And it is recorded.
	deploys, _ := st.AppDeployments(app.ID, 5)
	if len(deploys) != 1 || deploys[0].Status != store.DeployOK || deploys[0].Trigger != store.DeployPush {
		t.Errorf("deployments = %+v, want one successful push", deploys)
	}
}

// TestPushDeployKeepsTheOldBuildOnFailure: an upload that turns out not to be a
// readable archive must leave the site exactly as it was. Serving nothing is
// worse than serving the previous version.
func TestPushDeployKeepsTheOldBuildOnFailure(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := staticApp(t, st, proj, "dist")
	dist := filepath.Join(proj.Dir, app.Slug, "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<h1>v1</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.PushDeploy(app.ID, strings.NewReader("this is not a gzip stream"), store.DeployPush); err == nil {
		t.Fatal("a corrupt upload was accepted")
	}
	if b, err := os.ReadFile(filepath.Join(dist, "index.html")); err != nil || string(b) != "<h1>v1</h1>" {
		t.Errorf("index.html = %q (%v), want the previous build untouched", b, err)
	}
	// An empty archive is refused for the same reason.
	if _, err := svc.PushDeploy(app.ID, tarGzOf(t, nil), store.DeployPush); err == nil {
		t.Error("an empty archive was published over the site")
	}
	if b, _ := os.ReadFile(filepath.Join(dist, "index.html")); string(b) != "<h1>v1</h1>" {
		t.Error("an empty archive replaced the site")
	}
	deploys, _ := st.AppDeployments(app.ID, 5)
	if len(deploys) != 2 {
		t.Fatalf("recorded %d deploys, want both failures", len(deploys))
	}
	for _, d := range deploys {
		if d.Status != store.DeployFailed {
			t.Errorf("deploy %d status = %q, want failed", d.ID, d.Status)
		}
	}
}

// TestPushDeployRefusesWhatItMustNotOverwrite. Both cases would destroy
// something the user owns: their own folder, or the git checkout an app
// deploys from.
func TestPushDeployRefusesWhatItMustNotOverwrite(t *testing.T) {
	svc, st, proj := editFixture(t)

	own := t.TempDir()
	if err := os.WriteFile(filepath.Join(own, "keep-me"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	external, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "ext", Slug: "ext", Type: store.TypeStatic,
		Domain: "ext.demo.test", ServeMode: store.ServeStatic, SourceDir: own,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PushDeploy(external.ID, tarGzOf(t, map[string]string{"x": "y"}), store.DeployPush); err == nil {
		t.Error("an upload replaced a folder of the user's own")
	}
	if _, err := os.Stat(filepath.Join(own, "keep-me")); err != nil {
		t.Errorf("the user's file is gone: %v", err)
	}

	// A git app with no served subdirectory: replacing the app folder would take
	// .git with it, so it is refused until a folder to serve is set.
	gitApp, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "git", Slug: "git", Type: store.TypeStatic,
		Domain: "git.demo.test", ServeMode: store.ServeStatic,
		GitURL: "https://github.com/leyume/site",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.PushDeploy(gitApp.ID, tarGzOf(t, map[string]string{"x": "y"}), store.DeployPush)
	if err == nil || !strings.Contains(err.Error(), "Folder to serve") {
		t.Errorf("git app with no root dir: %v, want it to ask for a folder to serve", err)
	}

	// A container app has no served folder at all.
	compose, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "stack", Slug: "stack", Type: store.TypeCompose,
		Domain: "stack.demo.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PushDeploy(compose.ID, tarGzOf(t, map[string]string{"x": "y"}), store.DeployPush); err == nil {
		t.Error("a compose app accepted an uploaded build")
	}
}

// TestPushDeployRejectsEscapingArchives: a tarball is attacker-controlled input
// whenever a deploy token leaks, and an entry like ../../etc/cron.d/x must not
// be written outside the target.
func TestPushDeployRejectsEscapingArchives(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := staticApp(t, st, proj, "dist")

	escape := tarGzOf(t, map[string]string{"../../escaped.txt": "pwned"})
	if _, err := svc.PushDeploy(app.ID, escape, store.DeployPush); err == nil {
		t.Fatal("an archive escaping the target was accepted")
	}
	if _, err := os.Stat(filepath.Join(proj.Dir, "..", "escaped.txt")); err == nil {
		t.Error("a file was written outside the app directory")
	}
}

// TestDeployRefusesToOverlap: two deploys of one app writing the same output
// directory produce a mixture of both, so the second is refused rather than
// queued.
func TestDeployRefusesToOverlap(t *testing.T) {
	svc, st, proj := editFixture(t)
	app := staticApp(t, st, proj, "dist")

	if !svc.deploying.claim(app.ID) {
		t.Fatal("could not claim a fresh app")
	}
	if _, err := svc.PushDeploy(app.ID, tarGzOf(t, map[string]string{"x": "y"}), store.DeployPush); err != ErrDeployInProgress {
		t.Errorf("second deploy: %v, want ErrDeployInProgress", err)
	}
	if !svc.Deploying(app.ID) {
		t.Error("Deploying should report the claimed app as busy")
	}
	svc.deploying.release(app.ID)
	if svc.Deploying(app.ID) {
		t.Error("Deploying still reports busy after release")
	}
	// Released: it works again.
	if _, err := svc.PushDeploy(app.ID, tarGzOf(t, map[string]string{"x": "y"}), store.DeployPush); err != nil {
		t.Errorf("after release: %v", err)
	}
	_ = st
}
