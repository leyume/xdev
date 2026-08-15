#!/usr/bin/env bash
# xdev — move Caddy onto the public ports (80/443) and let it manage TLS.
#
#   sudo deploy/caddy-public-ports.sh             # switch, restart xdev, watch certs land
#   sudo deploy/caddy-public-ports.sh --dry-run   # print the exact changes, touch nothing
#   sudo deploy/caddy-public-ports.sh --revert    # back to the preview ports (8081/8444)
#
# For an install that has been running Caddy alongside another proxy on high
# ports. It changes three settings in xdev.env — XDEV_HTTP_PORT, XDEV_HTTPS_PORT
# and XDEV_DISABLE_HTTPS_REDIRECT — and restarts xdev, which pushes a config
# telling Caddy to listen on the new ports. The Caddy container is host-networked
# and binds whatever the config says, so its compose file is not touched.
#
# It does NOT stop the proxy currently holding 80/443. Stop that yourself first;
# this script refuses to run while either port is occupied, because a Caddy that
# cannot bind loses every route rather than just the two ports.
#
# Certificates: while Caddy sat on high ports it could not answer ACME HTTP-01
# challenges, so it likely holds certificates for almost none of your hostnames.
# It starts issuing them the moment it owns :80, and until a name's certificate
# lands that site fails the TLS handshake — a browser shows it as unreachable,
# not as a warning. This script waits and reports each hostname as its
# certificate arrives, so you can watch the outage close instead of guessing.
#
# Rolling back is --revert plus starting your old proxy again. Do not iterate on
# this: Let's Encrypt allows 5 certificates per week for the same hostname, so a
# handful of round trips will lock you out of issuing for a week.
set -euo pipefail

# --- output helpers (same vocabulary as install.sh / caddy-hostfs.sh) ---------
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

ENV_PATH="${XDEV_ENV:-/etc/xdev/xdev.env}"
HTTPS_PORT=443
HTTP_PORT=80
REDIRECT_DISABLED=false     # false = Caddy 308s http -> https, which is what you want on 80/443
WAIT_SECS=600               # how long to watch for certificates (10 minutes)
DRY_RUN=0
REVERT=0
STAMP="$(date +%Y%m%d%H%M%S)"

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)    DRY_RUN=1; shift ;;
    --revert)     REVERT=1; shift ;;
    --env)        ENV_PATH="${2:-}"; shift 2 ;;
    --https-port) HTTPS_PORT="${2:-}"; shift 2 ;;
    --http-port)  HTTP_PORT="${2:-}"; shift 2 ;;
    --wait)       WAIT_SECS="${2:-}"; shift 2 ;;
    -h|--help)    sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

# --revert puts back the preview setup: high ports, and plain HTTP served
# directly (no redirect) because no certificate can be issued up there.
if [ "$REVERT" -eq 1 ]; then
  [ "$HTTPS_PORT" = 443 ] && HTTPS_PORT=8444
  [ "$HTTP_PORT" = 80 ] && HTTP_PORT=8081
  REDIRECT_DISABLED=true
fi

[ "$(id -u)" -eq 0 ] || die "run this as root — xdev.env is root-owned (sudo $0 $*)"
[ -f "$ENV_PATH" ] || die "no xdev env at $ENV_PATH (pass --env /path/to/xdev.env)"

# Read a KEY=value out of the env file without sourcing it (it is not a script).
env_get() { sed -n "s/^$1=//p" "$ENV_PATH" | tail -n1; }

ADMIN="$(env_get XDEV_CADDY_ADMIN)"; ADMIN="${ADMIN:-127.0.0.1:2019}"
ACME_EMAIL="$(env_get XDEV_ACME_EMAIL)"
CUR_HTTPS="$(env_get XDEV_HTTPS_PORT)"
CUR_HTTP="$(env_get XDEV_HTTP_PORT)"
CUR_REDIR="$(env_get XDEV_DISABLE_HTTPS_REDIRECT)"

# --- what we are about to do ------------------------------------------------
info "env:   $ENV_PATH"
info "admin: $ADMIN"
printf "       %-30s %s -> %s\n" "XDEV_HTTP_PORT"  "${CUR_HTTP:-<unset>}"  "$HTTP_PORT"
printf "       %-30s %s -> %s\n" "XDEV_HTTPS_PORT" "${CUR_HTTPS:-<unset>}" "$HTTPS_PORT"
printf "       %-30s %s -> %s\n" "XDEV_DISABLE_HTTPS_REDIRECT" "${CUR_REDIR:-<unset>}" "$REDIRECT_DISABLED"

case "$ACME_EMAIL" in
  ""|ops@example.com)
    warn "XDEV_ACME_EMAIL is '${ACME_EMAIL:-<empty>}' — certificates will still issue,"
    warn "but you get no expiry warnings from Let's Encrypt. Worth setting to a real address." ;;
esac

# --- the ports have to be free ----------------------------------------------
# A Caddy that cannot bind its listener drops the whole config, not just the one
# port, so every site goes down instead of just the ones being moved. Check
# before writing anything.
port_busy() {
  if have ss; then
    ss -tlnH "sport = :$1" 2>/dev/null | grep -q . && return 0
  elif have netstat; then
    netstat -tln 2>/dev/null | grep -qE "[:.]$1[[:space:]]" && return 0
  fi
  return 1
}

busy=""
for p in "$HTTP_PORT" "$HTTPS_PORT"; do
  # Caddy already holding the target port (a re-run) is fine, not a conflict.
  if [ "$p" = "$CUR_HTTP" ] || [ "$p" = "$CUR_HTTPS" ]; then continue; fi
  port_busy "$p" && busy="$busy $p"
