// Package proxy manages Caddy as xdev's reverse proxy and TLS terminator. xdev
// owns the *configuration* and pushes it to Caddy's admin API (POST /load) on
// every change; a supervisor (supervisor.go) optionally runs Caddy as a child
// process. Decoupling via the admin API means the same code drives a
// Caddy-in-systemd setup on a server.
//
// Local domains (.test) are issued certificates by Caddy's built-in internal
// CA (tls "internal"); production ACME/Let's Encrypt issuance arrives in Phase 4.
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Route maps a hostname to how Caddy should serve it. Either Upstream (reverse
// proxy to a host port, or — for proxy apps — a full URL to another server) or
// Root (file-server a directory directly — serve-mode static apps) is set.
// Root plus FCGIPort is a PHP docroot (shared-WP sites): files served directly,
// *.php handed to a fastcgi upstream.
type Route struct {
	Host     string // e.g. frontend.demo.test
	Upstream string // e.g. 127.0.0.1:20000, or https://other-server.example (proxy apps)
	Root     string // e.g. /…/projects/demo/frontend/dist (file_server)
	FCGIPort int    // fastcgi host port for *.php under Root (shared-WP sites)
	Internal bool   // true = local CA (.test); false = public ACME/Let's Encrypt
}

// Manager talks to a Caddy admin API and knows which ports the public servers
// should listen on.
type Manager struct {
	adminAddr string // host:port of the Caddy admin API, e.g. 127.0.0.1:2019
	httpsPort int
	httpPort  int
	acmeEmail string // optional contact for Let's Encrypt registration
	// leafLifetime is how long locally-issued (.test/.localhost) certificates
	// last before Caddy auto-renews them. Caddy's bare default is only 12h;
	// xdev uses a longer, friendlier default. intermediateLifetime must exceed
	// it (a leaf can't outlive its issuing CA).
	leafLifetime         string
	intermediateLifetime string
	// upstreamHost is what loopback upstreams (host-published app/fpm ports) are
	// dialed as. Defaults to 127.0.0.1 (Caddy on the same host); set to
	// host.containers.internal / host.docker.internal when Caddy runs in a
	// container and must reach ports published on the host.
	upstreamHost string
	// adminListen, when set, makes every pushed config re-assert the admin
	// endpoint (listen address + origins). Required for a containerized Caddy:
	// a /load that omits admin makes Caddy revert admin to loopback-inside-the-
	// container, dropping the published binding xdev talks to. Empty leaves
	// admin alone (supervised or systemd Caddy, where loopback admin is fine).
	adminListen string
	client      *http.Client
}

// NewManager creates a proxy Manager. acmeEmail may be empty; leafLifetime
// defaults to 2160h (90 days) when blank.
func NewManager(adminAddr string, httpsPort, httpPort int, acmeEmail, leafLifetime string) *Manager {
	if leafLifetime == "" {
		leafLifetime = "2160h"
	}
	upstreamHost := os.Getenv("XDEV_UPSTREAM_HOST")
	if upstreamHost == "" {
		upstreamHost = "127.0.0.1"
	}
	return &Manager{
		adminAddr:            adminAddr,
		httpsPort:            httpsPort,
		httpPort:             httpPort,
		acmeEmail:            acmeEmail,
		leafLifetime:         leafLifetime,
		intermediateLifetime: "8760h", // 1 year; comfortably longer than the leaf
		upstreamHost:         upstreamHost,
		adminListen:          os.Getenv("XDEV_CADDY_ADMIN_LISTEN"),
		client:               &http.Client{Timeout: 10 * time.Second},
	}
}

// Reachable reports whether the Caddy admin API answers.
func (m *Manager) Reachable() bool {
	resp, err := m.client.Get(m.url("/config/"))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// Sync replaces Caddy's entire configuration with one derived from routes.
func (m *Manager) Sync(routes []Route) error {
	cfg := m.buildConfig(routes)
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	resp, err := m.client.Post(m.url("/load"), "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("caddy /load: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		out, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("caddy /load returned %s: %s", resp.Status, string(out))
	}
	return nil
}

// LocalCARoot fetches the PEM of Caddy's internal CA root certificate, so it can
// be installed into the system trust store (see `caddy trust`).
func (m *Manager) LocalCARoot() ([]byte, error) {
	resp, err := m.client.Get(m.url("/pki/ca/local"))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		RootCertificate string `json:"root_certificate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.RootCertificate == "" {
		return nil, fmt.Errorf("caddy returned no root certificate")
	}
	return []byte(payload.RootCertificate), nil
}

func (m *Manager) url(path string) string {
	return "http://" + m.adminAddr + path
}

// reverseProxyHandler builds the reverse_proxy handler JSON for an upstream:
// either a plain host:port (a local app port) or, for proxy apps, a full URL
// (http(s)://host[:port]) to another server. An https upstream gets a TLS
// transport, with SNI (server_name) when it's named rather than an IP so cert
// verification against the origin works. The incoming Host header passes
// through untouched (Caddy's default), keeping name-based vhosts — and
// websockets — on the other server working.
func reverseProxyHandler(upstream, upstreamHost string) map[string]any {
	h := map[string]any{"handler": "reverse_proxy"}
	dial := upstream
	if strings.Contains(upstream, "://") {
		if u, err := url.Parse(upstream); err == nil && u.Host != "" {
			host, port := u.Hostname(), u.Port()
			if port == "" {
				if u.Scheme == "https" {
					port = "443"
				} else {
					port = "80"
				}
			}
			dial = net.JoinHostPort(host, port)
			if u.Scheme == "https" {
				tls := map[string]any{}
				if net.ParseIP(host) == nil {
					tls["server_name"] = host
				}
				// A loopback https upstream is one of our own containers behind a
				// self-signed cert (e.g. the Stalwart mail admin) — there is no
				// public identity to verify, so skip verification.
				if isLoopback(host) {
					tls["insecure_skip_verify"] = true
				}
				h["transport"] = map[string]any{"protocol": "http", "tls": tls}
			}
		}
	}
	// TLS verify semantics above are decided on the original (loopback) host;
	// only now redirect the dial to upstreamHost so a containerized Caddy can
	// reach the port where it's actually published (the host).
	dial = swapLoopbackHost(dial, upstreamHost)
	h["upstreams"] = []any{map[string]any{"dial": dial}}
	return h
}

// swapLoopbackHost rewrites the host of a host:port dial to upstreamHost when it
// points at loopback, leaving the port and any non-loopback (remote) dial alone.
func swapLoopbackHost(dial, upstreamHost string) string {
	host, port, err := net.SplitHostPort(dial)
	if err != nil || !isLoopback(host) {
		return dial
	}
	return net.JoinHostPort(upstreamHost, port)
}

// adminOrigins is the Host-header allowlist Caddy's admin API accepts, derived
// from the address xdev dials it at (adminAddr). Caddy matches the request Host
// against this, so it must include exactly what xdev sends plus loopback aliases
// on the same port.
func adminOrigins(adminAddr string) []string {
	_, port, err := net.SplitHostPort(adminAddr)
	if err != nil {
		return []string{adminAddr}
	}
	return []string{adminAddr, net.JoinHostPort("localhost", port), net.JoinHostPort("::1", port)}
}

// isLoopback reports whether host is the loopback interface by name or IP.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// phpSiteSubroute builds the handler chain for a PHP docroot (shared-WP sites):
// Caddy file-serves root directly and hands *.php to the wp-host fpm pool over
// fastcgi on a loopback port. It is Caddy's own `php_fastcgi` directive
// expanded to JSON — trailing-slash redirect for directories holding an
// index.php, a try_files rewrite for pretty permalinks (falling back to
// /index.php), the fastcgi reverse_proxy, then a plain file_server for
// everything else. The docroot is bind-mounted into the fpm container at the
// same absolute path as on the host, so the root — and the SCRIPT_FILENAME the
// transport derives from it — resolves on both sides.
func phpSiteSubroute(root string, fcgiPort int, upstreamHost string) map[string]any {
	return map[string]any{
		"handler": "subroute",
		"routes": []any{
			map[string]any{"handle": []any{map[string]any{"handler": "vars", "root": root}}},
			// /wp-admin -> /wp-admin/ (308) when the directory has an index.php.
			map[string]any{
				"match": []any{map[string]any{
					"file": map[string]any{"try_files": []string{"{http.request.uri.path}/index.php"}},
					"not":  []any{map[string]any{"path": []string{"*/"}}},
				}},
				"handle": []any{map[string]any{
					"handler":     "static_response",
					"status_code": 308,
					"headers":     map[string]any{"Location": []string{"{http.request.orig_uri.path}/"}},
				}},
			},
			// Rewrite to the first existing candidate; the trailing index.php is
			// the permalink fallback for URIs that exist nowhere on disk.
			map[string]any{
				"match": []any{map[string]any{"file": map[string]any{
					"try_files":  []string{"{http.request.uri.path}", "{http.request.uri.path}/index.php", "index.php"},
					"split_path": []string{".php"},
				}}},
				"handle": []any{map[string]any{"handler": "rewrite", "uri": "{http.matchers.file.relative}"}},
			},
			map[string]any{
				"match": []any{map[string]any{"path": []string{"*.php"}}},
				"handle": []any{map[string]any{
					"handler":   "reverse_proxy",
					"transport": map[string]any{"protocol": "fastcgi", "split_path": []string{".php"}},
					"upstreams": []any{map[string]any{"dial": fmt.Sprintf("%s:%d", upstreamHost, fcgiPort)}},
				}},
			},
			map[string]any{"handle": []any{map[string]any{"handler": "file_server"}}},
		},
	}
}

// buildConfig assembles a full Caddy JSON config: one HTTP server listening on
// the configured http+https ports, a route per host reverse-proxying to its
// upstream, and a TLS automation policy using the internal (local) CA.
//
// The config is built with plain maps to keep the shape obvious and avoid a
// dependency on Caddy's Go types.
func (m *Manager) buildConfig(routes []Route) map[string]any {
	httpRoutes := make([]any, 0, len(routes))
	var internalHosts, publicHosts []string
	for _, r := range routes {
		if r.Internal {
			internalHosts = append(internalHosts, r.Host)
		} else {
			publicHosts = append(publicHosts, r.Host)
		}
		// Serve a directory directly when Root is set (the `vars` handler sets the
		// file_server root, mirroring the Caddyfile `root` directive) — with a
		// fastcgi handler for *.php when the root is a PHP docroot; otherwise
		// reverse-proxy to the app's host port.
		var handle []any
		switch {
		case r.Root != "" && r.FCGIPort > 0:
			handle = []any{phpSiteSubroute(r.Root, r.FCGIPort, m.upstreamHost)}
		case r.Root != "":
			handle = []any{
				map[string]any{"handler": "vars", "root": r.Root},
				map[string]any{"handler": "file_server"},
			}
		default:
			handle = []any{reverseProxyHandler(r.Upstream, m.upstreamHost)}
		}
		httpRoutes = append(httpRoutes, map[string]any{
			"match":    []any{map[string]any{"host": []string{r.Host}}},
			"handle":   handle,
			"terminal": true,
		})
	}

	// Listen only on the HTTPS port; Caddy's automatic HTTPS spins up its own
	// redirect server on http_port (set below) for these hosts.
	server := map[string]any{
		"listen": []string{fmt.Sprintf(":%d", m.httpsPort)},
		"routes": httpRoutes,
	}

	cfg := map[string]any{
		"apps": map[string]any{
			"http": map[string]any{
				// Tell Caddy which ports are plaintext vs TLS so its automatic
				// HTTP->HTTPS redirects work even on non-standard ports.
				"http_port":  m.httpPort,
				"https_port": m.httpsPort,
				"servers": map[string]any{
					"xdev": server,
				},
			},
		},
	}

	// Re-assert the admin endpoint on every push (containerized Caddy only): a
	// config without an admin block makes Caddy revert admin to loopback inside
	// the container, dropping the published binding xdev pushes to.
	if m.adminListen != "" {
		cfg["admin"] = map[string]any{
			"listen":  m.adminListen,
			"origins": adminOrigins(m.adminAddr),
		}
	}

	// TLS automation: local (.test) hosts use Caddy's internal CA; public hosts
	// get certificates from Let's Encrypt/ZeroSSL via ACME.
	var policies []any
	if len(internalHosts) > 0 {
		policies = append(policies, map[string]any{
			"subjects": internalHosts,
			"issuers": []any{map[string]any{
				"module":   "internal",
				"lifetime": m.leafLifetime,
			}},
		})
	}
	if len(publicHosts) > 0 {
		acme := map[string]any{"module": "acme"}
		if m.acmeEmail != "" {
			acme["email"] = m.acmeEmail
		}
		policies = append(policies, map[string]any{
			"subjects": publicHosts,
			"issuers":  []any{acme},
		})
	}
	if len(policies) > 0 {
		cfg["apps"].(map[string]any)["tls"] = map[string]any{
			"automation": map[string]any{"policies": policies},
		}
	}
	// Extend the local CA's intermediate lifetime so it outlives the longer leaf
	// certs (Caddy refuses to issue a leaf that would outlast its issuer).
	if len(internalHosts) > 0 {
		cfg["apps"].(map[string]any)["pki"] = map[string]any{
			"certificate_authorities": map[string]any{
				"local": map[string]any{"intermediate_lifetime": m.intermediateLifetime},
			},
		}
	}
	return cfg
}
