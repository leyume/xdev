package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// configJSON builds the Caddy config for routes and returns it as JSON, so
// tests can assert on the exact wire format pushed to the admin API.
func configJSON(t *testing.T, routes []Route) string {
	t.Helper()
	m := NewManager("127.0.0.1:2019", 443, 80, "", "")
	b, err := json.Marshal(m.buildConfig(routes))
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return string(b)
}

// TestProxyAppRoutes checks proxy-app upstreams: a plain http URL dials the
// given host:port with no TLS transport, and an https-by-name upstream dials
// :443 with a TLS transport carrying SNI (server_name).
func TestProxyAppRoutes(t *testing.T) {
	cfg := configJSON(t, []Route{
		{Host: "blog.example.com", Upstream: "http://10.0.0.5:3000"},
		{Host: "app.example.com", Upstream: "https://coolify.example"},
	})

	for _, want := range []string{
		`"dial":"10.0.0.5:3000"`,                 // http upstream: explicit port
		`"dial":"coolify.example:443"`,           // https upstream: default port
		`"tls":{"server_name":"coolify.example"`, // TLS + SNI for https-by-name
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q\n%s", want, cfg)
		}
	}
	// The plain-http upstream must not get a TLS transport: exactly one.
	if n := strings.Count(cfg, `"transport"`); n != 1 {
		t.Errorf("want exactly 1 transport (the https upstream), got %d\n%s", n, cfg)
	}
}

// TestLocalPortRouteUnchanged checks the pre-existing local-app form
// (host:port, no scheme) still dials as-is with no transport.
func TestLocalPortRouteUnchanged(t *testing.T) {
	cfg := configJSON(t, []Route{{Host: "site.demo.test", Upstream: "127.0.0.1:20000", Internal: true}})
	if !strings.Contains(cfg, `"dial":"127.0.0.1:20000"`) {
		t.Errorf("local port route missing dial\n%s", cfg)
	}
	if strings.Contains(cfg, `"transport"`) {
		t.Errorf("local port route must not have a transport\n%s", cfg)
	}
}

// TestUpstreamHostOverride checks that a non-default upstreamHost (containerized
// Caddy reaching host-published ports) redirects loopback dials — app port, fpm
// port, and the https loopback mail admin — while preserving that https loopback
// stays insecure_skip_verify, and leaving remote proxy-app upstreams untouched.
func TestUpstreamHostOverride(t *testing.T) {
	m := NewManager("127.0.0.1:2019", 443, 80, "", "")
	m.upstreamHost = "host.containers.internal"
	b, _ := json.Marshal(m.buildConfig([]Route{
		{Host: "app.demo.test", Upstream: "127.0.0.1:20000", Internal: true},
		{Host: "admin.mail.test", Upstream: "https://127.0.0.1:20001"},
		{Host: "wp.demo.test", Root: "/data/wp/sites/demo_wp", FCGIPort: 20099, Internal: true},
		{Host: "blog.example.com", Upstream: "http://10.0.0.5:3000"},
	}))
	cfg := string(b)
	for _, want := range []string{
		`"dial":"host.containers.internal:20000"`, // app port redirected
		`"dial":"host.containers.internal:20001"`, // https mail admin redirected
		`"dial":"host.containers.internal:20099"`, // fpm port redirected
		`"insecure_skip_verify":true`,             // still skips verify for the mail admin
		`"dial":"10.0.0.5:3000"`,                  // remote proxy app left alone
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q\n%s", want, cfg)
		}
	}
	if strings.Contains(cfg, "127.0.0.1") {
		t.Errorf("no loopback dial should remain\n%s", cfg)
	}
}

// TestAdminReassert checks that with adminListen set (containerized Caddy) every
// pushed config carries the admin block with matching origins, so a reload can't
// drop the published admin binding — and that it's absent by default.
func TestAdminReassert(t *testing.T) {
	def := NewManager("127.0.0.1:2019", 443, 80, "", "")
	if b, _ := json.Marshal(def.buildConfig(nil)); strings.Contains(string(b), `"admin"`) {
		t.Errorf("default (supervised) config must not touch admin\n%s", b)
	}

	m := NewManager("127.0.0.1:2019", 443, 80, "", "")
	m.adminListen = "0.0.0.0:2019"
	cfg, _ := json.Marshal(m.buildConfig(nil))
	for _, want := range []string{`"admin"`, `"listen":"0.0.0.0:2019"`, `"127.0.0.1:2019"`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("container config missing %q\n%s", want, cfg)
		}
	}
}

