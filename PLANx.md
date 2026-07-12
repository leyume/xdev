# xdev — Design Brief

For a design pass on the web UI. This summarizes what the product is, who
uses it, how the screens connect, and what the current UI looks like — so a
designer (human or Claude) can propose a modern visual direction without
reading the Go source.

> **Status (2026-07-11):** the "Nova" violet/glass redesign in
> `design/PLAN_UI.md` has been implemented in the real templates (branch
> `feat/nova-ui-redesign`), including a Dashboard/Projects route split and a
> topbar↔sidebar toggle. The screen map and feature-backlog sections below
> have been updated to match; the rest of this doc (stack, data model, style
> baseline) is still accurate background reading.

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

POST /settings/nav-layout  Toggles topbar ↔ sidebar chrome (cookie, not per-user data)
POST /settings/engine      Switch default container engine (redirects back to caller)
```

Nav chrome is a single global preference (topbar or sidebar, `xdev_nav` cookie),
switchable from a link in either chrome. Both render identical page content.

## Data → UI mapping (what each screen is really showing)

| Entity | Screen(s) | Real or placeholder |
|---|---|---|
| **Project** (name, base_domain, network) | Dashboard KPIs, Projects cards, Project detail header | Real |
| **App** (type, status, subdomain, limits) | Dashboard apps table, Project cards, Project detail app cards | Real |
| **Host** (CPU/mem/disk, hostname, uptime) | Host resources card (Dashboard + Projects) | Real |
| **Metrics** (cpu/mem time-series per app) | Metrics tab on each app card (uPlot, fetches `/apps/{id}/metrics.json`) | Real |
| **Events** (audit trail) | `/events`, Recent events / Project activity cards | Real |
| **Host utilization over time** | Dashboard chart | **Placeholder** — no host time-series stored |
| **Containers / Certificates / Backups counts** | Dashboard KPI strip | **Placeholder** — no aggregate tracked |
| **Logs / Env / Backups tab content** | Project detail app card tabs | **Placeholder preview** + real link to the full page |
| **Search** | Topbar search box | **Decorative** — no backend |

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
- Charts are uPlot (per-app Metrics, real) and static sample SVGs (Dashboard
  utilization chart, placeholder — see feature backlog).
- No component framework — one `web/static/app.css` plus Alpine for client
  state (per-app tab switching, form toggles); a shared `partials.html`
  holds the engine/host-resources/events fragments reused across pages.

## Feature backlog from the Nova redesign

The redesign implemented the full IA and visual system from `design/PLAN_UI.md`
using real data everywhere it already existed, and clearly-marked
placeholders where it didn't (see the table above). These are the follow-ups
to turn the placeholders into real features, roughly in the order they'd pay
off:

1. **Host utilization history.** The Dashboard's CPU/memory chart is a static
   sample SVG — there's no stored host-level time series (only per-app
   container metrics are persisted). Needs: a small collector sampling
   `metrics.HostSnapshot()` on an interval into a new table (or reuse the
   existing `metrics` table with a sentinel app_id), plus a `/host/metrics.json`
   endpoint, plus wiring the chart to fetch real data and the 1h/24h/7d pills
   to actually refetch.
2. **Containers KPI.** Currently a dash. Needs a real count of running
   containers (not apps) — e.g. shelling out to `<engine> ps` filtered by the
   xdev-managed network/label, or summing per-app container counts from the
   compose templates.
3. **Certificate tracking.** Currently a dash. The `domains` table has
   `ssl_mode` but no issued/expiry dates. Needs a `cert_status`/`expires_at`
   column (or a call to Caddy's admin API for cert info) to back a real count
   and a "renews in N days" warning.
4. **Backup history.** Currently a dash. Backups exist on disk per app but
   aren't tracked in the database. Needs either a lightweight `backups` table
   (written on `handleAppBackupCreate`) or an aggregate scan of each app's
   backup directory, to back a real count + "last run" timestamp.
5. **Inline Logs/Env/Backups tabs.** Only the Metrics tab is real (fetches
   `/apps/{id}/metrics.json` inline). Logs/Env/Backups tabs show a short
   sample preview + a link to the existing full page. Wiring these inline
   would mean fetch-based partials for tail-log lines, .env content, and the
   backup list/actions, replacing the "sample preview" caption.
6. **Global search.** The topbar's "Search projects & apps… ⌘K" box is
   decorative (matches the design mock's own inert placeholder div). Needs a
   real search endpoint across project/app names and a keyboard shortcut
   handler.
7. **Responsive sidebar.** The sidebar chrome is a fixed 220px column with no
   collapse/overlay behavior on narrow viewports; low priority for a
   single-admin desktop tool, but worth a mobile pass eventually.
8. **Nav-layout preference scope.** Topbar-vs-sidebar is a per-browser cookie
   today, not tied to the admin account — fine for a single admin, but would
   need to move into the `settings`/user record if multi-admin, multi-device
   use starts to matter.

Constraint that stays true for all of the above: server-rendered HTML +
Alpine for client state (no SPA build step) — keep new features to plain
fetch-and-swap patterns like the Metrics tab already uses.

---

## Source pointers (for whoever implements the design)

- Templates: `web/templates/*.html` (one file per screen, `layout.html` is
  the shared shell)
- Styles: `web/static/app.css` (single stylesheet, no preprocessor)
- Routes: `internal/server/server.go` (full route table)
- Full engineering plan / roadmap: `PLAN.md`
