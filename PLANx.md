# xdev — Design Brief

For a design pass on the web UI. This summarizes what the product is, who
uses it, how the screens connect, and what the current UI looks like — so a
designer (human or Claude) can propose a modern visual direction without
reading the Go source.

> **Status (2026-07-12):** the "Nova" violet/glass redesign in
> `design/PLAN_UI.md` has been implemented in the real templates (branch
> `feat/nova-ui-redesign`), including a Dashboard/Projects route split and a
> topbar↔sidebar toggle. The follow-up backlog below is now **mostly done** —
> items 1–7 shipped (real host-history chart, Containers/Certificates/Backups
> KPIs, inline Logs/Env/Backups tabs, global search, responsive pass, nav
> icons); only item 8 (per-user nav scope) is deliberately left as YAGNI. The
> rest of this doc (stack, data model, style baseline) is still accurate
> background reading.

---

## What it is

A self-hosted PaaS control panel — one Go binary, no cloud dependency. A
single admin uses it to spin up **Projects**, each containing one or more
**Apps** (Laravel, WordPress, or static sites), point subdomains at them with
auto-SSL (via Caddy), watch live CPU/RAM charts, tail logs, edit env vars, and
trigger backups. Think a lightweight, personal Coolify/CapRover.

Runs on macOS in dev and Ubuntu in prod. Rendered server-side (Go
`html/template` + Alpine.js for client-side state — no JS build step, no SPA
framework). Every "page" is a real URL; mutations are plain form POSTs with a
redirect back (htmx was in the original plan but the app never actually
adopted it — Alpine covers the client-state needs so far, e.g. per-app tab
switching on the Project detail page).

## Who uses it

One person: the developer/operator themselves (single-admin in v1, admins
page exists for adding more). Session-based login, everything behind auth
except `/login` and first-run `/setup`.

---

## The core mental model

```
Project  (e.g. "bizepp")
 ├─ shared docker/podman network + base domain (bizepp.test / bizepp.com)
 ├─ App: backend   (laravel)   → api.bizepp.test
 ├─ App: frontend  (static)    → bizepp.test
 └─ App: wordpress (optional)  → blog.bizepp.test
```

Every App has: a type (static / laravel / wordpress), a runtime status
(running/stopped), a subdomain, resource limits, env vars, logs, metrics
history, and optional backups. This Project → App nesting is the one
relationship the whole IA hangs off — almost every screen is either "list of
projects," "one project's apps," or "one app's detail."

---

## Screen map / navigation (current, post-Nova-redesign)

```
/setup                     first-run: create the single admin account
/login                     session login

/                          Dashboard (home) — KPI strip, host resources + utilization
                            chart, cross-project Apps table, recent events, engine card
/projects                  Projects — host resources + engine, one card per project
                            (with its apps listed inline), recent events
/projects/new              New Project form
/projects/{slug}           Project detail — per-app cards with inline Metrics/Logs/
                              Env/Backups tabs, Add-app form, project activity,
                              danger zone (2.1fr/1fr layout)

/apps/{id}/metrics         Full-page live CPU/RAM chart (uPlot) — still linked from
                            an app card's Logs/Env/Backups tabs as the "open full ↗"
/apps/{id}/logs            Full-page log viewer  (same — dummy preview links here)
/apps/{id}/env             Full-page .env editor (same — dummy preview links here)
/apps/{id}/backups         Full-page backup list (same — dummy preview links here)

/events                    Global audit log
/admins                    Manage admin accounts

POST /settings/engine      Switch default container engine (redirects back to caller)
```

Nav chrome is a single global preference (topbar or sidebar). The switch is now
**client-side** (Alpine): the sidebar is a fixed slide-in drawer and the topbar
toggles via `x-show`, so flipping between them animates with **no page reload**.
The choice is written to the `xdev_nav` cookie in JS and read back by the server
on the next hard load (to render the right initial chrome). Both modes render
identical page content.

## Data → UI mapping (what each screen is really showing)

