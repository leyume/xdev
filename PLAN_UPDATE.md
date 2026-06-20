# PLAN_UPDATE.md — Seamless install & distribution

> **Purpose.** This is an implementation spec for a set of distribution + UX
> updates to xdev. It is written to be handed to another developer or AI model to
> build from cold. Read [`GUIDELINE.md`](GUIDELINE.md) first for architecture,
> conventions, and the safety rules (especially: **never edit created projects
> under `projects/`**, add-don't-edit migrations, reconcile after mutations,
> never touch the `bizepp` `be_*` containers).
>
> **Repo:** `leyume/xdev` · **Targets:** Ubuntu/Debian + macOS · **Status:** not
> started — build after the maintainer approves.

---

## 0. Goals

Make xdev install and run with a single command, beautifully, on both a fresh
Ubuntu server **and** a local Mac:

```bash
curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | sudo bash
```

The installer detects the OS + CPU arch, installs missing prerequisites (a
container engine + Caddy), asks a few questions, downloads the right prebuilt
binary, writes config, creates the admin account, and starts the service.

Binaries are produced by **GitHub Actions on a version tag** — it cross-compiles
all targets and publishes them to a GitHub Release, so `install.sh` can fetch
them. A lightweight local pre-push hook just runs build/vet/test as a fast gate.

### Already done (do not re-implement)
- **The DB is already external** — a plain SQLite file at `<data-dir>/xdev.db`.
  Only schema migrations and UI assets are embedded in the binary. Data persists
  across binary upgrades. This spec only *standardizes its location* on servers.

### Non-goals (for this iteration)
- Windows support. RHEL/Fedora (`dnf`). Multi-node. Auto-update of the binary.

---

## 1. Distribution architecture

```
  Developer machine (Mac)            GitHub Actions (leyume/xdev)        Target host (Ubuntu/Mac)
  ─────────────────────────          ────────────────────────────       ────────────────────────
  git push        ─▶ ci.yml          repo (source, no binaries)          curl install.sh | sudo bash
   (pre-push gate:   build/vet/test                                        │ detect os/arch
    build/vet/test)                  Releases:                            │ install engine+caddy
  git tag v0.1.0  ─▶ release.yml ──▶ xdev-linux-amd64                     │ download matching binary
  git push --tags    build 4 bins    xdev-linux-arm64                     │ write /etc/xdev/xdev.env
                     + checksums      xdev-darwin-amd64                    │ systemd/launchd + start
                     publish Release  xdev-darwin-arm64                   │ xdev create-admin
                                      checksums.txt  ────────────────────▶└─ xdev doctor + next steps
```

- **Releases happen on a tag**, built by GitHub Actions on a clean runner — the
  artifacts provably match the tagged commit. Normal `git push` stays instant.
- **No binaries are committed to git.** They live in GitHub Releases.
- `install.sh` lives in the repo (`deploy/install.sh`) and is fetched raw; it then
  downloads the binary asset from the latest (or pinned) Release.

---

## 2. Part A — Binary (Go) changes

All changes are in `cmd/xdev/main.go` unless noted. Keep the stdlib-first,
heavily-commented style. Subcommands are dispatched at the very top of `run()`
(the existing `write-hosts` handler is the pattern to follow).

### A1. Version stamping
- Add `var version = "dev"` (package `main`). Inject at build time via
  `-ldflags "-X main.version=<v>"`.
- Add a `-version` flag and a `version` subcommand. Output:
  `xdev <version> (<goos>/<goarch>, go<goversion>)`.
- Source of `<version>`: the tag (`GITHUB_REF_NAME`) in the release workflow;
  `git describe --tags --always --dirty` for local builds (Makefile).

### A2. Help / usage (`-h`, `--help`)
- Set a custom `flag.Usage` that prints:
  1. one-line description,
  2. usage line `xdev [flags]` and `xdev <subcommand> [args]`,
  3. the **subcommands** (`version`, `doctor`, `create-admin`, `write-hosts`)
     with one-line descriptions,
  4. the flags (grouped: Core / Proxy & TLS / Engine / Hosts), each with default,
  5. 2–3 examples (local dev, prod server, sudo-free dev).
