package apps

import (
	"net"
	"path/filepath"
	"testing"

	"xdev/internal/store"
)

// TestChosenPortIsUsed: a port the user picks for a new app becomes its port,
// even outside xdev's own range, and the app's other ports are allocated
// around it.
func TestChosenPortIsUsed(t *testing.T) {
	svc, _, proj := editFixture(t)
	want := freePortOutsideRange(t)

	app := store.App{ProjectID: proj.ID, Slug: "web", Type: store.TypeStatic}
	opts := &CreateOpts{Type: store.TypeStatic, ServeMode: store.ServeCommand, Port: want}
	if err := svc.layoutStatic(&app, opts, filepath.Join(proj.Dir, "web")); err != nil {
		t.Fatalf("layoutStatic: %v", err)
	}
	if app.Port != want {
		t.Errorf("app port = %d, want the chosen %d", app.Port, want)
	}

	ports, err := svc.appPorts(2, want)
	if err != nil {
		t.Fatalf("appPorts: %v", err)
	}
	if ports[0] != want || ports[1] == want || ports[1] < portMin || ports[1] > portMax {
		t.Errorf("appPorts(2, %d) = %v, want [%d <allocated>]", want, ports, want)
	}
}

// TestChosenPortRejected: a port that is unbindable, owned by another app, or
// already listening on the host is refused rather than handed out.
func TestChosenPortRejected(t *testing.T) {
	svc, st, proj := editFixture(t)

	owned := hostAppIn(t, svc, st, proj, "owner", "", store.ServeCommand)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	bound := ln.Addr().(*net.TCPAddr).Port

	for name, p := range map[string]int{
		"privileged":      80,
		"out of range":    70000,
		"owned by an app": owned.Port,
		"bound on host":   bound,
	} {
		if err := svc.checkChosenPort(p); err == nil {
			t.Errorf("%s: checkChosenPort(%d) should be refused", name, p)
		}
	}
}

// TestTakesPort: only types that run a server can be given a port.
func TestTakesPort(t *testing.T) {
	for _, c := range []struct {
		opts CreateOpts
		want bool
	}{
		{CreateOpts{Type: store.TypeProxy}, false},
		{CreateOpts{Type: "wordpress"}, false},
		{CreateOpts{Type: "wordpress", WPMode: "separate"}, true},
		{CreateOpts{Type: store.TypeStatic}, false},
		{CreateOpts{Type: store.TypeStatic, ServeMode: store.ServeCommand}, true},
		{CreateOpts{Type: store.TypeGo}, true},
		{CreateOpts{Type: "laravel"}, true},
		{CreateOpts{Type: store.TypeCompose}, true},
	} {
		if got := takesPort(c.opts); got != c.want {
			t.Errorf("takesPort(%+v) = %v, want %v", c.opts, got, c.want)
		}
	}
}

// freePortOutsideRange finds a bindable port below xdev's allocation range, so
// the test proves the chosen port is used rather than coincidentally allocated.
func freePortOutsideRange(t *testing.T) int {
	t.Helper()
	for p := 15000; p < portMin; p++ {
		if portFree(p) {
			return p
		}
	}
	t.Skip("no free port below the allocation range")
	return 0
}
