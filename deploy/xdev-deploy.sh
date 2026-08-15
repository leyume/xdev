#!/usr/bin/env bash
# xdev — trigger a deploy from outside, without exposing anything to the internet.
#
#   xdev-deploy myapp            # deploy the app named in /etc/xdev/deploy/myapp.conf
#   xdev-deploy myapp <sha>      # same, recording which commit asked for it
#
# For servers that have no domain, or that already run another web server on
# 80/443 and should keep it that way. xdev's deploy endpoint is a normal route
# on the control plane's own listener (127.0.0.1:7331 by default), so anything
# that can reach loopback can trigger a deploy — including an ssh session. CI
# then needs no inbound port, no reverse-proxy rule, and no certificate.
#
# The app's webhook secret stays on this machine. CI holds only an ssh key, and
# that key should be restricted to this command (see the authorized_keys line at
# the bottom of this comment), so a leaked CI credential can deploy one app and
# do nothing else at all.
#
# Setup, once per app:
#
#   1. Turn the webhook on from the app's settings page. That mints the hook id
#      and secret — xdev shows both. (You will not use the payload URL it
#      displays: it is built from the app's domain, which is not how you are
#      reaching it.)
#
#   2. Write them here, readable only by the account CI logs in as:
#
#        sudo install -d -m 0750 -o deploy -g deploy /etc/xdev/deploy
#        sudo tee /etc/xdev/deploy/myapp.conf >/dev/null <<'EOF'
#        HOOK_ID=<the id from the payload URL's last path segment>
#        HOOK_SECRET=<the secret>
#        REF=main
#        EOF
#        sudo chown deploy:deploy /etc/xdev/deploy/myapp.conf
#        sudo chmod 0600 /etc/xdev/deploy/myapp.conf
#
#   3. Give CI a key that can do only this:
#
#        command="/usr/local/bin/xdev-deploy myapp",no-port-forwarding,\
#        no-agent-forwarding,no-X11-forwarding,no-pty ssh-ed25519 AAAA... ci@github
#
#      The forced command wins over whatever the client asks to run, so the app
#      name comes from this file rather than from the caller. $SSH_ORIGINAL_COMMAND
#      is deliberately ignored.
#
# Then from a workflow:  ssh deploy@your-server
# (no arguments — the forced command supplies them).
set -euo pipefail

CONF_DIR="${XDEV_DEPLOY_CONF_DIR:-/etc/xdev/deploy}"
ADDR="${XDEV_ADDR:-127.0.0.1:7331}"

die() { printf 'xdev-deploy: %s\n' "$*" >&2; exit 1; }

app="${1:-}"
[ -n "$app" ] || die "usage: xdev-deploy <app> [commit-sha]"
# The app name indexes a file path, so it may not wander out of CONF_DIR. With a
# forced command this is already fixed text, but the script is also run by hand.
case "$app" in
  */*|.*) die "invalid app name: $app" ;;
esac

conf="$CONF_DIR/$app.conf"
[ -f "$conf" ] || die "no config at $conf (see the comment at the top of this script)"

# Read KEY=value without sourcing the file. A webhook secret is random bytes and
# routinely contains $, ` and " — sourcing would expand them (or run them), and
# the value that reached the HMAC would not be the value on the settings page.
# Same reason deploy/caddy-public-ports.sh reads xdev.env this way.
conf_get() { # key -> value, last occurrence wins, optional surrounding quotes stripped
  sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "$conf" | tail -n1 |
    sed -e 's/^"\(.*\)"$/\1/' -e "s/^'\(.*\)'\$/\1/"
}
HOOK_ID="$(conf_get HOOK_ID)"
HOOK_SECRET="$(conf_get HOOK_SECRET)"
REF="$(conf_get REF)"
[ -n "$HOOK_ID" ]     || die "$conf has no HOOK_ID"
[ -n "$HOOK_SECRET" ] || die "$conf has no HOOK_SECRET"
REF="${REF:-main}"

# The commit is informational — xdev deploys whatever the tracked branch points
# at when it pulls. Accept only a hex sha so nothing from a caller reaches the
# body unchecked.
sha="${2:-}"
case "$sha" in
  "") sha="0000000000000000000000000000000000000000" ;;
  *[!0-9a-fA-F]*) die "commit sha is not hexadecimal: $sha" ;;
esac

command -v openssl >/dev/null || die "openssl is required to sign the request"
command -v curl >/dev/null    || die "curl is required"

# The body xdev expects from GitHub: which branch moved, and whether it was
# deleted. It checks the branch against the one the app tracks, so REF has to
# match the app's branch or the deploy is (correctly) ignored.
body="$(printf '{"ref":"refs/heads/%s","deleted":false,"after":"%s"}' "$REF" "$sha")"

# HMAC-SHA256 over the exact bytes sent, hex, prefixed — the same scheme GitHub
# uses, which is why xdev accepts this request identically to a real webhook.
# -hmac takes the secret as an argument: fine here (the process is this host's
# own, and the secret never crosses the network).
sig="$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$HOOK_SECRET" | awk '{print $NF}')"

# Print the answer either way, and exit non-zero on an error status — that is
# what makes a CI step go red with something readable rather than silently
# "succeeding" on a 401.
#
# Done by hand rather than with --fail-with-body: curl only grew that in 7.76,
# and Ubuntu 20.04 ships 7.68, where it is a hard "option is unknown" error.
# Plain --fail is no substitute — it discards the body, which is the only place
# xdev says *why* it refused.
resp="$(mktemp)"
trap 'rm -f "$resp"' EXIT

code="$(curl -sS -o "$resp" -w '%{http_code}' -X POST "http://$ADDR/_xdev/hook/$HOOK_ID" \
  -H "X-GitHub-Event: push" \
  -H "X-Hub-Signature-256: sha256=$sig" \
  -H "Content-Type: application/json" \
  --data "$body")" || die "could not reach xdev at $ADDR — is the service running?"

cat "$resp"; echo
case "$code" in
  2*) ;;
  404) die "HTTP 404 — no app has that HOOK_ID, or its webhook is switched off" ;;
  401) die "HTTP 401 — HOOK_SECRET does not match the one on the app's settings page" ;;
  *)   die "xdev refused the request (HTTP $code)" ;;
esac
