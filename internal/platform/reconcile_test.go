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
