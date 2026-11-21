package apps

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

// Mail TLS. Stalwart speaks IMAP/SMTP directly on its own ports, so the proxy's
// HTTPS certificates never reach it on their own. The proxy (ngx) already holds
// a valid, auto-renewing certificate for any hostname it routes, so rather than
// a second ACME client fighting ngx for port 80, xdev copies those certificates
// into the mail app's _/tls/ (mounted read-only at /etc/stalwart/tls) and
// restarts the mail service when a renewal changes them.
//
// Which hostnames: MAIL_TLS_HOSTS in _/.env (comma/space separated) — e.g. the
// new server name plus the name existing clients are configured with. Unset, it
// falls back to the app's admin domain. Each hostname must be routed by the
// proxy (some app's domain), or ngx never obtains a certificate for it.

// SetACMEDir points mail TLS sync at ngx's ACME state directory (the host path
// of its acme volume). "" disables the sync.
func (s *Service) SetACMEDir(dir string) { s.acmeDir = dir }

// mailUID is the user the Stalwart image runs as (uid/gid 2000 "stalwart",
// checked on v1.1.1). The copied keys stay 0600 and are handed to it — readable
// by the server, by nobody else in or out of the container.
const mailUID = 2000

// chownMail gives a path to the mail server's user. Only root can, and only a
// root xdev needs to (a rootless engine maps ownership itself); elsewhere —
// tests, local dev — it is a no-op.
var chownMail = func(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, mailUID, mailUID)
}

// ngxCertFor finds ngx's certificate for host: the ACME module's (http-01)
// first, then a DNS-01 wildcard's (certs/<name>.crt, named after its entry's
// first domain) or the local CA's (local/<name>.crt, a wildcard as _.<rest>).
func ngxCertFor(acmeDir, host string) (crt, key string) {
	if crt, key = newestNgxCert(acmeDir, host); crt != "" {
		return crt, key
	}
	names := []string{host}
	if _, parent, ok := strings.Cut(host, "."); ok && strings.Contains(parent, ".") {
		names = append(names, "*."+parent)
	}
	for _, name := range names {
		for _, c := range []string{
			filepath.Join(acmeDir, "certs", name),
			filepath.Join(acmeDir, "local", strings.Replace(name, "*", "_", 1)),
		} {
			if fileExists(c+".crt") && fileExists(c+".key") {
				return c + ".crt", c + ".key"
			}
		}
	}
	return "", ""
}

// ngxCertName matches ngx's (nginx-acme's) state files: <host>-<hex>.crt.
var ngxCertName = regexp.MustCompile(`^(.+)-[0-9a-f]+\.crt$`)

// newestNgxCert returns the newest <host>-<hex>.crt in dir and its key, or ""
// when ngx holds none for host. Newest rather than "the" file so a renewal that
// ever lands under a new name is still picked up.
func newestNgxCert(dir, host string) (crt, key string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", ""
	}
	var newest time.Time
	for _, e := range entries {
		m := ngxCertName.FindStringSubmatch(e.Name())
		if m == nil || m[1] != host {
			continue
		}
		k := filepath.Join(dir, strings.TrimSuffix(e.Name(), ".crt")+".key")
		if _, err := os.Stat(k); err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if crt == "" || info.ModTime().After(newest) {
			crt, key, newest = filepath.Join(dir, e.Name()), k, info.ModTime()
		}
	}
	return crt, key
}

// ensureMailPublicURL writes MAIL_PUBLIC_URL (-> STALWART_PUBLIC_URL) into
// _/.env as https://<admin domain> — the app's main domain when it has no
// admin domain — unless the line is already there: an explicit value is the
// user's and wins. Without it Stalwart sends its web login to the server
// hostname, which during a migration is still the old mail server.
func (s *Service) ensureMailPublicURL(app store.App) error {
	path := filepath.Join(filepath.Dir(app.ComposePath), ".env")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "MAIL_PUBLIC_URL=") {
			return nil
		}
	}
	host := app.Domain
	if svc, err := s.store.AppServiceDomains(app.ID); err == nil && len(svc) > 0 {
		host = svc[0].Host
	}
	if host == "" {
		host = "localhost"
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	b = append(b, "MAIL_PUBLIC_URL=https://"+host+"\n"...)
	return os.WriteFile(path, b, 0o644)
}

