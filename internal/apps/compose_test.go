package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// TestComposeHostPort covers how xdev decides where to route a bring-your-own
// compose app: a ${PORT} reference means "use the port xdev allocates", any
// hard-coded published port wins otherwise, and a file publishing nothing is an
// error (its domain would have nowhere to go).
func TestComposeHostPort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yaml     string
		wantPort int
		wantVar  bool
	}{
		{"port var, quoted", "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n", 0, true},
		{"port var, bare", "services:\n  web:\n    ports:\n      - $PORT:8000\n", 0, true},
		{"port var with default", "services:\n  web:\n    ports:\n      - \"${PORT:-8080}:80\"\n", 0, true},
		{"xdev port var", "services:\n  web:\n    ports:\n      - \"${XDEV_PORT}:80\"\n", 0, true},
		{"fixed short syntax", "services:\n  web:\n    ports:\n      - \"8080:80\"\n", 8080, false},
		{"fixed unquoted", "services:\n  web:\n    ports:\n      - 8080:80\n", 8080, false},
		{"fixed with bind address", "services:\n  web:\n    ports:\n      - \"127.0.0.1:9000:80/tcp\"\n", 9000, false},
		{"fixed long syntax", "services:\n  web:\n    ports:\n      - target: 80\n        published: \"8081\"\n", 8081, false},
		{"trailing comment", "services:\n  web:\n    ports:\n      - \"8082:80\" # web\n", 8082, false},
		{"first published port wins", "services:\n  db:\n    ports:\n      - \"3307:3306\"\n  web:\n    ports:\n      - \"8083:80\"\n", 3307, false},
		{"port var beats a fixed one elsewhere", "services:\n  db:\n    ports:\n      - \"3307:3306\"\n  web:\n    ports:\n      - \"${PORT}:80\"\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port, usesVar, err := composeHostPort(tc.yaml)
			if err != nil {
				t.Fatalf("composeHostPort: %v", err)
			}
			if usesVar != tc.wantVar || port != tc.wantPort {
				t.Errorf("got port=%d usesVar=%v, want port=%d usesVar=%v", port, usesVar, tc.wantPort, tc.wantVar)
			}
		})
	}

	// Nothing published: the app's domain would route nowhere, so refuse.
	for _, yaml := range []string{
		"services:\n  web:\n    image: nginx\n",
		"services:\n  web:\n    expose:\n      - 80\n",
		"services:\n  web:\n    ports:\n      - \"80\"\n", // container port only, host side random
		// ${PORT} outside port position is not a published port.
		"services:\n  web:\n    environment:\n      - BASE_URL=http://localhost:${PORT}\n",
		"# ports:\n#   - \"${PORT}:80\"\nservices:\n  web:\n    image: nginx\n",
	} {
		if _, _, err := composeHostPort(yaml); err == nil {
			t.Errorf("compose file with no published host port should be rejected:\n%s", yaml)
		}
	}
}

