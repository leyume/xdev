# PLAN_MAIL — mail on xdev (Stalwart + SnappyMail)

Mail for the 5 mail-carrying domains, replacing Mailcow on the same Linode
(same IP kept — that preserves PTR/rDNS, SPF, and IP reputation for free).

## Why Stalwart (not Mailcow, not hand-rolled)

- xdev never implements mail protocols — SMTP/IMAP/DKIM/spam is decades of
  edge cases. Mail runs as a normal xdev app: proven software in containers,
  xdev provides domain, TLS, lifecycle, logs, metrics.
- Stalwart = one container doing SMTP + IMAP + JMAP with a built-in web admin
  (domains, mailboxes, aliases, DKIM, and it lists the exact DNS records per
  domain) and a REST API. ~300–500MB — fits the 4GB budget.
- Mailcow is 15+ containers (Postfix/Dovecot/Rspamd/SOGo/MySQL/Redis/nginx),
  wants 6GB+ RAM, and insists on owning its own compose lifecycle — it fights
  any platform that manages it. SnappyMail covers webmail; SOGo's
  calendar/contacts are the one real loss (see migration notes).

## What's built (this branch)

The `mail` app type:

- `internal/templates/files/mail/compose.yml.tmpl` — Stalwart + SnappyMail
  services on the app's private network + the project network.