// mailTLSHosts reads the hostnames to serve certificates for.
func (s *Service) mailTLSHosts(app store.App) []string {
	v, _ := envValue(filepath.Join(filepath.Dir(app.ComposePath), ".env"), "MAIL_TLS_HOSTS")
	hosts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	// Unset means the admin console's hostname only: later service rows are
	// Rail's, which Stalwart never serves, and copying their certificates would
	// restart the mail server on every Rail move or renewal.
	if len(hosts) == 0 {
		if svc, err := s.store.AppServiceDomains(app.ID); err == nil && len(svc) > 0 {
			hosts = append(hosts, svc[0].Host)
		}
	}
	return hosts
}

// syncMailCerts copies each host's current ngx certificate into _/tls/ as
// <host>.crt / <host>.key, rewriting only what differs. It reports whether
// anything changed (so a running server needs a restart to load it).
func (s *Service) syncMailCerts(app store.App) (bool, error) {
	if s.acmeDir == "" || app.Type != "mail" || app.ComposePath == "" {
		return false, nil
	}
	dst := filepath.Join(filepath.Dir(app.ComposePath), "tls")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return false, err
	}
	if err := chownMail(dst); err != nil {
		return false, err
	}
	changed := false
	for _, host := range s.mailTLSHosts(app) {
		crt, key := ngxCertFor(s.acmeDir, host)
		if crt == "" {
			log.Printf("mail %s: no certificate for %s in %s yet — is it routed by an app (so ngx issues one)?", app.Slug, host, s.acmeDir)
			continue
		}
		for src, name := range map[string]string{crt: host + ".crt", key: host + ".key"} {
			c, err := copyIfDifferent(src, filepath.Join(dst, name))
			if err == nil {
				// every sync, not only on a copy: files written before the
				// ownership rule existed get fixed too
				err = chownMail(filepath.Join(dst, name))
			}
			if err != nil {
				return changed, fmt.Errorf("mail tls %s: %w", host, err)
			}
			changed = changed || c
		}
	}
	return changed, nil
}

// copyIfDifferent writes src's bytes to dst (0600, atomically) unless dst
// already holds them.
func copyIfDifferent(src, dst string) (bool, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return false, err
	}
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, b) {
		return false, nil
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, dst)
}

// RunMailCertSync re-syncs every running mail app's certificates on an interval
// and restarts its mail service when a renewal changed them. Stalwart reads the
// files at start; a restart is a few seconds of refused connections ~every 60
// days, which mail clients and sending servers retry through.
func (s *Service) RunMailCertSync(ctx context.Context, interval time.Duration) {
	if s.acmeDir == "" {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.syncAllMailCerts()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) syncAllMailCerts() {
	projects, err := s.store.ListProjects()
	if err != nil {
		return
	}
	for _, p := range projects {
		list, err := s.store.ListAppsByProject(p.ID)
		if err != nil {
			continue
		}
		for _, app := range list {
			if app.Type != "mail" || app.Status != store.AppRunning {
				continue
			}
			changed, err := s.syncMailCerts(app)
			if err != nil {
				log.Printf("mail %s: %v", app.Slug, err)
			}
			if !changed {
				continue
			}
			_, engine, workdir, pname, file, err := s.composeCtx(app.ID)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
			if _, err := runtime.Compose(ctx, engine, workdir, pname, file, "restart", "mail"); err != nil {
				log.Printf("mail %s: restart after certificate renewal: %v", app.Slug, err)
			} else {
				log.Printf("mail %s: certificates renewed, mail service restarted", app.Slug)
			}
			cancel()
		}
	}
}
