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
	body := n.render(routes, ngxFeature(n.envPath, "pct"))
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
		time.Sleep(10 * time.Millisecond) // ngx answers in ~150ms; poll finely so Sync returns right after
	}
}

// status returns ngx's last verdict if it is about the file with checksum want.
func (n *Ngx) status(want string) (string, bool) {
	line := NgxStatus(n.envPath)
	return line, strings.HasPrefix(line, want+" ")
}

// siteEntryRE is what ngx accepts for one SITES entry (kept in step with its
// 40-sites.sh). A route that doesn't fit is left out rather than failing the
// whole config for every other site. An ngx with the "pct" feature also takes
// %-encoded root paths, which is how a root with a space in it gets through.
var siteEntryRE = regexp.MustCompile(`^[A-Za-z0-9.,*-]+(/[A-Za-z0-9._~/-]*\*?)?=[A-Za-z0-9._:/+~%-]+$`)

// ngxFeature reports whether the ngx answering in envPath's directory
// announced feature on its status file's "features:" line.
func ngxFeature(envPath, feature string) bool {
	data, _ := os.ReadFile(ngxStatusPath(envPath))
	for _, line := range strings.Split(string(data), "\n") {
		if list, ok := strings.CutPrefix(line, "features:"); ok {
			for _, f := range strings.Fields(list) {
				if f == feature {
					return true
				}
			}
		}
	}
	return false
}

// pctPath %-encodes every byte of a filesystem path that SITES can't carry.
func pctPath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < 0x80 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:/+~-", c) >= 0) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// render builds ngx.env for routes; pct = ngx decodes %-encoded root paths.
func (n *Ngx) render(routes []Route, pct bool) string {
	root := func(p string) string {
		if pct {
			return pctPath(p)
		}
		return p
	}
	var sites, internal []string
	add := func(entry string) bool {
		if !siteEntryRE.MatchString(entry) || (!pct && strings.Contains(entry, "%")) {
			log.Printf("ngx: skipping route %q: ngx can't express it", entry)
			return false
		}
		sites = append(sites, entry)
		return true
	}
	target := func(r Route) string {
		switch {
		case r.Root != "" && r.FCGIPort > 0:
			// fpm opens the same path, so this root is not host-prefixed (see phpSiteSubroute)
			return fmt.Sprintf("fastcgi://%s%s", net.JoinHostPort(n.upstreamHost, fmt.Sprint(r.FCGIPort)), root(r.Root))
		case r.Root != "":
			return root(hostPathWith(n.rootPrefix, r.Root))
		default:
			return ngxUpstream(r.Upstream, n.upstreamHost, r.Insecure)
		}
	}
	// Each host's own entry first: ngx puts a path into the server block of the
	// host's entry, so a path whose host was left out would fail the whole
	// config — those wait for their host.
	hosts := map[string]bool{}
	for _, r := range routes {
		if len(r.Paths) > 0 || !add(r.Host+"="+target(r)) {
			continue
		}
		hosts[r.Host] = true
		if r.Internal {
			internal = append(internal, r.Host)
		}
		// A proxy app at https://origin/webmail serves pages that link to /webmail/... themselves.
		// Prefixing those again would ask the origin for /webmail/webmail/..., so a request already
		// under the prefix (or for the prefix itself) goes through as it is; everything else still
		// gets the prefix.
		if base, prefix := splitUpstreamPrefix(r.Upstream); prefix != "" {
			up := ngxUpstream(base, n.upstreamHost, r.Insecure)
			add(r.Host + prefix + "=" + up)
			add(r.Host + prefix + "/*=" + up)
		}
	}
	for _, r := range routes {
		if len(r.Paths) == 0 || !hosts[r.Host] {
			continue
		}
		t := target(r)
		for _, p := range r.Paths {
			add(r.Host + p + "=" + t)
		}
	}

	var b strings.Builder
	b.WriteString("# Written by xdev on every route change; edits here are overwritten.\n")
	kv := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, shellQuote(v)) }
	kv("SITES", strings.Join(sites, " "))
	kv("TLS_INTERNAL", strings.Join(internal, " "))
	if n.acmeEmail != "" { // unset leaves ngx's own (container env) value in force
		kv("ACME_EMAIL", n.acmeEmail)
	}
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
// cert, so it is not verified; anything else https is, unless insecure (a
// proxy app whose origin has only a self-signed certificate).
func ngxUpstream(upstream, upstreamHost string, insecure bool) string {
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
	if scheme == "https" && (isLoopback(u.Hostname()) || insecure) {
		scheme = "https+insecure"
	}
	return scheme + "://" + swapLoopbackHost(net.JoinHostPort(u.Hostname(), port), upstreamHost) + strings.TrimSuffix(u.EscapedPath(), "/")
}

// splitUpstreamPrefix splits a proxy app's URL into its origin and its path
// prefix ("https://o.example/webmail/" -> "https://o.example", "/webmail"), or
// returns "" for the prefix when there is none.
func splitUpstreamPrefix(upstream string) (base, prefix string) {
	if !strings.Contains(upstream, "://") {
		return upstream, ""
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return upstream, ""
	}
	prefix = strings.TrimSuffix(u.EscapedPath(), "/")
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""
	return u.String(), prefix
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
