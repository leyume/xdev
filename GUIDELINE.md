# xdev — Engineering Guideline

> A complete reference for working on xdev. Written so that **any** developer or
> AI model can pick up the project cold and be productive immediately. If you
> change behavior described here, update this file in the same change.

> ⛔ **Hands off created projects.** Anything xdev has generated under
> `projects/` — and the apps, containers, volumes, and databases it represents —
> is **user data**, not part of the codebase. An AI working on xdev must **never**
> edit, regenerate, move, delete, or run lifecycle actions (start/stop/down/
> backup/etc.) against a created project or app **unless the user specifically
> targets that project/app by name**. Treat them like the `bizepp` containers:
> off-limits by default. This applies even while debugging or testing — spin up
> an isolated throwaway instead (see [§15](#15-verification-methodology)).

---

## Table of contents

1. [What xdev is](#1-what-xdev-is)
2. [Tech stack & principles](#2-tech-stack--principles)
3. [Build · run · test · verify](#3-build--run--test--verify)
4. [Repository layout](#4-repository-layout)
5. [Architecture & request flow](#5-architecture--request-flow)
6. [Package reference](#6-package-reference)
7. [Data model & migrations](#7-data-model--migrations)
8. [HTTP routes](#8-http-routes)
9. [Subsystems in depth](#9-subsystems-in-depth)
10. [Conventions & invariants](#10-conventions--invariants)
11. [Configuration reference](#11-configuration-reference)
12. [Production deployment](#12-production-deployment)
13. [How to extend (recipes)](#13-how-to-extend-recipes)
14. [Gotchas & non-obvious behavior](#14-gotchas--non-obvious-behavior)
15. [Verification methodology](#15-verification-methodology)
16. [Roadmap / backlog](#16-roadmap--backlog)

---

## 1. What xdev is

xdev is a **lean, self-hosted control plane** for developing and deploying
containerized apps. It runs on macOS (development) and Ubuntu (production). It's
a single Go binary that:

- Manages **Projects**, each containing one or more **Apps** (components).
- Generates a `docker-compose`-style stack per app and runs it via **podman**
  (default on macOS) or **docker** (default on Linux).
- Fronts every app with **Caddy** for reverse proxying + automatic TLS
  (local internal CA for `.test`/`.localhost`, Let's Encrypt for production).
- Manages local DNS (an `/etc/hosts` block), resource limits, live metrics &
  charts, logs, a per-app `.env` editor, backups, and an audit log.
- Exposes all of this through a server-rendered web UI and a small CLI.

Think: a minimal, focused mix of Coolify / CapRover / Laravel Herd.

**Core model — Projects contain Apps:**

```
Project "bizepp" (engine pinned, shared network, base domain)
  ├── App: backend   (laravel)          → api.bizepp.localhost
  ├── App: frontend  (static-build/Vite) → bizepp.localhost
  └── App: blog      (wordpress)        → blog.bizepp.localhost
```

`/Users/li/Projects/ai/bizepp` is the reference real-world project xdev's
templates are modeled on (Laravel `api/` + Vue `ui/`, with `_/compose.yml`,
bind-mounted `app/`, `_volumes/`, prefixed service names, resource limits).

Related docs: [`PLAN.md`](PLAN.md) (original design + roadmap),
[`deploy/README.md`](deploy/README.md) (production deploy).

---

## 2. Tech stack & principles

| Concern | Choice |
|---|---|
| Language | **Go 1.26**, standard-library-first, heavily commented |
| Database | **SQLite** via `modernc.org/sqlite` (pure-Go, **no CGO** → trivial cross-compile) |
| HTTP | stdlib `net/http` + Go 1.22 `ServeMux` (method+path patterns). No web framework. |
| Frontend | Server-rendered Go `html/template` + **htmx** + **Alpine.js**; charts via **uPlot**. No JS build step; assets vendored & embedded. |
| Container control | Shell out to `<engine> compose ...`; socket-free. `stats` for metrics. |
| Reverse proxy / TLS | **Caddy**, driven entirely by its **admin API** (`POST /load`, full-config sync) |
| Host metrics | `github.com/shirou/gopsutil/v3` |
| Passwords | `golang.org/x/crypto/bcrypt` |

**Direct dependencies** (`go.mod`): `modernc.org/sqlite`, `golang.org/x/crypto`,
`github.com/shirou/gopsutil/v3`. Everything else is indirect. Vendored static
assets live in `web/static/` (htmx, Alpine, uPlot).

**Principles**
- **Readability over cleverness.** The user explicitly wants code any model can
  follow. Match the surrounding style; comment the *why*.
- **Lean.** Few dependencies. Prefer stdlib. Single binary, embedded assets.
- **Everything embeds.** Templates, static assets, SQL migrations, and app
  templates are all `go:embed`-ed — the binary is fully self-contained.
- **The composition root is `cmd/xdev/main.go`** and the **server handlers**.
  Services (`projects`, `apps`, …) are decoupled; handlers coordinate them.
- **Cross-platform.** No CGO. macOS + Linux are first-class.

---

## 3. Build · run · test · verify

Go is installed via Homebrew; recipes export `PATH` to include it.

```bash
make run          # go run ./cmd/xdev  → http://127.0.0.1:7331
make build        # → ./xdev (single binary, version-stamped via ldflags)
make build-linux  # GOOS=linux GOARCH=amd64 CGO_ENABLED=0 → dist/xdev-linux-amd64
make build-all    # cross-compile all 4 release targets into dist/
make checksums    # build-all + dist/checksums.txt (sha256)
make hooks        # enable the .githooks pre-push gate (build/vet/test)
make tidy fmt vet # housekeeping
go test ./...     # tests (currently internal/templates/render_test.go)
```

**First run** walks you through creating the single admin account at `/setup`.

**Local dev with clean HTTPS** needs to bind 443/80 and edit `/etc/hosts`
(macOS allows non-root 443 on recent versions, but `/etc/hosts` + `caddy trust`
need sudo):

```bash
sudo ./xdev                                   # clean https://app.project.localhost
# — or fully sudo-free on high ports:
./xdev -https-port 8443 -http-port 8080 -hosts-file ./dev-hosts
```

**Prerequisites on the host:** a container engine (podman or docker) with its
compose plugin, and `caddy` on `PATH` (`brew install caddy` / apt).

**CLI subcommands** (dispatched at the top of `run()` before flag parsing):

```bash
xdev version                  # version + go/os/arch (also: xdev -version)
xdev doctor                   # preflight: engine/compose/daemon, caddy, ports, data dir, admin
xdev create-admin <email>     # create the first admin (idempotent; pw from $XDEV_ADMIN_PASSWORD or TTY)
xdev write-hosts <file> [h…]  # rewrite the managed hosts block (internal; run as root via the OS prompt)
xdev -h                       # grouped help: flags + env vars + examples
```

`doctor` resolves config exactly like the server and exits non-zero when a
*required* check fails, so installers/automation can gate on it.

---

## 4. Repository layout

```
xdev/
  cmd/xdev/main.go            # entrypoint, flags/env, composition root, CLI subcommand
  internal/
    config/      config.go    # data/projects dirs, addr, path derivation
    store/                    # SQLite: open, migrations, all queries
      store.go                #   Open(), embedded migration runner
      migrations/*.sql        #   schema (embedded)
      users.go sessions.go settings.go projects.go apps.go domains.go metrics.go events.go
    auth/        auth.go      # single-admin, bcrypt, sessions, CSRF, middleware
    naming/      naming.go    # Slugify + unique-slug resolution
    runtime/                  # container engine layer
      runtime.go              #   Detect(): podman/docker, installed/compose/Ready
      selector.go             #   Selector: current engine, hot-switchable
      compose.go              #   Compose/Up/Down/Start/Stop/Running/Logs, Network*
    templates/                # compose template engine
      templates.go            #   Catalog(), RenderCompose(), ScaffoldFiles(), Data
      files/<type>/           #   compose.yml.tmpl, compose.prod.yml.tmpl, scaffold/
      render_test.go
    projects/    projects.go  # project lifecycle: dir + network + engine pin
    apps/                     # app lifecycle
      apps.go                 #   Create/Start/Stop/Delete/RefreshStatus, SetDomain, port alloc
      ops.go                  #   Logs, ReadEnv/WriteEnv, Backup/ListBackups, targz
    proxy/                    # Caddy integration
      proxy.go                #   Manager: build config + POST /load, ACME/internal, LocalCARoot
      supervisor.go           #   run caddy as a child, wait for admin API, Stop
      ca.go                   #   local CA helpers: refresh stale intermediate
    domains/     hosts.go     # /etc/hosts managed block, MissingFromHosts, elevated write
    platform/    reconcile.go # Reconciler: rebuild Caddy + hosts from DB after mutations
    metrics/                  # monitoring
      collector.go            #   goroutine: poll `<engine> stats` → metrics table
      host.go                 #   gopsutil host snapshot (CPU/mem/disk)
    server/                   # HTTP layer
      server.go               #   Server struct, New(), routes, template parsing, render(), reconcile()
      funcs.go                #   template helper funcs (mib, f1, gib)
      handlers.go             #   setup/login/logout/dashboard, currentUser
      handlers_projects.go    #   project + app CRUD/actions, appAction
      handlers_metrics.go     #   metrics page + JSON
      handlers_ops.go         #   logs, env, backups, domain edit, engine switch, hosts sync, events
  web/
    web.go                    # embed templates/ and static/
    templates/*.html          # layout + pages
    static/                   # app.css + vendored htmx/alpine/uPlot
  deploy/                     # install.sh, uninstall.sh, xdev.env.example,
                              #   xdev.service (systemd), com.leyume.xdev.plist (launchd), README.md
  .github/workflows/          # release.yml (tag v* → Release), ci.yml (build/vet/test)
  .githooks/pre-push          # local build/vet/test gate (enable: make hooks)
  migrations live under internal/store/migrations (not top-level)
  data/                       # RUNTIME (gitignored): sqlite db, backups/
  projects/                   # RUNTIME (gitignored): generated per-app stacks
  PLAN.md  GUIDELINE.md  Makefile  go.mod
```

---

## 5. Architecture & request flow

```
            ┌──────────────────────────── xdev (one Go process) ────────────────────────────┐
 Browser ──▶│  net/http ServeMux ─▶ auth.RequireAuth ─▶ handler (server/*)                   │
            │       │                                      │                                  │
            │       │                                      ├─▶ projects.Service ─┐            │
            │  html/template (htmx/Alpine)                 ├─▶ apps.Service ──────┤            │
            │                                              ├─▶ store (SQLite) ◀───┘            │
            │                                              ├─▶ platform.Reconciler            │
            │                                              └─▶ runtime.Selector               │
            │  metrics.Collector (goroutine) ─▶ store      proxy.Manager ─▶ Caddy admin API   │
            └───────────────────────────────────────────────┬──────────────────┬─────────────┘
                                                             │ compose CLI      │ /load (JSON)
                                                   ┌─────────▼──────┐   ┌───────▼────────┐
                                                   │ podman / docker │   │  Caddy (child) │
                                                   │   (containers)  │   │  :443/:80 TLS  │
                                                   └─────────────────┘   └────────────────┘
```

**Composition root** (`main.go`): open store → run migrations → build
`auth.Service`, `runtime.Selector`, `projects.Service`, `apps.Service`,
`proxy.Manager` (+ `proxy.Supervisor`), `platform.Reconciler`,
`metrics.Collector`, then `server.New(...)`. Graceful shutdown stops Caddy and
the metrics goroutine.

**After any state mutation**, the handler calls `s.reconcile()`, which makes
`platform.Reconciler.Sync()` rebuild and push the full Caddy config and rewrite
the hosts-file block from the DB. State in the DB is the source of truth;
Caddy + hosts are derived.

**Per-app lifecycle** (`apps.Service`): render compose from the template →
scaffold starter files → allocate a host port → `compose up -d` on the app's
engine. Each app records its `runtime` (engine) and `compose_path`.

---

## 6. Package reference

| Package | Responsibility | Key types / functions |
|---|---|---|
| `config` | Resolve + create data/projects dirs, listen addr | `Config`, `Load()` |
| `store` | SQLite open, embedded migrations, **all** queries | `Open`, `Store`, per-entity CRUD |
| `auth` | Single-admin auth, sessions, CSRF, middleware | `Service`, `RequireAuth`, `StartSession`, `UserFrom` |
| `naming` | Human name → slug, uniqueness | `Slugify`, `Unique` |
| `runtime` | Engine detection + selection + compose driver | `Detect`, `Info`, `EngineStatus`, `Selector`, `Compose/Up/Down/Start/Stop/Logs`, `Network*` |
| `templates` | App-type catalog + compose rendering + scaffold | `Catalog`, `IsValidType`, `RenderCompose`, `ScaffoldFiles`, `Data` |
| `projects` | Project create/delete (dir, network, engine pin) | `Service.Create`, `Service.Delete` |
| `apps` | App create/start/stop/delete, domain, port, logs, env, backups | `Service` (+ `ops.go`) |
| `proxy` | Caddy config builder, admin client, supervisor, local CA | `Manager`, `Supervisor`, `RefreshStaleIntermediate` |
| `domains` | hosts-file managed block + elevated write | `SyncHosts`, `MissingFromHosts`, `SyncHostsElevated` |
| `platform` | Reconcile DB → Caddy + hosts | `Reconciler.Sync`, `MissingHosts`, `WriteHostsElevated` |
| `metrics` | Per-app stats collector + host snapshot | `Collector.Run`, `HostSnapshot` |
| `server` | HTTP routing, templates, all handlers | `Server`, `New`, handlers |
| `web` | `go:embed` of templates + static assets | `TemplatesFS`, `StaticFS` |

---

## 7. Data model & migrations

SQLite, WAL mode, foreign keys ON. Migrations are embedded SQL applied in
filename order and tracked in `schema_migrations`. **Never edit an applied
migration — add a new one.**

- `0001_init.sql` — full base schema.
- `0002_app_domain_backfill.sql` — backfill `apps.subdomain` (now the full
  domain) from the `domains` table for pre-existing rows.
- `0003_project_engine.sql` — add `projects.engine`.

**Tables** (see `0001_init.sql` for exact columns):

- **users** — single admin (email, bcrypt `password_hash`).
- **sessions** — `token` (cookie), `user_id`, `csrf_token`, `expires_at`.
- **settings** — global key/value (e.g. `engine`).
- **projects** — `slug`, `base_domain`, `environment` (local|prod),
  `network_name` (`xdev_<slug>`), `engine` (podman|docker), `dir`.
- **apps** — `project_id`, `slug`, `type`, `runtime` (engine), `status`,
  **`subdomain`** *(historical column name; now holds the app's FULL domain — Go
  field is `App.Domain`)*, `cpu_limit` (cores), `mem_limit` (bytes), `port`
  (host port), `compose_path`.
- **app_env** — reserved (per-app key/value); the `.env` editor currently writes
  the app's `app/.env` file directly.
- **domains** — `app_id`, `hostname` (UNIQUE), `is_local`, `ssl_mode`
  (internal|letsencrypt). One row per app (replaced on domain change).
- **metrics** — time series: `app_id`, `ts`, `cpu_pct`, `mem_bytes`,
  `mem_limit`. Raw, pruned to 24h.
- **events** — audit log: `project_id?`, `app_id?`, `ts`, `level`, `message`.

> ⚠️ The `apps.subdomain` column name is a legacy artifact. In Go it's
> `App.Domain` and stores the **entire** hostname (e.g. `api.bizepp.localhost`),
> not a label. Don't reintroduce `<sub>.<base>` concatenation.

---

## 8. HTTP routes

All routes except `/setup`, `/login` require auth (`auth.RequireAuth`), which
also enforces CSRF on non-GET requests (form field `csrf_token` must equal the
session's token).

```
GET  /setup                       first-run admin creation
POST /setup
GET  /login                       POST /login          POST /logout
GET  /{$}                         dashboard (projects, host card, engine switch)
GET  /projects/new                POST /projects        create project
GET  /projects/{slug}             project detail (apps, add-app form, hosts banner)
POST /projects/{slug}/delete
POST /projects/{slug}/apps        create app
POST /apps/{id}/start | stop | delete | refresh
GET  /apps/{id}/metrics           per-app chart page
GET  /apps/{id}/metrics.json      chart data (arrays t/cpu/mem)
GET  /apps/{id}/logs              tail of compose logs
GET  /apps/{id}/env               POST /apps/{id}/env   edit .env + restart
POST /apps/{id}/domain            change the app's hostname
POST /apps/{id}/backup            GET /apps/{id}/backups        GET /apps/{id}/backups/{name}
GET  /events                      audit log
POST /settings/engine             switch default container engine
POST /settings/hosts-sync         one-click /etc/hosts write (elevates if needed)
GET  /static/...                  embedded assets
```

---

## 9. Subsystems in depth

### 9.1 Auth (`internal/auth`)
Single admin. First run (`UserCount()==0`) forces `/setup`. Passwords hashed
with bcrypt. Server-side sessions in SQLite, keyed by an opaque cookie
(`xdev_session`, HttpOnly, SameSite=Lax, `Secure` when `-secure`). A CSRF token
is bound to each session and validated on unsafe methods by `RequireAuth`.
`UserFrom(r)` / `SessionFrom(r)` read the request context set by the middleware.

### 9.2 Container engine (`internal/runtime`)
- `Detect()` probes podman & docker for: **installed** (binary on PATH),
  **ComposeOK** (`<engine> compose version`), and **Ready** (`<engine> ps`
  succeeds = daemon/machine up). `pickDefault` prefers an engine whose daemon is
  **Ready** (so docker isn't chosen when Docker Desktop is off), then OS pref
  (podman on macOS, docker on Linux).
- `Selector` holds the **current** engine and is hot-switchable at runtime
  (mutex-guarded). Precedence at startup: **persisted `engine` setting > `-engine`
  flag / `XDEV_ENGINE` > auto-detect**. Threaded through `projects`, `apps`,
  `metrics`, `server` — never use a static default.
- **Per-project pinning:** a project records the engine it was created with
  (`projects.engine`). Its apps use that engine (`apps.Create` reads
  `proj.Engine`), so a project's network and containers always agree even if the
  global default is later switched. Switching only affects **new** projects.
- `compose.go` shells out: `Compose(ctx, engine, workdir, project, file, args…)`
  runs `<engine> compose -p <project> -f <file> …` with `cwd=workdir`. Helpers:
  `Up/Down/Start/Stop/Running/Logs`, `NetworkCreate/Remove` (idempotent).

### 9.3 Compose templates (`internal/templates`)
- `Catalog()` lists app types and whether each is selectable (`Available`).
  Current types: `static-prebuilt`, `static-build`, `wordpress`, `laravel`.
- Template files live in `files/<type>/`:
  - `compose.yml.tmpl` (dev) and optional `compose.prod.yml.tmpl` (prod);
    `RenderCompose` picks the prod variant when `Data.Env=="prod"` and it exists.
  - `scaffold/` — files copied into the app's `app/` dir on creation (skipping
    existing files).
- `Data` is the template context: `ProjectSlug`, `NetworkName`, `AppSlug`,
  `AppType`, `Env`, `HostPort`, `CPULimit`, `MemLimit`. Methods `HasLimits`,
  `CPUStr`, `MemStr` drive the optional `deploy.resources.limits` block.

### 9.4 App lifecycle (`internal/apps`)
- `Create(projectID, name, type, domain, cpu, mem)`:
  resolve engine (project's), unique slug, **free-form domain** (blank → bare
  project base domain, else `<slug>.<base>`; validated; uniqueness checked),
  allocate a host port, build `projects/<project>/<app>/{_/compose.yml, app/}`,
  scaffold, persist, attach domain (`ReplaceAppDomain`), then `Start`.
- `Start` = `compose up -d` (idempotent), sets status. `Stop` = `compose stop`.
  `Delete` = `compose down` + remove row + `RemoveAll(appDir)`.
- `SetDomain` changes the hostname (validates + uniqueness, updates app row +
  domains row). Caller reconciles.
- **Port allocation** scans `[20000, 29999]`, skipping DB-used ports and any port
  not free. `portFree` binds the **wildcard** `:p` (not loopback) to match how
  engines publish ports.
- `ops.go`: `Logs` (compose logs tail), `ReadEnv`/`WriteEnv` (`app/.env`),
  `Backup`/`ListBackups`/`BackupPath` (`.tar.gz` of the app dir under
  `data/backups/<project>_<app>/`; named volumes like DBs are **not** included).

### 9.5 Reverse proxy + TLS (`internal/proxy`)
- `Supervisor.Start` runs `caddy run` as a child with `CADDY_ADMIN` set, waits
  for the admin API, and stops it on shutdown. On a server you'd instead run
  Caddy under systemd and set `-caddy=false`.
- `Manager.Sync(routes)` builds the **entire** Caddy JSON config and `POST`s it
  to `/load`. One HTTP server (`xdev`) listens on the HTTPS port; Caddy
  auto-creates the HTTP→HTTPS redirect using `http_port`/`https_port`. Each route
  matches a host and `reverse_proxy`s to `127.0.0.1:<app port>`.
- **TLS automation:** `internal` issuer (local CA) for local hosts; **ACME**
  issuer for public (prod) hosts (with `-acme-email` if set). Caddy's default
  storage holds the CA.
- **Cert lifetimes:** leaf default **90 days** (`-local-cert-lifetime`, `2160h`);
  intermediate **1 year**; root **~10 years**. A leaf can't outlive its
  intermediate, so on startup `RefreshStaleIntermediate("local", leafLifetime)`
  deletes a too-short intermediate (keeping the root → **no re-trust**) so Caddy
  mints a fresh long-lived one. `caddyDataDir()` mirrors Caddy's storage path
  (honors `XDG_DATA_HOME`; macOS `~/Library/Application Support/Caddy`).

### 9.6 Domains & hosts (`internal/domains`, `internal/platform`)
- Local base domains default to **`<slug>.localhost`**, which resolves to
  127.0.0.1 at the OS level on macOS (and in browsers everywhere) — **no
  `/etc/hosts` edit needed**.
- `.test`/`.local` need a hosts entry. `SyncHosts(path, hosts)` rewrites an
  xdev-owned block (`# >>> xdev (managed) >>>`). The reconciler writes it
  automatically if it can (xdev running as root / writable file).
- When xdev can't write the file, the project page shows a banner (only listing
  hosts actually **missing**, via `MissingFromHosts`) with an **"Add to hosts
  file"** button → `Reconciler.WriteHostsElevated()` → tries a direct write, then
  elevates via the OS prompt (`osascript … with administrator privileges` on
  macOS, `pkexec` on Linux), re-invoking `xdev write-hosts <file> <host…>`.
- `caddy trust` (sudo, one-time) installs the local root CA for a trusted
  padlock; xdev prints a hint at startup.

### 9.7 Metrics (`internal/metrics`)
- `Collector.Run` ticks every 10s: reads `<engine> stats --no-stream` for **all
  usable engines**, attributes containers to apps by name prefix
  (`<project>_<app>_`), aggregates cpu%/mem per app, inserts into `metrics`, and
  prunes >24h.
- Per-app chart page (`/apps/{id}/metrics`) uses uPlot fed by
  `/apps/{id}/metrics.json` (arrays `t`/`cpu`/`mem`).
- `HostSnapshot()` (gopsutil) powers the dashboard host card (CPU/mem/disk).

---

## 10. Conventions & invariants

- **Slugs**: lowercase `[a-z0-9-]`, collisions resolved with `-2`, `-3`…
  (`naming.Unique`).
- **Per-app on-disk layout** (mirrors bizepp):
  `projects/<project-slug>/<app-slug>/_/compose.yml` (generated) +
  `projects/<project-slug>/<app-slug>/app/` (bind-mounted content).
- **Container names**: `<project-slug>_<app-slug>_<service>` (e.g.
  `bizepp_blog_web`). **Compose project name**: `<project-slug>_<app-slug>`.
- **Networks**: per-project external network `xdev_<project-slug>`; app compose
  files reference it as external `projectnet` for cross-app reach, plus an
  `internal` network for an app's own services (web↔db). DB/Redis data use
  **named volumes** (not bind mounts).
- **Host ports**: allocated `20000–29999`, stored on `apps.port`; Caddy upstreams
  point at `127.0.0.1:<port>`.
- **Reconcile after mutations**: any handler that changes projects/apps/domains
  calls `s.reconcile()` before redirecting.
- **DB is source of truth**; Caddy config + hosts file are always rebuilt from it.
- **Never touch the `bizepp` containers** (`be_*`) during testing.
- **Created projects are off-limits.** Do not edit, regenerate, or run lifecycle
  actions against anything under `projects/` (or its containers/volumes/DBs)
  unless the user explicitly targets that project/app by name. It's user data,
  not code. (See the ⛔ callout at the top.)

---

## 11. Configuration reference

**Every** flag has an `XDEV_*` env fallback (so a service manager can configure
xdev entirely from an `EnvironmentFile`). Precedence is **explicit flag > env >
built-in default**. The canonical, commented reference is
[`deploy/xdev.env.example`](deploy/xdev.env.example) — keep it in sync with this
table and the installer.

| Flag | Env var | Default | Purpose |
|---|---|---|---|
| `-data` | `XDEV_DATA` | `./data` | sqlite db + `backups/` |
| `-projects` | `XDEV_PROJECTS` | `./projects` | generated per-app stacks |
| `-addr` | `XDEV_ADDR` | `127.0.0.1:7331` | web UI listen address |
| `-secure` | `XDEV_SECURE` | `false` | set `Secure` on cookies (serve over HTTPS) |
| `-engine` | `XDEV_ENGINE` | auto | `podman` \| `docker` (persisted UI setting wins) |
| `-caddy` | `XDEV_CADDY` | `true` | supervise Caddy as a child (`false` = external Caddy) |
| `-caddy-admin` | `XDEV_CADDY_ADMIN` | `127.0.0.1:2019` | Caddy admin API address |
| `-https-port` | `XDEV_HTTPS_PORT` | `443` | public HTTPS port |
| `-http-port` | `XDEV_HTTP_PORT` | `80` | public HTTP port |
| `-hosts-file` | `XDEV_HOSTS_FILE` | `/etc/hosts` | hosts file to manage |
| `-manage-hosts` | `XDEV_MANAGE_HOSTS` | `true` | auto-write local domains to the hosts file |
| `-acme-email` | `XDEV_ACME_EMAIL` | "" | Let's Encrypt contact (prod) |
| `-local-cert-lifetime` | `XDEV_LOCAL_CERT_LIFETIME` | `2160h` | local TLS leaf validity (keep < ~8000h) |

Bool env vars parse `1/true/yes/on` (and `0/false/no/off`), case-insensitive.
`create-admin` also reads `XDEV_ADMIN_PASSWORD` for non-interactive installs.

CLI subcommands: `version`, `doctor`, `create-admin <email>`, `write-hosts
<file> [host...]` (the last is internal, used by hosts-sync). See §3.

### Install & release

Distribution is **GitHub Actions on a version tag → GitHub Releases → `curl |
bash`**. No binaries are committed to git.

- **`deploy/install.sh`** — one-command installer (Linux + macOS): detects
  os/arch, installs engine + Caddy, downloads the matching Release binary and
  **verifies its sha256**, writes `/etc/xdev/xdev.env`, installs the service
  (systemd / launchd), runs `create-admin` + `doctor`. Reads prompts from
  `/dev/tty`; fully non-interactive via `XDEV_*` env + `XDEV_NONINTERACTIVE=1`.
- **`.github/workflows/release.yml`** — on a `v*` tag: vet + test, cross-compile
  the four targets (`xdev-{linux,darwin}-{amd64,arm64}`) with
  `-ldflags "-X main.version=$TAG"`, write `checksums.txt`, publish the Release.
- **`.github/workflows/ci.yml`** — build/vet/test on push/PR to `main`.
- **`.githooks/pre-push`** — local fast gate (build/vet/test only; never builds
  release binaries). Enable with `make hooks`; bypass with `git push --no-verify`.
- **Release ritual:** `git tag v0.1.0 && git push origin v0.1.0`.

---

## 12. Production deployment

See [`deploy/`](deploy). On Linux the engine defaults to **docker**; Caddy gets
**real Let's Encrypt** certs for `environment=prod` projects with real domains.

```bash
# One-liner (detects os/arch, installs engine+Caddy, downloads + verifies the
# Release binary, writes /etc/xdev/xdev.env, installs the service, creates admin):
curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | sudo bash
```

The installer puts the binary at `/usr/local/bin/xdev`, data at `/var/lib/xdev`,
and config at `/etc/xdev/xdev.env` (loaded by the unit's `EnvironmentFile`).
`deploy/xdev.service` runs xdev with `CAP_NET_BIND_SERVICE` (bind 80/443
without full root). The admin UI binds to `127.0.0.1:7331` — reach it via SSH
tunnel. Laravel apps in prod render `compose.prod.yml.tmpl` (hardened Swoole
image, read-only code mount, healthcheck, log rotation, limits).

---

## 13. How to extend (recipes)

**Add an app type**
1. Create `internal/templates/files/<type>/compose.yml.tmpl` (and optionally
   `compose.prod.yml.tmpl`, `scaffold/…`). Use `Data` fields; include the
   `{{if .HasLimits}}deploy:…{{end}}` block on the main service; publish
   `{{.HostPort}}:<container-port>`; join `internal` + external `projectnet`.
2. Add an entry to `templates.Catalog()` with `Available: true`.
3. Add a case to `render_test.go` expectations if needed. No other code changes —
   the lifecycle/proxy/TLS machinery is generic.

**Add a route/page**
1. Add `mux.HandleFunc(...)` in `server.go` `routes()` (wrap with
   `s.auth.RequireAuth`).
2. Write the handler in the appropriate `handlers_*.go`.
3. For a new page, add `web/templates/<name>.html` defining `{{define "content"}}`,
   and add `"<name>"` to the `pages` slice in `server.go` `parseTemplates`.
4. Mutating handlers: write an audit event (`store.AddEvent`) and call
   `s.reconcile()` if proxy/hosts state changed.

**Add a migration**: drop `internal/store/migrations/000N_name.sql`. It runs in
order on next start. Never edit applied ones.

**Add a setting**: use `store.GetSetting/SetSetting`; read at startup or per
request. Persisted settings should win over flags (see `engine`).

---

## 14. Gotchas & non-obvious behavior

- **`apps.subdomain` = full domain.** Legacy column name; Go field `App.Domain`.
  Don't reconstruct `<sub>.<base>`.
- **`.localhost` resolves OS-wide on macOS** (verified) → no hosts edit. `.test`
  needs `/etc/hosts`; **avoid `.local`** (collides with macOS mDNS/Bonjour).
- **Binding 443/80, editing `/etc/hosts`, `caddy trust`** need root. The hosts
  banner + one-click button handle the hosts case via OS elevation.
- **Caddy leaf certs are capped by the intermediate.** A long leaf needs a long
  intermediate; xdev auto-refreshes a stale intermediate on startup (root
  preserved → no re-trust).
- **Caddy storage is shared & persistent** (`~/Library/Application Support/Caddy`),
  not per-xdev-instance. Multiple instances share one CA. Setting
  `XDG_DATA_HOME` relocates it (used in tests for isolation).
- **HTTP→HTTPS redirect** requires Caddy to know `http_port`/`https_port` — set
  in the config; only the HTTPS port is bound by our server (Caddy adds the
  redirect server itself).
- **`docker compose version` works without the daemon**, so "installed +
  compose" ≠ usable. That's why detection also checks **Ready** (`<engine> ps`).
- **Port detection uses wildcard bind** (`:p`), not loopback, to catch ports
  another engine/instance already publishes.
- **Engine switch only affects new projects**; existing apps keep their pinned
  engine. Network ops on delete use the project's stored engine.
- **First `compose up` may pull images** (multi-minute) — `apps.composeTimeout`
  is 5 min.

---

## 15. Verification methodology

How changes were validated (and how you should validate yours):

- **Build gates:** `go build ./cmd/xdev`, `go vet ./...`, `go test ./...` must be
  clean before claiming done.
- **Real runtime checks**, not just compilation: start xdev, drive it with
  `curl` (cookie jar + CSRF token scraped from a page), create real
  projects/apps, hit the served site, then tear down.
- **Isolation when an instance is already running:** use distinct `-addr`,
  `-caddy-admin`, `-https-port`/`-http-port`, a temp `-data` dir, and a temp
  `-hosts-file`. For Caddy CA isolation, set `XDG_DATA_HOME` to a temp dir.
- **Never disrupt the user's running instance or the `bizepp` (`be_*`)
  containers.** Clean up created containers/networks/dirs after each test.
- **Inspect, don't assume:** read the SQLite db (`sqlite3 data/xdev.db …`), Caddy
  config (`curl :2019/config/`), generated `compose.yml`, cert dates
  (`openssl x509`), and `/etc/hosts`.

---

## 16. Roadmap / backlog

Implemented: Phases 0–6 (foundations, projects/apps, proxy+local SSL, app
templates, production/ACME+systemd, resources+metrics+charts, polish) plus
post-launch fixes (free-form domains, `.localhost`, switchable engine,
one-click hosts sync, 90-day auto-refreshed local certs) and
**install/distribution** (one-command `install.sh`, `doctor`/`create-admin`
subcommands, full env-file config, GitHub-Actions-on-tag → Releases).

Backlog (see `PLAN.md` §10): live log **streaming** (SSE), git-based deploy,
multi-user/roles, scheduled backups, **named-volume (DB) backups**, dry-run
compose preview, full CLI parity, optionally blocking an engine switch until the
daemon is Ready.

---

*Keep this document current. If you change a flag, route, schema, convention, or
behavior, edit the relevant section in the same change. The config/env table
(§11) is mirrored in [`deploy/xdev.env.example`](deploy/xdev.env.example) and
`deploy/install.sh` — change all three together.*
