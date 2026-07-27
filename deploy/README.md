# Deploying xdev

xdev is a single static binary. The installer detects your OS + CPU arch,
installs a container engine (**docker** or **podman**), downloads the matching
prebuilt binary from a GitHub Release (verifying its checksum), writes config,
installs a service, and creates your admin account.

The reverse proxy (Caddy) runs one of two ways — pick before you install:

- **Native Caddy** (installer default): apt/brew Caddy, supervised by xdev
  (`XDEV_CADDY=true`). Simplest; the installer sets it up for you.
- **Containerized Caddy** (`XDEV_CADDY=false`): Caddy runs as a container so the
  host needs nothing but the engine. Set up separately — see
  [`deploy/caddy/README.md`](caddy/README.md). Recommended if you want the host
  to depend only on Docker/Podman, or to migrate behind an existing proxy.

## Quick install

**Ubuntu/Debian server (production):**

```bash
curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | sudo bash
```

**macOS (local dev):**

```bash
curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | bash
```

The script asks a few questions (mode, engine, domain, admin email/password) and
handles the rest. Press Enter to accept the shown defaults.

## Non-interactive / automated install

Every prompt has a matching `XDEV_*` env var; set them and pass
`XDEV_NONINTERACTIVE=1` to skip all prompts:

```bash
curl -fsSL .../deploy/install.sh | sudo XDEV_NONINTERACTIVE=1 \
  XDEV_MODE=prod XDEV_ENGINE=docker \
  XDEV_BASE_DOMAIN=apps.example.com XDEV_ACME_EMAIL=ops@example.com \
  XDEV_ADMIN_EMAIL=me@example.com XDEV_ADMIN_PASSWORD='a-strong-password' \
  bash
```

Other knobs: `XDEV_REPO` (default `leyume/xdev`), `XDEV_VERSION` (default
`latest`, or pin a tag like `v0.1.0`).

## What gets installed

| Thing | Linux | macOS |
|---|---|---|
| Binary | `/usr/local/bin/xdev` | `/usr/local/bin/xdev` |
| Config (env file) | `/etc/xdev/xdev.env` | `/usr/local/etc/xdev/xdev.env` (daemon) or `~/Library/Application Support/xdev/xdev.env` |
| Data dir (sqlite + projects) | `/var/lib/xdev` | `/var/lib/xdev` or `~/Library/Application Support/xdev/data` |
| Service | systemd `xdev.service` | launchd `com.leyume.xdev` (optional) |

All runtime configuration lives in the env file — see
[`xdev.env.example`](xdev.env.example) for every documented `XDEV_*` variable.
The service loads it (systemd `EnvironmentFile`, launchd via a wrapper), so
editing config is just: edit the env file, restart the service.

## Manual install (no installer)

```bash
# 1. Get a binary (or `make build` / `make build-all` from source)
curl -fsSLo /usr/local/bin/xdev \
  https://github.com/leyume/xdev/releases/latest/download/xdev-linux-amd64
chmod +x /usr/local/bin/xdev

# 2. Write config + create the data dir
sudo install -d /etc/xdev /var/lib/xdev
sudo cp deploy/xdev.env.example /etc/xdev/xdev.env   # then edit it

# 3. Create the admin account
XDEV_DATA=/var/lib/xdev sudo -E xdev create-admin you@example.com

# 4. Install + start the service
sudo cp deploy/xdev.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now xdev

# 5. Verify
sudo XDEV_DATA=/var/lib/xdev xdev doctor
```

## Subcommands

```bash
xdev version                  # version + go/os/arch
xdev doctor                   # preflight: engine, caddy, ports, data dir, admin
xdev create-admin you@x.com   # create the first admin (idempotent)
```

`xdev doctor` exits non-zero when a required item is missing, so you can gate on
it in automation.

## Accessing the control plane

The admin UI binds to `127.0.0.1:7331` and is **not** exposed publicly. On a
server, reach it via an SSH tunnel:

```bash
ssh -L 7331:127.0.0.1:7331 user@server   # then open http://127.0.0.1:7331
```

Public site domains are served by Caddy on `:80`/`:443` (real Let's Encrypt
certs for `environment=prod` projects; Caddy's internal CA for local `.test` /
`.localhost`).