- Go's `flag` already maps `-h`/`-help` to `Usage`; just make `Usage` nice.

### A3. `xdev doctor` subcommand
Preflight/health check. Resolves config the same way the server does (flags +
env), then prints a readiness report and exits non-zero if anything required is
missing. Checks:
- For **podman** and **docker**: installed (PATH), compose plugin
  (`<engine> compose version`), daemon Ready (`<engine> ps`). Reuse
  `runtime.Detect()` / `runtime.EngineStatus`.
- **Selected engine** (from `runtime.Selector` precedence) is usable.
- **caddy** on PATH (+ version) when `-caddy` is true.
- **Ports** `-http-port` / `-https-port` bindable (try `net.Listen(":p")`; reuse
  the wildcard approach from `apps.portFree`).
- **Data dir** exists/writable; report DB path + whether an admin exists
  (`store.UserCount()`).
- **/etc/hosts** writable (only relevant when `-manage-hosts` and local).

Output format (use ✓ / ✗ / – and exit 1 on any ✗ of a *required* item):
```
xdev doctor
  engine: podman   ✓ installed  ✓ compose  ✓ daemon up
  engine: docker   – not installed
  selected engine  ✓ podman
  caddy            ✓ v2.11.4
  ports 80 / 443   ✓ bindable
  data dir         ✓ /var/lib/xdev (db: /var/lib/xdev/xdev.db)
  admin account    ✗ none yet  → run: xdev create-admin you@example.com
```

### A4. `xdev create-admin <email>` subcommand
- Resolves config (data dir/env), opens the store, runs migrations (reuse
  `store.Open`).
- Reads the password from `$XDEV_ADMIN_PASSWORD` if set, else prompts on the TTY
  twice (no echo — use `golang.org/x/term.ReadPassword`; this adds
  `golang.org/x/term`, a tiny, already-transitive-friendly dep). Minimum 8 chars
  (mirror `auth.CreateAdmin`).
- Calls `auth.New(store, false).CreateAdmin(email, password)`. If an admin
  already exists, print a friendly message and exit 0 (idempotent for installers)
  — or exit non-zero with `--fail-if-exists`. Default: succeed/no-op.