done
if [ -n "$busy" ]; then
  err "port(s)$busy are still in use — stop the proxy holding them first"
  for p in $busy; do info "find it with:  sudo ss -tlnp 'sport = :$p'"; done
  info "then re-run this script"
  exit 1
fi
ok "ports $HTTP_PORT and $HTTPS_PORT are free"

if [ "$DRY_RUN" -eq 1 ]; then
  warn "--dry-run: stopping here, nothing changed"
  exit 0
fi

# --- edit the env file ------------------------------------------------------
# One backup for the whole edit, so a rollback is a single copy back.
cp -p "$ENV_PATH" "$ENV_PATH.$STAMP.bak"

set_env() { # key value
  if grep -q "^$1=" "$ENV_PATH"; then
    sed -i "s|^$1=.*|$1=$2|" "$ENV_PATH"
  else
    printf '%s=%s\n' "$1" "$2" >> "$ENV_PATH"
  fi
}
set_env XDEV_HTTP_PORT "$HTTP_PORT"
set_env XDEV_HTTPS_PORT "$HTTPS_PORT"
set_env XDEV_DISABLE_HTTPS_REDIRECT "$REDIRECT_DISABLED"
ok "xdev.env updated (previous kept as $(basename "$ENV_PATH").$STAMP.bak)"

# --- restart xdev -----------------------------------------------------------
# xdev pushes the whole Caddy config on startup, so the restart is what moves
# the listeners. Caddy itself is left alone — it is host-networked and rebinds
# from the pushed config.
if have systemctl && systemctl list-unit-files 2>/dev/null | grep -q '^xdev\.service'; then
  systemctl restart xdev
  ok "xdev restarted"
else
  die "no xdev systemd unit found — restart xdev yourself, then re-run to verify"
fi

# --- verify the listeners moved ---------------------------------------------
info "waiting for Caddy to pick up the new listeners…"
listen=""
for _ in $(seq 1 60); do
  sleep 1
  listen="$(curl -fsS "http://$ADMIN/config/apps/http/servers/xdev/listen" 2>/dev/null || true)"
  case "$listen" in *":$HTTPS_PORT\""*) break ;; esac
done
case "$listen" in
  *":$HTTPS_PORT\""*) ok "Caddy is listening on $listen" ;;
  "") die "Caddy admin at $ADMIN never answered — check: journalctl -u xdev -n 50, and the caddy container's logs" ;;
  *)  err "Caddy is still on $listen"
      err "roll back with:  sudo $0 --revert"
      exit 1 ;;
esac

# --- watch the certificates land --------------------------------------------
# The honest check is a real TLS handshake per hostname: 000 from curl means no
# certificate yet (or an issuance still in flight), anything else means Caddy
# answered and the site is being served.
hosts="$(curl -fsS "http://$ADMIN/config/apps/http/servers/xdev/routes" 2>/dev/null |
  python3 -c 'import json,sys
for r in json.load(sys.stdin):
    for m in r.get("match", []):
        for h in m.get("host", []):
            print(h)' 2>/dev/null || true)"

if [ -z "$hosts" ]; then
  warn "no hostnames in the pushed config — nothing to verify"
  exit 0
fi
total="$(printf '%s\n' "$hosts" | wc -l | tr -d ' ')"
if [ "$WAIT_SECS" -ge 60 ]; then WAIT_LABEL="$((WAIT_SECS / 60)) minutes"; else WAIT_LABEL="${WAIT_SECS}s"; fi
info "watching $total hostname(s) for up to $WAIT_LABEL…"

tls_ok() { # host -> 0 when the handshake completes
  local code
  code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 8 \
    --resolve "$1:$HTTPS_PORT:127.0.0.1" "https://$1:$HTTPS_PORT/" </dev/null 2>/dev/null || echo 000)"
  [ "$code" != 000 ]
}

deadline=$(( $(date +%s) + WAIT_SECS ))
pending="$hosts"
while [ -n "$pending" ] && [ "$(date +%s)" -lt "$deadline" ]; do
  still=""
  for h in $pending; do
    if tls_ok "$h"; then
      ok "$h"
    else
      still="$still $h"
    fi
  done
  pending="$(printf '%s\n' $still)"
  [ -z "$pending" ] && break
  n="$(printf '%s\n' $pending | wc -l | tr -d ' ')"
  info "$((total - n))/$total serving — still waiting on $n; next check in 15s"
  sleep 15
done

echo
if [ -z "$pending" ]; then
  ok "every hostname has a certificate and is serving on :$HTTPS_PORT"
else
  warn "these still have no certificate after $WAIT_LABEL:"
  for h in $pending; do printf "    %s\n" "$h"; done
  warn "check why with:  journalctl -u xdev -n 100  /  docker logs \$(docker ps -q -f name=caddy) 2>&1 | tail -50"
  warn "a hostname whose backend is down still gets a certificate — a 502 here is the app, not TLS"
fi

# --- what the sites actually return -----------------------------------------
echo
info "final status per hostname (HTTPS on :$HTTPS_PORT):"
for h in $hosts; do
  code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 8 \
    --resolve "$h:$HTTPS_PORT:127.0.0.1" "https://$h:$HTTPS_PORT/" </dev/null 2>/dev/null || echo 000)"
  case "$code" in
    2*|3*) printf "    %s%-34s %s%s\n" "$C_GREEN" "$h" "$code" "$C_RESET" ;;
    000)   printf "    %s%-34s no TLS%s\n" "$C_RED" "$h" "$C_RESET" ;;
    *)     printf "    %s%-34s %s%s\n" "$C_YELLOW" "$h" "$code" "$C_RESET" ;;
  esac
done

echo
info "to roll back:  sudo $0 --revert   (then start your old proxy again)"
info "the old env is at $ENV_PATH.$STAMP.bak"
