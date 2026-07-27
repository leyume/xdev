# Caddy in a container

Runs the reverse proxy as a container so the host only depends on the engine
(podman/docker) — no `brew`/`apt` Caddy. xdev stays native and drives Caddy over
its admin API, exactly as it does the supervised local binary.

## How it fits together

```
browser ──▶ caddy (container) :80/:443        publishes host ports
              │  admin :2019  ◀── xdev (native) pushes routes via /load
              │  reverse_proxy ──▶ host.containers.internal:<port>
              │      (app containers + host-Node dev servers publish host ports)
              └─ file_server + fastcgi ──▶ shared-WP tree, bind-mounted at the
                                            same absolute path as the host
```

Because a container can't reach the host's `127.0.0.1` across the Podman/Docker
VM, xdev rewrites loopback upstreams to `XDEV_UPSTREAM_HOST` when building the
Caddy config. Set it to the engine's host alias.

## Run it

```bash
export XDEV_DATA=/var/lib/xdev            # same as xdev.env; on macOS your data dir
podman compose -f deploy/caddy/docker-compose.yml up -d    # macOS
# docker compose -f deploy/caddy/docker-compose.yml up -d  # Linux server
```

Then run xdev with (in `xdev.env`):

```ini
XDEV_CADDY=false                              # stop supervising a local caddy
XDEV_UPSTREAM_HOST=host.containers.internal   # podman; host.docker.internal for docker
XDEV_CADDY_ADMIN=127.0.0.1:2019               # default; where this compose publishes admin
XDEV_CADDY_ADMIN_LISTEN=0.0.0.0:2019          # re-assert admin on every push (required)
```

`XDEV_CADDY_ADMIN_LISTEN` is not optional here: Caddy applies admin changes on
every config reload, so a pushed config that omitted the admin block would drop
the endpoint off its published `0.0.0.0:2019` binding and xdev could never push
again. Setting it makes xdev re-assert the same admin binding in every sync.

xdev degrades gracefully if the container isn't up yet (`pm.Reachable()`), and
syncs routes on the next mutation once it is.

## TLS trust (local `.test`)

The container's local CA lives in the repo data dir at
`${XDEV_DATA}/caddy/pki/authorities/local/root.crt`. Trust it once so browsers
accept `https://*.test`:

```bash
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain \
  "${XDEV_DATA}/caddy/pki/authorities/local/root.crt"     # macOS
```

## On a Linux server (Docker) — use the host-network variant

The macOS compose above **port-maps** the container and dials
`host.containers.internal`. That does not translate to Linux Docker: most xdev
upstreams (Go apps, static/Node dev servers, shared-WP fpm, shared MariaDB,
adminer) bind `127.0.0.1`, and a bridged container **cannot** reach the host's
loopback on Linux (podman/gvproxy only papers over this on macOS). Use
`docker-compose.linux.yml`, which runs Caddy with `network_mode: host` so it
reaches every upstream exactly like the native binary:

```bash
export XDEV_DATA=/var/lib/xdev
docker compose -f deploy/caddy/docker-compose.linux.yml up -d
```

`xdev.env` for the server:

```ini
XDEV_CADDY=false                        # Caddy is this container, not supervised
XDEV_UPSTREAM_HOST=127.0.0.1            # host networking => plain loopback
XDEV_CADDY_ADMIN_LISTEN=127.0.0.1:2019 # re-assert loopback admin (unexposed)
XDEV_MANAGE_HOSTS=false                 # real DNS, not /etc/hosts
XDEV_SECURE=true
XDEV_ACME_EMAIL=you@example.com         # real Let's Encrypt certs on 80/443
```

### Lifecycle ("bring up yourself")

The installer's systemd service manages the **xdev binary**, not Caddy. With
`XDEV_CADDY=false` you start Caddy as a separate stack: `docker compose … up -d`.
`restart: unless-stopped` keeps it running across reboots — no systemd unit
needed. Install xdev with `XDEV_CADDY=false` so the installer skips apt Caddy.

### Coexisting with an existing Traefik, then switching to 80/443

Listen ports come from xdev (`XDEV_HTTP_PORT` / `XDEV_HTTPS_PORT`), **not** the
compose file — so migrating alongside Traefik is an env change only, no compose
edit and no restart of the Caddy container:

```ini
# migration phase — Traefik keeps 80/443
XDEV_HTTP_PORT=8081
XDEV_HTTPS_PORT=8444
```
Restart xdev; Caddy rebinds to 8081/8444. Test routing over `http://…:8081`
(this validates Caddy reaching your upstreams — the risky part). Public
Let's Encrypt certs **can't issue on 8444** because the CA validates over 80/443
(held by Traefik); real HTTPS starts working the moment you switch. When happy:
set `XDEV_HTTP_PORT=80` / `XDEV_HTTPS_PORT=443`, free those ports on Traefik,
restart xdev.

### Testing HTTPS on the alt port

HTTPS on `:8444` behaves differently by cert type:

- **Real public domains** use Let's Encrypt, whose challenge validates over
  ports **80/443** (it ignores custom ports). With Traefik holding those, no
  cert issues on `:8444` and the TLS handshake fails. (Plain HTTP won't help
  either — Caddy 308-redirects HTTP→HTTPS.)
- **Local / internal domains** (`.test`, `.localhost`, or a non-prod project)
  use Caddy's **internal CA**, which self-signs with no external challenge — so
  HTTPS works on any port.

So to validate the container end-to-end over HTTPS before switching, add a
throwaway app on an internal domain and hit it directly:

```bash
# after: New Project (non-prod) -> Add App -> Start -> set domain e.g. probe.test
curl -k https://probe.test:8444/         # full path: Caddy -> upstream -> app body
```

A `200` with the app's body confirms routing, upstream reachability, and TLS all
work. Real Let's Encrypt certs for your actual domains kick in automatically once
you switch `XDEV_HTTPS_PORT=443` / `XDEV_HTTP_PORT=80`.

## Notes

- Admin is on `127.0.0.1:2019` (loopback), so nothing off-host can reach it.
- The `${XDEV_DATA}/wp` mount is read-only — Caddy only serves those files; the
  fpm container owns writes.
- macOS: uninstall the native Caddy once this is serving: `brew uninstall caddy`.
