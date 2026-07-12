# Handoff: xdev Dashboard Redesign (Nova direction)

## Overview
A visual redesign of xdev's three core screens — home Dashboard, Projects list, and Project detail — for a solo/small-team local dev-server manager (podman/docker container orchestration, per-project domains, SSL). Dark theme, violet accent, glassmorphism cards. Nav can be a top bar or a left sidebar (same content, user-toggleable in the prototype's Tweaks panel — pick one for the real app, or keep both behind a setting).

## About the Design Files
The bundled file (`xDev Dashboard Directions.dc.html`) is a **design reference built in HTML** — a prototype demonstrating layout, color, type, spacing, and states. It is not production code to copy verbatim. The task is to **recreate this design in xdev's existing frontend stack** (whatever framework/component library the app already uses), following its existing patterns for routing, data-fetching, and componentization — using this file and README purely as the visual/structural spec.

The file contains multiple design iterations stacked vertically (a working canvas). Only three sections matter for this handoff, identified by `id` attributes in the HTML:
- `#3a` — **Dashboard** (home screen)
- `#2a` — **Projects** (project list)
- `#4a` — **Project detail** (single project, e.g. "WV")

Ignore sections `1a/1b/1c/1d/1e/1f` (earlier exploratory directions/logos, superseded).

## Fidelity
**High-fidelity.** Colors, type sizes/weights, spacing, radii, and copy below are final — implement pixel-accurately using xdev's existing component library/CSS approach (Tailwind, CSS modules, styled-components, whatever the codebase uses). Do not substitute the app's current dark-theme values; use the tokens below.

## Design Tokens

**Colors**
- Background base: `#0D0B12` (near-black, violet-tinted)
- Background wash: `radial-gradient(1000px 500px at 20% -10%, rgba(180,140,255,0.08), transparent 60%)` layered over base — applied once at the page-root level
- Card surface: `rgba(255,255,255,0.025)` fill, `1px solid rgba(255,255,255,0.07)` border, `backdrop-filter: blur(16px)`
- Card surface (topbar/sidebar): `rgba(255,255,255,0.02)` fill, `1px solid rgba(255,255,255,0.07)` border
- Primary text: `#ECEAF2`
- Secondary/muted text: `#96909F`
- Accent (violet): `#B48CFF` — buttons, active nav, links, chart primary line
- Accent-on-dark text (on violet buttons): `#161022`
- Success/healthy/running: `#8CE8B8`
- Warning/degraded: `#F2C14E`
- Error/critical/stopped-attention: `#F58C8C`
- Hairline dividers: `rgba(255,255,255,0.06)` to `rgba(255,255,255,0.07)`
- Danger-zone panel: fill `rgba(245,140,140,0.04)`, border `rgba(245,140,140,0.18)`

**Typography**
- Font: **DM Sans** (400/500/600/700), loaded via Google Fonts
- Page title (e.g. "Good morning, Li" / "Projects" / "WV"): 28px / 700 / letter-spacing -0.02em
- Section card headings ("Host resources", "Apps", "Recent events"): 12px / 600 / uppercase / letter-spacing 0.08em / color `#96909F` — OR 15.5–16px / 700 for content-card titles (e.g. "Apps", "Add app") — see per-screen notes below
- KPI numeral: 32px / 700 / letter-spacing -0.03em
- Body/table text: 13–13.5px / 400–600
- Small meta text (timestamps, subtext): 11.5–12.5px / `#96909F`