- Catalog entry in `internal/templates/templates.go` (`mail`).
- Stalwart's admin UI rides the generalized secondary-domain path
  (`secondaryPrefix` in `internal/apps/apps.go` — the mechanism built for
  Laravel's Adminer): default `admin.<app-domain>`, proxied by Caddy with TLS.
- SnappyMail webmail at the app's main domain.
- Ports: prod publishes 25/465/587/993; local uses high ports
  (2525/4650/5870/9930 — rootless podman can't bind <1024).
- Checks: `TestMailPorts` + the catalog-wide render test; both images verified
  present on Docker Hub (`stalwartlabs/stalwart`, `djmaze/snappymail`).

## First-run notes

- **Stalwart admin password** is auto-generated and printed in the `mail`
  service logs on first start (xdev's Logs tab). Log in at the admin domain.
- **SnappyMail admin password** is written into the webmail volume at
  `_data_/_default_/admin_password.txt`; admin panel at `/?admin` on the app
  domain. One-time setup: point the domain(s) at IMAP host `mail:143`, SMTP
  `mail:587` (service name on the app's internal network).
- Other apps on the same project network can send via `mail:587`.

## Remaining dev

1. ~~**e2e verify locally**~~ ✅ Full wizard run + send/receive proven
   (SMTPS submit → IMAPS fetch, DKIM-signed). The e2e surfaced and fixed two
   compose/wiring bugs:
   - **Persistence:** Stalwart v0.16 stores everything in `/var/lib/stalwart`,
     not `/opt/stalwart` — the volume now mounts the former.
   - **Admin console:** 8080 is Stalwart's *bootstrap/recovery* listener; the
     real admin UI serves over HTTPS on 443 post-setup. Compose now publishes
     443 (Caddy proxies the admin domain to it over TLS, verification skipped
     for the loopback self-signed cert — `proxy.reverseProxyHandler`) and keeps
     8080 for the one-time first-run wizard. `store.ProxyRoutes` routes the mail
     admin domain via an `https://127.0.0.1:<port>` upstream.
   - Default submission listener is 465 (SMTPS), not 587 — dropped the 587
     publish and pointed the SnappyMail note at 465.
2. *(Optional, later)* xdev-native domain/mailbox CRUD pages calling
   Stalwart's REST API — only if hopping to the Stalwart admin gets old.
   YAGNI until then.

## Migration runbook — Mailcow → Stalwart (5 domains)

Prerequisites: web-side proxy flip done (PLANx runbook — ngx on 80/443),
Linode at 4GB with Mailcow's ClamAV off (`SKIP_CLAMD=y`) for headroom. Mailcow keeps serving mail on 25/465/587/993 until the final step.

1. Create the mail app in xdev (prod). Mailcow holds the standard ports, so
   xdev writes high-port overrides into the app's `_/.env` (Env tab) at create
   time — SMTP 2525, SMTPS 4650, submission 5870, IMAP 1430, IMAPS 9930,
   POP3 1100, POP3S 9950, Sieve 14190, setup 18080 — and Stalwart starts beside
   Mailcow. (With the ports free it uses the standard ones directly.)
2. In Stalwart's admin, recreate all 5 domains, mailboxes, and aliases
   (export the lists from Mailcow's admin first; script via Stalwart's REST
   API if there are many).
3. **Import each domain's DKIM private key from Mailcow** into Stalwart so the
   existing DKIM DNS records stay valid — the one step that silently hurts
   deliverability if skipped. Passwords: Mailcow's bcrypt hashes can be copied
   into Stalwart, or just set fresh ones for a handful of users.
4. `imapsync` every mailbox: Mailcow Dovecot (`:993`) → Stalwart (temp IMAPS
   port). Preserves folders, flags, dates; resumable; re-runs copy only
   deltas.
5. **Cutover:** stop Mailcow's postfix (senders queue and retry) → final
   `imapsync` delta pass → stop Mailcow → delete the `MAIL_*_PORT` lines from
   the mail app's Env tab → restart the app. Downtime is minutes, and
   sending servers retry for days — nothing is lost.
6. **DNS: nothing changes.** Same IP → MX, SPF, PTR untouched; DKIM imported.
7. Not migrated automatically: sieve filters (both sides speak Sieve — copy
   scripts per user), SOGo calendars/contacts (export `.ics`/`.vcf`, import
   into Stalwart's CalDAV/CardDAV or elsewhere), ActiveSync (gone — clients
   reconnect over IMAP), spam training (starts fresh).
8. Keep Mailcow stopped-not-deleted (especially the vmail volume) for a few
   weeks; rollback is stop-Stalwart / start-Mailcow.

## Migration log — done 2026-11-20

Mailcow → Stalwart v1.1.1 on the same host, app `zail` in project bizev
(webmail sail.leyume.com, admin zail.leyume.com). 7 domains, 33 mailboxes,
all mail copied, DKIM keys carried over; downtime = the minutes Mailcow's
SMTP was stopped for the final delta. What differed from the runbook above:

- **Stalwart is v1.x**, not 0.16 (no in-place upgrade between them — the
  image is now pinned). Config is objects over JMAP (`x:Domain`, `x:Account`,
  `x:DkimSignature`, `x:Certificate`, …); account credentials are an id-keyed
  map; Mailcow's `{BLF-CRYPT}$2y$…` imports as `$2y$…` (users kept passwords).
- **TLS:** the proxy is ngx; xdev copies ngx's ACME certs for
  `MAIL_TLS_HOSTS` into `_/tls/` (owned by Stalwart's uid 2000) and restarts
  the mail service on renewal. `STALWART_PUBLIC_URL` (from `MAIL_PUBLIC_URL`)
  keeps the web login on the admin domain while EHLO stays mail.leyume.com.
- **Listeners:** v1 setup creates 25/465/993/995/4190 only; 587/143/110 were
  added by hand in the admin.
- **Auto-ban:** every host→container connection arrives from a Docker bridge
  gateway, so repeated banner probes got that gateway banned for "port
  scanning" (and with it ngx → admin 502). Fix: allow-list 172.16.0.0/12,
  192.168.0.0/16, 10.0.0.0/8 in Stalwart (Settings > Security > Allowed IPs).
- **Found on the way:** Mailcow had served an expired cert (Sep 2024) on all
  mail ports; leyume.com/barjaconstruction.com DKIM DNS didn't match Mailcow's
  keys (fixed); loafbasket.com DNS is broken at the registrar.
- Scripts used (outside the repo): ~/mail-migrate/{mailcow2stalwart.py,
  sync.sh, cutover.sh, rollback.sh, snappymail-domains.php}.
