// Package platform reconciles xdev's desired state (the projects/apps/domains in
// the database) with the moving parts that live outside it: the Caddy reverse
// proxy and the hosts file. Handlers call Sync after any mutation so routing and
// local DNS always match the database.
package platform

import (
	"fmt"
	"log"
	"strings"

	"xdev/internal/domains"
	"xdev/internal/proxy"
	"xdev/internal/store"
)

// Reconciler pushes the current DB state into Caddy and the hosts file.
type Reconciler struct {
	store     *store.Store
	proxy     *proxy.Manager
	hostsPath string

	// Enabled gates all work; set false when no usable Caddy is available, so
	// the rest of xdev keeps working with direct host-port access.
	Enabled bool
	// ManageHosts toggles writing the hosts file (best-effort; failures are
	// logged, not fatal, since it usually needs elevated privileges).
	ManageHosts bool
}

// NewReconciler builds a Reconciler. Enable it via the Enabled field.
func NewReconciler(st *store.Store, pm *proxy.Manager, hostsPath string, manageHosts bool) *Reconciler {
	return &Reconciler{store: st, proxy: pm, hostsPath: hostsPath, ManageHosts: manageHosts}
}

// Sync rebuilds the Caddy config and hosts block from the database. Proxy
// failures are returned (the caller may disable the proxy); hosts failures are
// logged but non-fatal.
func (r *Reconciler) Sync() error {
	if !r.Enabled {
		return nil
	}
	infos, err := r.store.ProxyRoutes()
	if err != nil {
		return err
	}
	routes := make([]proxy.Route, 0, len(infos))
	hostnames := make([]string, 0, len(infos))
	for _, in := range infos {
		routes = append(routes, proxy.Route{
			Host:     in.Host,
			Upstream: fmt.Sprintf("127.0.0.1:%d", in.Port),
			Internal: in.Local,
		})
		// Local domains need a hosts entry — except *.localhost, which browsers
		// resolve to loopback automatically. Public domains resolve via real DNS.
		if in.Local && !strings.HasSuffix(in.Host, ".localhost") {
			hostnames = append(hostnames, in.Host)
		}
	}

	if err := r.proxy.Sync(routes); err != nil {
		return err
	}
	if r.ManageHosts {
		if err := domains.SyncHosts(r.hostsPath, hostnames); err != nil {
			log.Printf("hosts sync (%s) failed: %v — use the dashboard button or add entries manually", r.hostsPath, err)
		}
	}
	return nil
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