| Entity | Screen(s) | Real or placeholder |
|---|---|---|
| **Project** (name, base_domain, network) | Dashboard KPIs, Projects cards, Project detail header | Real |
| **App** (type, status, subdomain, limits) | Dashboard apps table, Project cards, Project detail app cards | Real |
| **Host** (CPU/mem/disk, hostname, uptime) | Host resources card (Dashboard + Projects) | Real |
| **Metrics** (cpu/mem time-series per app) | Metrics tab on each app card (uPlot, fetches `/apps/{id}/metrics.json`) | Real |
| **Events** (audit trail) | `/events`, Recent events / Project activity cards | Real |
| **Host utilization over time** | Dashboard chart | Real — `host_metrics` table sampled by the collector, live uPlot chart via `/host/metrics.json` |
| **Containers / Certificates / Backups counts** | Dashboard KPI strip | Real — running-container count (`<engine> ps`), TLS-domain count, on-disk backup scan; each degrades to "—" when unavailable |
| **Logs / Env / Backups tab content** | Project detail app card tabs | Real — lazy fetch-on-open `/apps/{id}/{logs,env,backups}/partial` fragments (+ still linking the full page) |
| **Search** | Topbar search box | Real — debounced `/search?q=` across project/app names, ⌘K dropdown |

---

## Current visual style ("Nova" — implemented)

- Dark theme only, near-black violet-tinted background (`#0d0b12`) with a
  radial accent wash, glass-panel cards (`rgba(255,255,255,0.025)` fill +
  `backdrop-filter: blur(16px)`).
- Accent violet (`#b48cff`), green/amber/red for running/warn/danger status.
- 8px/10px radii, hairline dividers, a CSS-drawn conic-gradient logo mark (no
  image asset), avatar-initials badge.
- System font stack (not the mock's Google-Fonts DM Sans — kept the UI
  dependency-free/offline-friendly), ~14.5px base.
- Charts are uPlot throughout — per-app Metrics and the Dashboard host
  utilization chart (both real, the latter fed by `/host/metrics.json`).
- Styling is **Tailwind CSS v4** via the *vendored browser build*
  (`web/static/tailwind.js`, `@tailwindcss/browser`) — compiles utilities in the
  browser at runtime, so the app stays build-step-free and offline. The Nova
  design system (palette `@theme` tokens + component classes) lives in
  `web/templates/twstyles.html`, inlined into every page as
  `<style type="text/tailwindcss">`. Alpine still drives client state (nav
  switch, per-app tabs, search, realtime toggle, add-app modal); a shared
  `partials.html` holds the engine/host-resources/events fragments.

## Feature backlog from the Nova redesign

The redesign implemented the full IA and visual system from `design/PLAN_UI.md`
using real data everywhere it already existed, and clearly-marked
placeholders where it didn't. Items 1–7 have since been implemented; item 8 is
deliberately deferred (YAGNI for a single admin).

1. ✅ **Host utilization history.** `host_metrics(ts, cpu_pct, mem_pct)` table
   (migration `0006`), sampled each tick by the existing metrics collector via
   `HostSnapshot()`; served by `/host/metrics.json?range=1h|24h|7d`; the
   Dashboard chart is now a live uPlot with working range pills. *Known ceiling:*
   history is capped by the 24h metrics retention, so the 7d pill only shows
   what exists — extend retention if 7d history matters.
2. ✅ **Containers KPI.** Real running-container count via `<engine> ps -q`
   summed across usable engines (`runtime.Selector.RunningContainers`); shows
   "—" when no engine is usable. *Known ceiling:* counts all host containers,
   not only xdev-managed ones — add a name/label filter to scope it.
3. ✅ **Certificate tracking.** KPI shows the count of TLS-enabled domains
   (`domains.ssl_mode != ''`) when the proxy is up, "—" otherwise. Left the
   lazy real number; issued/expiry dates + a "renews in N days" warning (Caddy
   admin API or a `cert_status`/`expires_at` column) are still open if wanted.
4. ✅ **Backup history.** Aggregate scan of the backups directory (count +
   newest modtime → "last run"); no DB table needed. Shows "none yet" when empty.
5. ✅ **Inline Logs/Env/Backups tabs.** Lazy fetch-on-open partial endpoints
   `/apps/{id}/{logs,env,backups}/partial` (escaped fragments) replace the
   sample previews; the "open full ↗" links to the full pages remain. Backups
   panel is list-only (actions stay on the full page).
6. ✅ **Global search.** `/search?q=` filters project/app names+slugs in-Go
   (JSON results, capped at 10); topbar box is a debounced Alpine input with a
   results dropdown and a ⌘K/Ctrl-K focus shortcut.
7. ✅ **Responsive pass.** CSS-only `@media (max-width:760px)` block stacks the
   sidebar to a top strip and lets the topbar wrap; desktop untouched.
8. ⬜ **Nav-layout preference scope.** Topbar-vs-sidebar is still a per-browser
   cookie, not tied to the admin account — fine for a single admin. Move it into
   the `settings`/user record only if multi-admin, multi-device use matters.

