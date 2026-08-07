package store

import (
	"path/filepath"
	"testing"
)

// TestUnmonitoredReason pins which app shapes can never report resource usage.
// Each is a different reason, and each is the default for its type — a
// WordPress site is shared-mode unless you opt out, and a static app is
// serve-mode unless you give it a start command — so getting these wrong makes
// the common case look broken rather than the exotic one.
func TestUnmonitoredReason(t *testing.T) {
	cases := []struct {
		name        string
		app         App
		unmonitored bool
	}{
		{"proxy", App{Type: TypeProxy}, true},
		{"shared wordpress", App{Type: "wordpress", WPMode: WPShared}, true},
		{"static serving files", App{Type: TypeStatic, ServeMode: ServeStatic}, true},
		// A static app with no serve mode recorded still isn't a process.
		{"static with no serve mode", App{Type: TypeStatic}, true},

		{"static running a command", App{Type: TypeStatic, ServeMode: ServeCommand}, false},
		{"go app", App{Type: TypeGo, ServeMode: ServeCommand}, false},
		{"laravel", App{Type: "laravel"}, false},
		// Separate-mode WordPress has containers of its own, unlike the default.
		{"separate wordpress", App{Type: "wordpress"}, false},
		{"mail", App{Type: "mail"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := tc.app.UnmonitoredReason()
			if (reason != "") != tc.unmonitored {
				t.Errorf("UnmonitoredReason() = %q, want unmonitored=%v", reason, tc.unmonitored)
			}
			if tc.app.Monitorable() == tc.unmonitored {
				t.Errorf("Monitorable() = %v contradicts the reason %q",
					tc.app.Monitorable(), reason)
			}
		})
	}
}

// TestMonitorableMatchesAppPrefixes is the anti-drift check. UnmonitoredReason
// tells the UI an app will never report a sample; AppPrefixes decides which
// apps the collector actually samples containers for. They are separately
// written and separately edited, and if they disagree the UI either promises
// numbers that never arrive or writes off an app that is being measured.
//
// Host-process apps are the deliberate gap between them: they're monitorable,
// but sampled from supervisor PIDs rather than container stats, so they are
// absent from AppPrefixes by design.
func TestMonitorableMatchesAppPrefixes(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	proj, err := st.CreateProject(Project{Name: "demo", Slug: "demo",
		BaseDomain: "demo.test", Environment: "local", NetworkName: "xdev_demo", Dir: "/tmp/demo"})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	apps := []App{
		{Type: TypeProxy, Name: "px", Slug: "px"},
		{Type: "wordpress", WPMode: WPShared, Name: "wp", Slug: "wp"},
		{Type: TypeStatic, ServeMode: ServeStatic, Name: "files", Slug: "files"},
		{Type: TypeStatic, ServeMode: ServeCommand, Name: "vite", Slug: "vite"},
		{Type: TypeGo, ServeMode: ServeCommand, Name: "api", Slug: "api"},
		{Type: "laravel", Name: "app", Slug: "app"},
		{Type: "wordpress", Name: "blog", Slug: "blog"},
		{Type: "mail", Name: "mail", Slug: "mail"},
	}
	want := map[string]bool{} // slug -> should appear in AppPrefixes
	for _, a := range apps {
		a.ProjectID = proj.ID
		a.Status = AppRunning
		saved, err := st.CreateApp(a)
		if err != nil {
			t.Fatalf("create app %s: %v", a.Slug, err)
		}
		want[saved.Slug] = saved.Monitorable() && !saved.IsHostProc()
	}

	prefixes, err := st.AppPrefixes()
	if err != nil {
		t.Fatalf("AppPrefixes: %v", err)
	}
	got := map[string]bool{}
	for _, p := range prefixes {
		got[p.Prefix] = true
	}
	for slug, shouldSample := range want {
		prefix := proj.Slug + "_" + slug
		if got[prefix] != shouldSample {
			t.Errorf("%s: AppPrefixes includes=%v, but Monitorable&&!IsHostProc=%v",
				slug, got[prefix], shouldSample)
		}
	}
}
