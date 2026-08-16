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
xdev help                     # grouped help: flags + env vars + examples (also: xdev -h / --help)
xdev version                  # version + go/os/arch (also: xdev -version)
xdev doctor                   # preflight: engine/compose/daemon, caddy, ports, data dir, admin
xdev create-admin <email>     # add an admin (idempotent; --reset to set a new password)
xdev write-hosts <file> [h…]  # rewrite the managed hosts block (internal; run as root via the OS prompt)
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
      deploykeys.go           #   per-app SSH deploy keys (private half encrypted)
      deployments.go          #   deploy history + the endpoint paths/credentials
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
      apps.go                 #   Create/Start/Stop/Delete, port alloc
      edit.go                 #   Update(): the settings page — everything editable after create
      compose.go              #   bring-your-own compose: slots, ports, _/.env
      gitapp.go               #   apps cloned from a repository: clone, build detection
      deploy.go               #   deploys: async runner, history, uploaded builds
      ops.go                  #   Logs, ReadEnv/WriteEnv, Backup/ListBackups, targz
    gitsrc/      gitsrc.go    # git CLI wrapper: ParseRepo, Clone/Update, deploy keys
    secrets/     secrets.go   # AES-256-GCM box keyed from <data-dir>/secret.key
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
      handlers_ops.go         #   logs, env, backups, engine switch, hosts sync, events
      handlers_app_settings.go #  per-app settings page (edit every field after create)
      handlers_git.go         #   deploy-key generation, Deploy, key rotation
      handlers_deploy.go      #   public webhook + upload endpoints, token/secret admin
  web/
    web.go                    # embed templates/ and static/
    templates/*.html          # layout + pages
    static/                   # app.css + vendored htmx/alpine/uPlot
  deploy/                     # install.sh, uninstall.sh, xdev.env.example,
                              #   xdev.service (systemd), com.leyume.xdev.plist (launchd), README.md
  .github/workflows/          # release.yml (tag v* → Release), ci.yml (build/vet/test)
  .githooks/pre-push          # local build/vet/test gate (enable: make hooks)
  migrations live under internal/store/migrations (not top-level)
  data/                       # RUNTIME (gitignored): sqlite db, secret.key, backups/
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
| `apps` | App create/start/stop/delete/edit/deploy, domain, port, logs, env, backups | `Service` (+ `ops.go`, `edit.go`, `gitapp.go`, `deploy.go`) |
| `gitsrc` | Clone/fetch a repository for a git-backed app; deploy keys | `ParseRepo`, `Repo`, `Source`, `Clone`, `Update`, `NewDeployKey` |
| `secrets` | Encrypt the few values that must not sit in the clear | `New`, `Box.Seal`, `Box.Unseal` |
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
- `0010_app_source_dir.sql` — add `apps.source_dir` (host apps pointed at a
  directory the user already has).
- `0011_app_git.sql` — add `apps.git_url/git_ref/git_subdir/deployed_sha/
  deployed_at` and the `deploy_keys` table (host apps cloned from a repository).
- `0012_deploys.sql` — add `apps.hook_id/hook_secret/push_token_hash/
  push_token_hint` and the `deployments` table (deploying from outside the UI).

**Tables** (see `0001_init.sql` for exact columns):

- **users** — single admin (email, bcrypt `password_hash`).
- **sessions** — `token` (cookie), `user_id`, `csrf_token`, `expires_at`.
- **settings** — global key/value (e.g. `engine`).
- **projects** — `slug`, `base_domain`, `environment` (local|prod),
  `network_name` (`xdev_<slug>`), `engine` (podman|docker), `dir`.
- **apps** — `project_id`, `slug`, `type`, `runtime` (engine), `status`,
  **`subdomain`** *(historical column name; now holds the app's FULL domain — Go
  field is `App.Domain`)*, `cpu_limit` (cores), `mem_limit` (bytes), `port`
  (host port), `compose_path`, `source_dir` (host apps only: an existing
  directory the app was pointed at; `''` = the managed `<project.dir>/<slug>`),
  `git_url`/`git_ref`/`git_subdir` (host apps only: the repository the app is a
  clone of; `''` = not git-backed) and `deployed_sha`/`deployed_at` (which
  commit is built and serving — observed state, not settings). `source_dir` and
  `git_url` are **mutually exclusive**; see [§9.9](#99-git-backed-apps).
- **app_env** — reserved (per-app key/value); the `.env` editor currently writes
  the app's `app/.env` file directly.
- **domains** — `app_id`, `hostname` (UNIQUE), `is_local`, `ssl_mode`
  (internal|letsencrypt), `port`. **One or more** `port = 0` rows per app — the
  hostnames it answers on, in entry order (`AppHostnames`) — plus one `port > 0`
  row per secondary service (Adminer, a compose stack's extra `${PORT_n}`
  domains), rewritten together by `ReplaceAppDomains`. `apps.subdomain` holds
  only the **first** `port = 0` hostname: the app's address in the UI. The rest
  live only here, so anything that needs the full set reads `AppHostnames`.
- **deployments** — one row per deploy attempt: `trigger` (manual|webhook|push),
  `status` (running|ok|failed), `sha`, `message`, and the build `log` of a
  failure. Deploys are asynchronous, so this row *is* the progress indicator.
  Trimmed to the last 20 per app.
- **deploy_keys** — one SSH keypair per git-backed app with a private
  repository: `app_id` (0 while unbound), `public_key`, `private_key`
  (**encrypted**), `fingerprint`. A key is created *before* its app, because the
  public half has to reach GitHub before the first clone; unbound rows older
  than a day are pruned at startup.
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
POST /apps/{id}/start | stop | delete
POST /deploy-keys                 generate an unbound deploy key (JSON: id, public_key, fingerprint)
POST /apps/{id}/deploy            start a deploy (returns at once; it runs in the background)
POST /apps/{id}/deploy-key        replace this app's deploy key
POST /apps/{id}/webhook           enable / rotate / disable the GitHub webhook
POST /apps/{id}/push-token        issue / revoke the CI deploy token
POST /apps/{id}/action            run one allowlisted container command (see 9.11);
                                  answers JSON {label,output,failed} to Accept: application/json,
                                  else re-renders the page — a *failed command* is still 200
POST /apps/{id}/db-dump           switch pre-migration database dumps on/off
POST /apps/{id}/adminer           add / remove the bundled Adminer (laravel)
GET  /apps/{id}/deploys/partial   deploy history fragment (polled while one runs)
POST /apps/{id}/deploys/clear     empty the deploy history (a running deploy is kept)
GET  /jobs/{id}                   progress of a background create (polled by the dialog)

Public (no session, no CSRF — see §9.10). Caddy publishes these only on the
hostname of an app that switched them on:

POST /_xdev/hook/{hook}           GitHub push event; HMAC-verified, then deploys
POST /_xdev/deploy                a .tar.gz of a built site; bearer-token auth
GET  /apps/{id}/metrics           per-app chart page
GET  /apps/{id}/metrics.json      chart data (arrays t/cpu/mem)
GET  /apps/{id}/logs              tail of compose logs
POST /apps/{id}/logs/clear        empty a host app's log file (container logs are the engine's)
GET  /apps/{id}/env               POST /apps/{id}/env   edit .env + restart
GET  /apps/{id}/settings          POST /apps/{id}/settings  edit the app (see 9.5)
POST /apps/{id}/backup            GET /apps/{id}/backups        GET /apps/{id}/backups/{name}
GET  /events                      audit log
POST /events/clear                empty it (all, or one project via project_id)
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
- The `compose` type inverts this: the **user** supplies the compose file
  (uploaded or pasted on the add-app form) and xdev runs it as-is;
  `files/compose/compose.yml.tmpl` is only the starter written when they supply
  nothing. See `internal/apps/compose.go`.
- `partials/*.tmpl` — `{{define}}` blocks associated with that type's compose
  templates by `parseWithPartials`, and renderable on their own with
  `RenderPartial`. There is exactly one today, `adminer_service`, and it exists
  because that service can be added or removed after the app is created: a
  single definition is what keeps the block xdev *writes* identical to the block
  it *removes*, and keeps the local and prod stacks from drifting apart.

### 9.4 App lifecycle (`internal/apps`)
- `Create(projectID, name, type, domain, cpu, mem)`:
  resolve engine (project's), unique slug, **free-form domain** (blank → bare
  project base domain, else `<slug>.<base>`; validated; uniqueness checked),
  allocate a host port, build `projects/<project>/<app>/{_/compose.yml, app/}`,
  scaffold, persist, attach domain (`ReplaceAppDomain`), then `Start`.
- `Start` = `compose up -d` (idempotent), sets status. `Stop` = `compose stop`.
  `RefreshStatus` reconciles a stored status with reality but has **no caller**
  since the project page's ↻ button was dropped — status is only recomputed by
  a start/stop.
  `Delete` = `compose down` + remove row + `RemoveAll(appDir)`.
- `Update(id, EditOpts)` (`edit.go`) is the settings page's one entry point —
  see §9.5.
- **Port allocation** scans `[20000, 29999]`, skipping DB-used ports and any port
  not free. `portFree` binds the **wildcard** `:p` (not loopback) to match how
  engines publish ports.
- **Host apps can come from a repository** (`apps.git_url`) — cloned at create,
  redeployed with `Service.DeployAsync`, a webhook, or an upload from CI. See
  [§9.9](#99-git-backed-apps) and [§9.10](#910-deploying-from-outside-the-ui-internalappsdeploygo-internalserverhandlers_deploygo).
- **Host apps can live outside the project** (`apps.source_dir`, `App.SourceDir`,
  `validSourceDir`). The add-app form and the settings page take an optional
  absolute **App folder** — `/home/li/ui/xyz`, `~` expanded — and when it is set
  the app runs from there instead of `<project.dir>/<slug>`. The directory
  **must already exist**: xdev writes inside it but never creates it, so a typo
  fails validation rather than materializing a folder.
  - `s.appDir(app)` is the single resolver (`SourceDir` wins over the compose
    path and the project), so logs, backups, imports, the `.env` editor, the
    build's working directory and the Caddy `Root` all follow one answer.
  - **xdev is a tenant in an external folder, not its owner** — the invariant the
    whole feature rests on. `layoutStatic` writes no scaffold and no placeholder
    `index.html` into one; `Delete` removes the app row but **not** the
    directory; a failed `Create` unwinds only what it made (`discard`); a shared
    DB's credentials are **appended** to an existing `.env` (`writeDBEnv`)
    instead of replacing it.
  - It is editable on the settings page — the one directory-naming field that is,
    because a host app's files aren't generated, so re-pointing changes only
    where the build runs and which folder is served. Neither the old nor the new
    directory is touched; moving back to blank re-creates the managed folder.
- **Bring-your-own compose apps** (`compose.go`): **ports are xdev's, domains are
  the user's.** The add-app form asks how many domains the stack needs and takes
  a hostname for each; `layoutCompose` allocates one free host port per domain
  (`allocPorts`) and writes them to `_/.env` as `PORT`/`PORT_1`, `PORT_2`, …
  (plus `XDEV_` aliases). The file publishes those variables — `"${PORT}:80"`,
  `"${PORT_2}:8000"` — so nothing is hard-coded and two stacks can't collide.
  The mapping is **positional and fixed at create time**: domain *n* is
  `${PORT_n}`, domain 1 being the app's own. Slots 2..N become secondary
  `domains` rows (`port > 0`), routed straight to their port exactly like
  Adminer's. `apps.MaxComposeSlots` (**5**) caps how many a stack may ask for and
  is the single source of that number — the add-app form reads it as the
  `MaxDomains` view value for its `max`, its clamp, and its slot scan.
  - `ComposePorts` scans the file for both kinds of published port: `${PORT_n}`
    slots (`ComposePort.Slot`, bare `${PORT}` = slot 1) and hard-coded numbers
    (`Slot == 0`). `checkComposeSlots` then pairs it against the domain count —
    every slot 1..N must be published, and a `${PORT_n}` past the last domain is
    an error rather than a port nothing routes to. Hard-coded ports are left
    alone beyond two guards: no other app may own one (`UsedPorts`), and the
    allocator won't hand out the same number (passed as `avoid`).
  - Hostnames are validated against `DomainOwner` (and each other, and the app's
    own domain) before anything is written; a **blank** extra domain is an error,
    not something to skip — dropping it would renumber every `${PORT_n}` after
    it.
  - `prepareCompose` re-reads the file on every `Start` and checks each slot the
    app owns still has somewhere to go, then rewrites the managed `.env` lines —
    the file compose reads for `${VAR}` substitution, and the one the Env tab
    edits for these apps. Ports **don't move**: the file names them by variable,
    so an edit changes which service is behind a domain, not which number. Two
    accommodations for older apps: a slot is also satisfied by the file
    hard-coding that exact number, and a single-domain app whose file hard-codes
    its port still follows a change (`SetAppPort`). Domains are left alone —
    user configuration, not derived state. Their `_/` is user content, so
    imports restore it (`unpacksAll`) instead of skipping it.
- The add-app form mirrors the slot scan in JS (`detectComposePorts` /
  `detectComposeSlots` in `project.html`) to name the service behind each slot as
  the file is pasted or loaded, and bumps the domain count when a pasted file
  publishes more slots than are shown; the server re-checks on submit and is the
  source of truth.
  - The app's own domain (slot 1) is the dialog's single **Domain** field, in the
    same place for every type — `addAppForm.domain`, submitted as `domain`. The
    port-map rows below it are the *extras*, slots 2..N (`domains[i]` →
    `${PORT_(i+2)}`, submitted as `extra_domain`), matching how the settings page
    splits them. Slot 1 still gets a row, but a read-only one echoing the field
    above: a stack's port map is only legible whole. `domainCount` counts the
    domains in total, so `syncDomains` keeps one row *fewer* than it.
- `ops.go`: `Logs` (compose logs tail), `ReadEnv`/`WriteEnv` (`app/.env`, or
  `_/.env` for compose apps; `.env` at the root for host apps),
  `Backup`/`ListBackups`/`BackupPath` (`.tar.gz` of the app dir under
  `data/backups/<project>_<app>/`; named volumes like DBs are **not** included).

### 9.5 Editing an app after it exists (`internal/apps/edit.go`)
`Update(id, EditOpts)` backs `GET`/`POST /apps/{id}/settings` — one page per app
holding everything the add-app form collected. The split it enforces:

- **Configuration** — editable: name, the app's domains (one field, the full
  comma-separated list — see §9.7), a compose app's extra
  domains, the bundled UI's hostname (Adminer / mail admin), a host app's serve
  mode + root dir + app folder (`source_dir`) + build/start commands, a
  git-backed app's repository/branch/subdirectory, a proxy app's upstream, and
  the `_/compose.yml` of any app that has one. A git app shows its repository
  fields *instead of* the app folder — its directory is the clone and cannot
  move, because a deploy resets it hard ([§9.9](#99-git-backed-apps)).
- **Identity** — not editable: id, project, slug, type, engine, `db_mode`,
  `wp_mode`. They name the app's containers, database and route shape, so
  changing one is a migration, not a setting. `store.UpdateApp` writes only the
  first list, which is where that rule is actually enforced. `source_dir` is on
  the editable side despite naming a directory: a host app's files are the
  user's own, so re-pointing copies, moves and deletes nothing.

Invariants worth keeping:
- **Everything is validated before anything is written** — hostnames, the
  compose body, the slot/domain pairing, the upstream — so a rejected save
  leaves the row, the file and the proxy exactly as they were
  (`TestUpdateRejectsWithoutChanging`). The handler re-renders the page with
  the submitted values, so a hand-edited compose file survives a rejection.
- **Ports don't move on an edit.** The app keeps its own port; an existing
  `${PORT_n}` slot keeps the port it was allocated even when its hostname
  changes, so an unchanged service stays where it was. Only a *new* slot
  allocates, and a removed one frees.
- Domains are rewritten with `store.ReplaceAppDomains` — primary and service
  rows in one transaction, because an edit can swap a hostname between the two
  roles and two separate replaces would trip the unique hostname index midway.
- A templated stack's compose file is editable too, but only structurally
  checked (`validCompose`): its ports were written in at create, so it is not
  held to the `${PORT_n}` contract. The replaced file is kept as
  `compose.yml.bak`.
- A db-backed app (`apps.UsesDB`: wordpress, laravel) gets a read-only
  **Database** section in *both* modes — shared is a choice that was made, not
  the absence of one. It names the database, the user, the server the app's
  containers reach (`xdev-db:3306` vs the stack's own `db:3306`), links shared
  ones to `/database/<name>`, and names the bundled Adminer and which server it
  opens on. Shared names come from `apps.SharedDBName`, the same function that
  provisions them; **dedicated names are duplicated from the type's compose
  template** (`internal/templates/files/{laravel,wordpress}/compose.yml.tmpl`)
  and have to be kept in step with it — see `appDB` in
  `handlers_app_settings.go`.
- Container **resource limits** live in the file's own
  `deploy.resources.limits` blocks — the settings page points there rather than
  offering fields that would drift from what the stack actually runs.
- The handler restarts the app (stop → start) when it was running: a host app
  that switched away from command mode has a process to retire, and a compose
  stack should come up from the new file.

### 9.6 Reverse proxy + TLS (`internal/proxy`)
- `Supervisor.Start` runs `caddy run` as a child with `CADDY_ADMIN` set, waits
  for the admin API, and stops it on shutdown. On a server you'd instead run
  Caddy under systemd and set `-caddy=false`.
- `Manager.Sync(routes)` builds the **entire** Caddy JSON config and `POST`s it
  to `/load`. One HTTP server (`xdev`) listens on the HTTPS port; Caddy
  auto-creates the HTTP→HTTPS redirect using `http_port`/`https_port`. Each route
  matches a host and `reverse_proxy`s to `127.0.0.1:<app port>`.
- **Proxy apps** take `http(s)://host[:port][/path]` (`validUpstream`). An
  `https` upstream gets a TLS transport, with SNI unless it's addressed by IP;
  the incoming `Host` header always passes through, so the other server's vhost
  and websockets keep working. A **path** means the app lives under a prefix
  there, and `reverseProxyHandlers` puts a `rewrite` to `<prefix>{uri}` in front
  of the proxy. That rewrites only what we *send* — an app answering with its own
  absolute links (`/webmail/app.js`) gets them prefixed twice, so it must be told
  its public base path, or be proxied at the prefix it already uses.
- **Serve-mode static apps** get `staticSiteHandlers`: `vars`(root) → a
  `try_files` subroute → `file_server`, i.e. Caddy's
  `try_files {path} {path}/ /index.html`. Real files win; a directory keeps its
  index; **anything else falls back to `/index.html`**, which is what makes a
  built SPA's deep links (`/dashboard/settings`) load instead of 404. Note the
  consequence: a genuinely missing asset returns `index.html` with **200**, not
  a 404. A site with no `index.html` matches nothing and gets file_server's 404.
- **`deploy/caddy-hostfs.sh`** applies the prefix + mount to an *existing*
  install (idempotent, `--dry-run`, `--revert`); new installs get it from
  `install.sh`. It exits without changing anything when Caddy isn't
  containerized, since a prefix would break paths that already work.
- **`deploy/caddy-public-ports.sh`** moves an install from the preview ports
  (8081/8444) onto 80/443 by rewriting three settings in `xdev.env` and
  restarting — the Caddy container is host-networked, so it rebinds from the
  pushed config and is never touched. It refuses to run while either port is
  occupied: a Caddy that cannot bind a listener drops the *whole* config, so a
  conflict costs every site, not the two ports being moved. Up on high ports
  Caddy could not answer ACME HTTP-01, so it holds almost no certificates; the
  script waits and reports each hostname as its certificate lands. Do not
  iterate on it — Let's Encrypt allows 5 certificates per name per week.
- **`XDEV_CADDY_ROOT_PREFIX` (`Manager.hostPath`)** — a containerized Caddy only
  opens what is bind-mounted into it, so a static root that is a real host path
  (`/home/li/ui/xyz`, or anything under `projects/`) resolves to nothing inside
  and every request 404s. The generated stacks mount `/` read-only at `/hostfs`
  and set this to `/hostfs`; every `file_server` root is then prefixed. Empty for
  a Caddy on the host. **Shared-WP docroots are deliberately exempt**: fpm opens
  that same path, so prefixing it would break `SCRIPT_FILENAME`.
- **TLS automation:** `internal` issuer (local CA) for local hosts; **ACME**
  issuer for public (prod) hosts (with `-acme-email` if set). Caddy's default
  storage holds the CA.
- **Cert lifetimes:** leaf default **90 days** (`-local-cert-lifetime`, `2160h`);
  intermediate **1 year**; root **~10 years**. A leaf can't outlive its
  intermediate, so on startup `RefreshStaleIntermediate("local", leafLifetime)`
  deletes a too-short intermediate (keeping the root → **no re-trust**) so Caddy
  mints a fresh long-lived one. `caddyDataDir()` mirrors Caddy's storage path
  (honors `XDG_DATA_HOME`; macOS `~/Library/Application Support/Caddy`).

### 9.7 Domains & hosts (`internal/domains`, `internal/platform`)
- **An app's Domain field is a list.** One text input, comma-separated (spaces,
  semicolons and newlines separate too) — `apps.parseHosts` normalizes, validates
  and de-duplicates it, keeping order. The **first** name is the app's own
  domain (`apps.subdomain`, what the UI links to, what service hostnames are
  checked against); the rest are equals, not aliases — each is its own
  `domains` row, its own Caddy route, its own certificate, serving the same
  thing. Removing one from the field is how it stops routing, since
  `ReplaceAppDomains` rewrites the whole set rather than merging into it.
  Every name in the list is checked for collisions *before* anything is written,
  so a list containing one taken hostname fails whole.
  Service domains (Adminer, a compose slot) stay **one hostname each**: a slot's
  position is what maps it to `${PORT_n}`, and a list there would blur how many
  slots exist.
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

### 9.8 Metrics (`internal/metrics`)
- `Collector.Run` ticks every 10s: reads `<engine> stats --no-stream` for **all
  usable engines**, attributes containers to apps by name prefix
  (`<project>_<app>_`), aggregates cpu%/mem per app, inserts into `metrics`, and
  prunes >24h.
- Per-app chart page (`/apps/{id}/metrics`) uses uPlot fed by
  `/apps/{id}/metrics.json` (arrays `t`/`cpu`/`mem`).
- `HostSnapshot()` (gopsutil) powers the dashboard host card (CPU/mem/disk).

### 9.9 Git-backed apps (`internal/gitsrc`, `internal/apps/gitapp.go`)

A host app (static, go) can take its code from a repository instead of a
scaffold or a folder you already have. xdev clones it into the app's managed
directory, builds it there, and serves the build output; **Deploy** is `fetch` +
`reset --hard` + build.

- **Three sources, one choice.** `source_dir` (your folder), `git_url` (a
  clone), or neither (xdev scaffolds a starter). `git_url` + `source_dir`
  together is refused at create *and* at edit, because a deploy resets the
  working tree hard and xdev's standing promise about a folder of your own is
  that it never rewrites it. Attaching a repository to an app that already
  exists is refused for the same reason: making room for a clone would mean
  deleting files, and the settings page deletes nothing.
- **The repository is the app.** `Update` discards local modifications, so
  editing a file on the server is not a way to change a git app — it is
  something the next deploy silently undoes. Untracked files are *kept*, so
  `node_modules/` and the previous build survive to make the next build faster.
- **Defaults come from the repo.** After the clone, `detectNodeBuild` reads
  `package.json`: no `scripts.build` means no build command at all (a plain HTML
  repo just gets served), the lockfile picks the package manager (`npm ci`,
  `pnpm install --frozen-lockfile`, `yarn`, `bun` — and `npm install` when there
  is no lockfile, since `npm ci` fails without one), and the framework in
  `devDependencies` guesses the output directory (`dist`, `build` for CRA,
  `.output/public` for Nuxt). All of it is only a default: anything typed in the
  form wins, and the settings page can change it afterwards.
- **`root_dir` stays relative to the app directory**, so a monorepo's output is
  `<git_subdir>/<outdir>` — the route builder joins `root_dir` to the app dir
  and knows nothing about repositories. `git_subdir` moves only where the
  *build* runs (`Service.buildDir`); the `.env`, the logs and the backups still
  belong to the app directory.
- **Private repositories use a per-app SSH deploy key.** GitHub allows a deploy
  key on exactly one repository per account, so these cannot be shared between
  apps. The key is generated **before** the app exists (`POST /deploy-keys`
  returns an unbound row) because the clone happens during create, and a private
  repo whose key is not yet on GitHub is indistinguishable from one that does
  not exist — `cloneAdvice` turns git's "Repository not found" into that
  sentence. The key binds to the app once the row is written, and is deleted
  with the app.
- **The private half is encrypted at rest** (`internal/secrets`, AES-256-GCM,
  key at `<data-dir>/secret.key`, 0600, created on first use). This protects a
  database that leaves the host — a backup, an scp — and explicitly **not**
  against someone who is already root here, since the key file sits beside the
  database. A wrong-sized key file stops startup rather than being replaced:
  regenerating it would make every stored secret permanently unreadable.
- **Credentials never persist.** The key is written to a 0600 file in a temp
  directory that is removed before the call returns, and passed via
  `GIT_SSH_COMMAND` — never into `.git/config`, a remote URL, or `git remote -v`
  output. `ParseRepo` rejects a URL carrying credentials for the same reason: a
  token in a field that is displayed back and stored in the clear is the mistake
  the design exists to prevent.
- **GitHub's SSH host key is pinned** (`githubHostKey`, verified against the
  published fingerprint) so the first clone is checked rather than trusted
  blindly. Other hosts are `accept-new` into a throwaway known_hosts, which
  means a non-GitHub host is effectively trust-on-first-use *every* time —
  a real limitation, and the reason to pin more hosts if xdev ever targets them.
- **The served root refuses `.git` and `.env`** (`staticSiteHandlers`). A git
  app's root *is* a checkout, so without that refusal `/.git/config` is a
  download and the repository's history goes with it. The matcher runs before
  the try_files rewrite — after it, the rewrite would have found the real file
  already — and answers 404 rather than 403, which tells a scanner less.
- **A deploy runs in the background** (`Service.DeployAsync`, §9.10) and reports
  through the `deployments` table. Only the first clone, at create, is
  synchronous — there is no app to show progress against yet.

### 9.10 Deploying from outside the UI (`internal/apps/deploy.go`, `internal/server/handlers_deploy.go`)

Two ways in, for the two places a build can happen:

- **Pull** — GitHub posts a push event, xdev fetches and builds *on this server*
  (`store.DeployWebhook`). Nothing to configure in the repository beyond a URL
  and a secret.
- **Push** — CI builds and uploads a `.tar.gz` of the finished site, xdev only
  unpacks it (`store.DeployPush`). For builds that need private packages, a warm
  cache, or a toolchain you would rather not install on the server.

Both share the deploy record, the concurrency rule, and the endpoint mechanism.

**Where the endpoints live.** On the app's *own* hostname, under `/_xdev/`
(`store.HookPathPrefix`, `store.PushPath`). That hostname already resolves and
already has a certificate, so there is nothing extra to point at the server —
and Caddy is given a **path-scoped** route (`proxy.Route.Paths`) that forwards
only those paths to the control plane. `platform.Reconciler.endpointRoutes`
emits them **before** the app's own route, because Caddy takes the first match
and a host-wide route would otherwise swallow `/_xdev/` too. An app with neither
endpoint enabled gets no such route: the control plane is reachable from the
internet only where somebody switched it on, and then only at a path that
answers nothing without a signature or a token.

**How each proves itself.** The webhook is HMAC-SHA256 over the *raw* body
(`X-Hub-Signature-256`) — re-serializing the JSON would change the bytes and
every delivery would fail. The push endpoint takes a bearer token compared
against a stored SHA-256; the token itself is shown once and never written down,
so a copy of the database cannot deploy. Both comparisons are constant-time.
Neither handler trusts the URL for anything but finding the app, and the push
endpoint identifies the app by **Host header**, so a token cannot be used
against a hostname that is not its app's.

**What the webhook ignores** rather than treats as an error, because a red
delivery in GitHub's list should mean something is actually wrong: a `ping`, an
event other than `push`, a push to a branch this app does not track, a branch
deletion, and a push arriving while a deploy is already running.

**One deploy at a time per app** (`apps.inflight`, `ErrDeployInProgress`). Two
builds writing one output directory produce a mixture of both. A second trigger
is refused rather than queued — queueing would mean a burst of five pushes
builds five times to reach the state the last one describes. In-flight rows are
reaped at startup (`ReapRunningDeployments`): a deploy is a goroutine, so xdev
stopping mid-build would otherwise leave a row claiming to run forever, blocking
every later deploy.

**An uploaded build is swapped in, not merged into.** `PushDeploy` unpacks to a
staging directory beside the target, then renames — so the site is never half of
two builds, and a corrupt or empty archive leaves the previous build serving.
The rules about what may be replaced are in `pushTarget`, and they are the same
promises as everywhere else: never a folder of the user's own, and never the
directory holding a git checkout (set a *Folder to serve* first, so an upload
replaces the build output rather than `.git`).

### 9.11 Laravel from a repository (`internal/apps/laravel.go`, `internal/apps/actions.go`)

A Laravel app can be deployed from a repository like a static or Go one, but
almost none of the machinery is shared past the `git fetch`.

**The checkout is `app/`, not the app directory** (`Service.codeDir`). The app
directory also holds what xdev generates (`_/compose.yml`, `_/laravel.env`) and
what it keeps (`_volumes/`), none of which a deploy may touch. That boundary
already existed for the bind mount; making it the checkout root is what lets the
ordinary deploy path work here unchanged. `deployNow` resolves it through
`codeDir` for every app type.

**State is mounted in from outside the checkout.** A deploy is `git reset
--hard`, so anything inside `app/` is disposable by definition:

	_volumes/storage/  → /var/www/html/storage   uploads, sessions, cache, logs
	_/laravel.env      → /var/www/html/.env      app key, DB credentials

Both are gitignored in a stock Laravel repo, so before this they survived a
reset only by luck — and a re-clone or a pushed build would still have taken
them. `writeLaravelEnv` generates `APP_KEY` once at create and never rewrites
it: rotating it invalidates every encrypted column, signed URL and session the
app has issued.

**The build runs in a container, but not always the app's own**
(`Service.deployContainer`, `composerInstall`). The host has no PHP and the
versions that matter are the image's — but the *production* Swoole image is
runtime-only and ships no composer, and a fresh clone has no `vendor/` at all
(repositories gitignore it), so the production image cannot even boot until
dependencies exist. Requiring a live container to install them would deadlock.
So `composerInstall` probes the running container for composer and falls back to
a one-off `composerToolchainImage` container bind-mounting the same checkout.

That image is the official `composer:2`, and the reason is architecture: the
Swoole family is published as single-arch images, `-prod` for amd64 and `-dev`
for arm64, so the dev image cannot be borrowed as a toolchain on an amd64 host
("exec format error"). The app's own image family is therefore not something a
deploy can rely on. Building outside the app's image costs two flags:
`--ignore-platform-reqs`, because the toolchain has no ext-swoole and would
refuse a lockfile that installs fine (safe only because `composer.lock` already
exists — this unpacks an already-resolved set rather than choosing one, and a
genuine PHP-version mismatch now surfaces at boot instead of here); and
`--no-scripts`, because Laravel's post-autoload-dump hook boots the framework.
`deployContainer` runs that hook — `artisan package:discover` — afterwards, or
packages would be on disk but unregistered.

**Every artisan step runs in a one-off container** (`compose run --rm
--entrypoint php app artisan …`), not `compose exec`. A deploy exists to make an
app healthy, so it must not require the app to already be healthy: the container
being fixed is usually crash-looping on exactly the state the deploy is fixing,
and exec'ing into it fails with "Container … is restarting". A one-off container
has the same image, mounts, environment and networks, and writes to the same
bind-mounted checkout. `exec` is used for one thing only — `octane:reload`,
which by definition needs a live server — and its failure is not fatal: it means
there was no healthy server to talk to, which the `up -d` that follows
addresses. If the app still will not stay up, the deploy attaches the last 40
lines of container logs to the failure, because that is the one failure whose
cause is never in any earlier step. Order: `composer install --no-dev` → migrations → clear+rebuild config
/route/view caches → `octane:reload`. Migrations come *before* the caches so a
failed one leaves the caches describing the code that still works; the reload is
last because Swoole serves the old code from memory until told otherwise — a
deploy that skipped it would report success while serving the previous release.

**Migrations are dumped before they are applied, and never auto-restored.**
`dumpBeforeMigrate` writes into the app's backups directory (`SetBackupsRoot`,
wired in `main.go`), and a dump that was wanted but failed *stops the deploy* —
running an irreversible change with no way back is worse than not deploying.
On failure the deployment records the artisan output and points at the dump.
Restoring stays a human act: rows written between the dump and the failure are
real, and an automatic restore would discard them to fix a problem nobody has
looked at yet. `apps.skip_db_dump` (migration 0013) opts out per app, for a
database too large to snapshot every time; the settings page states plainly what
that costs.

**`init.sh` does nothing to a checkout.** A git app that is missing
`laravel/octane` or `config/octane.php` fails with a message saying to commit
them, rather than running `composer require` — which would edit `composer.json`
inside the checkout and be reverted by the next deploy, forever. It also skips
the boot-time `migrate`, since migrating is the deploy's job and a plain restart
must not apply a schema change with no dump taken.

**Maintenance commands are an allowlist, not a command box**
(`internal/apps/actions.go`). An arbitrary-command endpoint in the web UI is a
root shell in the container, reachable from the internet behind a session
cookie. `RunContainerAction` resolves a *key* against a fixed list, so no
request can name a command that is not in it; adding one is a code change.
`migrate` from a button takes the same dump the deploy does.

**Only some types can be git-backed** (`canDeployFromGit`): static, go, laravel.
WordPress and bring-your-own compose have no deploy path a repository could
drive, so attaching one is refused at create rather than cloned and ignored.

**The bundled Adminer is optional and switchable** (`internal/apps/adminer.go`).
Adminer is a database client on a public hostname: wanted while an app is being
built, unwanted once it is live. The add-app form asks (`CreateOpts.NoAdminer`,
default off = include it) and the settings page switches it either way
afterwards (`SetAdminer`).

There is **no column** for it. "Has Adminer" is already written in the two
places that must agree for it to work — the `adminer` service in the compose
file, and the service-domain row routing a hostname to its port — so the domain
row *is* the answer, and `SetAdminer` keeps the file in step with it. A third
copy in `apps` could only ever disagree with them.

The compose file is **edited, not re-rendered**. A Laravel stack's file is
generated, but the settings page also lets it be hand-edited, and re-rendering
would silently discard those edits on every toggle. `composeRemoveService` /
`composeAddService` work on lines, not on a parsed document: xdev has no YAML
library, and round-tripping through one would rewrite comments, quoting and key
order to change one service. The two directions are tested as exact inverses of
the rendered file (`adminer_test.go`), and the previous file is kept as
`compose.yml.bak` the same way a hand edit keeps one.

Turning it off removes the container *before* rewriting the file: once the
service is gone from the compose file neither `down` nor `up` knows the
container belongs to the stack. Best-effort — a stopped stack has nothing to
remove, and a leftover container is not a reason to refuse the setting.

### 9.12 Port-only installs (`internal/apps/address.go`)

Some servers have no domain — a bare IP, or a box already running another web
server on 80/443 that should keep them. `XDEV_PUBLIC_HOST` says how the machine
is reached; setting it is what makes the mode exist. Empty (every normal
install) changes nothing at all.

`apps.Reach` holds the three facts an address needs — public host, whether the
proxy is live, the HTTPS port — and `Address(app)` is the only thing that
answers where an app is. A routed hostname wins; otherwise the published port;
otherwise "" (a serve-mode static app with no domain genuinely has nowhere to be
reached). `PortAddress` is the port answer alone, shown beside a hostname so a
stack that is up but not yet routed can still be opened.

Two copies exist by design: the apps service's (`SetReach`, from config, with
`ProxyEnabled: true` — a hostname only exists when xdev means to route it) and
the server's (`s.reach()`, with the *live* proxy state, because the UI also has
to describe an install whose Caddy is not answering).

`PortOnly()` is what makes the Domain field optional in `Create` and `Update`:
with a public host, blank is a decision, and inventing `<slug>.<base-domain>`
would produce a dead link, a route Caddy cannot serve, and a wrong `APP_URL`.
Without one, blank still defaults from the project's base domain exactly as
before.

Limits, all deliberate and tested:

- **Serve-mode static apps** publish no port, so they still need a domain
  (`TestServeModeStaticHasNoPortToBeReachedBy`).
- **A compose stack's extra slots** still need hostnames: the slot's port is
  stored on its domain row (`domains.hostname` is `NOT NULL UNIQUE`), so a
  nameless slot has nowhere to keep its port. Lifting this needs a ports table,
  i.e. a migration.
- **Deploy endpoints** are path-scoped routes on the app's hostname, so a
  port-only app has none. The app's own port reaches the *app*, not the control
  plane, so the settings page shows the path and the loopback URL rather than
  inventing a public one (`deployInfo.Unrouted`).

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
- **Domains go on last.** `Create` starts the stack and runs the first build
  *before* writing any `domains` row. A domain row is live routing: the next
  reconcile points Caddy at the app and, in a prod project, asks Let's Encrypt
  for a certificate — so publishing first would leave a failed create with a
  hostname serving 502 and an ACME issuance spent on it. The app row itself
  survives a failed start (the UI shows it in an error state); the hostname
  stays unclaimed. Guarded by
  `TestCreateDoesNotPublishDomainsWhenStartFails`.
- **A poll must not undo what the user just did.** Both self-refreshing views —
  the deploy list (replaces its HTML every 3s) and the create dialog's step list
  (re-renders every ~1s) — used to reset every `<details>` on each tick, so a
  panel opened to read a slow build closed itself a second later. Expanded state
  is now held outside the markup that gets replaced: `deployWatch.openKeys()` /
  `restore()` keyed on `data-deploy`/`data-step`, and `openSteps` in the add-app
  dialog. Anything else that re-renders on a timer owes the same. Guarded by
  `TestDeployPollRestoresExpandedEntries` and `TestCreateStepsStayOpenAcrossPolls`.
- **Icon-only controls carry two labels.** `data-tip` is a CSS `::after` and
  reaches no screen reader, so every `.iconbtn` also needs an `aria-label` —
  without it the control announces as "button". Guarded by
  `TestRefreshAndClearAreLabelledIconButtons`. A control at the top of a
  scrolling container (the Deploys card heads the sticky side column, which is
  `overflow-y: auto`) needs `.tip-below`, or its label is clipped and never
  appears at all.
- **`data-confirm` works on any form** (and on any submit button), via one
  capture-phase listener in `layout.html` registered *before* the others, which
  check `defaultPrevented`. It used to be read only inside the
  start/stop/delete handler, which returns early for every other action — so 13
  of 15 confirmations in the app silently never appeared, including "Run
  migrate? This changes the database". Guarded by `TestConfirmWorksOnAnyForm`.
- **Clearing a log never clears live state.** `ClearDeployments` keeps a
  *running* row: the goroutine building right now finishes into it, and its
  presence is how a second deploy is refused. `ClearLogs` covers host-process
  apps only — xdev owns that file — and `Start` opens it `O_APPEND` so a
  truncation underneath a live writer leaves an empty file rather than a hole of
  NULs (`TestClearLogsWhileRunning`). A container app's output belongs to the
  engine's logging driver, so there is no button for it.
- **One place answers "where is this app".** `apps.Reach.Address` — a routed
  hostname if there is one, else the published port, else "". It was assumed in
  three places at once (the card's link, the deploy endpoints, and a Laravel
  app's `APP_URL`), and on a domainless install all three were wrong. Only the
  last one fails quietly, which is why it is the one that mattered: Laravel
  builds signed URLs and reset links from `APP_URL`. Anything that needs to
  render an app's address asks `Reach`, never `app.Domain` plus a scheme.
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
| `-public-host` | `XDEV_PUBLIC_HOST` | "" | address this server is reached at when an app has no domain; enables port-only apps (§9.12) |
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
