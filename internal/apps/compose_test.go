package apps

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// TestComposePorts covers the published-port scan the add-app form and the
// create path both rely on: every port syntax, the service each port belongs to,
// and which slot a ${PORT_n} placeholder claims.
func TestComposePorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want []ComposePort
	}{
		{"port var, quoted", "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n",
			[]ComposePort{{Service: "web", Slot: 1}}},
		{"port var, bare", "services:\n  web:\n    ports:\n      - $PORT:8000\n",
			[]ComposePort{{Service: "web", Slot: 1}}},
		{"port var with default", "services:\n  web:\n    ports:\n      - \"${PORT:-8080}:80\"\n",
			[]ComposePort{{Service: "web", Slot: 1}}},
		{"xdev port var", "services:\n  web:\n    ports:\n      - \"${XDEV_PORT}:80\"\n",
			[]ComposePort{{Service: "web", Slot: 1}}},
		{"numbered slots", "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  api:\n    ports:\n" +
			"      - \"${PORT_2}:3000\"\n  admin:\n    ports:\n      - \"${PORT_3}:9000\"\n",
			[]ComposePort{{Service: "web", Slot: 1}, {Service: "api", Slot: 2}, {Service: "admin", Slot: 3}}},
		{"numbered slot, xdev alias and long syntax",
			"services:\n  api:\n    ports:\n      - target: 3000\n        published: ${XDEV_PORT_2}\n",
			[]ComposePort{{Service: "api", Slot: 2}}},
		{"slot 1 may be written the long way", "services:\n  web:\n    ports:\n      - \"${PORT_1}:80\"\n",
			[]ComposePort{{Service: "web", Slot: 1}}},
		// What a browser textarea actually posts, and what a Windows editor saves.
		{"crlf line endings", "services:\r\n  web:\r\n    ports:\r\n      - \"${PORT}:80\"\r\n",
			[]ComposePort{{Service: "web", Slot: 1}}},
		{"utf-8 bom", "\ufeffservices:\n  web:\n    ports:\n      - \"${PORT}:80\"\n",
			[]ComposePort{{Service: "web", Slot: 1}}},
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
		{"var alongside fixed ports",
			"services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  api:\n    ports:\n      - \"9090:9090\"\n",
			[]ComposePort{{Service: "web", Slot: 1}, {Port: 9090, Service: "api"}}},
		{"a repeated fixed port is listed once",
			"services:\n  a:\n    ports:\n      - \"8080:80\"\n  b:\n    ports:\n      - \"8080:80\"\n",
			[]ComposePort{{Port: 8080, Service: "a"}}},
		{"top-level keys after services don't leak service names",
			"services:\n  web:\n    ports:\n      - \"8080:80\"\nnetworks:\n  internal:\n",
			[]ComposePort{{Port: 8080, Service: "web"}}},
		// Nothing published is not an error here — how many slots are needed is
		// the caller's question (checkComposeSlots), not the scanner's.
		{"nothing published", "services:\n  web:\n    image: nginx\n", nil},
		{"container port only", "services:\n  web:\n    ports:\n      - \"80\"\n", nil},
		{"expose is not a published port", "services:\n  web:\n    expose:\n      - 80\n", nil},
		{"${PORT} outside port position doesn't count",
			"services:\n  web:\n    environment:\n      - BASE_URL=http://localhost:${PORT}\n", nil},
		{"a commented-out port doesn't count",
			"# ports:\n#   - \"${PORT}:80\"\nservices:\n  web:\n    image: nginx\n", nil},
		{"${PORTAL} is not ${PORT}", "services:\n  web:\n    ports:\n      - \"${PORTAL}:80\"\n", nil},
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

	// Two services on the same slot would expand to the same number and collide.
	for name, yaml := range map[string]string{
		"same slot twice": "services:\n  a:\n    ports:\n      - \"${PORT}:80\"\n  b:\n    ports:\n      - \"${PORT}:90\"\n",
		"slot 1 twice, spelled differently": "services:\n  a:\n    ports:\n      - \"${PORT}:80\"\n" +
			"  b:\n    ports:\n      - \"${PORT_1}:90\"\n",
		"slot out of range": "services:\n  a:\n    ports:\n      - \"${PORT_99}:80\"\n",
	} {
		if _, err := ComposePorts(yaml); err == nil {
			t.Errorf("%s: should have been rejected:\n%s", name, yaml)
		}
	}
}