Not in the original list, shipped alongside: **inline-SVG icons** on the nav
links and Log-out button in both chromes (dependency-free, no icon font).

Constraint that stays true for all of the above: server-rendered HTML +
Alpine for client state (no SPA build step) — keep new features to plain
fetch-and-swap patterns like the Metrics tab already uses.

## UI refinements — round 2 (2026-07-12)

Post-review polish on the shipped Nova UI. All CSS/template-level except
where noted; same server-rendered + Alpine constraint.

1. **Fix host-utilization ranges.** The 1h/24h/7d pills didn't visibly change
   the chart — host metrics were pruned at 24h (so 7d never had data) and the
   chart double-initialised (`x-init` + Alpine's implicit `init()`). Give host
   metrics their own 7-day retention and load once.
2. **Topbar log-out is icon-only.** Drop the "Log out" text on the topbar
   (keep the icon + an aria-label).
3. **Topbar sidebar-toggle is an icon.** Replace the "Sidebar" text button
   with a panel icon.
4. **Sidebar nav links are white.** In the sidebar chrome the links read as
   the default link colour; make them `--text` with proper hover/active.
5. **Sidebar footer sinks to the bottom.** Engine mini-status, admin identity,
   and log-out should sit at the bottom of the sidebar — the spacer wasn't
   expanding (`.sidebar .spacer { flex:1 }`).
6. **Realtime toggle on the Dashboard apps list.** An on/off switch that live-
   polls CPU/memory for the apps table via a small `/apps/metrics.json`
   aggregate endpoint (reuse `allAppRows`) + an Alpine poll. The collector now
   also samples **static apps** (host-process CPU/RSS across the `npm`→`node`
   tree; CPU% derived from the CPU-time delta between ticks) so they report
   real usage instead of "—" — containers were already sampled via the engine.
7. **Stack the engine cards.** Container-engine card shows podman above docker,
   not side-by-side.
8. **Trim the Projects page.** Remove the Container-engine and Host-resources
   cards from the Projects list (also drops a per-load `HostSnapshot()`).
9. **Project detail: readability + Add-app modal.** Ensure all text meets the
   dark-theme contrast, and move the "Add app" form into a modal triggered by
   the "+ Add app" button (frees the side column).

---

## Platform plan — server consolidation (2026-07-13)

Driving goal: replace the current Linode (nginx + Mailcow, 12+ domains, 5 mail
domains, several subdomains proxied to the Coolify box) with xdev on a **4GB
Linode — hard cap**, then add ~8 more domains (Laravel, WordPress, Node,
SolidStart, compiled React/Vue). Consolidation below is a **RAM requirement,
not a nicety**: dedicated per-app MariaDB (~200MB each) and per-site WP
containers alone would blow 4GB.

**Status:** features below are planned, not started. The mail side of the
migration (already-built `mail` app type, Mailcow cutover) lives in
`PLAN_MAIL.md` — it is deliberately not covered here.

### RAM budget (prod, 4GB target)

| Service | Est. RSS |
|---|---|
| xdev + Caddy | ~100MB |
| Mail app (see `PLAN_MAIL.md`) | ~300–500MB |
| Shared MariaDB (one, all sites) | ~250–400MB |
| Shared WP host (one container, N sites) | ~200–350MB |
| Laravel apps (Octane, each) | ~80–150MB |
| Node/SolidStart services (each) | ~80–120MB |
| Compiled React/Vue (Caddy file_server) | ~0 |

Rule the plan enforces: **one DB process, one WP process pool, zero containers
for static/proxy domains.** Escape hatches (dedicated DB / separate WP
container) exist per app for the odd site that needs isolation.

### A. Proxy app type (smallest; unblocks the server migration)

A domain that just reverse-proxies to another server (replaces the nginx
`proxy_pass` vhosts pointing at the Coolify box).

- New type `proxy`: **no container, no process, no host port.** One field:
  upstream URL (`http(s)://host[:port]`). New nullable `upstream` column on
  `apps` (migration); the Caddy route uses it instead of `127.0.0.1:port`.
  Websockets come free with Caddy; pass Host through, set TLS-SNI when the
  upstream is https-by-name.
- UI: upstream field in the add-app modal; app card shows the upstream;
  hide Metrics/Logs/Env/Backups tabs (nothing local to measure).
- Start/stop = route added/removed in reconcile.
- Check: route-JSON unit test + one `curl --resolve` e2e.

### B. Shared MariaDB (platform service)

- One xdev-managed `xdev-db` MariaDB container on a new external network
  `xdev_shared`, created lazily the first time an app opts in. Root password
  generated once, kept in xdev's DB.
- App create (wordpress/laravel): **Database: shared (default) | dedicated**.
  - Shared → xdev runs `CREATE DATABASE/USER/GRANT` (name `project_app`),
    injects `DB_HOST=xdev-db` + creds into the compose env; the app's compose
    drops its db service and additionally joins `xdev_shared`.
  - Dedicated → today's per-app db service, unchanged.
- Delete app → dump to backups dir, then drop db+user. Backups: per-database
  `mariadb-dump` via the existing backups flow.
- Ceiling: one MariaDB is a shared blast radius (restart touches every shared
  site) — acceptable at this scale; dedicated mode is the escape hatch.

### C. Unified WordPress (shared WP host)

Goal: N WP sites without N Apache+PHP containers. One **wp-host** container
serves every shared-mode site; each site keeps **only its own `wp-config.php`
and `wp-content/`**, sharing a read-only WP core (classic shared-hosting
layout — chosen over WP Multisite so sites stay independent, keep separate
wp-content, and can be split out later; Multisite remains possible manually
inside one app).

```
data/wp/
  core/              # one WP core, owned/updated by xdev (never auto-updates itself)
  pool/plugins/      # global pools — each plugin/theme downloaded once
  pool/themes/
  sites/<slug>/
    wp-config.php    # generated: shared-db creds, unique table prefix + salts
    wp-content/      # per-site; plugins/ + themes/ mix symlinks into the pools
                     # with site-local installs
```

- wp-host: **no second web server.** One `php:8.3-fpm` container; xdev's
  existing Caddy serves each site's files directly (`file_server`, permalinks
  via `try_files`) and hands `*.php` to the fpm port over fastcgi. Site dirs
  are bind-mounted into the container at the **same absolute path** as on the
  host so `SCRIPT_FILENAME` resolves on both sides. Add/remove a site = a
  Caddy route update (the admin-API flow xdev already uses) — no vhost files,
  no container reload. Fallback if fastcgi path-mapping fights us: FrankenPHP
  (Caddy-embedded PHP, worker mode) — still not Apache.
