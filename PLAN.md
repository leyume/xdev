# xdev — Plan

A lean, self-hosted control plane for developing and deploying containerized
apps. Runs on macOS (dev) and Ubuntu server (prod). One Go binary that manages
containers via podman/docker, with a web UI to create projects, add/start/stop
apps, attach domains + SSL, set resource limits, and watch live metrics.

Think: a minimal, focused mix of Coolify / CapRover / Laravel Herd — tuned to
the way `bizepp` is already structured.

---

## 1. Core concepts — Projects contain Apps

A **two-level hierarchy** (this is the key model):

```
Project "bizepp"   ──┬── App: backend    (Laravel)     → api.bizepp.test
  shared network     ├── App: frontend   (Vue static)  → bizepp.test
  base domain        └── App: wordpress  (optional)    → blog.bizepp.test
```

- **Project** — a top-level group. Owns its own directory, a **shared private
  container network** (so the frontend can reach the backend by name), and a
  **base domain** (`bizepp.test` locally, `bizepp.com` in prod). Lists in the
  dashboard.
- **App (component)** — one deployable unit inside a project (backend /
  frontend / wordpress / static). Has its **own compose file, own lifecycle
  (start/stop independently), own resource limits, own subdomain**.

`bizepp` is the canonical example: one project = a Laravel `api/` backend + a
Vue `ui/` frontend.

---

## 2. Stack