**Spacing & Shape**
- Border radius: **8px** standard (buttons, inputs, chips); **10px** for cards/panels. (Reduced from an earlier 16–20px pass — keep consistently small.)
- Card padding: 24–28px vertical/horizontal typical; 22–26px for denser cards
- Grid gaps: 18–24px between major cards; 12px within table rows/columns
- Page content padding: ~34–38px top, 52–56px sides (generous "airy" margins — this is intentional, don't tighten)
- Status dot: 6–8px circle, color-coded (green/amber/red/gray-outline for stopped)
- Buttons: 40px height, primary = solid violet `#B48CFF` bg / `#161022` text / 700 weight / box-shadow `0 6px 20px rgba(180,140,255,0.28)`; secondary = `1px solid rgba(255,255,255,0.14)` outline, transparent bg
- Search input: 36–42px height, `rgba(255,255,255,0.04)` bg, `1px solid rgba(255,255,255,0.08)` border, `⌘K` shortcut hint right-aligned inside

## Navigation — two interchangeable layouts
Both render the identical page content region; only the chrome differs. Implement as one layout component with a `nav: 'topbar' | 'sidebar'` prop/setting.

**Topbar** (default): 64px-tall bar, logo mark + "xdev" wordmark left, nav links inline (active = violet pill bg `rgba(180,140,255,0.12)`, text `#B48CFF`, radius 8px), search input + user avatar right-aligned. Links: Dashboard, Projects, Events, Admins.

**Sidebar**: 232px-wide fixed left column, logo top, same nav links stacked vertically (active = same violet pill treatment, left-aligned with a status-dot glyph), pushed content: engine-status mini-card ("podman 5.8.2 · daemon up") + user card (avatar, email, "Administrator" role) pinned to the bottom via `flex:1` spacer above them.

Logo mark: 28px circle, `conic-gradient(from 210deg, #B48CFF, #6E4BC4 55%, #B48CFF)` ring with a solid `#0D0B12` center dot — an "orbital ring". Wordmark "xdev" lowercase, 16.5px/700.

## Screens

### 1. Dashboard (`#3a`)
**Purpose:** At-a-glance health of the whole local dev environment on load.
**Layout:** nav chrome → content column (gap 30px):
1. Header row: greeting "Good morning, {name}" (28px/700) + subtext summarizing running/stopped app counts; right-aligned "+ New project" primary button.
2. **KPI strip** — single card, 5-column grid, hairline `1px solid rgba(255,255,255,0.06)` verticals between cells (no gaps/rounded sub-cards). Each cell: uppercase label (11.5px/600/`#96909F`), big numeral (32px/700), muted caption. Cells: Projects (count + "N apps total"), Running (green numeral + "N stopped · app/name"), Containers (count + engine + restart count/24h), Certificates (amber numeral + days-to-renew), Backups (count + last-run time).
3. **Host resources** card — uppercase label + hostname/uptime meta top-right; 3-column grid of CPU / Memory / Disk, each: label + value pair on one line, 6px-tall rounded progress bar below (violet fill; amber fill when a resource is near capacity, e.g. disk at 92%).
4. **Host utilization chart** — full-width card. Header: title + subtitle, inline legend (CPU=violet line swatch, Memory=green line swatch), and a 3-way time-range pill switcher (1h/24h/7d, active = violet pill). Body: SVG line/area chart, 220px tall, 3 horizontal gridlines, one filled area (CPU, violet gradient fill + line) and one plain line (Memory, green) sharing the same axis; x-axis hour labels below (00:00…now).
5. **Apps + right column** — 2.1fr/1fr grid, `align-items:start` (critical — right column must NOT stretch to match left column height).
   - Left: "Apps" table card. Columns: App (shown as `project/appname`), Type (laravel/static/wordpress…), CPU%, Memory, Status. Status renders as a colored dot + label; special states: "Error · restarting" (red, glowing dot via box-shadow) and "Stopped" with an inline "Start" button (outline pill) in the same cell.
   - Right: "Recent events" card (timestamped dot-prefixed log lines, dot color = event severity) + "Container engine" card (engine name/version + ACTIVE pill for the active engine; the inactive engine shown muted with a "Use docker"/"Use podman" switch button).

### 2. Projects (`#2a`)
**Purpose:** Overview of every project (a project = one docker/podman network + a domain + N apps).
**Layout:** nav chrome → content column (gap 28px):
1. Header: "Projects" title + subtext "N projects · N apps · N running · {hostname}"; "+ New project" button.
2. **Host resources + Container engine** — 1.6fr/1fr grid, two cards side by side. Host resources identical to the Dashboard's version. Container engine card lists both engines (podman/docker) with status dot, ACTIVE badge, and a note: "Switching applies to new projects only."
3. **Project cards grid** — 3-column grid, one card per project + a trailing dashed "+ New project" ghost card. Each project card: name (16.5px/700) + app-count chip (or a colored "N stopped" warning chip if any app is down, amber card border in that case) on one line; domain meta line (`domain · local · network-name`); divider; per-app rows (status dot, app name, type chip, right-aligned domain — or a "Start" button if stopped).
4. **Recent events** — full-width card, 2-column grid of timestamped event lines (same event-log styling as Dashboard).

### 3. Project detail (`#4a`)
**Purpose:** Manage one project's apps.
**Layout:** nav chrome → content column (gap 26px):
1. Breadcrumb: "Projects / {ProjectName}" (violet link + muted separator + bold current).
2. Header: project name (28px/700) + inline "N of N running" status pill; subtext line: domain · local · network chip (`xdev_{project}` in a bordered mono-ish chip); right-aligned "+ Add app" button.
3. **Local-HTTPS callout** — amber-tinted banner (lock emoji + instruction text + inline `sudo caddy trust` code chip). Dismissible in the real app (not shown dismissed here).
4. **Apps + side column** — 2.1fr/1fr grid, `align-items:start`.
   - Left: one full-width card per app. Each: status dot + app name (17px/700) + type chip, right-aligned Stop/Restart(↻)/Delete action buttons (Delete styled red-outline). Below: a meta line — HTTPS URL (violet, bold), internal port ("direct" mode label), optional "Adminer" link, uptime/CPU%/memory. Below a divider: a tab strip (Metrics active/violet-pill, Logs, Env, Backups — plain text tabs) with a right-aligned inline domain chip + "Set domain" button.
   - Right column: **Add app** form card (App name text input, Type select, optional Domain input, "Create app" primary button) → **Project activity** card (same dot-log style as events) → **Danger zone** card (red-tinted, "Delete project" destructive button with a one-line consequence warning).

## Interactions & Behavior
- Nav active state: violet pill background, switches per current route.
- Nav layout is a single global setting/toggle (topbar vs. sidebar) — not per-screen.
- Time-range pills (1h/24h/7d) on charts: only one active at a time, click to switch and refetch/recompute the chart data.
- Table/list "Start" buttons: outline pill, triggers a start action for a stopped app; status dot + label update in place (no page nav).
- App action buttons (Stop/Restart/Delete) on the project detail cards: Delete should require a confirm step in the real implementation (shown here without one, for brevity).
- Tabs (Metrics/Logs/Env/Backups) on each app card: switch the content shown below that specific app's card; only one tab active per app at a time.
- Hover state on project cards (`#2a`): border brightens to `rgba(180,140,255,0.35)`.
- "Set domain" opens an inline/inline-modal input in the real app; the mock shows it as a static button.
- Card hover/press states throughout should get a subtle lift — increase border opacity, no large translate — keep it minimal per the sleek/premium tone.

## State Management (implementation guidance)
- Global: current nav layout (topbar/sidebar), current logged-in admin (avatar initials, email, role).
- Dashboard: KPI aggregates, host resource snapshot (CPU/mem/disk %), utilization time series (per range), apps-needing-attention list, recent events feed, engine status (podman/docker active + versions).
- Projects: project list with nested apps (name, type, domain, status, resource use), engine status.
- Project detail: single project record + its apps array; each app carries status, urls/ports, resource metrics, and tab-specific data (metrics series, log lines, env vars, backup list) fetched lazily per active tab.

## Assets
No image assets — logo is drawn inline (conic-gradient ring + center dot, in CSS/SVG). No icon font; the few glyphs used (search magnifier, refresh ↻, lock 🔒) are inline SVG or emoji — replace with the app's existing icon set if it has one.

## Files
- `xDev Dashboard Directions.dc.html` — full design reference. Look at sections `id="3a"` (Dashboard), `id="2a"` (Projects), `id="4a"` (Project detail) specifically; ignore the rest.
