package apps

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// mailFixture lays out a mail app the way Create would (admin console as its
// first service domain), without starting anything.
func mailFixture(t *testing.T) (*Service, *store.Store, store.App, int) {
	t.Helper()
	svc, st, proj := editFixture(t)
	app := store.App{ProjectID: proj.ID, Name: "post", Slug: "post", Type: "mail",
		Domain: "sail.demo.test", Runtime: "docker"}
	opts := CreateOpts{Name: "post", Type: "mail", Domain: "sail.demo.test"}
	adminPort, err := svc.layoutContainer(&app, &opts, proj, filepath.Join(proj.Dir, app.Slug))
	if err != nil {
		t.Fatalf("layoutContainer: %v", err)
	}
	saved, err := st.CreateApp(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAppDomains(saved.ID, []string{app.Domain},
		[]store.ServiceDomain{{Host: "zail.demo.test", Port: adminPort}}, true, "internal"); err != nil {
		t.Fatal(err)
	}
	return svc, st, saved, adminPort
}

func TestRailPartialRenders(t *testing.T) {
	out, err := templates.RenderPartial("mail", "rail_service", templates.Data{
		ProjectSlug: "biz", AppSlug: "post", Env: "prod", RailPort: 20031})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  rail:", "leyume/rail", `"20031:8080"`, "IMAP_HOST: mail",
		"SMTP_PORT: \"465\"", `RAIL_SECURE_COOKIE: "true"`, "ADMIN_PASSWORD: ${RAIL_ADMIN_PASSWORD:-}", "./rail-data:/data", "biz_post_rail"} {
		if !strings.Contains(out, want) {
			t.Errorf("rail block missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "RAIL_DOMAIN") {
		t.Error("RAIL_DOMAIN must never be set: rail would try to do its own HTTPS")
	}
	if got := composeRailPort("services:\n  mail:\n    image: x\n" + out); got != 20031 {
		t.Errorf("composeRailPort = %d, want 20031", got)
	}
}

// Rail goes on beside SnappyMail, moves hostname keeping its port, and comes
// off again — the admin console's route untouched throughout.
func TestSetRailRoundTrip(t *testing.T) {
	svc, st, app, adminPort := mailFixture(t)
	if h, p := svc.RailHost(app); h != "" || p != 0 {
		t.Fatalf("new mail app has Rail: %q %d", h, p)
	}

	host, err := svc.SetRail(app.ID, true, "")
	if err != nil || host != "rail.sail.demo.test" {
		t.Fatalf("SetRail(on) = %q, %v", host, err)
	}
	h, port := svc.RailHost(app)
	if h != host || port == 0 || port == adminPort || port == app.Port {
		t.Fatalf("RailHost = %q %d (admin %d, app %d)", h, port, adminPort, app.Port)
	}
	if !strings.Contains(composeOf(t, app), "  webmail:") {
		t.Error("SnappyMail went away")
	}
	ds, _ := st.AppServiceDomains(app.ID)
	if len(ds) != 2 || ds[0].Host != "zail.demo.test" || ds[0].Port != adminPort {
		t.Fatalf("service domains = %v, want admin console first", ds)
	}

	// The admin console is proxied over HTTPS; Rail is plain HTTP on its port.
	routes, err := st.ProxyRoutes()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, r := range routes {
		switch r.Host {
		case "zail.demo.test":
			seen++
			if !strings.HasPrefix(r.Upstream, "https://127.0.0.1:") {
				t.Errorf("admin console route = %+v, want https upstream", r)
			}
		case host:
			seen++
			if r.Port != port || r.Upstream != "" {
				t.Errorf("rail route = %+v, want plain port %d", r, port)
			}
		}
	}
	if seen != 2 {
		t.Errorf("found %d of the 2 service routes in %+v", seen, routes)
	}

	if _, err := svc.SetRail(app.ID, true, "Mail.Demo.Test"); err != nil {
		t.Fatal(err)
	}
	if h2, p2 := svc.RailHost(app); h2 != "mail.demo.test" || p2 != port {
		t.Errorf("after move RailHost = %q %d, want mail.demo.test %d", h2, p2, port)
	}
	if n := strings.Count(composeOf(t, app), "  rail:"); n != 1 {
		t.Errorf("%d rail services after a move, want 1", n)
	}

	if _, err := svc.SetRail(app.ID, true, "sail.demo.test"); err == nil {
		t.Error("Rail took SnappyMail's own domain")
	}
	if _, err := svc.SetRail(app.ID, true, "zail.demo.test"); err == nil {
		t.Error("Rail took the admin console's domain")
	}

	if _, err := svc.SetRail(app.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(composeOf(t, app), "  rail:") {
		t.Error("rail service survived being switched off")
	}
	ds, _ = st.AppServiceDomains(app.ID)
	if len(ds) != 1 || ds[0].Host != "zail.demo.test" {
		t.Errorf("service domains after off = %v", ds)
	}
	if hs, _ := st.AppHostnames(app.ID); len(hs) != 1 || hs[0] != "sail.demo.test" {
		t.Errorf("app hostnames = %v", hs)
	}
}

func TestSetRailRefusesOtherTypes(t *testing.T) {
	svc, _, app := laravelFixture(t, true)
	if _, err := svc.SetRail(app.ID, true, ""); err == nil {
		t.Error("SetRail accepted a laravel app")
	}
}

// A hostname another app owns (the old webmail proxy) is refused, not taken.
func TestSetRailRefusesForeignHost(t *testing.T) {
	svc, st, app, _ := mailFixture(t)
	other, err := st.CreateApp(store.App{ProjectID: app.ProjectID, Name: "old", Slug: "old", Type: "proxy",
		Domain: "mail.demo.test", Upstream: "https://sail.demo.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDomain(other.ID, "mail.demo.test", true, "internal", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetRail(app.ID, true, "mail.demo.test"); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("SetRail on a foreign host = %v", err)
	}
}

// SnappyMail can go off only while Rail is on; Rail then also publishes the
// app's own port, and turning SnappyMail back on gives that port back.
func TestSetSnappyMail(t *testing.T) {
	svc, _, app, _ := mailFixture(t)
	if err := svc.SetSnappyMail(app.ID, false); err == nil {
		t.Fatal("SnappyMail turned off with no Rail to take its domain")
	}
	if _, err := svc.SetRail(app.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	appPort := `"` + strconv.Itoa(app.Port) + `:8080"`

	if err := svc.SetSnappyMail(app.ID, false); err != nil {
		t.Fatal(err)
	}
	y := composeOf(t, app)
	if strings.Contains(y, "  webmail:") || !strings.Contains(y, appPort) {
		t.Errorf("SnappyMail off: want no webmail service and Rail on %s:\n%s", appPort, y)
	}
	w := svc.WebmailState(app)
	if w.Snappy || w.RailPort == 0 || w.RailPort == app.Port {
		t.Errorf("state = %+v (app port %d)", w, app.Port)
	}
	if _, err := svc.SetRail(app.ID, false, ""); err == nil {
		t.Error("Rail turned off while it is the only webmail")
	}
	// Moving Rail keeps it on the app's port while SnappyMail is off.
	if _, err := svc.SetRail(app.ID, true, "r2.demo.test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(composeOf(t, app), appPort) {
		t.Error("moving Rail dropped the app's own port")
	}

	if err := svc.SetSnappyMail(app.ID, true); err != nil {
		t.Fatal(err)
	}
	y = composeOf(t, app)
	if !strings.Contains(y, "  webmail:") || strings.Contains(y, appPort) {
		t.Errorf("SnappyMail on: want webmail back and Rail off the app port:\n%s", y)
	}
	if w := svc.WebmailState(app); !w.Snappy || w.RailHost != "r2.demo.test" {
		t.Errorf("state after on = %+v", w)
	}
}

// SnappyMail's hostnames are the app's own; changing them leaves the admin
// console and Rail routes alone and refuses theirs.
func TestSetOwnHosts(t *testing.T) {
	svc, st, app, adminPort := mailFixture(t)
	if _, err := svc.SetRail(app.ID, true, "rail.demo.test"); err != nil {
		t.Fatal(err)
	}
	hosts, err := svc.SetOwnHosts(app.ID, "Webmail.Demo.Test, sail.demo.test")
	if err != nil || len(hosts) != 2 || hosts[0] != "webmail.demo.test" {
		t.Fatalf("SetOwnHosts = %v, %v", hosts, err)
	}
	got, _ := st.AppByID(app.ID)
	if got.Domain != "webmail.demo.test" {
		t.Errorf("app domain = %q", got.Domain)
	}
	ds, _ := st.AppServiceDomains(app.ID)
	if len(ds) != 2 || ds[0].Host != "zail.demo.test" || ds[0].Port != adminPort || ds[1].Host != "rail.demo.test" {
		t.Errorf("service domains = %v", ds)
	}
	for _, bad := range []string{"", "zail.demo.test", "rail.demo.test"} {
		if _, err := svc.SetOwnHosts(app.ID, bad); err == nil {
			t.Errorf("SetOwnHosts(%q) accepted", bad)
		}
	}
}
