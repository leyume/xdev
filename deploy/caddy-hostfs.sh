#!/usr/bin/env bash
# xdev — let a containerized Caddy serve static apps from any host path.
#
#   sudo deploy/caddy-hostfs.sh             # apply, restart Caddy + xdev, verify
#   sudo deploy/caddy-hostfs.sh --dry-run   # print the exact changes, touch nothing
#   sudo deploy/caddy-hostfs.sh --revert    # put it back
#
# Serve-mode static apps are file-served by Caddy straight off disk: from
# <data>/projects/<project>/<app> for a managed app, or from any folder you
# pointed one at (the App folder field). A Caddy running in a container can only
# open what is bind-mounted into it, so those real host paths resolve to nothing
# inside it and every request 404s — with an empty body, which is what a browser
# shows as "No web page was found for the web address".
#
# The fix is one mount and one setting: the host filesystem is mounted READ-ONLY
# at /hostfs, and XDEV_CADDY_ROOT_PREFIX=/hostfs makes xdev rewrite every
# file_server root to a path Caddy can actually open. It covers every static app
# at once — the ones under projects/ and any folder you point at later — with no
# extra container and no further mounts.
#
# Shared-WordPress docroots are deliberately NOT rewritten: the wp-host fpm
# container opens that same path to run PHP, so both sides must keep seeing the
# real one. Its existing same-path mount already covers it.
#
# Safe to re-run. Exits without changing anything when Caddy is not
# containerized (a Caddy on the host already sees the real paths, and setting a
# prefix would break the ones that work today).
#
# Note: /hostfs gives the Caddy container read-only visibility of the whole
# filesystem. It cannot write through it. If you would rather keep the exposure
# narrow, skip this script and add same-path mounts per folder instead — see
# --help output at the end.
set -euo pipefail

# --- output helpers (same vocabulary as install.sh / update.sh) --------------
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_RESET=$'\033[0m'; C_BLUE=$'\033[34m'; C_GREEN=$'\033[32m'
  C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'; C_BOLD=$'\033[1m'
else
  C_RESET=""; C_BLUE=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_BOLD=""
fi
info() { printf "%s▸%s %s\n" "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf "%s✓%s %s\n" "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf "%s!%s %s\n" "$C_YELLOW" "$C_RESET" "$*" >&2; }
err()  { printf "%s✗%s %s\n" "$C_RED" "$C_RESET" "$*" >&2; }
die()  { err "$*"; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }
as_root() { if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo "$@"; fi; }

MOUNT_LINE='      - "/:/hostfs:ro"'
PREFIX="/hostfs"
ENV_PATH="${XDEV_ENV:-/etc/xdev/xdev.env}"
DRY_RUN=0
REVERT=0
STAMP="$(date +%Y%m%d%H%M%S)"

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --revert)  REVERT=1; shift ;;
    --env)     ENV_PATH="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,31p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

[ -f "$ENV_PATH" ] || die "no xdev env at $ENV_PATH (pass --env /path/to/xdev.env)"
COMPOSE="$(dirname "$ENV_PATH")/caddy/docker-compose.yml"

# Read a KEY=value out of the env file without sourcing it (it is not a script).
env_get() { sed -n "s/^$1=//p" "$ENV_PATH" | tail -n1; }

ENGINE="$(env_get XDEV_ENGINE)"; ENGINE="${ENGINE:-docker}"
HTTP_PORT="$(env_get XDEV_HTTP_PORT)"; HTTP_PORT="${HTTP_PORT:-80}"
ADMIN="$(env_get XDEV_CADDY_ADMIN)"; ADMIN="${ADMIN:-127.0.0.1:2019}"
CUR_PREFIX="$(env_get XDEV_CADDY_ROOT_PREFIX)"

# --- is this even the right fix? --------------------------------------------
# No generated compose file means Caddy is native or self-managed. Then the
# paths xdev writes are already the paths Caddy opens, and a prefix would send
# it looking under a directory that does not exist. Say so and stop.
if [ ! -f "$COMPOSE" ]; then
  warn "no Caddy container stack at $COMPOSE"
  info "Caddy appears to run on the host, where file_server roots already resolve."
  info "Nothing to do — if static apps still 404 there, check the folder's"
  info "permissions for the user Caddy runs as, not its mounts."
  exit 0
fi

# --- what we are about to do ------------------------------------------------
if [ "$REVERT" -eq 1 ]; then
  info "reverting: removing the /hostfs mount and clearing XDEV_CADDY_ROOT_PREFIX"
else
  info "compose:  $COMPOSE"
  info "env:      $ENV_PATH"
  info "engine:   $ENGINE"
  if grep -q '/hostfs' "$COMPOSE"; then
    ok "mount already present"
  else
    info "will add to the caddy service:$MOUNT_LINE"
  fi
  if [ "$CUR_PREFIX" = "$PREFIX" ]; then
    ok "XDEV_CADDY_ROOT_PREFIX already $PREFIX"
  elif [ -n "$CUR_PREFIX" ]; then
    warn "XDEV_CADDY_ROOT_PREFIX is currently '$CUR_PREFIX' — it will be set to $PREFIX"
  else
    info "will set XDEV_CADDY_ROOT_PREFIX=$PREFIX"
  fi
fi

if [ "$DRY_RUN" -eq 1 ]; then
  warn "--dry-run: stopping here, nothing changed"
  exit 0
fi

# --- edit the compose file --------------------------------------------------
# Both generated stacks (docker host-network, podman port-mapped) have exactly
# one service with one volumes: block, so inserting after that key is
# unambiguous. Everything is written to a temp file and moved into place, so an
# interrupted run cannot leave a half-written stack.
tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT

if [ "$REVERT" -eq 1 ]; then
  grep -v '/hostfs' "$COMPOSE" > "$tmp"
else
  if grep -q '/hostfs' "$COMPOSE"; then
    cp "$COMPOSE" "$tmp"
  else
    awk -v line="$MOUNT_LINE" '
      { print }
      !done && /^[[:space:]]*volumes:[[:space:]]*$/ { print line; done = 1 }
    ' "$COMPOSE" > "$tmp"
    grep -q '/hostfs' "$tmp" || die "could not find a volumes: block in $COMPOSE — add $MOUNT_LINE by hand"
  fi
fi

if ! cmp -s "$COMPOSE" "$tmp"; then
  as_root cp -p "$COMPOSE" "$COMPOSE.$STAMP.bak"
  as_root cp "$tmp" "$COMPOSE"
  ok "compose updated (previous kept as $(basename "$COMPOSE").$STAMP.bak)"
else
  ok "compose already correct"
fi

# --- edit the env file ------------------------------------------------------
want_prefix="$PREFIX"
[ "$REVERT" -eq 1 ] && want_prefix=""

if [ "$CUR_PREFIX" != "$want_prefix" ]; then
  as_root cp -p "$ENV_PATH" "$ENV_PATH.$STAMP.bak"
  if grep -q '^XDEV_CADDY_ROOT_PREFIX=' "$ENV_PATH"; then
    as_root sed -i "s|^XDEV_CADDY_ROOT_PREFIX=.*|XDEV_CADDY_ROOT_PREFIX=$want_prefix|" "$ENV_PATH"
  else
    printf '%s\n' \
      '# Where Caddy sees the host filesystem (containerized Caddy serves static' \
      '# apps from real host paths). Blank when Caddy runs on the host.' \
      "XDEV_CADDY_ROOT_PREFIX=$want_prefix" | as_root tee -a "$ENV_PATH" >/dev/null
  fi
  ok "XDEV_CADDY_ROOT_PREFIX=${want_prefix:-<empty>} (previous env kept as $(basename "$ENV_PATH").$STAMP.bak)"
else
  ok "env already correct"
fi

# --- restart Caddy ----------------------------------------------------------
# docker's daemon needs root; podman is rootless and must run as the invoking
# user even when this script was started with sudo.
info "recreating the Caddy container (brief interruption for every site it fronts)…"
if [ "$ENGINE" = docker ]; then
  as_root docker compose -f "$COMPOSE" up -d
elif [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
  sudo -u "$SUDO_USER" "$ENGINE" compose -f "$COMPOSE" up -d
else
  "$ENGINE" compose -f "$COMPOSE" up -d
fi
ok "Caddy restarted"

# --- restart xdev so it re-pushes routes with the new prefix -----------------
if have systemctl && systemctl list-unit-files 2>/dev/null | grep -q '^xdev\.service'; then
  as_root systemctl restart xdev
  ok "xdev restarted"
else
  warn "no xdev systemd unit found — restart xdev yourself so it re-pushes routes"
fi

# --- verify -----------------------------------------------------------------
# The pushed config is the honest check: it says what Caddy was actually told,
# and a request against a real static host says whether it can open the files.
info "waiting for xdev to push routes…"
cfg=""
for _ in $(seq 1 30); do
  sleep 1
  cfg="$(curl -fsS "http://$ADMIN/config/apps/http/servers/xdev/routes" 2>/dev/null || true)"
  [ -n "$cfg" ] && break
done
[ -n "$cfg" ] || die "Caddy admin at $ADMIN never answered — check: $ENGINE logs caddy"

if [ "$REVERT" -eq 1 ]; then
  ok "reverted; roots are back to real host paths"
  exit 0
fi

case "$cfg" in
  *'"root":"'"$PREFIX"'/'*) ok "file_server roots are now under $PREFIX" ;;
  *'"root":"'*) die "roots are still unprefixed — is XDEV_CADDY_ROOT_PREFIX set in $ENV_PATH, and did xdev restart?" ;;
  *) warn "no file_server roots in the config (no serve-mode static apps yet) — nothing to verify" ; exit 0 ;;
esac

# Ask Caddy for each static host over the plain HTTP port. 200 means the
# container opened the files; 404 means it still cannot see them.
hosts=""
if have python3; then
  hosts="$(printf '%s' "$cfg" | python3 -c '
import json,sys
for r in json.load(sys.stdin):
    if any(h.get("handler")=="vars" and str(h.get("root","")).startswith("'"$PREFIX"'/") for h in r.get("handle",[])):
        for m in r.get("match",[]):
            for h in m.get("host",[]):
                print(h)
' 2>/dev/null || true)"
fi

if [ -z "$hosts" ]; then
  info "config looks right; check a static site yourself on port $HTTP_PORT"
  exit 0
fi

fail=0
for h in $hosts; do
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H "Host: $h" "http://127.0.0.1:$HTTP_PORT/" || echo 000)"
  if [ "$code" = 200 ]; then
    ok "$h -> $code"
  else
    err "$h -> $code"
    fail=1
  fi
done

if [ "$fail" -eq 0 ]; then
  ok "every static app is being served"
else
  warn "a site still isn't serving. Most likely its folder has no index.html,"
  warn "or the path in its App folder setting doesn't exist. Check with:"
  warn "  $ENGINE exec \$($ENGINE ps -q -f name=caddy) ls /hostfs<the app folder>"
  exit 1
fi
