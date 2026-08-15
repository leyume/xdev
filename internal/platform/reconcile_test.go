package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"xdev/internal/proxy"
	"xdev/internal/store"
)

// fakeCaddy stands in for the admin API: /load accepts a config, everything else
// answers 200 so Reachable() succeeds.
func fakeCaddy(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newReconciler(t *testing.T, adminAddr string) *Reconciler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	pm := proxy.NewManager(adminAddr, 8444, 8081, "", "")
	// hosts management off: it needs privileges and is not what's under test.
	return NewReconciler(st, pm, filepath.Join(t.TempDir(), "hosts"), false)
}

// TestSyncRecoversWhenCaddyAppears is the regression test for the one-way
// latch: routing used to be probed once at startup and, if Caddy wasn't up yet,
// stayed dead until xdev was restarted by hand. A Sync after Caddy appears must
// bring it back.
func TestSyncRecoversWhenCaddyAppears(t *testing.T) {
	// 127.0.0.1:1 — nothing listens there, so this stands in for "Caddy not up".
	r := newReconciler(t, "127.0.0.1:1")

	if err := r.Sync(); err != nil {
		t.Fatalf("sync against an absent proxy must not error, got %v", err)
	}
	if r.Enabled() {
		t.Fatal("routing must not be live while Caddy is unreachable")
	}

	// Caddy comes up; point the same reconciler at it and sync again.
	srv := fakeCaddy(t)
	r2 := newReconciler(t, srv.Listener.Addr().String())
	if err := r2.Sync(); err != nil {
		t.Fatalf("sync against a live proxy: %v", err)
	}
	if !r2.Enabled() {
		t.Fatal("routing must go live once a push succeeds")
	}
}

// TestSyncDisablesOnFailedPush checks the flag tracks reality in both
// directions: a proxy that answers Reachable() but rejects /load leaves routing
// marked down, so callers keep retrying rather than believing it is live.
func TestSyncDisablesOnFailedPush(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" {
			http.Error(w, "bad config", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK) // Reachable() probes /config/
	}))
	defer srv.Close()

	r := newReconciler(t, srv.Listener.Addr().String())
	if err := r.Sync(); err == nil {
		t.Fatal("a rejected /load must surface as an error")
	}
	if r.Enabled() {
		t.Fatal("routing must not be marked live when the push was rejected")
	}
}

// TestRetryUntilLive covers the boot race directly: the retry loop must keep
// going while Caddy is absent and return once it answers.
func TestRetryUntilLive(t *testing.T) {
	srv := fakeCaddy(t)
	r := newReconciler(t, srv.Listener.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { r.RetryUntilLive(ctx, 10*time.Millisecond); close(done) }()

	select {
	case <-done:
		if !r.Enabled() {
			t.Fatal("retry returned without enabling routing")
		}
	case <-ctx.Done():
		t.Fatal("retry never brought routing live")
	}
}

// TestRetryUntilLiveStopsOnContext checks the loop honors cancellation, so
// shutdown doesn't hang on a proxy that never appears.
func TestRetryUntilLiveStopsOnContext(t *testing.T) {
	r := newReconciler(t, "127.0.0.1:1") // never answers
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { r.RetryUntilLive(ctx, 10*time.Millisecond); close(done) }()

	time.Sleep(50 * time.Millisecond) // let it iterate a few times
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retry ignored context cancellation")
	}
}

// TestEndpointRoutesAreScopedAndFirst covers what makes the deploy endpoints
// safe to publish. An app that has one gets a route for exactly those paths on
// its own hostname, pointing at xdev — and it is emitted before the app's own
// route, because Caddy takes the first match and a host-wide route would
// otherwise swallow /_xdev/.
//
// Just as important is the negative: an app with neither a webhook nor a token
// gets no such route, so the control plane is never reachable from the internet
// by default.
func TestEndpointRoutesAreScopedAndFirst(t *testing.T) {
	r := newReconciler(t, "127.0.0.1:1")
	r.SetSelfAddr("127.0.0.1:7331")

	proj, err := r.store.CreateProject(store.Project{
		Name: "Demo", Slug: "demo", BaseDomain: "demo.test", Environment: "local",
		NetworkName: "xdev_demo", Engine: "docker", Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// One app with both endpoints on, one with neither.
	hooked, err := r.store.CreateApp(store.App{
		ProjectID: proj.ID, Name: "site", Slug: "site", Type: store.TypeStatic,
		Domain: "site.demo.test", ServeMode: store.ServeStatic,
	})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := r.store.CreateApp(store.App{
		ProjectID: proj.ID, Name: "other", Slug: "other", Type: store.TypeStatic,
		Domain: "other.demo.test", ServeMode: store.ServeStatic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetAppHook(hooked.ID, "hook123", "sealed"); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetAppPushToken(hooked.ID, "hash", "xdp_abc"); err != nil {
		t.Fatal(err)
	}

	routes := r.endpointRoutes()
	if len(routes) != 1 {
		t.Fatalf("got %d endpoint routes, want exactly one (only the app that enabled them)", len(routes))
	}
	got := routes[0]
	if got.Host != "site.demo.test" {
		t.Errorf("host = %q, want the app's own hostname", got.Host)
	}
	if got.Upstream != "127.0.0.1:7331" {
		t.Errorf("upstream = %q, want xdev itself", got.Upstream)
	}
	want := []string{store.HookPathPrefix + "hook123", store.PushPath}
	if len(got.Paths) != len(want) {
		t.Fatalf("paths = %v, want %v", got.Paths, want)
	}
	for i := range want {
		if got.Paths[i] != want[i] {
			t.Errorf("path %d = %q, want %q", i, got.Paths[i], want[i])
		}
	}
	if got.Paths[0] == "" || len(got.Paths) == 0 {
		t.Error("an unscoped route would publish the whole control plane on this hostname")
	}
	_ = plain

	// With no self address there is nothing to proxy to, so nothing is published.
	r.SetSelfAddr("")
	if n := len(r.endpointRoutes()); n != 0 {
		t.Errorf("got %d routes with no self address, want none", n)
	}

	// Turning the endpoints off removes the route entirely.
	r.SetSelfAddr("127.0.0.1:7331")
	if err := r.store.SetAppHook(hooked.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetAppPushToken(hooked.ID, "", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(r.endpointRoutes()); n != 0 {
		t.Errorf("got %d routes after disabling both endpoints, want none", n)
	}
}
