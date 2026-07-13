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

1. **e2e verify locally** — create a mail app in a test project, both
   containers up, admin + webmail reachable, send/receive between two local
   mailboxes.
2. *(Optional, later)* xdev-native domain/mailbox CRUD pages calling
   Stalwart's REST API — only if hopping to the Stalwart admin gets old.
   YAGNI until then.

## Migration runbook — Mailcow → Stalwart (5 domains)

Prerequisites: web-side proxy flip done (PLANx runbook), Linode resized to
4GB. Mailcow keeps serving mail on 25/465/587/993 until the final step.

1. Create the mail app in xdev **on high ports** (the local port scheme) so it
   coexists with Mailcow.
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
5. **Cutover:** final `imapsync` delta pass → stop Mailcow → switch the mail
   app's compose to the real ports → restart. Downtime is minutes, and
   sending servers retry for days — nothing is lost.
6. **DNS: nothing changes.** Same IP → MX, SPF, PTR untouched; DKIM imported.
7. Not migrated automatically: sieve filters (both sides speak Sieve — copy
   scripts per user), SOGo calendars/contacts (export `.ics`/`.vcf`, import
   into Stalwart's CalDAV/CardDAV or elsewhere), ActiveSync (gone — clients
   reconnect over IMAP), spam training (starts fresh).
8. Keep Mailcow stopped-not-deleted (especially the vmail volume) for a few
   weeks; rollback is stop-Stalwart / start-Mailcow.