// TestValidCompose checks the structural gate a supplied file has to pass
// before xdev writes it: real text, a top-level services block, no tab indents
// (YAML forbids them, and the engine's error for it is cryptic).
func TestValidCompose(t *testing.T) {
	good := "# my stack\nservices:\n  web:\n    image: nginx:alpine\n"
	if err := validCompose(good); err != nil {
		t.Errorf("valid compose rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"empty":           "   \n",
		"no services":     "version: \"3\"\nweb:\n  image: nginx\n",
		"tab indented":    "services:\n\tweb:\n\t  image: nginx\n",
		"services nested": "x:\n  services:\n    web:\n      image: nginx\n",
		"too large":       "services:\n" + strings.Repeat("# pad\n", maxComposeBytes/6+1),
		"not utf-8":       "services:\n  web:\n    image: \xff\xfe\n",
	} {
		if err := validCompose(bad); err == nil {
			t.Errorf("%s: should have been rejected", name)
		}
	}
}

// TestEnsureComposeEnv checks the _/.env xdev keeps beside a user's compose
// file: the managed port is written, the user's own lines survive a rewrite,
// and a stale PORT is corrected instead of duplicated.
func TestEnsureComposeEnv(t *testing.T) {
	dir := t.TempDir()
	if err := ensureComposeEnv(dir, 20001); err != nil {
		t.Fatalf("ensureComposeEnv: %v", err)
	}
	body := readFile(t, filepath.Join(dir, ".env"))
	if !strings.Contains(body, "PORT=20001\n") || !strings.Contains(body, "XDEV_PORT=20001\n") {
		t.Fatalf(".env missing the managed port:\n%s", body)
	}

	// A user adds their own variable and (wrongly) edits the managed one.
	if err := os.WriteFile(filepath.Join(dir, ".env"),
		[]byte("PORT=1\nAPI_KEY=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureComposeEnv(dir, 20002); err != nil {
		t.Fatalf("ensureComposeEnv rewrite: %v", err)
	}
	body = readFile(t, filepath.Join(dir, ".env"))
	if !strings.Contains(body, "API_KEY=secret") {
		t.Errorf("user variable was dropped:\n%s", body)
	}
	if strings.Count(body, "PORT=") != 2 { // PORT + XDEV_PORT, each exactly once
		t.Errorf("stale PORT not replaced:\n%s", body)
	}
	if !strings.Contains(body, "PORT=20002\n") {
		t.Errorf("PORT not corrected to the app's port:\n%s", body)
	}
}

// TestComposeStarterIsAcceptedByItsOwnRules renders the starter compose file
// xdev writes when no file is supplied and pushes it back through the checks a
// supplied file faces — the file xdev ships must be one xdev would accept, and
// its published port must be the one that was allocated.
func TestComposeStarterIsAcceptedByItsOwnRules(t *testing.T) {
	out, err := templates.RenderCompose(store.TypeCompose, templates.Data{
		ProjectSlug: "demo", NetworkName: "xdev_demo", AppSlug: "stack",
		AppType: store.TypeCompose, HostPort: 20005,
	})
	if err != nil {
		t.Fatalf("render starter: %v", err)
	}
	if err := validCompose(out); err != nil {
		t.Fatalf("starter compose fails validation: %v\n%s", err, out)
	}
	port, usesVar, err := composeHostPort(out)
	if err != nil {
		t.Fatalf("starter compose port: %v\n%s", err, out)
	}
	if usesVar || port != 20005 {
		t.Errorf("starter publishes port=%d usesVar=%v, want the allocated 20005", port, usesVar)
	}
}

// TestLayoutCompose writes the on-disk layout for both ways of creating a
// compose app — a supplied file and the starter — and checks the pieces the
// rest of xdev depends on: the file lands at _/compose.yml untouched, the app
// row points at it, the routed port follows the file, and _/.env carries the
// port for ${PORT} substitution.
func TestLayoutCompose(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	proj, err := st.CreateProject(store.Project{
		Name: "Demo", Slug: "demo", BaseDomain: "demo.test", Environment: "local",
		NetworkName: "xdev_demo", Engine: "docker", Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	svc := New(st, nil, nil, "")

	t.Run("supplied file with a fixed port", func(t *testing.T) {
		yaml := "services:\n  web:\n    image: caddy:2\n    ports:\n      - \"8099:80\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "byo", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: yaml}, proj, appDir); err != nil {
			t.Fatalf("layoutCompose: %v", err)
		}
		if got := readFile(t, filepath.Join(appDir, "_", "compose.yml")); got != yaml {
			t.Errorf("compose file was rewritten:\n%s", got)
		}
		if app.ComposePath != filepath.Join(appDir, "_", "compose.yml") {
			t.Errorf("ComposePath = %q", app.ComposePath)
		}
		if app.Port != 8099 {
			t.Errorf("app port = %d, want the 8099 the file publishes", app.Port)
		}
		if env := readFile(t, filepath.Join(appDir, "_", ".env")); !strings.Contains(env, "PORT=8099\n") {
			t.Errorf(".env missing the routed port:\n%s", env)
		}
		if _, err := os.Stat(filepath.Join(appDir, "app")); err != nil {
			t.Errorf("app content dir missing: %v", err)
		}
	})

	t.Run("no file gets the starter", func(t *testing.T) {
		app := store.App{ProjectID: proj.ID, Slug: "starter", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose}, proj, appDir); err != nil {
			t.Fatalf("layoutCompose: %v", err)
		}
		if app.Port < portMin || app.Port > portMax {
			t.Errorf("starter app port = %d, want one from the allocator range", app.Port)
		}
		body := readFile(t, filepath.Join(appDir, "_", "compose.yml"))
		if !strings.Contains(body, "demo_starter_web") || !strings.Contains(body, "name: xdev_demo") {
			t.Errorf("starter compose not wired to the project:\n%s", body)
		}
		// The starter serves the scaffolded page from the app content dir.
		if _, err := os.Stat(filepath.Join(appDir, "app", "index.html")); err != nil {
			t.Errorf("starter scaffold missing: %v", err)
		}
	})

	t.Run("bad file is rejected before anything is written", func(t *testing.T) {
		app := store.App{ProjectID: proj.ID, Slug: "bad", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: "web:\n  image: nginx\n"}, proj, appDir); err == nil {
			t.Fatal("a file without a services block should be rejected")
		}
		if _, err := os.Stat(appDir); !os.IsNotExist(err) {
			t.Errorf("rejected app left %s behind", appDir)
		}
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
