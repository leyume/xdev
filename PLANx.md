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
- No component framework — one `web/static/app.css` plus Alpine for client
  state (per-app tab switching, form toggles); a shared `partials.html`
  holds the engine/host-resources/events fragments reused across pages.

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

## Source pointers (for whoever implements the design)

- Templates: `web/templates/*.html` (one file per screen, `layout.html` is
  the shared shell)
- Styles: `web/static/app.css` (single stylesheet, no preprocessor)
- Routes: `internal/server/server.go` (full route table)
- Full engineering plan / roadmap: `PLAN.md`
