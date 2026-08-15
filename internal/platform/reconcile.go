// Package platform reconciles xdev's desired state (the projects/apps/domains in
// the database) with the moving parts that live outside it: the Caddy reverse
// proxy and the hosts file. Handlers call Sync after any mutation so routing and
// local DNS always match the database.
package platform

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xdev/internal/domains"
	"xdev/internal/proxy"
	"xdev/internal/store"
)

// Reconciler pushes the current DB state into Caddy and the hosts file.
type Reconciler struct {
	store     *store.Store
	proxy     *proxy.Manager
	hostsPath string
	// selfAddr is xdev's own listen address, the upstream for the per-app deploy
	// endpoints published under /_xdev/ on each app's hostname. Empty disables
	// them — with nowhere to send a webhook, publishing the path would only
	// produce a 502.
	selfAddr string

	// syncMu serializes Sync so a background retry and a handler mutation can't
	// interleave two pushes (and two enabled writes) for the same state.
	syncMu sync.Mutex
	// enabled reports whether routing is live: true exactly while the last push
	// to Caddy succeeded. When false the rest of xdev keeps working with direct
	// host-port access. Read from handler goroutines, so atomic.
	enabled atomic.Bool

	// ManageHosts toggles writing the hosts file (best-effort; failures are
	// logged, not fatal, since it usually needs elevated privileges).
	ManageHosts bool
}

// NewReconciler builds a Reconciler. Routing starts disabled; the first
// successful Sync enables it.
func NewReconciler(st *store.Store, pm *proxy.Manager, hostsPath string, manageHosts bool) *Reconciler {
	return &Reconciler{store: st, proxy: pm, hostsPath: hostsPath, ManageHosts: manageHosts}
}

// SetSelfAddr tells the reconciler where xdev itself listens, so it can publish
// the deploy endpoints (/_xdev/hook/…, /_xdev/deploy) on the hostnames of the
// apps that have them switched on.
func (r *Reconciler) SetSelfAddr(addr string) { r.selfAddr = addr }

// Enabled reports whether Caddy routing is live (the last push succeeded).
func (r *Reconciler) Enabled() bool { return r.enabled.Load() }

// Sync rebuilds the Caddy config and hosts block from the database, and is the
// single writer of the enabled flag: routing is live exactly while the most
// recent push succeeded.
//
// When routing is down, Sync first re-probes the admin API rather than giving
// up permanently. Caddy commonly isn't up yet when xdev starts — its container
// boots alongside xdev with no ordering between the two — and a one-shot check
// at startup would leave routing dead until xdev was restarted by hand.
//
// Proxy failures are returned; hosts failures are logged but non-fatal.
func (r *Reconciler) Sync() error {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()

	if !r.enabled.Load() && !r.proxy.Reachable() {
		return nil // Caddy still absent — stay disabled, and don't call it an error
	}
	infos, err := r.store.ProxyRoutes()
	if err != nil {
		return err
	}
	// Deploy endpoints go in first: they are path-scoped, and Caddy takes the
	// first matching route, so an app's own (host-wide) route has to come after
	// or it would swallow /_xdev/ too.
	routes := r.endpointRoutes()
	hostnames := make([]string, 0, len(infos))
	for _, in := range infos {
		route := proxy.Route{Host: in.Host, Internal: in.Local}
		switch {
		case in.Upstream != "":
			route.Upstream = in.Upstream // proxy app: forwards to another server (URL)
		case in.Root != "":
			route.Root = in.Root         // serve-mode static app or shared-WP docroot: file-served
			route.FCGIPort = in.FCGIPort // shared-WP sites: *.php goes to the wp-host fpm port
		default:
			route.Upstream = fmt.Sprintf("127.0.0.1:%d", in.Port)
		}
		routes = append(routes, route)
		// Local domains need a hosts entry — except *.localhost, which browsers
		// resolve to loopback automatically. Public domains resolve via real DNS.
		if in.Local && !strings.HasSuffix(in.Host, ".localhost") {
			hostnames = append(hostnames, in.Host)
		}
	}

	if err := r.proxy.Sync(routes); err != nil {
		r.enabled.Store(false) // the push didn't land: routing is not live
		return err
	}
	if r.enabled.CompareAndSwap(false, true) {
		log.Printf("proxy: Caddy reachable — routing enabled (%d routes)", len(routes))
	}
	if r.ManageHosts {
		if err := domains.SyncHosts(r.hostsPath, hostnames); err != nil {
			log.Printf("hosts sync (%s) failed: %v — use the dashboard button or add entries manually", r.hostsPath, err)
		}
	}
	return nil
}

// endpointRoutes publishes the deploy endpoints of every app that has one, on
// that app's own hostname. Each is a path-scoped route to xdev itself.
//
// Only the paths an app actually enabled are routed: an app with a webhook and
// no push token exposes the webhook path alone. An app with neither gets no
// route at all, so the control plane is reachable from the internet only where
// somebody deliberately switched it on — and then only at a path that answers
// nothing without a signature or a token.
func (r *Reconciler) endpointRoutes() []proxy.Route {
	if r.selfAddr == "" {
		return nil
	}
	apps, err := r.store.AppsWithEndpoints()
	if err != nil {
		log.Printf("deploy endpoints: %v", err)
		return nil
	}
	routes := make([]proxy.Route, 0, len(apps))
	for _, a := range apps {
		paths := a.EndpointPaths()
		if len(paths) == 0 || a.Domain == "" {
			continue
		}
		routes = append(routes, proxy.Route{
			Host:     a.Domain,
			Paths:    paths,
			Upstream: r.selfAddr,
		})
	}
	return routes
}

// RetryUntilLive re-attempts Sync every interval until routing goes live, then
// returns. It covers the boot race: on a server xdev and the Caddy container are
// started independently, so a Sync at startup can legitimately find nothing
// listening. Without this, routing would stay dead until someone triggered a UI
// mutation — after a reboot that means every site is down with nothing on screen
// saying why.
//
// ponytail: fixed interval, no backoff — a loopback dial every few seconds costs
// nothing. Add backoff if this ever probes something off-host.
func (r *Reconciler) RetryUntilLive(ctx context.Context, interval time.Duration) {
	logged := false
	for !r.Enabled() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		// Sync re-probes and flips enabled itself; only report a push that
		// reached Caddy and failed anyway, and only once — this loop is silent
		// while it waits.
		if err := r.Sync(); err != nil && !logged {
			log.Printf("proxy: sync failed, retrying every %s: %v", interval, err)
			logged = true
		}
	}
}

// localHosts returns the local (.test/.local, non-.localhost) hostnames that
// need a hosts-file entry to resolve.
func (r *Reconciler) localHosts() []string {
	infos, err := r.store.ProxyRoutes()
	if err != nil {
		return nil
	}
	var hosts []string
	for _, in := range infos {
		if in.Local && !strings.HasSuffix(in.Host, ".localhost") {
			hosts = append(hosts, in.Host)
		}
	}
	return hosts
}

// MissingHosts filters candidates to those not currently in the hosts file.
func (r *Reconciler) MissingHosts(candidates []string) []string {
	return domains.MissingFromHosts(r.hostsPath, candidates)
}

// WriteHostsElevated writes every local hostname into the hosts file, prompting
// for admin rights via the OS when xdev can't write it directly.
func (r *Reconciler) WriteHostsElevated() error {
	hosts := r.localHosts()
	if err := domains.SyncHosts(r.hostsPath, hosts); err == nil {
		return nil // xdev could write it directly (running as root)
	}
	return domains.SyncHostsElevated(r.hostsPath, hosts)
}
