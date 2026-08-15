# Deploying xdev

xdev is a single static binary. The installer detects your OS + CPU arch,
installs a container engine (**docker** or **podman**), downloads the matching
prebuilt binary from a GitHub Release (verifying its checksum), writes config,
installs a service, and creates your admin account.

The reverse proxy (Caddy) runs one of three ways — the installer prompts you:

- **Container** (default): the installer **generates a Caddy compose stack and
  starts it**, so the host needs nothing but the engine (`XDEV_CADDY=false`).
  Recommended, and no manual steps. Internals: [`deploy/caddy/README.md`](caddy/README.md).
- **Native**: apt/brew Caddy, supervised by xdev (`XDEV_CADDY=true`).
- **Self-managed**: you already run a proxy — the installer touches Caddy not at
  all and just records how xdev reaches it (`XDEV_UPSTREAM_HOST`, `XDEV_CADDY_ADMIN_LISTEN`).

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

**Server (Ubuntu/Docker) — the installer does everything, including the Caddy container:**

```bash
curl -fsSL https://raw.githubusercontent.com/leyume/xdev/main/deploy/install.sh | sudo bash
```
Answer the prompts: `Mode=prod`, `Engine=docker`, `Caddy=container`, ports
(`443`/`80`, or `8444`/`8081` to sit beside an existing proxy like Traefik),
base domain, Let's Encrypt email, admin. The installer then installs Docker,
downloads the binary, writes `xdev.env`, **generates and starts the Caddy
container**, installs the systemd service, and creates your admin. Then:

```bash
# Reach the private control plane over an SSH tunnel
ssh -L 7331:127.0.0.1:7331 user@server           # open http://127.0.0.1:7331
# In the UI: New Project -> Add App (Laravel/WordPress/static/Go/proxy) -> Start
#   -> set its domain (DNS A/AAAA -> this server). Real cert issues automatically on :80/:443.
sudo XDEV_DATA=/var/lib/xdev xdev doctor         # a ports ✗ is expected — the container holds them
```

Non-interactive equivalent:
```bash
curl -fsSL .../deploy/install.sh | sudo XDEV_NONINTERACTIVE=1 \
  XDEV_MODE=prod XDEV_ENGINE=docker XDEV_CADDY_MODE=container \
  XDEV_HTTPS_PORT=8444 XDEV_HTTP_PORT=8081 \
  XDEV_BASE_DOMAIN=apps.example.com XDEV_ACME_EMAIL=ops@example.com \
  XDEV_ADMIN_EMAIL=me@example.com XDEV_ADMIN_PASSWORD='a-strong-password' bash
```

**Native Caddy:** choose `native` at the prompt (or `XDEV_CADDY_MODE=native`) —
xdev installs and supervises Caddy; no container. **Local dev (macOS/podman):**
choose `container` with `Engine=podman`; the installer generates the port-mapped
variant and uses `host.containers.internal`. See [`deploy/caddy/README.md`](caddy/README.md)
for the generated stacks and local-CA trust.

**Day-to-day:** edit `xdev.env` then `systemctl restart xdev` for config; port
switches (e.g. `8444/8081` → `443/80`) live in `xdev.env` — the container stack
only changes if you edit *it*.

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

### Deploying a build of your own

`update.sh` moves the binary and nothing else — no prompts, no config rewrite —
which is what you want when you're deploying a checkout rather than a release:

```bash
sudo ./deploy/update.sh                 # build this checkout, install, restart
sudo ./deploy/update.sh --binary ./xdev # install a binary you built elsewhere
sudo ./deploy/update.sh --no-build      # ditto, using ./xdev in the repo root
sudo ./deploy/update.sh --dry-run       # say what would happen, change nothing
```

It backs the current binary up to `/usr/local/bin/xdev.<timestamp>.bak` (keeping
the newest three), swaps by rename so the running process is never truncated,
and if the service doesn't come back it restores the backup, restarts, and
prints the log.

