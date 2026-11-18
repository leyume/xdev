package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNgxRender(t *testing.T) {
	n := &Ngx{httpsPort: 443, upstreamHost: "host.docker.internal", acmeEmail: "a@b.c"}
	got := n.render([]Route{
		{Host: "app.test", Paths: []string{"/_xdev/deploy"}, Upstream: "127.0.0.1:7331"},
		{Host: "orphan.test", Paths: []string{"/_xdev/deploy"}, Upstream: "127.0.0.1:7331"}, // no own route: dropped
		{Host: "app.test", Upstream: "127.0.0.1:20000", Internal: true},
		{Host: "static.test", Root: "/srv/site", Internal: true},
		{Host: "wp.test", Root: "/srv/wp", FCGIPort: 9001, Internal: true},
		{Host: "mail.example.com", Upstream: "https://127.0.0.1:8443/webmail/"},
		{Host: "remote.example.com", Upstream: "https://origin.example.net"},
		{Host: "plain.example.com", Upstream: "http://10.0.0.5:8080/x"},
		{Host: "rz.example.com", Upstream: "https://203.0.113.9:7443/app", Insecure: true}, // self-signed origin
		{Host: "bad.test", Root: "/srv/my site", Internal: true},                           // space: ngx can't express it
	})
	// SITES as whole entries, so "a=x" can't pass by matching the start of "a=x/y"
	_, sites, _ := strings.Cut(got, "SITES='")
	sites, _, _ = strings.Cut(sites, "'")
	entries := map[string]bool{}
	for _, e := range strings.Fields(sites) {
		entries[e] = true
	}
	for _, want := range []string{
		"app.test/_xdev/deploy=host.docker.internal:7331",
		"app.test=host.docker.internal:20000",
		"static.test=/srv/site",
		"wp.test=fastcgi://host.docker.internal:9001/srv/wp",
		"mail.example.com=https+insecure://host.docker.internal:8443/webmail",
		"remote.example.com=https://origin.example.net:443",
		"plain.example.com=http://10.0.0.5:8080/x",
		"rz.example.com=https+insecure://203.0.113.9:7443/app",
		// already under the prefix: through as it is, not prefixed twice
		"mail.example.com/webmail/*=https+insecure://host.docker.internal:8443",
		"plain.example.com/x/*=http://10.0.0.5:8080",
		"rz.example.com/app/*=https+insecure://203.0.113.9:7443", // unverified like its app
	} {
		if !entries[want] {
			t.Errorf("SITES has no entry %q:\n%s", want, sites)
		}
	}
	for _, want := range []string{"TLS_INTERNAL='app.test static.test wp.test'", "ACME_EMAIL='a@b.c'"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	for _, bad := range []string{"orphan.test", "bad.test", "HTTPS_PORT", "HTTPS_REDIRECT", "remote.example.com/"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in\n%s", bad, got)
		}
	}
	n.httpsPort, n.disableHTTPSRedirect = 8443, true
	if got := n.render(nil); !strings.Contains(got, "HTTPS_PORT='8443'") || !strings.Contains(got, "HTTPS_REDIRECT='off'") {
		t.Errorf("ports not rendered:\n%s", got)
	}
}

// fakeNgx answers each new ngx.env the way ngx's watcher does.
func fakeNgx(t *testing.T, env string, verdict func(body string) string) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			data, err := os.ReadFile(env)
			if err != nil {
				continue
			}
			sum := sha256.Sum256(data)
			os.WriteFile(ngxStatusPath(env), []byte(hex.EncodeToString(sum[:])+" "+verdict(string(data))+"\n"), 0o644)
		}
	}()
}

func TestNgxSync(t *testing.T) {
	env := filepath.Join(t.TempDir(), "ngx.env")
	n := &Ngx{envPath: env, httpsPort: 443, upstreamHost: "127.0.0.1", wait: 2 * time.Second}
	if n.Reachable() {
		t.Fatal("reachable before ngx ever answered")
	}
	if err := n.Sync(nil); err == nil || !strings.Contains(err.Error(), "did not pick up") {
		t.Fatalf("no ngx: want a timeout error, got %v", err)
	}
	fakeNgx(t, env, func(body string) string {
		if strings.Contains(body, "broken.test") {
			return "error: 40-sites.sh: bad SITES entry"
		}
		return "ok"
	})
	if err := n.Sync([]Route{{Host: "a.test", Upstream: "127.0.0.1:1"}}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !n.Reachable() {
		t.Error("not reachable after ngx answered")
	}
	err := n.Sync([]Route{{Host: "broken.test", Upstream: "127.0.0.1:1"}})
	if err == nil || !strings.Contains(err.Error(), "bad SITES entry") {
		t.Fatalf("want ngx's error, got %v", err)
	}
}
