# xdev

**xdev** is a lean, self-hosted PaaS in a single Go binary. It organizes your
work as **projects → apps**, runs each app as a container stack (docker or
podman + Compose), and puts everything behind **Caddy** with automatic HTTPS —
real Let's Encrypt certs in production, a trusted local CA for `*.localhost` /
`*.test` in development. All state lives in one SQLite file, so the whole install
is easy to move, back up, and reason about.

## Install

**Ubuntu/Debian server:**

```bash
curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | sudo bash
```

**macOS (local dev):**

```bash
curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | bash
```

The installer detects your OS + CPU arch, installs a container engine and Caddy
if they're missing, downloads the matching prebuilt binary (verifying its
checksum), writes config, installs a service, and creates your admin account.
Full details — non-interactive/automated install, manual steps, uninstall — are
in [`deploy/README.md`](deploy/README.md).

## What you get

- **Projects & apps** — group apps under a project with a shared base domain and
  a dedicated container network.
- **App templates** — static (prebuilt or Vite-built), WordPress, Laravel; add
  your own by dropping in a Compose template.
- **Automatic HTTPS** — Caddy obtains/renews certs; local domains use a trusted
  internal CA (`sudo caddy trust` once for green locks).
- **One web UI** — create/start/stop/delete apps, edit `.env`, stream logs, set
  CPU/RAM limits, take backups, and watch per-app + host metrics.
- **Single binary, single DB** — no external services; upgrades swap the binary
  and keep your data.

## Run it from source

```bash
make run          # go run ./cmd/xdev → http://127.0.0.1:7331
make build        # → ./xdev (version-stamped)
make build-all    # cross-compile all release targets into dist/
go test ./...     # tests
```

First run walks you through creating the single admin account at `/setup` (or
run `xdev create-admin you@example.com`).

## CLI

```bash
xdev                          # run the control plane (default)
xdev version                  # version + go/os/arch
xdev doctor                   # preflight: engine, caddy, ports, data dir, admin
xdev create-admin you@x.com   # create the first admin (idempotent)
xdev -h                       # full help (flags + env vars + examples)
```

Every flag has an `XDEV_*` env fallback, so a service can be configured entirely
from an env file (`/etc/xdev/xdev.env`). See
[`deploy/xdev.env.example`](deploy/xdev.env.example) for the full reference.

## Docs

- [`GUIDELINE.md`](GUIDELINE.md) — architecture, conventions, package reference,
  config, and the safety invariants.
- [`deploy/README.md`](deploy/README.md) — install, production deployment,
  upgrade, uninstall.
- [`PLAN.md`](PLAN.md) — product plan and roadmap.
