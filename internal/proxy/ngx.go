package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Ngx drives ngx (nginx + automatic HTTPS, github.com/leyume/ngx) instead of
// Caddy. There is no admin API: xdev writes the routes into ngx's ngx.env as one
// SITES line, ngx notices the change, regenerates its config, checks it with
// nginx -t and hot-reloads. It answers in ngx.status next to ngx.env with
// "<sha256 of ngx.env> ok" or "... error: <why>", which is how Sync learns
// whether, and when, its config took effect. A config nginx rejects is never
// loaded; the previous one keeps serving, as with a rejected Caddy /load.
type Ngx struct {
	envPath              string // ngx.env on this host (mounted at /etc/ngx in the container)
	httpsPort            int
	acmeEmail            string
	upstreamHost         string // see Manager.upstreamHost
	rootPrefix           string // see Manager.rootPrefix
	disableHTTPSRedirect bool
	wait                 time.Duration // how long Sync waits for ngx to answer
}

// NewNgx returns a proxy that writes envPath. It reads the same XDEV_* proxy
// settings as NewManager.
func NewNgx(envPath string, httpsPort int, acmeEmail string) *Ngx {
	m := NewManager("", httpsPort, 0, acmeEmail, "")
	return &Ngx{
		envPath:              envPath,
		httpsPort:            httpsPort,
		acmeEmail:            acmeEmail,
		upstreamHost:         m.upstreamHost,
		rootPrefix:           m.rootPrefix,
		disableHTTPSRedirect: m.disableHTTPSRedirect,
		wait:                 20 * time.Second,
	}
}

func ngxStatusPath(envPath string) string { return filepath.Join(filepath.Dir(envPath), "ngx.status") }

// Reachable reports whether ngx has ever answered in this directory. ngx writes
// the status file at boot, so a missing one means it was never started here.
func (n *Ngx) Reachable() bool {
	_, err := os.Stat(ngxStatusPath(n.envPath))
	return err == nil
}

// Sync writes the routes to ngx.env and waits for ngx to apply them.
func (n *Ngx) Sync(routes []Route) error {
	body := n.render(routes)
	sum := sha256.Sum256([]byte(body))
	want := hex.EncodeToString(sum[:])
	if cur, err := os.ReadFile(n.envPath); err != nil || string(cur) != body {
		tmp := n.envPath + ".tmp"
		if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
			return fmt.Errorf("ngx: %w", err)
		}
		// rename, so ngx never reads a half-written file
		if err := os.Rename(tmp, n.envPath); err != nil {
			return fmt.Errorf("ngx: %w", err)
		}
	}
	deadline := time.Now().Add(n.wait)
	for {
		if line, ok := n.status(want); ok {
			if verdict := strings.TrimPrefix(line, want+" "); verdict != "ok" {
				return fmt.Errorf("ngx rejected the config, still serving the previous one: %s", strings.TrimPrefix(verdict, "error: "))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ngx did not pick up %s within %s — is its container running and watching that directory?", n.envPath, n.wait)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// status returns ngx's last verdict if it is about the file with checksum want.
func (n *Ngx) status(want string) (string, bool) {
	line := NgxStatus(n.envPath)
	return line, strings.HasPrefix(line, want+" ")
}

// siteEntryRE is what ngx accepts for one SITES entry (kept in step with its
// 40-sites.sh). A route that doesn't fit, such as a root path with a space in
// it, is left out rather than failing the whole config for every other site.
var siteEntryRE = regexp.MustCompile(`^[A-Za-z0-9.,*-]+(/[A-Za-z0-9._~/-]*\*?)?=[A-Za-z0-9._:/+~-]+$`)

// render builds ngx.env for routes.
func (n *Ngx) render(routes []Route) string {
	hosts := map[string]bool{}
	for _, r := range routes {
		if len(r.Paths) == 0 {
			hosts[r.Host] = true
		}
	}
	var sites, internal []string
	add := func(entry string) bool {
		if !siteEntryRE.MatchString(entry) {
			log.Printf("ngx: skipping route %q: ngx can't express it", entry)
			return false
		}
		sites = append(sites, entry)
		return true
	}
	for _, r := range routes {
		var target string
		switch {
		case r.Root != "" && r.FCGIPort > 0:
			// fpm opens the same path, so this root is not host-prefixed (see phpSiteSubroute)
			target = fmt.Sprintf("fastcgi://%s%s", net.JoinHostPort(n.upstreamHost, fmt.Sprint(r.FCGIPort)), r.Root)
		case r.Root != "":
			target = hostPathWith(n.rootPrefix, r.Root)
		default:
			target = ngxUpstream(r.Upstream, n.upstreamHost)
		}
		if len(r.Paths) > 0 {
			// ngx puts a path into the server block of the host's own entry; a host
			// without one would fail the whole config, so its paths wait for it.
			if !hosts[r.Host] {
				continue
			}
			for _, p := range r.Paths {
				add(r.Host + p + "=" + target)
			}
			continue
		}
		if add(r.Host+"="+target) && r.Internal {
			internal = append(internal, r.Host)
		}
	}

	var b strings.Builder
	b.WriteString("# Written by xdev on every route change; edits here are overwritten.\n")
	kv := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, shellQuote(v)) }
	kv("SITES", strings.Join(sites, " "))
	kv("TLS_INTERNAL", strings.Join(internal, " "))
	kv("ACME_EMAIL", n.acmeEmail)
	if n.httpsPort != 443 && n.httpsPort != 0 {
		kv("HTTPS_PORT", fmt.Sprint(n.httpsPort))
	}
	if n.disableHTTPSRedirect {
		kv("HTTPS_REDIRECT", "off")
	}
	return b.String()
}

// ngxUpstream turns a route upstream (host:port, or a proxy app's URL) into an
// ngx target, with the same loopback handling as reverseProxyHandlers: a
// loopback https upstream is one of our own containers behind a self-signed
// cert, so it is not verified; anything else https is.
func ngxUpstream(upstream, upstreamHost string) string {
	if !strings.Contains(upstream, "://") {
		return swapLoopbackHost(upstream, upstreamHost)
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return upstream
	}
	scheme, port := u.Scheme, u.Port()
	if port == "" {
		port = map[string]string{"https": "443"}[scheme]
		if port == "" {
			port = "80"
		}
	}
	if scheme == "https" && isLoopback(u.Hostname()) {
		scheme = "https+insecure"
	}
	return scheme + "://" + swapLoopbackHost(net.JoinHostPort(u.Hostname(), port), upstreamHost) + strings.TrimSuffix(u.EscapedPath(), "/")
}

func hostPathWith(prefix, dir string) string {
	if prefix == "" || dir == "" {
		return dir
	}
	return strings.TrimSuffix(prefix, "/") + dir
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// NgxStatus is the last verdict ngx wrote for envPath ("" when none), for doctor.
func NgxStatus(envPath string) string {
	data, _ := os.ReadFile(ngxStatusPath(envPath))
	line, _, _ := strings.Cut(string(data), "\n")
	return line
}
