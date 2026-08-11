package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// TestComposePorts covers the published-port scan the add-app form and the
// router both rely on: every port syntax, the service each port belongs to, and
// the ${PORT} placeholder whose number xdev allocates.
func TestComposePorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want []ComposePort
	}{
		{"port var, quoted", "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n",
			[]ComposePort{{Service: "web", Var: true}}},
		{"port var, bare", "services:\n  web:\n    ports:\n      - $PORT:8000\n",
			[]ComposePort{{Service: "web", Var: true}}},
		{"port var with default", "services:\n  web:\n    ports:\n      - \"${PORT:-8080}:80\"\n",
			[]ComposePort{{Service: "web", Var: true}}},
		{"xdev port var", "services:\n  web:\n    ports:\n      - \"${XDEV_PORT}:80\"\n",
			[]ComposePort{{Service: "web", Var: true}}},
		{"fixed short syntax", "services:\n  web:\n    ports:\n      - \"8080:80\"\n",
			[]ComposePort{{Port: 8080, Service: "web"}}},
		{"fixed unquoted", "services:\n  web:\n    ports:\n      - 8080:80\n",
			[]ComposePort{{Port: 8080, Service: "web"}}},
		{"fixed with bind address", "services:\n  web:\n    ports:\n      - \"127.0.0.1:9000:80/tcp\"\n",
			[]ComposePort{{Port: 9000, Service: "web"}}},
		{"fixed long syntax", "services:\n  web:\n    ports:\n      - target: 80\n        published: \"8081\"\n",
			[]ComposePort{{Port: 8081, Service: "web"}}},
		{"trailing comment", "services:\n  web:\n    ports:\n      - \"8082:80\" # web\n",
			[]ComposePort{{Port: 8082, Service: "web"}}},
		{"one port per service, in file order",
			"services:\n  db:\n    ports:\n      - \"3307:3306\"\n  web:\n    ports:\n      - \"8083:80\"\n",
			[]ComposePort{{Port: 3307, Service: "db"}, {Port: 8083, Service: "web"}}},
		{"several ports on one service",
			"services:\n  app:\n    ports:\n      - \"8000:80\"\n      - \"8443:443\"\n      - \"9000:9000\"\n",
			[]ComposePort{{Port: 8000, Service: "app"}, {Port: 8443, Service: "app"}, {Port: 9000, Service: "app"}}},
		{"var alongside fixed ports",
			"services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  api:\n    ports:\n      - \"9090:9090\"\n",
			[]ComposePort{{Service: "web", Var: true}, {Port: 9090, Service: "api"}}},
		{"a repeated port is listed once",
			"services:\n  a:\n    ports:\n      - \"8080:80\"\n  b:\n    ports:\n      - \"8080:80\"\n",
			[]ComposePort{{Port: 8080, Service: "a"}}},
		{"top-level keys after services don't leak service names",
			"services:\n  web:\n    ports:\n      - \"8080:80\"\nnetworks:\n  internal:\n",
			[]ComposePort{{Port: 8080, Service: "web"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComposePorts(tc.yaml)
			if err != nil {
				t.Fatalf("ComposePorts: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("port %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
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
		// Two ${PORT}s would both expand to the same number and collide.
		"services:\n  a:\n    ports:\n      - \"${PORT}:80\"\n  b:\n    ports:\n      - \"${PORT}:90\"\n",
	} {
		if _, err := ComposePorts(yaml); err == nil {
			t.Errorf("compose file should have been rejected:\n%s", yaml)
		}
	}
}

// TestPickPrimary checks which published port carries the app's own domain: the
// requested one when the file still publishes it, else the first.
func TestPickPrimary(t *testing.T) {
	ports := []ComposePort{{Port: 8080, Service: "web"}, {Port: 9000, Service: "api"}}
	if got, ok := pickPrimary(ports, 9000); !ok || got.Port != 9000 {
		t.Errorf("requested port: got %+v ok=%v, want 9000", got, ok)
	}
	if got, ok := pickPrimary(ports, 0); !ok || got.Port != 8080 {
		t.Errorf("no request: got %+v ok=%v, want the first port", got, ok)
	}
	if got, ok := pickPrimary(ports, 7777); ok || got.Port != 8080 {
		t.Errorf("stale request: got %+v ok=%v, want a fallback to the first port", got, ok)
	}

	// A ${PORT} entry is always the primary: its number is the allocated one, so
	// letting a fixed port take the app's domain would put two services on it.
	withVar := []ComposePort{{Port: 8080, Service: "web"}, {Service: "api", Var: true}}
	if got, _ := pickPrimary(withVar, 8080); !got.Var {
		t.Errorf("got %+v, want the ${PORT} entry to stay primary", got)
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
	ports, err := ComposePorts(out)
	if err != nil {
		t.Fatalf("starter compose port: %v\n%s", err, out)
	}
	if len(ports) != 1 || ports[0].Var || ports[0].Port != 20005 {
		t.Errorf("starter publishes %+v, want just the allocated 20005", ports)
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
		if _, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: yaml}, proj, appDir); err != nil {
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
		if _, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose}, proj, appDir); err != nil {
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

	t.Run("multi-port file routes a hostname per port", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"8100:80\"\n  api:\n    ports:\n" +
			"      - \"8101:3000\"\n  admin:\n    ports:\n      - \"8102:9000\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "multi", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		extras, err := svc.layoutCompose(&app, &CreateOpts{
			Type:        store.TypeCompose,
			ComposeFile: yaml,
			PrimaryPort: 8101, // the app's own domain follows the api service
			PortDomains: map[int]string{
				8100: "web.demo.test",
				8102: "admin.demo.test",
				8101: "ignored.demo.test", // the primary uses the app's domain
			},
		}, proj, appDir)
		if err != nil {
			t.Fatalf("layoutCompose: %v", err)
		}
		if app.Port != 8101 {
			t.Errorf("app port = %d, want the requested primary 8101", app.Port)
		}
		want := map[string]int{"web.demo.test": 8100, "admin.demo.test": 8102}
		if len(extras) != len(want) {
			t.Fatalf("extra routes = %+v, want one per non-primary port", extras)
		}
		for _, r := range extras {
			if want[r.Host] != r.Port {
				t.Errorf("route %+v does not match %v", r, want)
			}
		}
		// The env still carries the primary port for ${PORT} substitution.
		if env := readFile(t, filepath.Join(appDir, "_", ".env")); !strings.Contains(env, "PORT=8101\n") {
			t.Errorf(".env should carry the primary port:\n%s", env)
		}
	})

	t.Run("a port with no hostname is published but not routed", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"8200:80\"\n  metrics:\n    ports:\n      - \"8201:9090\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "partial", Type: store.TypeCompose}
		extras, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: yaml},
			proj, filepath.Join(proj.Dir, app.Slug))
		if err != nil {
			t.Fatalf("layoutCompose: %v", err)
		}
		if len(extras) != 0 {
			t.Errorf("extra routes = %+v, want none (no hostnames given)", extras)
		}
		if app.Port != 8200 {
			t.Errorf("app port = %d, want the first published port", app.Port)
		}
	})

	t.Run("a taken hostname fails the create", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"8300:80\"\n  api:\n    ports:\n      - \"8301:3000\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "dupe", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		// Park the hostname on another app first.
		other, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "Other", Slug: "other", Type: store.TypeCompose})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateDomain(other.ID, "taken.demo.test", true, "internal", 8999); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.layoutCompose(&app, &CreateOpts{
			Type: store.TypeCompose, ComposeFile: yaml,
			PortDomains: map[int]string{8301: "taken.demo.test"},
		}, proj, appDir); err == nil {
			t.Fatal("a hostname another app owns should be rejected")
		}
		if _, err := os.Stat(appDir); !os.IsNotExist(err) {
			t.Errorf("rejected app left %s behind", appDir)
		}
	})

	t.Run("an extra port cannot claim the app's own domain", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"8400:80\"\n  api:\n    ports:\n      - \"8401:3000\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "self", Type: store.TypeCompose, Domain: "self.demo.test"}
		if _, err := svc.layoutCompose(&app, &CreateOpts{
			Type: store.TypeCompose, ComposeFile: yaml,
			PortDomains: map[int]string{8401: "self.demo.test"},
		}, proj, filepath.Join(proj.Dir, app.Slug)); err == nil {
			t.Fatal("an extra port pointing at the app's own domain should be rejected")
		}
	})

	t.Run("bad file is rejected before anything is written", func(t *testing.T) {
		app := store.App{ProjectID: proj.ID, Slug: "bad", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if _, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: "web:\n  image: nginx\n"}, proj, appDir); err == nil {
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
