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
}
