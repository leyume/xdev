# Deploying xdev

xdev is a single static binary. The installer detects your OS + CPU arch,
installs a container engine (**docker** or **podman**) and **Caddy** if missing,
downloads the matching prebuilt binary from a GitHub Release (verifying its
checksum), writes config, installs a service, and creates your admin account.

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
