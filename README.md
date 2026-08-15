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

The installer detects your OS + CPU arch, installs a container engine and Node
(for static apps) if they're missing, downloads the matching prebuilt binary
(verifying its checksum), writes config, installs a service, and creates your
admin account. Caddy defaults to running as a container the installer generates
and starts — choose `native` at the prompt to install and supervise a Caddy
binary instead, or `self` to wire xdev to a proxy you already run.
Full details — non-interactive/automated install, manual steps, uninstall — are
in [`deploy/README.md`](deploy/README.md).

## What you get

- **Projects & apps** — group apps under a project with a shared base domain and
  a dedicated container network.
- **App templates** — Static (runs on your system Node, no container — serve a
  folder or run a build/dev command), WordPress, Laravel (auto-installs a fresh
  Laravel + Octane/Swoole with MariaDB + Redis + Adminer); add your own by
  dropping in a Compose template.
- **Bring your own Compose** — pick the **Compose** type and upload or paste a
  `docker-compose.yml` / `compose.yml`: xdev runs your stack as-is. Say how many
  domains the stack needs and name them; xdev allocates a host port per domain
  and your file publishes them as `"${PORT}:80"`, `"${PORT_2}:8000"`,
  `"${PORT_3}:9000"` — domain 2 is always `${PORT_2}`, up to five per stack. No
  port numbers to pick or keep out of each other's way. Leave the file blank for
  a starter to edit.
- **Deploy from GitHub** — point a static or Go app at a repository and xdev
  clones it, reads `package.json` to work out how to build it (npm/pnpm/yarn/bun,
  from the lockfile), and serves the build output. **Deploy** pulls the branch
  and rebuilds. Private repos get their own SSH deploy key: xdev generates it,
  shows you the public half to paste into the repository, and keeps the private
  half encrypted on disk. Monorepos build from a subdirectory.
- **…and keep it deployed, two ways.** Turn on a **webhook** and every push to
  the tracked branch deploys itself — xdev gives you the URL and secret to paste
  into the repository. Or issue a **deploy token** and let GitHub Actions build
  and upload the finished site; xdev swaps it in atomically, so nothing is ever
  half-published. Both endpoints live on the app's own domain, so there is no
  extra hostname or certificate to arrange, and only those two paths are exposed
  — your admin UI stays off the internet. Deploys run in the background with a
  history and build logs on the app's page.
- **Laravel from a repository, too** — point a Laravel app at a repo and each
  deploy pulls, runs `composer install` and your migrations inside the app's own
  container, rebuilds the config/route/view caches, and reloads Octane. Your
  `.env` and `storage/` are mounted from outside the checkout, so a deploy can
  never take the app key or user uploads with it. Pending migrations get a
  database dump first — kept with your backups, and never auto-restored, because
  rows written since the dump are real. A short list of named commands
  (`migrate`, `optimize:clear`, `octane:reload`, `storage:link`…) runs inside the
  container from the app's page; there is no free-form command box, on purpose.
- **Several domains per app** — the Domain field takes a comma-separated list:
  `test.com, www.test.com, test.com.ng`. Every name serves the same app and gets
  its own certificate; the first is the app's address. Add or remove one any time
  from the app's settings.
- **Automatic HTTPS** — Caddy obtains/renews certs; local domains use an
  internal CA. Trust its root once for green locks — Caddy mints it when the
  first `.test`/`.localhost` site is served, and `xdev doctor` then prints the
  root's path and the trust command for your OS.
- **One web UI** — create/start/stop/delete apps, edit `.env`, stream logs, set
  CPU/RAM limits, take backups, and watch per-app + host metrics.
- **Editable after create** — every app has a settings page: rename it, move its
  domain, add or drop a compose stack's extra domains, change a static app's
  build/start commands or a proxy's upstream, and edit the `compose.yml` itself.
  Changes are checked before they're written and applied on restart.
- **Bring your own folder** — static and Go apps can also run from code you already
  have (`/home/li/ui/xyz`) instead of a folder xdev creates, set when you add
  the app or changed later. xdev never scaffolds into that folder, and deleting
  the app unhooks it rather than deleting your code.
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
xdev help                     # full help (flags + env vars + examples); also -h / --help
xdev version                  # version + go/os/arch
xdev doctor                   # preflight: engine, caddy, ports, data dir, admin
xdev create-admin you@x.com   # add an admin (idempotent)
xdev create-admin you@x.com --reset   # reset a forgotten password
```

Every flag has an `XDEV_*` env fallback, so a service can be configured entirely
from an env file (`/etc/xdev/xdev.env`). See
[`deploy/xdev.env.example`](deploy/xdev.env.example) for the full reference.

## Where your data lives (choosing the database)

All state is one SQLite file, `<data-dir>/xdev.db`. **Which data dir is used
depends on how you start xdev** — this trips people up, so it's worth knowing:

| How you start xdev | Data dir | Why |
|---|---|---|
| `xdev` run directly (no `-data`/`XDEV_DATA`) | `./data` — **relative to your current directory** | the bare-binary default |
| Installed as a **background service** | `/var/lib/xdev` | a service has no meaningful working dir, so it needs an absolute, root-writable path |

To pick a specific database locally, point xdev at it explicitly — the same DB
is then used for the server, `create-admin`, and `doctor`:

```bash
xdev -data ./data                 # project-local database
XDEV_DATA=/path/to/dir xdev       # or via env var
```

**Common gotcha — admin password resets.** A reset only takes effect on the DB
the running server actually uses. If you installed the service, that's
`/var/lib/xdev`, **not** `./data` — so target it explicitly (root owns it):

```bash
sudo XDEV_DATA=/var/lib/xdev XDEV_ADMIN_PASSWORD='new-password' \
  xdev create-admin you@example.com --reset
```

The running server re-reads the password on every login, so a reset takes
effect immediately — no restart needed. (`xdev doctor` prints the resolved DB
path so you can confirm which one is in use.)

xdev supports **multiple admins** — all accounts have equal access. Add or
remove them from the **Admins** page in the UI, or with `create-admin`.

## Docs

- [`GUIDELINE.md`](GUIDELINE.md) — architecture, conventions, package reference,
  config, and the safety invariants.
- [`deploy/README.md`](deploy/README.md) — install, production deployment,
  upgrade, uninstall.
- [`PLAN.md`](PLAN.md) — product plan and roadmap.
