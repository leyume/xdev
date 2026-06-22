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
	"net/http"
	"time"
)

// Route maps a hostname to how Caddy should serve it. Either Upstream (reverse
// proxy to a host port — containers and command-mode static apps) or Root
// (file-server a directory directly — serve-mode static apps) is set.
type Route struct {
	Host     string // e.g. frontend.demo.test
	Upstream string // e.g. 127.0.0.1:20000
	Root     string // e.g. /…/projects/demo/frontend/dist (file_server)
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
	client               *http.Client
}

// NewManager creates a proxy Manager. acmeEmail may be empty; leafLifetime
// defaults to 2160h (90 days) when blank.
func NewManager(adminAddr string, httpsPort, httpPort int, acmeEmail, leafLifetime string) *Manager {
	if leafLifetime == "" {
		leafLifetime = "2160h"
	}
	return &Manager{
		adminAddr:            adminAddr,
		httpsPort:            httpsPort,
		httpPort:             httpPort,
		acmeEmail:            acmeEmail,
		leafLifetime:         leafLifetime,
		intermediateLifetime: "8760h", // 1 year; comfortably longer than the leaf
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
		// file_server root, mirroring the Caddyfile `root` directive); otherwise
		// reverse-proxy to the app's host port.
		var handle []any
		if r.Root != "" {
			handle = []any{
				map[string]any{"handler": "vars", "root": r.Root},
				map[string]any{"handler": "file_server"},
			}
		} else {
			handle = []any{map[string]any{
				"handler":   "reverse_proxy",
				"upstreams": []any{map[string]any{"dial": r.Upstream}},
			}}
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