| Concern | Choice | Why |
|---|---|---|
| Language | **Go 1.23+, stdlib-first, well-commented** | Single static binary, low RAM, trivial Ubuntu deploy. Minimal deps, `net/http` routing, no heavy framework |
| Database | **SQLite via `modernc.org/sqlite`** (pure-Go, no CGO) | Cross-compiles mac↔linux with no C toolchain. `database/sql` + embedded `.sql` migrations |
| Frontend | **Go html/template + htmx + Alpine.js**, charts via **uPlot** | No JS build step; server-rendered, leanest, embedded in the binary |
| Container control | **Shell out to `docker compose` / `podman compose`** for lifecycle; socket/CLI `stats` for metrics | Mirrors bizepp's Makefile; compose is the deploy unit; readable |
| Reverse proxy / SSL | **Caddy**, driven by its **admin API** (`localhost:2019`) | One tool for local (`tls internal`) + prod (auto Let's Encrypt); live reconfig |
| Process model | One `xdev` binary = web UI + API + CLI subcommands | Same binary serves the UI and offers `xdev project add ...` for scripting |

Detected on this machine: podman ✔, docker ✔, `docker compose`/`podman compose`
v5.1.3 ✔, Homebrew (arm64) ✔. Go installed via Homebrew.

---

## 3. Repo structure

```
xdev/
  cmd/xdev/main.go          # entrypoint + CLI subcommands
  internal/
    server/                 # http server, routing, middleware, handlers
    auth/                   # single-admin login, sessions, CSRF
    projects/               # project CRUD + per-project network/domain
    apps/                   # app CRUD + lifecycle orchestration
    templates/              # render compose/Dockerfile per app type
    runtime/                # podman|docker driver (compose CLI + stats)
    proxy/                  # Caddy admin-API client, route sync
    domains/                # .test domains, /etc/hosts, cert trust
    metrics/                # stats collector → time-series + rollups
    store/                  # sqlite open, migrations, queries
    config/                 # config + paths
  web/
    templates/              # html/template files (embedded)
    static/                 # htmx, alpine, uPlot, css (embedded)
  migrations/*.sql          # embedded schema migrations
  apptemplates/             # embedded compose/Dockerfile templates per type
  projects/                 # RUNTIME: generated per-project dirs (gitignored)
    <project>/
      <app>/
        _/compose.yml
        _/compose.prod.yml
        app/                # bind-mounted app code
        _volumes/           # persistent data
```

The generated `projects/<project>/<app>/_/` layout intentionally mirrors
`bizepp/api/_/`.

---

## 4. Data model (SQLite)

- **users** — id, email, password_hash, created_at  *(single admin in v1)*
- **sessions** — token, user_id, expires_at
- **settings** — key, value  *(default runtime, hosts-mgmt on/off, base TLD, ...)*
- **projects** — id, name, slug, base_domain, environment (local|prod),
  network_name, dir, created_at
- **apps** — id, project_id (FK), name, slug, type
  (wordpress|laravel|static-prebuilt|static-build), runtime (podman|docker),
  status, subdomain, cpu_limit, mem_limit, port, compose_path, created_at, updated_at
- **app_env** — app_id, key, value  *(written to the app's .env)*
- **domains** — id, app_id (FK), hostname, is_local, ssl_mode
  (internal|letsencrypt), cert_status
- **metrics** — id, app_id, ts, cpu_pct, mem_bytes, mem_limit, net_rx, net_tx
  *(raw 24h + hourly rollups, auto-pruned)*
- **events** — id, project_id, app_id, ts, level, message  *(audit log)*

---

## 5. App templates (modeled on bizepp's `_/`)

Each template renders into `projects/<project>/<app>/` with `_/compose.yml`
(dev), `_/compose.prod.yml` (prod), bind-mounted `app/`, and `_volumes/`.
Conventions carried over from bizepp: prefixed service names, `COMPOSE_PROJECT_NAME`
namespacing, healthchecks, `logging` rotation, prod resource limits.

1. **static-prebuilt** *(built first — simplest)* — Caddy serves a `dist/`
   folder. May be served directly by the main Caddy with no container.
2. **static-build** — node container runs Vite dev server (HMR) *or* builds
   `dist/` then serves it. Covers "before and after build".
3. **wordpress** — wordpress + mariadb (+ optional redis).
4. **laravel** — ports bizepp's Swoole/Octane image (app + db + redis +
   adminer) with the dev/prod split.

Templates live in `apptemplates/` and are extensible — drop in a new template
dir and it appears in the UI.

---

## 6. Networking & SSL

- **Per-project shared network**: all of a project's app containers + Caddy
  join the project network, so apps reach each other by service name (no host
  port juggling). Caddy also sits on a global proxy network.
- **Local**: TLD **`.test`** (reserved, safe — unlike `.dev`/`.local`). On app
  create → add `127.0.0.1 <sub>.<project>.test` to `/etc/hosts`; Caddy serves
  HTTPS via `tls internal`; run `caddy trust` once so browsers trust the root CA.
- **Production**: point a real domain at the server → Caddy auto-provisions
  Let's Encrypt (needs :80/:443 + DNS A record). xdev registers the route via
  the admin API.

Privileged actions (editing `/etc/hosts`, `caddy trust`) are isolated to a
small helper / documented sudo step.

---

## 7. Resources & monitoring

- **Limits**: UI sliders write `deploy.resources.limits.{cpus,memory}` (+
  `reservations`) into the generated compose, then `up -d` re-applies. A
  guardrail warns if the sum of limits exceeds host capacity.
- **Monitoring**: a collector goroutine polls `docker/podman stats` per running
  app into `metrics` (raw 24h + hourly rollups, auto-pruned). Charts via uPlot.
  Host-level CPU/RAM/disk overview via `gopsutil`.

---

## 8. Security (v1)

Single admin: argon2/bcrypt password, secure session cookie (HttpOnly,
SameSite), CSRF on mutations. Admin UI binds to localhost by default; on a
server it sits behind Caddy on its own domain. Schema is designed to grow into
multi-user later.

---

## 9. Roadmap

| Phase | Delivers |
|---|---|
| **0 — Foundations** | Go skeleton, SQLite+migrations, admin login+sessions, dashboard shell (Projects list, empty state), runtime detection |
| **1 — Projects & Apps core** | Project + App CRUD, template engine, compose up/down/start/stop, manage from UI — **static-prebuilt end-to-end** |
| **2 — Proxy + local SSL** | Caddy admin API, per-project network, `.test` subdomains + hosts file, `tls internal` + trust → `https://app.project.test` |
| **3 — App templates** | WordPress, Laravel (port bizepp), static-build (Vite before/after) |
| **4 — Production** | Live domains + Let's Encrypt, prod compose, deploy flow, systemd unit + docker default |
| **5 — Resources + charts** | CPU/RAM limit controls, stats collector, charts, host overview |
| **6 — Polish** | Live log streaming, per-app `.env` editor, volume/db backups, audit log |

---

## 10. Extra ideas (backlog)

**Done:** ✅ One-command installer (`deploy/install.sh`, Linux + macOS) + systemd
unit / launchd plist · `version`/`doctor`/`create-admin` subcommands · full
env-file (`XDEV_*`) config · GitHub-Actions-on-tag → Releases distribution (see
`PLAN_UPDATE.md`).

**Remaining:** full CLI parity for every UI action · live container log
streaming (websocket) · scheduled volume/db backups · per-app `.env` editor ·
dry-run preview of generated compose before applying · git-based deploy ·
health/auto-restart surfaced in UI.
