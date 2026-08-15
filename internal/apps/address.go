package apps

// Where an app is actually reachable.
//
// xdev's default answer is "at its hostname, through Caddy, over HTTPS", and
// for an install with a domain that is the only answer worth giving. But xdev
// also runs on servers that have no domain at all — a bare IP, or a box already
// running another web server on 80/443 that should keep them. There, every app
// is still reachable: a container app publishes its port on every interface, so
// http://<server>:<port> works with nothing else configured.
//
// The trouble is that "its hostname over HTTPS" was assumed in several places
// at once — the app card's link, the deploy endpoints shown for pasting into
// GitHub, and (worst) the APP_URL written into a Laravel app's .env, from which
// Laravel builds signed URLs, password-reset links and asset paths. On a
// domainless install all three were confidently wrong, and only the last one
// fails quietly, weeks later, when somebody clicks a reset link.
//
// So the question is asked in exactly one place now.

import (
	"strconv"

	"xdev/internal/store"
)

// Reach describes how this install is reached from outside, which is the part
// of the answer that is the same for every app.
type Reach struct {
	// PublicHost is the address this server answers on when there is no
	// hostname to use — an IP or a bare name (XDEV_PUBLIC_HOST). Empty means
	// nobody said, and xdev will not guess: it falls back to loopback, which is
	// true for whoever is reading the UI through an ssh tunnel.
	PublicHost string
	// ProxyEnabled is whether Caddy is actually routing. A hostname with no
	// proxy behind it is not an address, it is a plan.
	ProxyEnabled bool
	// HTTPSPort is the port Caddy serves on, omitted from URLs when it is 443.
	HTTPSPort int
}

// Address returns the URL an app is reachable at, or "" when it has no address
// at all — a serve-mode static app with no domain has no port either, so there
// is genuinely nowhere to send anyone.
//
// The order is "most public first": a routed hostname beats a published port,
// because that is the address the app is meant to be known by.
func (r Reach) Address(app store.App) string {
	if app.Domain != "" && r.ProxyEnabled {
		if r.HTTPSPort != 0 && r.HTTPSPort != 443 {
			return "https://" + app.Domain + ":" + strconv.Itoa(r.HTTPSPort)
		}
		return "https://" + app.Domain
	}
	if app.Port > 0 {
		return "http://" + r.Host() + ":" + strconv.Itoa(app.Port)
	}
	return ""
}

// PortAddress is Address ignoring any hostname: where the app answers on its
// published port. Shown alongside the domain so a stack that is up but not yet
// routed (no DNS, no certificate) can still be opened and checked.
func (r Reach) PortAddress(port int) string {
	if port <= 0 {
		return ""
	}
	return "http://" + r.Host() + ":" + strconv.Itoa(port)
}

// Host is the name to put in a port URL: the configured public host, or
// loopback when there is none. Loopback is not a guess — it is exactly right
// for the ssh tunnel the control plane is normally reached through, and wrong
// only in the way an unset setting should be.
func (r Reach) Host() string {
	if r.PublicHost != "" {
		return r.PublicHost
	}
	return "127.0.0.1"
}

// PortOnly reports whether this install expects apps to be reached by port
// rather than by hostname. It is what makes the domain field optional: with a
// public host configured, an app created without a domain is a deliberate
// choice, not an omission to be filled in from the project's base domain.
func (r Reach) PortOnly() bool { return r.PublicHost != "" }
