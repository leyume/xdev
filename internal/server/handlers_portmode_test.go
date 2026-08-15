package server

import (
	"strings"
	"testing"

	xapps "xdev/internal/apps"
	"xdev/internal/store"
)

// On a domainless install the app card's address is the port URL, not a dead
// link to a hostname nobody can resolve.
func TestAppCardAddressesAPortOnlyApp(t *testing.T) {
	portOnly := store.App{ID: 4, Name: "api", Slug: "api", Type: "laravel",
		Status: store.AppRunning, Port: 20431} // no Domain

	out := renderProjectWith(t, []store.App{portOnly}, viewData{
		"Reach": xapps.Reach{PublicHost: "203.0.113.10", ProxyEnabled: true},
	})
	if !strings.Contains(out, "http://203.0.113.10:20431") {
		t.Error("the app card does not show the address a port-only app is actually reached at")
	}
	// And nothing pretends there is a hostname.
	if strings.Contains(out, "https://demo.test") {
		t.Error("the card links to a hostname this app does not have")
	}
}

// The host-port link is the server's address, not loopback: 127.0.0.1 is right
// only for whoever is reading the UI through an ssh tunnel, which is not the
// person the link gets sent to.
func TestHostPortLinkUsesThePublicHost(t *testing.T) {
	app := store.App{ID: 5, Name: "web", Slug: "web", Type: "laravel",
		Domain: "web.demo.test", Status: store.AppRunning, Port: 20001}

	out := renderProjectWith(t, []store.App{app}, viewData{
		"Reach": xapps.Reach{PublicHost: "203.0.113.10", ProxyEnabled: true, HTTPSPort: 443},
	})
	if !strings.Contains(out, "http://203.0.113.10:20001") {
		t.Error("the host-port link does not use the configured public host")
	}
	// The routed hostname is still the app's headline address.
	if !strings.Contains(out, "https://web.demo.test") {
		t.Error("an app that does have a routed hostname lost it")
	}
}

// Without a public host nothing changes: loopback, exactly as before.
func TestHostPortLinkStaysLoopbackByDefault(t *testing.T) {
	app := store.App{ID: 6, Name: "web", Slug: "web", Type: "laravel",
		Domain: "web.demo.test", Status: store.AppRunning, Port: 20001}
	out := renderProjectWith(t, []store.App{app}, nil)
	if !strings.Contains(out, "http://127.0.0.1:20001") {
		t.Error("the default install no longer links the host port at loopback")
	}
}

// A webhook on an app with no hostname has no public URL — and must not be
// given the app's own port, which reaches the app's container rather than the
// control plane. Say what it is instead: a path to publish, and the endpoint on
// xdev's own listener for the ssh-triggered route.
func TestUnroutedWebhookShowsAPathNotAWrongURL(t *testing.T) {
	app := gitApp()
	app.Domain = ""

	out := renderSettings(t, app, appSettingsForm{Name: "web"}, viewData{
		"Deploy": &deployInfo{
			Ref: "main", HookSecret: "the-signing-secret",
			Unrouted: true, HookPath: "/_xdev/hook/hk",
			LocalHookURL: "http://127.0.0.1:7331/_xdev/hook/hk",
		},
	})
	if !strings.Contains(out, "/_xdev/hook/hk") {
		t.Fatal("the webhook path is not shown at all")
	}
	if !strings.Contains(out, "http://127.0.0.1:7331/_xdev/hook/hk") {
		t.Error("the on-this-machine URL is missing, so the ssh route has nothing to copy")
	}
	// The secret is still needed either way.
	if !inCopyRow(out, "the-signing-secret") {
		t.Error("the webhook secret is not copyable on an unrouted app")
	}
	// Nothing that looks like a working public URL.
	if strings.Contains(out, "https:///_xdev") || strings.Contains(out, "://:") {
		t.Error("a malformed payload URL was rendered from an app with no hostname")
	}
}

// localAddr turns a listen address into one that can be dialled. A wildcard
// bind is not an address to connect to.
func TestLocalAddr(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7331": "127.0.0.1:7331",
		"0.0.0.0:7331":   "127.0.0.1:7331",
		"[::]:7331":      "127.0.0.1:7331",
		":7331":          "127.0.0.1:7331",
		"":               "127.0.0.1:7331",
	} {
		if got := localAddr(in); got != want {
			t.Errorf("localAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

// The domain field stops being mandatory only where a hostname is not how apps
// are reached — otherwise an empty field would silently unpublish an app.
func TestDomainRequiredUnlessPortOnly(t *testing.T) {
	normal := renderSettings(t, gitApp(), appSettingsForm{Name: "web", Domain: "web.demo.test"}, nil)
	if !strings.Contains(normal, `name="domain" value="web.demo.test" spellcheck="false" required`) {
		t.Error("the domain field is not required on a normal install")
	}

	portOnly := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, viewData{"PortOnly": true})
	if strings.Contains(portOnly, `name="domain" value="" spellcheck="false" required`) {
		t.Error("the domain field is still required on a port-only install, so a hostname can never be removed")
	}
}