// TestCheckComposeSlots pairs a file against the number of domains asked for:
// every slot needs a service, and a slot past the last domain is a mistake.
func TestCheckComposeSlots(t *testing.T) {
	three := []ComposePort{{Slot: 1}, {Slot: 2}, {Slot: 3}}
	if err := checkComposeSlots(three, 3); err != nil {
		t.Errorf("three slots for three domains: %v", err)
	}
	if err := checkComposeSlots([]ComposePort{{Slot: 1}}, 1); err != nil {
		t.Errorf("one slot for one domain: %v", err)
	}
	// A gap: the file jumps from ${PORT} to ${PORT_3}, so domain 2 routes nowhere.
	if err := checkComposeSlots([]ComposePort{{Slot: 1}, {Slot: 3}}, 3); err == nil {
		t.Error("a missing middle slot should be rejected")
	}
	if err := checkComposeSlots([]ComposePort{{Slot: 1}}, 2); err == nil {
		t.Error("more domains than published slots should be rejected")
	}
	if err := checkComposeSlots(three, 2); err == nil {
		t.Error("a ${PORT_3} with only two domains should be rejected")
	}
	// Hard-coded ports are not slots: they satisfy nothing.
	if err := checkComposeSlots([]ComposePort{{Port: 8080}}, 1); err == nil {
		t.Error("a hard-coded port should not satisfy domain 1")
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
	// A file typed into the browser's textarea arrives CRLF-terminated (the HTML
	// spec says so), and editors add a BOM: neither is a broken compose file, so
	// neither may be rejected as one.
	for name, ok := range map[string]string{
		"crlf":     "# my stack\r\nservices:\r\n  web:\r\n    image: nginx:alpine\r\n",
		"bom":      "\ufeffservices:\n  web:\n    image: nginx:alpine\n",
		"bom+crlf": "\ufeffservices:\r\n  web:\r\n    image: nginx:alpine\r\n",
	} {
		if err := validCompose(ok); err != nil {
			t.Errorf("%s: valid compose rejected: %v", name, err)
		}
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
// file: one variable per allocated port (slot 1 under both spellings), the
// user's own lines surviving a rewrite, and a stale PORT corrected rather than
// duplicated.
func TestEnsureComposeEnv(t *testing.T) {
	dir := t.TempDir()
	if err := ensureComposeEnv(dir, []int{20001}); err != nil {
		t.Fatalf("ensureComposeEnv: %v", err)
	}
	body := readFile(t, filepath.Join(dir, ".env"))
	for _, want := range []string{"PORT=20001\n", "XDEV_PORT=20001\n", "PORT_1=20001\n", "XDEV_PORT_1=20001\n"} {
		if !strings.Contains(body, want) {
			t.Fatalf(".env missing %q:\n%s", want, body)
		}
	}

	// A multi-domain app: one numbered pair per slot, PORT still the first.
	if err := ensureComposeEnv(dir, []int{20001, 20002, 20003}); err != nil {
		t.Fatalf("ensureComposeEnv multi: %v", err)
	}
	body = readFile(t, filepath.Join(dir, ".env"))
	for _, want := range []string{"PORT=20001\n", "PORT_2=20002\n", "XDEV_PORT_2=20002\n", "PORT_3=20003\n"} {
		if !strings.Contains(body, want) {
			t.Errorf(".env missing %q:\n%s", want, body)
		}
	}

	// A user adds their own variable and (wrongly) edits the managed one.
	if err := os.WriteFile(filepath.Join(dir, ".env"),
		[]byte("PORT=1\nPORT_2=1\nAPI_KEY=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureComposeEnv(dir, []int{20004, 20005}); err != nil {
		t.Fatalf("ensureComposeEnv rewrite: %v", err)
	}
	body = readFile(t, filepath.Join(dir, ".env"))
	if !strings.Contains(body, "API_KEY=secret") {
		t.Errorf("user variable was dropped:\n%s", body)
	}
	// PORT, XDEV_PORT, PORT_1, XDEV_PORT_1, PORT_2, XDEV_PORT_2 — each once.
	if got := strings.Count(body, "PORT"); got != 6 {
		t.Errorf("stale port lines not replaced (%d PORT lines):\n%s", got, body)
	}
	if !strings.Contains(body, "PORT=20004\n") || !strings.Contains(body, "PORT_2=20005\n") {
		t.Errorf("ports not corrected:\n%s", body)
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
	if len(ports) != 1 || ports[0].Var() || ports[0].Port != 20005 {
		t.Errorf("starter publishes %+v, want just the allocated 20005", ports)
	}
}

// TestLayoutCompose writes the on-disk layout for both ways of creating a
// compose app — a supplied file and the starter — and checks the pieces the
// rest of xdev depends on: the file lands at _/compose.yml untouched, the app
// row points at it, one allocated port per domain asked for, and a _/.env that
// carries them for ${PORT_n} substitution.
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

	t.Run("single-domain file gets one allocated port", func(t *testing.T) {
		yaml := "services:\n  web:\n    image: caddy:2\n    ports:\n      - \"${PORT}:80\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "byo", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		extras, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: yaml}, proj, appDir, []string{app.Domain})
		if err != nil {
			t.Fatalf("layoutCompose: %v", err)
		}
		if len(extras) != 0 {
			t.Errorf("extra routes = %+v, want none for a single domain", extras)
		}
		if got := readFile(t, filepath.Join(appDir, "_", "compose.yml")); got != yaml {
			t.Errorf("compose file was rewritten:\n%s", got)
		}
		if app.ComposePath != filepath.Join(appDir, "_", "compose.yml") {
			t.Errorf("ComposePath = %q", app.ComposePath)
		}
		if app.Port < portMin || app.Port > portMax {
			t.Errorf("app port = %d, want one from the allocator range", app.Port)
		}
		if _, err := os.Stat(filepath.Join(appDir, "app")); err != nil {
			t.Errorf("app content dir missing: %v", err)
		}
	})

	// A pasted file arrives with CRLF line endings — the browser's doing, not the
	// user's. It has to be accepted, and stored as the LF text the engine reads.
	t.Run("a pasted crlf file is accepted and stored as lf", func(t *testing.T) {
		yaml := "services:\r\n  web:\r\n    image: nginx:alpine\r\n    ports:\r\n      - \"${PORT}:80\"\r\n"
		app := store.App{ProjectID: proj.ID, Slug: "crlf", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if _, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: yaml}, proj, appDir, []string{app.Domain}); err != nil {
			t.Fatalf("layoutCompose: %v", err)
		}
		got := readFile(t, filepath.Join(appDir, "_", "compose.yml"))
		if strings.Contains(got, "\r") {
			t.Errorf("carriage returns survived into the written file:\n%q", got)
		}
		if got != strings.ReplaceAll(yaml, "\r\n", "\n") {
			t.Errorf("compose file was rewritten:\n%q", got)
		}
	})

	t.Run("no file gets the starter", func(t *testing.T) {
		app := store.App{ProjectID: proj.ID, Slug: "starter", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if _, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose}, proj, appDir, []string{app.Domain}); err != nil {
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

	t.Run("a domain per slot, each on its own allocated port", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  api:\n    ports:\n" +
			"      - \"${PORT_2}:3000\"\n  admin:\n    ports:\n      - \"${PORT_3}:9000\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "multi", Type: store.TypeCompose, Domain: "multi.demo.test"}
		appDir := filepath.Join(proj.Dir, app.Slug)
		extras, err := svc.layoutCompose(&app, &CreateOpts{
			Type:         store.TypeCompose,
			ComposeFile:  yaml,
			ExtraDomains: []string{"api.demo.test", "admin.demo.test"},
		}, proj, appDir, []string{app.Domain})
		if err != nil {
			t.Fatalf("layoutCompose: %v", err)
		}
		if len(extras) != 2 {
			t.Fatalf("extra routes = %+v, want one per extra domain", extras)
		}
		// Positional: domain 2 is ${PORT_2}, domain 3 is ${PORT_3}.
		if extras[0].Host != "api.demo.test" || extras[1].Host != "admin.demo.test" {
			t.Errorf("extras out of order: %+v", extras)
		}
		seen := map[int]bool{app.Port: true}
		for _, r := range extras {
			if r.Port < portMin || r.Port > portMax {
				t.Errorf("route %+v has no allocated port", r)
			}
			if seen[r.Port] {
				t.Errorf("port %d handed out twice (%+v)", r.Port, extras)
			}
			seen[r.Port] = true
		}
		// Every slot the file publishes resolves in .env.
		env := readFile(t, filepath.Join(appDir, "_", ".env"))
		for _, want := range []string{
			"PORT=" + itoa(app.Port), "PORT_2=" + itoa(extras[0].Port), "PORT_3=" + itoa(extras[1].Port),
		} {
			if !strings.Contains(env, want+"\n") {
				t.Errorf(".env missing %q:\n%s", want, env)
			}
		}
	})

	t.Run("a slot the file never publishes fails the create", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "gap", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if _, err := svc.layoutCompose(&app, &CreateOpts{
			Type: store.TypeCompose, ComposeFile: yaml,
			ExtraDomains: []string{"api.demo.test"},
		}, proj, appDir, []string{app.Domain}); err == nil {
			t.Fatal("a second domain with no ${PORT_2} should be rejected")
		}
		if _, err := os.Stat(appDir); !os.IsNotExist(err) {
			t.Errorf("rejected app left %s behind", appDir)
		}
	})

	t.Run("a blank domain is rejected, not skipped", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  api:\n    ports:\n      - \"${PORT_2}:3000\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "blank", Type: store.TypeCompose}
		if _, err := svc.layoutCompose(&app, &CreateOpts{
			Type: store.TypeCompose, ComposeFile: yaml,
			ExtraDomains: []string{"  "},
		}, proj, filepath.Join(proj.Dir, app.Slug), []string{app.Domain}); err == nil {
			t.Fatal("a blank domain should be rejected — skipping it would renumber the slots")
		}
	})

	t.Run("hard-coded ports are left alone but never reallocated", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  metrics:\n    ports:\n      - \"" +
			itoa(portMin) + ":9090\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "fixed", Type: store.TypeCompose}
		if _, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: yaml},
			proj, filepath.Join(proj.Dir, app.Slug), []string{app.Domain}); err != nil {
			t.Fatalf("layoutCompose: %v", err)
		}
		if app.Port == portMin {
			t.Errorf("allocated %d, the port the file hard-codes", app.Port)
		}
	})

	t.Run("a taken hostname fails the create", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  api:\n    ports:\n      - \"${PORT_2}:3000\"\n"
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
			ExtraDomains: []string{"taken.demo.test"},
		}, proj, appDir, []string{app.Domain}); err == nil {
			t.Fatal("a hostname another app owns should be rejected")
		}
		if _, err := os.Stat(appDir); !os.IsNotExist(err) {
			t.Errorf("rejected app left %s behind", appDir)
		}
	})

	t.Run("an extra domain cannot claim the app's own domain", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  api:\n    ports:\n      - \"${PORT_2}:3000\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "self", Type: store.TypeCompose, Domain: "self.demo.test"}
		if _, err := svc.layoutCompose(&app, &CreateOpts{
			Type: store.TypeCompose, ComposeFile: yaml,
			ExtraDomains: []string{"self.demo.test"},
		}, proj, filepath.Join(proj.Dir, app.Slug), []string{app.Domain}); err == nil {
			t.Fatal("an extra domain repeating the app's own should be rejected")
		}
	})

	t.Run("the same domain twice is rejected", func(t *testing.T) {
		yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  a:\n    ports:\n      - \"${PORT_2}:3000\"\n" +
			"  b:\n    ports:\n      - \"${PORT_3}:4000\"\n"
		app := store.App{ProjectID: proj.ID, Slug: "twice", Type: store.TypeCompose}
		if _, err := svc.layoutCompose(&app, &CreateOpts{
			Type: store.TypeCompose, ComposeFile: yaml,
			ExtraDomains: []string{"same.demo.test", "same.demo.test"},
		}, proj, filepath.Join(proj.Dir, app.Slug), []string{app.Domain}); err == nil {
			t.Fatal("two slots on one hostname should be rejected")
		}
	})

	t.Run("the starter cannot serve more than one domain", func(t *testing.T) {
		app := store.App{ProjectID: proj.ID, Slug: "starter2", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if _, err := svc.layoutCompose(&app, &CreateOpts{
			Type: store.TypeCompose, ExtraDomains: []string{"api.demo.test"},
		}, proj, appDir, []string{app.Domain}); err == nil {
			t.Fatal("asking the starter for two domains should be rejected")
		}
		if _, err := os.Stat(appDir); !os.IsNotExist(err) {
			t.Errorf("rejected app left %s behind", appDir)
		}
	})

	t.Run("more domains than the cap is rejected", func(t *testing.T) {
		var yaml strings.Builder
		yaml.WriteString("services:\n")
		extra := make([]string, 0, MaxComposeSlots)
		for i := 1; i <= MaxComposeSlots+1; i++ {
			yaml.WriteString("  s" + itoa(i) + ":\n    ports:\n      - \"" + portVar(i) + ":80\"\n")
			if i > 1 {
				extra = append(extra, "s"+itoa(i)+".demo.test")
			}
		}
		app := store.App{ProjectID: proj.ID, Slug: "toomany", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if _, err := svc.layoutCompose(&app, &CreateOpts{
			Type: store.TypeCompose, ComposeFile: yaml.String(), ExtraDomains: extra,
		}, proj, appDir, []string{app.Domain}); err == nil {
			t.Fatalf("asking for %d domains should be rejected (cap is %d)", MaxComposeSlots+1, MaxComposeSlots)
		}
		if _, err := os.Stat(appDir); !os.IsNotExist(err) {
			t.Errorf("rejected app left %s behind", appDir)
		}
	})

	t.Run("bad file is rejected before anything is written", func(t *testing.T) {
		app := store.App{ProjectID: proj.ID, Slug: "bad", Type: store.TypeCompose}
		appDir := filepath.Join(proj.Dir, app.Slug)
		if _, err := svc.layoutCompose(&app, &CreateOpts{Type: store.TypeCompose, ComposeFile: "web:\n  image: nginx\n"}, proj, appDir, []string{app.Domain}); err == nil {
			t.Fatal("a file without a services block should be rejected")
		}
		if _, err := os.Stat(appDir); !os.IsNotExist(err) {
			t.Errorf("rejected app left %s behind", appDir)
		}
	})
}

// TestPrepareCompose covers the pre-start pass over a compose app: the managed
// .env is refreshed for every domain the app owns, a slot the file stopped
// publishing is refused with a message naming it, and an app whose file
// hard-codes its port keeps following that number.
func TestPrepareCompose(t *testing.T) {
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

	// A two-domain app, laid out and persisted the way Create does it.
	yaml := "services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n  api:\n    ports:\n      - \"${PORT_2}:3000\"\n"
	app := store.App{ProjectID: proj.ID, Name: "Multi", Slug: "multi", Type: store.TypeCompose, Domain: "multi.demo.test"}
	appDir := filepath.Join(proj.Dir, app.Slug)
	extras, err := svc.layoutCompose(&app, &CreateOpts{
		Type: store.TypeCompose, ComposeFile: yaml, ExtraDomains: []string{"api.demo.test"},
	}, proj, appDir, []string{app.Domain})
	if err != nil {
		t.Fatalf("layoutCompose: %v", err)
	}
	saved, err := st.CreateApp(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDomain(saved.ID, extras[0].Host, true, "internal", extras[0].Port); err != nil {
		t.Fatal(err)
	}

	t.Run("every slot is refreshed in .env", func(t *testing.T) {
		// Blow the .env away: a start must be able to rebuild it from the rows.
		if err := os.Remove(filepath.Join(appDir, "_", ".env")); err != nil {
			t.Fatal(err)
		}
		got, err := svc.prepareCompose(saved)
		if err != nil {
			t.Fatalf("prepareCompose: %v", err)
		}
		if got.Port != saved.Port {
			t.Errorf("port moved to %d; xdev's ports don't move", got.Port)
		}
		env := readFile(t, filepath.Join(appDir, "_", ".env"))
		for _, want := range []string{"PORT=" + itoa(saved.Port), "PORT_2=" + itoa(extras[0].Port)} {
			if !strings.Contains(env, want+"\n") {
				t.Errorf(".env missing %q:\n%s", want, env)
			}
		}
	})

	t.Run("a slot the file stopped publishing is refused", func(t *testing.T) {
		if err := os.WriteFile(saved.ComposePath,
			[]byte("services:\n  web:\n    ports:\n      - \"${PORT}:80\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := svc.prepareCompose(saved)
		if err == nil {
			t.Fatal("dropping ${PORT_2} should stop the start")
		}
		if !strings.Contains(err.Error(), "${PORT_2}") || !strings.Contains(err.Error(), "domain 2") {
			t.Errorf("error should name the slot and the domain: %v", err)
		}
		// Put it back for any later subtest.
		if err := os.WriteFile(saved.ComposePath, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a single-domain app still follows a hard-coded port", func(t *testing.T) {
		// Apps created before ports were variables: the file names a number, and
		// editing it repoints the domain on the next start.
		legacy := store.App{ProjectID: proj.ID, Name: "Legacy", Slug: "legacy", Type: store.TypeCompose, Port: 8099}
		dir := filepath.Join(proj.Dir, legacy.Slug, "_")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		legacy.ComposePath = filepath.Join(dir, "compose.yml")
		if err := os.WriteFile(legacy.ComposePath,
			[]byte("services:\n  web:\n    ports:\n      - \"8098:80\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		row, err := st.CreateApp(legacy)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.prepareCompose(row)
		if err != nil {
			t.Fatalf("prepareCompose: %v", err)
		}
		if got.Port != 8098 {
			t.Errorf("port = %d, want the 8098 the edited file publishes", got.Port)
		}
		if env := readFile(t, filepath.Join(dir, ".env")); !strings.Contains(env, "PORT=8098\n") {
			t.Errorf(".env not repointed:\n%s", env)
		}
	})
}

func itoa(n int) string { return strconv.Itoa(n) }

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