- **"Install plugin/theme for all sites":** download once into the pool,
  symlink into each shared site's `wp-content/plugins|themes`. Activation
  stays per-site. Update = update the pool once.
- App create: **WordPress: shared host (default) | separate container**
  (separate = existing `wordpress` type, untouched). Shared mode requires B
  (always uses the shared DB).
- Ceilings: one PHP pool is a noisy-neighbor across WP sites (move to
  per-site FPM pools if it ever matters); core updates are an explicit xdev
  action, not WP self-update.
- Phasing: **C1** wp-host + per-site docroots + shared DB → **C2** pools +
  install-for-all UI → **C3** core-update button.

### D. Coverage for the upcoming 8 domains (mostly exists)

- Compiled React/Vue → existing `static` type (build cmd + serve mode). Done.
- Node / SolidStart → `static` command mode runs host Node processes today;
  decide later whether prod wants a small containerized `node` type instead.
  Note only — no work planned yet.
- Laravel → exists; gains the shared-DB option via B.

### Order of work

**A (proxy) → mail (see `PLAN_MAIL.md`) → B (shared DB) → C1 → C2/C3 when
needed.** A is everything the *current* server's web side needs; B/C are what
makes the +8 domains fit in 4GB.

### Migration runbook — web side (current server: nginx, same IP kept)

The mail cutover has its own runbook in `PLAN_MAIL.md`; do it after step 3.

1. Inventory nginx vhosts (`server_name` + `proxy_pass` targets) into a
   checklist; lower DNS TTLs to 300s. No DNS records change (same IP).
2. Recreate every vhost as an xdev app (mostly `proxy` type); verify each via
   `curl --resolve` while nginx still owns 80/443.
3. **Proxy flip:** stop nginx, start xdev's Caddy (clean swap — Caddy then
   ACMEs all domains at once). Rollback = reverse.
4. Delete nothing: nginx configs stay on disk for weeks; rollback at every
   stage is stop-new/start-old. Resize 2GB→4GB before the mail step.

---

## Source pointers (for whoever implements the design)

- Templates: `web/templates/*.html` (one file per screen, `layout.html` is
  the shared shell)
- Styles: `web/templates/twstyles.html` (Tailwind v4 source: `@theme` + component
  layer), compiled in-browser by `web/static/tailwind.js` (vendored, no build step)
- Routes: `internal/server/server.go` (full route table)
- Full engineering plan / roadmap: `PLAN.md`
