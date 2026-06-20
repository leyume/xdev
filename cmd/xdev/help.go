package main

import (
	"flag"
	"fmt"
	goruntime "runtime"
)

// versionString renders the one-line version banner:
//
//	xdev <version> (<goos>/<goarch>, go<goversion>)
func versionString() string {
	return fmt.Sprintf("xdev %s (%s/%s, %s)",
		version, goruntime.GOOS, goruntime.GOARCH, goruntime.Version())
}

// printUsage renders the `xdev -h` help: description, usage, subcommands, the
// flags (grouped), and a few examples. fs is the server flag set so flag
// defaults stay in one place.
func printUsage(fs *flag.FlagSet) {
	out := fs.Output()

	fmt.Fprintln(out, "xdev — a single-binary, self-hosted PaaS: projects → apps, containers,")
	fmt.Fprintln(out, "automatic HTTPS via Caddy, and a web UI. All state lives in one sqlite file.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  xdev [flags]                 run the control plane (default)")
	fmt.Fprintln(out, "  xdev <subcommand> [args]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  help                         print this help (same as -h)")
	fmt.Fprintln(out, "  version                      print version and exit")
	fmt.Fprintln(out, "  doctor                       preflight/health check (engine, caddy, ports, data dir, admin)")
	fmt.Fprintln(out, "  create-admin <email>         add an admin, or --reset its password (idempotent)")
	fmt.Fprintln(out, "  write-hosts <file> [host…]   rewrite the managed hosts-file block (used internally)")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Flags (each also reads an XDEV_* env var; explicit flag > env > default):")
	printFlagGroup(fs, "Core", "data", "projects", "addr", "secure")
	printFlagGroup(fs, "Proxy & TLS", "caddy", "caddy-admin", "https-port", "http-port", "acme-email", "local-cert-lifetime")
	printFlagGroup(fs, "Engine", "engine")
	printFlagGroup(fs, "Hosts", "hosts-file", "manage-hosts")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Examples:")
	fmt.Fprintln(out, "  # Local dev with clean HTTPS (binds 443/80 + edits /etc/hosts → needs sudo):")
	fmt.Fprintln(out, "  sudo xdev")
	fmt.Fprintln(out, "  # Sudo-free local dev on high ports with a throwaway hosts file:")
	fmt.Fprintln(out, "  xdev -https-port 8443 -http-port 8080 -hosts-file ./dev-hosts")
	fmt.Fprintln(out, "  # Pick a specific database (default is ./data relative to the current dir):")
	fmt.Fprintln(out, "  xdev -data ./data            # or: XDEV_DATA=/path/to/dir xdev")
	fmt.Fprintln(out, "  # Production server (config usually comes from /etc/xdev/xdev.env):")
	fmt.Fprintln(out, "  xdev -addr 127.0.0.1:7331 -secure -manage-hosts=false -acme-email you@example.com")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Admin accounts:")
	fmt.Fprintln(out, "  # Add another admin (all admins have equal access):")
	fmt.Fprintln(out, "  xdev create-admin you@example.com")
	fmt.Fprintln(out, "  # Reset a forgotten password (set XDEV_ADMIN_PASSWORD, or you'll be prompted):")
	fmt.Fprintln(out, "  xdev create-admin you@example.com --reset")
	fmt.Fprintln(out, "  # IMPORTANT: point -data / XDEV_DATA at the SAME db the server uses.")
	fmt.Fprintln(out, "  # A service install lives in /var/lib/xdev — not ./data:")
	fmt.Fprintln(out, "  sudo XDEV_DATA=/var/lib/xdev XDEV_ADMIN_PASSWORD=secret123 \\")
	fmt.Fprintln(out, "    xdev create-admin you@example.com --reset")
}

// printFlagGroup prints a named group of flags with their defaults, in the
// given order. Unknown names are skipped.
func printFlagGroup(fs *flag.FlagSet, title string, names ...string) {
	out := fs.Output()
	fmt.Fprintf(out, "  %s:\n", title)
	for _, name := range names {
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		def := f.DefValue
		if def == "" {
			def = `""`
		}
		fmt.Fprintf(out, "    -%-20s %s (default %s)\n", name, f.Usage, def)
	}
}