## Usage: from install to first live site

**Server (Ubuntu/Docker), containerized Caddy:**

```bash
# 1. Install xdev (skip apt Caddy — we run it as a container)
curl -fsSL .../deploy/install.sh | sudo XDEV_NONINTERACTIVE=1 \
  XDEV_MODE=prod XDEV_ENGINE=docker XDEV_CADDY=false \
  XDEV_BASE_DOMAIN=apps.example.com XDEV_ACME_EMAIL=ops@example.com \
  XDEV_ADMIN_EMAIL=me@example.com XDEV_ADMIN_PASSWORD='a-strong-password' bash

# 2. Point xdev at a containerized Caddy — edit /etc/xdev/xdev.env:
#      XDEV_CADDY=false
#      XDEV_UPSTREAM_HOST=127.0.0.1
#      XDEV_CADDY_ADMIN_LISTEN=127.0.0.1:2019
#      XDEV_HTTPS_PORT=443   XDEV_HTTP_PORT=80   (or 8444/8081 to test behind Traefik)
sudo systemctl restart xdev

# 3. Bring up Caddy (its own lifecycle; survives reboots via restart:unless-stopped)
export XDEV_DATA=/var/lib/xdev
docker compose -f deploy/caddy/docker-compose.linux.yml up -d

# 4. Reach the control plane over an SSH tunnel
ssh -L 7331:127.0.0.1:7331 user@server      # open http://127.0.0.1:7331

# 5. In the UI: New Project -> Add App (Laravel/WordPress/static/Go/proxy)
#    -> Start -> set its domain. DNS A/AAAA for that domain must point here.
#    Caddy fetches a real cert automatically once you're on :80/:443.

# 6. Verify
sudo XDEV_DATA=/var/lib/xdev xdev doctor
docker compose -f deploy/caddy/docker-compose.linux.yml logs --tail=20
```

**Local dev (macOS), containerized Caddy:** same idea with the podman compose —
see [`deploy/caddy/README.md`](caddy/README.md) (`XDEV_UPSTREAM_HOST=host.containers.internal`,
`XDEV_CADDY_ADMIN_LISTEN=0.0.0.0:2019`, trust the local CA once). Domains use
`.test`/`.localhost` and Caddy's internal CA instead of Let's Encrypt.

**Native Caddy (either OS):** skip steps 2–3 — the installer already set
`XDEV_CADDY=true` and supervises Caddy. Go straight to the UI (step 4).

**Day-to-day:** edit `xdev.env` then `systemctl restart xdev` for config; the
Caddy container only restarts if you change *its* compose (not for port
switches — those live in `xdev.env`).

## Upgrading

The installer is **re-run safe**. On a box that already has xdev it detects the
existing install and upgrades in place: it replaces the binary and restarts the
service **without touching `XDEV_DATA` (your DB) or anything under `projects/`**,
keeps your existing admin account (no password prompt), and backs up the current
`xdev.env` to `xdev.env.bak` before writing a fresh one. Already-present engine
and Caddy are left as-is. Just re-run (optionally pin a version):

```bash
curl -fsSL .../deploy/install.sh | sudo XDEV_VERSION=v0.2.0 bash
```

## Uninstall

```bash
sudo ./deploy/uninstall.sh            # remove service + binary, keep data
sudo ./deploy/uninstall.sh --purge    # also delete the data dir + config
```

## Notes

- `/etc/hosts` management is for local dev; on a server set
  `XDEV_MANAGE_HOSTS=false` (the installer does this automatically in prod mode).
- The systemd unit grants `CAP_NET_BIND_SERVICE` so xdev/Caddy can bind 80/443
  without running networking as full root.
- macOS: binding `:443` and the LaunchDaemon need sudo; Docker Desktop can't be
  fully headless-installed (launch it once), while podman is more scriptable
  (`podman machine`).
- With containerized Caddy (`XDEV_CADDY=false`) the xdev service does **not**
  start Caddy — bring the container up separately (see
  [`deploy/caddy/README.md`](caddy/README.md)). `xdev doctor` will report the
  `caddy` binary as missing; that's expected and harmless in this mode.