### A5. Full env-file configuration
Add `XDEV_*` env fallbacks for **every** flag so `/etc/xdev/xdev.env` (or a mac
equivalent) can fully configure xdev and the service manager just sets
`EnvironmentFile`. Precedence stays **explicit flag > env > built-in default**
(the existing `envOr(key, fallback)` already implements "env as the flag
default"; replicate it for the flags that don't have it yet).

Add small helpers next to `envOr`:
```go
func envBoolOr(key string, def bool) bool   // parses 1/true/yes/on (case-insens)
func envIntOr(key string, def int) int
```
Use them as the flag defaults.

**Complete flag ⇄ env map** (this table is the source of truth for the env file):

| Flag | Env var | Default | Type |
|---|---|---|---|
| `-data` | `XDEV_DATA` | `./data` | path |
| `-projects` | `XDEV_PROJECTS` | `./projects` | path |
| `-addr` | `XDEV_ADDR` | `127.0.0.1:7331` | host:port |
| `-secure` | `XDEV_SECURE` | `false` | bool |
| `-engine` | `XDEV_ENGINE` | auto | podman\|docker |
| `-caddy` | `XDEV_CADDY` | `true` | bool |
| `-caddy-admin` | `XDEV_CADDY_ADMIN` | `127.0.0.1:2019` | host:port |
| `-https-port` | `XDEV_HTTPS_PORT` | `443` | int |
| `-http-port` | `XDEV_HTTP_PORT` | `80` | int |
| `-hosts-file` | `XDEV_HOSTS_FILE` | `/etc/hosts` | path |
| `-manage-hosts` | `XDEV_MANAGE_HOSTS` | `true` | bool |
| `-acme-email` | `XDEV_ACME_EMAIL` | "" | email |
| `-local-cert-lifetime` | `XDEV_LOCAL_CERT_LIFETIME` | `2160h` | duration |

> Already env-backed: `XDEV_DATA`, `XDEV_PROJECTS`, `XDEV_ADDR`, `XDEV_ENGINE`,
> `XDEV_ACME_EMAIL`. Add the rest.

### A6. (Optional, nice) default base domain setting
The installer can ask for a "primary base domain". If provided, store it via
`store.SetSetting("default_base_domain", v)` and have the new-project form
(`handlers_projects.go` → `project_new.html`, field `base_domain`) pre-fill from
that setting. Small, optional; skip if it complicates the first cut.

### A7. Subcommand dispatch
At the top of `run()` (before flag parsing), dispatch on `os.Args[1]`:
`version`, `doctor`, `create-admin`, `write-hosts` (existing). Each subcommand
parses only what it needs. Keep the default (no subcommand) = run the server.

---

## 3. Part B — `deploy/install.sh`

A single POSIX-friendly **bash** script. Must work when run directly *and* when
piped (`curl … | sudo bash`) — so **read all prompts from `/dev/tty`**, and
support a fully non-interactive mode driven by env vars (for automation/CI).

### B0. Conventions
- `set -euo pipefail`. Color/emoji output helpers (`info`, `ok`, `warn`, `err`,
  `ask`). Respect `NO_COLOR`.
- Variables overridable by env (so non-interactive works): every prompt has a
  matching `XDEV_*` env var; if set, skip the prompt.
- `XDEV_REPO` (default `leyume/xdev`) and `XDEV_VERSION` (default `latest`)
  control where the binary comes from.
- `XDEV_NONINTERACTIVE=1` → never prompt; require the needed vars or use defaults.

### B1. Detect OS + arch
- OS: `uname -s` → `Linux` | `Darwin` (else: error "unsupported OS").
- Arch: `uname -m` → map `x86_64|amd64`→`amd64`, `aarch64|arm64`→`arm64` (else
  error).
- Compose the asset name: `xdev-<os>-<arch>` where `<os>` ∈ `linux|darwin`.

### B2. Privilege
- Linux: needs root (for apt, ports, systemd, `/etc/xdev`). If not root, re-exec
  with `sudo` (or instruct).
- macOS: most steps run as the user via Homebrew; only `caddy trust`, binding
  443/80, and a LaunchDaemon need sudo — prompt for those specifically.

### B3. Prerequisite install (ask, then install if missing)
- **Engine** — prompt **docker** or **podman** (`XDEV_ENGINE`). Detect existing;
  if missing, install:
  - Ubuntu/Debian: docker → `apt-get install -y docker.io docker-compose-v2` (or
    the official get.docker.com script); podman → `apt-get install -y podman
    podman-compose` (or rely on the docker compose plugin). Ensure
    `<engine> compose version` works.
  - macOS: `brew install --cask docker` (Docker Desktop) or `brew install podman`
    (+ `podman machine init && podman machine start`); caddy via `brew install
    caddy`. Verify compose plugin.
- **Caddy** — detect; if missing install (Ubuntu: official Caddy apt repo; macOS:
  `brew install caddy`). Or accept an existing system Caddy and set
  `XDEV_CADDY=false` if the user wants to manage it themselves (ask).
- Re-run checks after install; abort with a clear message if still missing.

### B4. Interactive configuration
Ask the following (each with a default and an `XDEV_*` env override). See the
**Prompt spec** table in §7.

- **Mode**: `local` or `prod` (drives smart defaults below).
- **Primary/base domain** (e.g. `example.com` for prod, or blank → `.localhost`
  default for local).
- **Let's Encrypt email** (prod only; for ACME).
- **Admin UI address** (default `127.0.0.1:7331`).
- **Admin email** + **password** (for `create-admin`; password hidden, confirm).
- Derived/secure defaults:
  - local → `XDEV_MANAGE_HOSTS=true`, `XDEV_SECURE=false`, ports `443/80` (or
    offer sudo-free `8443/8080`), base domain default `.localhost`.
  - prod → `XDEV_MANAGE_HOSTS=false`, `XDEV_SECURE=true`, ports `443/80`,
    `XDEV_ACME_EMAIL` required, real base domain.

### B5. Download the binary
- URL: `https://github.com/$XDEV_REPO/releases/<latest|download/$XDEV_VERSION>/download/xdev-<os>-<arch>`.
- Download to a temp file, fetch `checksums.txt`, **verify sha256**, then
  `install -m 0755` to:
  - Linux: `/usr/local/bin/xdev` (and symlink/copy to `/opt/xdev/xdev` if using
    the unit's WorkingDirectory).
  - macOS: `/usr/local/bin/xdev` or `/opt/homebrew/bin/xdev` (whichever is on
    PATH/writable).
- Fallback: if no release asset and Go is present (or `--from-source`), build
  from a cloned/working copy (`make build`).

### B6. Write config
- Linux: `/etc/xdev/xdev.env` (0640, root) from the answers — see
  `deploy/xdev.env.example`. Data dir default `/var/lib/xdev`.
- macOS: `~/Library/Application Support/xdev/xdev.env` (or
  `/usr/local/etc/xdev/xdev.env`). Data dir default
  `~/Library/Application Support/xdev/data` (or `/var/lib/xdev` if root).

### B7. Service setup
- **Linux (systemd):** install `deploy/xdev.service` →
  `/etc/systemd/system/xdev.service` (uses `EnvironmentFile=/etc/xdev/xdev.env`,
  `AmbientCapabilities=CAP_NET_BIND_SERVICE`). `systemctl daemon-reload` +
  `enable --now xdev`.
- **macOS (launchd, optional):** install `deploy/com.leyume.xdev.plist` →
  `/Library/LaunchDaemons/` (root, to bind 443) **or** offer "don't install a
  service, I'll run `xdev` myself". For local-only use, the LaunchDaemon is
  optional; default = install it but make it easy to skip.

### B8. Admin + finishing touches
- Run `xdev create-admin "$ADMIN_EMAIL"` with `XDEV_ADMIN_PASSWORD` exported
  (using the same data dir/env) **before or after** first start (it just writes
  to the DB).
- macOS local: offer to run `sudo caddy trust` (trust the local CA so
  `https://*.localhost` is green).
- Run `xdev doctor` and print it.
- Print **next steps**: for prod → the DNS A/AAAA records to create + that the
  admin UI is on `127.0.0.1:7331` reachable via
  `ssh -L 7331:127.0.0.1:7331 user@server`; for local → the URL to open and the
  trust note.

### B9. Sample transcripts (for reference, not literal)
**Ubuntu (prod):**
```
$ curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | sudo bash
▸ Detected: linux/amd64
▸ Mode? [local/prod]: prod
▸ Container engine? [docker/podman] (docker): docker
  installing docker + compose plugin… ok
  installing caddy… ok
▸ Primary domain: apps.example.com
▸ Let's Encrypt email: ops@example.com
▸ Admin email: me@example.com
▸ Admin password: ********  (confirm) ********
  downloading xdev-linux-amd64… verified ✓
  wrote /etc/xdev/xdev.env · installed systemd unit · started xdev ✓
  created admin me@example.com ✓
xdev doctor … all green
Next:
  • Point DNS:  apps.example.com  A  <this server IP>
  • Admin UI (private):  ssh -L 7331:127.0.0.1:7331 me@server  → http://127.0.0.1:7331
```
**macOS (local):**
```
$ curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | bash
▸ Detected: darwin/arm64
▸ Mode? [local/prod] (local): local
▸ Container engine? [docker/podman] (podman): podman
  brew install podman… podman machine start… ok
  brew install caddy… ok
▸ Admin email: me@local
▸ Admin password: ****  ****
  downloading xdev-darwin-arm64… verified ✓
  wrote ~/Library/Application Support/xdev/xdev.env
  install background service? [Y/n]: Y  (LaunchDaemon, needs sudo for :443)
  sudo caddy trust? [Y/n]: Y  ✓
xdev doctor … all green
Open: http://127.0.0.1:7331  ·  your apps will be at https://<app>.<project>.localhost
```

---

## 4. Part C — Deploy bundle (files under `deploy/`)

| File | Purpose |
|---|---|
| `deploy/install.sh` | The installer (Part B). Executable. |
| `deploy/uninstall.sh` | Stop+disable+remove service & binary; keep data unless `--purge`. |
| `deploy/xdev.service` | systemd unit; `EnvironmentFile=/etc/xdev/xdev.env`. |
| `deploy/com.leyume.xdev.plist` | launchd daemon for macOS (optional service). |
| `deploy/xdev.env.example` | Documented env file (every `XDEV_*` var with comments). |
| `deploy/README.md` | Updated: curl quickstart, manual steps, prod & local, uninstall. |

`xdev.env.example` must list **every** variable from the §2 A5 table, grouped and
commented, e.g.:
```sh
# --- core ---
XDEV_DATA=/var/lib/xdev
XDEV_ADDR=127.0.0.1:7331
XDEV_SECURE=true
# --- engine ---
XDEV_ENGINE=docker
# --- proxy & TLS ---
XDEV_HTTPS_PORT=443
XDEV_HTTP_PORT=80
XDEV_ACME_EMAIL=ops@example.com
XDEV_LOCAL_CERT_LIFETIME=2160h
# --- hosts (servers: off) ---
XDEV_MANAGE_HOSTS=false
```

---

## 5. Part D — Release pipeline (GitHub Actions) + local gate

Releases are built on a clean runner and triggered by a **version tag**, so the
published artifacts provably match that commit. Normal pushes stay fast.

### D1. Release workflow — `.github/workflows/release.yml`
- **Trigger:** `push` of tags matching `v*`.
- **Permissions:** `contents: write` (to create the Release; uses the built-in
  `GITHUB_TOKEN` — no PAT needed).
- **Steps:**
  1. `actions/checkout` with `fetch-depth: 0` (so `git describe` sees the tag).
  2. `actions/setup-go` (Go 1.26).
  3. `go vet ./...` and `go test ./...` (fail the release on red).
  4. Cross-compile the four targets (CGO-free) with version ldflags into `dist/`:
     `xdev-{linux,darwin}-{amd64,arm64}`. Version = the tag
     (`${GITHUB_REF_NAME}` or `git describe`).
  5. Generate `dist/checksums.txt` (sha256 of all four).
  6. Publish the Release with all artifacts — use `softprops/action-gh-release`
     (or `gh release create "$GITHUB_REF_NAME" dist/* --generate-notes`).
- **Result:** `https://github.com/leyume/xdev/releases/latest/download/xdev-<os>-<arch>`
  resolves to the newest tag's binary (what `install.sh` fetches).

### D2. CI workflow — `.github/workflows/ci.yml`
- **Trigger:** `push`/`pull_request` to `main`.
- Steps: `actions/checkout`, `setup-go`, `go build ./...`, `go vet ./...`,
  `go test ./...`. Keep it single-job (ubuntu-latest) for speed.

### D3. Local pre-push gate — `.githooks/pre-push`
- Lightweight **safety check only**: `go build ./... && go vet ./... && go test
  ./...`. **Does not** build or publish binaries (that's Actions' job).
- Tracked in-repo; enable once with `git config core.hooksPath .githooks`
  (provide a `make hooks` target). Bypassable with `--no-verify` when needed.

### D4. Makefile targets
- `make build-all` → the four cross-compiles into `dist/` with version ldflags
  (for **local** testing of cross-builds; not the release path).
- `make hooks` → `git config core.hooksPath .githooks`.
- Update `build`/`build-linux` to inject `-ldflags "-X main.version=$(VERSION)"`
  where `VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)`.

**Release ritual:**
```bash
git tag v0.1.0
git push origin v0.1.0     # → release.yml builds + publishes the GitHub Release
```

> **Tradeoff note (decided):** binaries go to **Releases**, built by **Actions on
> a tag** (reproducible, decoupled from pushes, machine-independent). They are
> **not** committed to git. No local `gh`/toolchain dependency is required to cut
> a release — just push a tag.

---

## 6. Part E — Docs

- **`README.md`** (new, top-level): one-paragraph what-it-is, the curl quickstart
  for Ubuntu and macOS, link to `GUIDELINE.md` / `deploy/README.md`, a short
  feature list. This is the repo's front page.
- **`GUIDELINE.md`**: update §3 (new subcommands `version`/`doctor`/`create-admin`),
  §11 (full env table — keep in sync with §2 A5 here), and add an "Install &
  release" subsection (the Actions-on-tag → Releases → curl model). Update the
  footer reminder.
- **`PLAN.md`**: tick these items off the backlog; note install/distribution done.

---

## 7. Prompt spec (installer questions)

| Prompt | Env override | Default | Notes / validation |
|---|---|---|---|
| Mode | `XDEV_MODE` | `local` | `local`\|`prod`; sets the derived defaults below |
| Engine | `XDEV_ENGINE` | `podman` (mac) / `docker` (linux) | install if missing |
| Manage Caddy? | `XDEV_CADDY` | `true` | if false, user runs Caddy themselves |
| Primary base domain | `XDEV_BASE_DOMAIN` | prod: (required) · local: `""` (→`.localhost`) | hostname chars only |
| Let's Encrypt email | `XDEV_ACME_EMAIL` | "" | required when mode=prod |
| Admin UI addr | `XDEV_ADDR` | `127.0.0.1:7331` | keep bound to loopback |
| HTTPS / HTTP ports | `XDEV_HTTPS_PORT`/`XDEV_HTTP_PORT` | `443`/`80` | local sudo-free option `8443`/`8080` |
| Admin email | `XDEV_ADMIN_EMAIL` | (required) | valid email |
| Admin password | `XDEV_ADMIN_PASSWORD` | (prompted, hidden) | ≥ 8 chars, confirm |
| Install service? | `XDEV_INSTALL_SERVICE` | `true` | systemd (linux) / launchd (mac) |
| Trust local CA? (mac local) | `XDEV_TRUST_CA` | ask | runs `sudo caddy trust` |

`XDEV_MODE` derives: `XDEV_SECURE`, `XDEV_MANAGE_HOSTS`, base-domain default, and
whether `XDEV_ACME_EMAIL` is required (see B4).

---

## 8. Acceptance criteria (how to verify)

Build/lint gates first: `go build ./cmd/xdev`, `go vet ./...`, `go test ./...`
clean. Then:

1. **Help:** `xdev -h` prints the grouped help + subcommands + examples.
   `xdev version` prints version/os/arch.
2. **Doctor:** `xdev doctor` reports engine/compose/daemon/caddy/ports/data/admin
   accurately; exits non-zero when a required item is missing.
3. **create-admin:** `XDEV_ADMIN_PASSWORD=… xdev create-admin a@b.com` creates the
   admin; re-running is a no-op; web `/setup` then redirects to `/login`.
4. **Env config:** setting every `XDEV_*` var (no flags) configures the server
   identically to the flags; an explicit flag still overrides its env var.
5. **Installer (Ubuntu VM/container):** fresh box → curl one-liner → engine+caddy
   installed → binary verified by checksum → service running → admin created →
   `doctor` green. Non-interactive run via env vars works.
6. **Installer (macOS):** fresh-ish Mac → installs via brew → local mode →
   `https://<app>.<project>.localhost` works after `caddy trust`.
7. **Release & CI:** pushing a `vX.Y.Z` tag triggers `release.yml`, which builds
   the four binaries + `checksums.txt` and publishes a GitHub Release whose
   `releases/latest/download/...` URLs resolve; `ci.yml` runs build/vet/test on
   push/PR to `main`; the local pre-push hook runs build/vet/test and blocks a
   broken push (bypassable with `--no-verify`).
8. **Idempotency:** re-running `install.sh` upgrades the binary and restarts the
   service without touching `XDEV_DATA` (the DB) or anything under `projects/`.

> Use the **isolation discipline** from `GUIDELINE.md` §15 when testing locally
> (distinct ports, temp data dir, temp hosts file, isolated `XDG_DATA_HOME` for
> Caddy) and **never disturb the maintainer's running instance, created projects,
> or the `bizepp` containers.**

---

## 9. File manifest

**New**
```
deploy/install.sh
deploy/uninstall.sh
deploy/xdev.env.example
deploy/com.leyume.xdev.plist
.github/workflows/release.yml    # tag v* → build 4 binaries + publish Release
.github/workflows/ci.yml         # push/PR → build + vet + test
.githooks/pre-push               # local gate: build/vet/test only
README.md
PLAN_UPDATE.md            (this file)
```
**Changed**
```
cmd/xdev/main.go          # version, help, subcommands (doctor, create-admin), env fallbacks
internal/auth/auth.go     # (reuse CreateAdmin; no change expected)
internal/store/...        # (reuse; optional default_base_domain setting)
Makefile                  # build-all, release, hooks, VERSION ldflags
deploy/xdev.service       # EnvironmentFile=/etc/xdev/xdev.env
deploy/README.md          # rewrite for installer + mac + uninstall
GUIDELINE.md              # §3, §11, install/release section
PLAN.md                   # mark distribution done
go.mod / go.sum           # add golang.org/x/term (for hidden password prompt)
```

---

## 10. Build order (suggested)

1. **A (binary):** version + help + `doctor` + `create-admin` + env fallbacks.
   Ship + verify locally (`go test`, manual `doctor`/`create-admin`). This is the
   foundation the installer relies on.
2. **D (release pipeline + Makefile):** version ldflags, `.github/workflows/`
   (`release.yml` + `ci.yml`), local pre-push gate. Cut a `v0.1.0` tag to produce
   the first Release so the installer has something to download.
3. **B + C (installer + bundle):** `install.sh` (Linux first, then macOS branch),
   `xdev.env.example`, systemd unit, launchd plist, uninstall.
4. **E (docs):** `README.md`, GUIDELINE/PLAN updates.

---

## 11. Open considerations (call out if hit during build)

- **Public access for the install flow.** `curl | bash` of `install.sh` (raw
  GitHub content) and the Release binary download both require the repo/Releases
  to be **public** — or the install URL must carry a token. Keep `leyume/xdev`
  public for the frictionless one-liner.
- **Release workflow permissions:** `release.yml` needs `permissions: contents:
  write`; it uses the built-in `GITHUB_TOKEN` (no PAT). Tag pushes trigger it.
- **`curl | sudo bash` + interactive prompts** rely on `/dev/tty`; ensure the
  script never reads config from stdin. Always provide the non-interactive env
  path.
- **Docker Desktop on macOS** can't be fully headless-installed; the script may
  need to tell the user to launch it once. Podman is more scriptable on mac
  (`podman machine`).
- **macOS LaunchDaemon binding :443** needs root; keep it opt-in.
- Keep the env table in **three** places in sync (this doc §2/§7, GUIDELINE §11,
  `deploy/xdev.env.example`) — or better, treat `deploy/xdev.env.example` as the
  canonical reference and link to it.
```