"Nothing to do" means the installed binary is **byte-identical** to the new one,
not that the two report the same version: builds from a dirty working tree all
carry the same `git describe` string, so comparing versions would skip every
rebuild between commits.

## A server that already runs something on 80/443

The installer checks those ports before it asks about Caddy. If another server
holds them it says which process, defaults the Caddy question to **self**, and —
in a non-interactive install — refuses rather than binding them.

That is stricter than it looks for a reason: Caddy binds all-or-nothing, so a
listener it cannot open makes it drop its *entire* config. Losing the fight for
:80 would take down every site xdev serves, not just that port.

An upgrade is never blocked by its own proxy: a port held by this install's
Caddy — the supervised binary, or the container stack the installer generated —
is recognised as ours and ignored, provided it was already configured for that
port.

## A server with no domain (or one already running nginx)

Two settings, and xdev stays off 80/443 entirely while apps are reached by port:

```bash
XDEV_CADDY_MODE=self        # the installer touches Caddy not at all
XDEV_PUBLIC_HOST=203.0.113.10   # how this server is reached
```

`XDEV_CADDY_MODE=self` means nothing xdev owns binds the public ports, so an
existing nginx/Apache keeps them. `XDEV_PUBLIC_HOST` makes the **Domain field
optional**: an app created without one gets no hostname and no Caddy route, and
is addressed as `http://203.0.113.10:<its host port>` — the address the UI links
to, and the one written into a Laravel app's `APP_URL`. Open those ports in your
firewall; they come from the range 20000–29999 and each app's is on its card.

Keep projects on **dev**, not prod: prod hands every hostname to Let's Encrypt,
and a domainless install has none to give it.

Three things this mode does not cover, by design:

- **Serve-mode static apps** publish no port at all — Caddy serves their files,
  keyed by hostname — so they still need a domain. Anything that runs a process
  (command-mode static, Go, and every container type) is fine.
- **A compose stack's extra domains** still need hostnames: a slot's port is
  recorded on its domain row, so a slot with no name has nowhere to keep its
  port. The stack's first port (`${PORT}`) works normally.
- **The deploy endpoints** are published per hostname, so a port-only app has no
  public URL for them — the app's own port reaches the app, not the control
  plane. Forward `/_xdev/hook/` from whatever fronts the machine, or use the ssh
  route below, which needs nothing exposed. The app's settings page shows the
  path and the on-this-machine URL instead of a payload URL.

### Triggering a deploy from CI, over SSH

The deploy endpoints are normally published on the app's own hostname, which
needs a domain and a working proxy. `xdev-deploy.sh` is the alternative for a
server that has neither: it runs **on the server** and posts a signed webhook
request to the control plane's own listener over loopback, so CI needs no
inbound port, no proxy rule, and no certificate.

```bash
sudo install -m 0755 deploy/xdev-deploy.sh /usr/local/bin/xdev-deploy
sudo install -d -m 0750 -o deploy -g deploy /etc/xdev/deploy
# HOOK_ID / HOOK_SECRET come from the app's settings page, after you turn its
# webhook on. REF is the branch the app tracks.
sudo -u deploy tee /etc/xdev/deploy/myapp.conf >/dev/null <<'EOF'
HOOK_ID=...
HOOK_SECRET=...
REF=main
EOF
sudo chmod 0600 /etc/xdev/deploy/myapp.conf
```

Then give CI an ssh key pinned to that one command, in the deploy account's
`authorized_keys`:

```
command="/usr/local/bin/xdev-deploy myapp",no-port-forwarding,no-agent-forwarding,no-pty ssh-ed25519 AAAA... ci@github
```

A forced command replaces whatever the client asks to run, so the workflow step
is just `ssh deploy@your-server` with no arguments — and a leaked CI key can
deploy that one app and do nothing else. The webhook secret never leaves the
server.

This works for any git-backed app, including Laravel: the deploy is a **pull**,
so the server needs outbound access to the repository (and the app's deploy key
for a private one), while CI only presses the button.

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