// TestDisableHTTPSRedirect checks that with the preview toggle the public server
// also listens on the HTTP port and carries automatic_https.disable_redirects,
// so sites serve over plain HTTP instead of 308-ing to HTTPS.
func TestDisableHTTPSRedirect(t *testing.T) {
	off := NewManager("127.0.0.1:2019", 8444, 8081, "", "")
	if b, _ := json.Marshal(off.buildConfig(nil)); strings.Contains(string(b), "disable_redirects") {
		t.Errorf("redirect disable must be off by default\n%s", b)
	}

	m := NewManager("127.0.0.1:2019", 8444, 8081, "", "")
	m.disableHTTPSRedirect = true
	cfg, _ := json.Marshal(m.buildConfig(nil))
	for _, want := range []string{`"disable_redirects":true`, `":8081"`, `":8444"`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("preview config missing %q\n%s", want, cfg)
		}
	}
}

// TestWPSharedSiteRoute checks the shared-WP php-site route: file_server rooted
// at the site dir, the try_files permalink rewrite falling back to index.php,
// and *.php handed to the wp-host fpm port over fastcgi — while a plain Root
// route (serve-mode static app) stays a bare file_server.
func TestWPSharedSiteRoute(t *testing.T) {
	cfg := configJSON(t, []Route{
		{Host: "blog.demo.test", Root: "/data/wp/sites/demo_blog", FCGIPort: 20099, Internal: true},
	})
	for _, want := range []string{
		`"handler":"vars","root":"/data/wp/sites/demo_blog"`, // file_server root = site docroot
		`"try_files":["{http.request.uri.path}","{http.request.uri.path}/index.php","index.php"]`,
		`"handler":"rewrite","uri":"{http.matchers.file.relative}"`,
		`"path":["*.php"]`,         // only PHP goes to fpm
		`"protocol":"fastcgi"`,     // fastcgi transport…
		`"split_path":[".php"]`,    // …with PATH_INFO splitting
		`"dial":"127.0.0.1:20099"`, // …dialing the wp-host fpm host port
		`"handler":"file_server"`,  // everything else served off disk
		`"status_code":308`,        // canonical /dir -> /dir/ redirect
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q\n%s", want, cfg)
		}
	}

	// Root without FCGIPort must stay a plain file_server (no PHP handler).
	cfg = configJSON(t, []Route{{Host: "site.demo.test", Root: "/srv/site/dist", Internal: true}})
	if strings.Contains(cfg, "fastcgi") || strings.Contains(cfg, "subroute") {
		t.Errorf("plain Root route must not gain a PHP handler\n%s", cfg)
	}
}

// TestHTTPSByIPSkipsSNI checks an https upstream addressed by IP gets TLS but
// no server_name (there is no name to verify).
func TestHTTPSByIPSkipsSNI(t *testing.T) {
	cfg := configJSON(t, []Route{{Host: "x.example.com", Upstream: "https://10.0.0.5:8443"}})
	if !strings.Contains(cfg, `"dial":"10.0.0.5:8443"`) || !strings.Contains(cfg, `"tls":{}`) {
		t.Errorf("https-by-IP should dial with an empty tls transport\n%s", cfg)
	}
	if strings.Contains(cfg, "server_name") {
		t.Errorf("https-by-IP must not set SNI\n%s", cfg)
	}
	// A remote IP is not our container: verification must stay on.
	if strings.Contains(cfg, "insecure_skip_verify") {
		t.Errorf("https to a non-loopback IP must keep TLS verification\n%s", cfg)
	}
}

// TestHTTPSLoopbackSkipsVerify checks an https upstream to loopback (our own
// self-signed container, e.g. the Stalwart mail admin) skips TLS verification.
func TestHTTPSLoopbackSkipsVerify(t *testing.T) {
	cfg := configJSON(t, []Route{{Host: "admin.mail.test", Upstream: "https://127.0.0.1:20001"}})
	if !strings.Contains(cfg, `"dial":"127.0.0.1:20001"`) {
		t.Errorf("loopback https should dial the port\n%s", cfg)
	}
	if !strings.Contains(cfg, `"insecure_skip_verify":true`) {
		t.Errorf("loopback https must skip TLS verification\n%s", cfg)
	}
}
