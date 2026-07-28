#!/bin/sh
# xdev Laravel entrypoint (POSIX sh). On first boot it bootstraps a fresh Laravel
# install + Octane/Swoole into the bind-mounted app/ dir, generates the app key,
# waits for the database, and migrates; then it serves with Swoole. On later
# starts everything is already in place, so it skips straight to serving.
#
# DB_* / REDIS_* come from the compose `environment:` block. The published host
# port maps to :8000 below.
set -e
cd /var/www/html

log() { echo "▶ xdev: $*"; }

# Production (a prod-environment project) installs without dev dependencies and
# caches config/routes at the end. Everything else is identical, so a Laravel
# app created in a prod project still comes up instead of needing a deploy
# pipeline that doesn't exist yet.
# (`if` rather than `test && x=y`: under `set -e` a failing test as the last
# command of an && list would abort the script.)
prod=0
install_flags="--no-interaction"
if [ "$APP_ENV" = "production" ]; then
  prod=1
  install_flags="--no-interaction --no-dev --optimize-autoloader"
fi

# Bootstrap-only mode: we are a throwaway container xdev runs purely to scaffold
# app/ with composer, because the image the real stack serves from may not have
# composer at all. We resolve dependencies for a PHP that isn't ours, so ignore
# platform requirements — the runtime image is what has to satisfy them, and it
# will, since it's the one that runs the app.
# base_flags is what's safe on every composer subcommand; install_flags adds the
# prod tuning that only `composer install` accepts (create-project and require
# reject --optimize-autoloader / --no-dev).
#
# --no-scripts is what keeps the composer/artisan split honest. Laravel's
# create-project scripts run `artisan key:generate` and `artisan migrate
# --graceful`, and `require` runs `artisan package:discover` — all of which boot
# the app on a PHP that isn't the runtime's, against a database this container
# can't reach. (That migrate is what failed here: exit 1 from
# post-create-project-cmd.) Everything they would have done is either redone
# below in the runtime half or rebuilt lazily by Laravel itself: the package
# manifest regenerates on first request when bootstrap/cache has no packages.php.
bootstrap_flags=""
if [ "$XDEV_BOOTSTRAP_ONLY" = "1" ]; then
  bootstrap_flags="--ignore-platform-reqs --no-scripts"
fi
base_flags="--no-interaction $bootstrap_flags"
install_flags="$install_flags $bootstrap_flags"

# 1. Install Laravel on first boot. Build into a temp dir and copy in (incl.
#    dotfiles) so a stray file in app/ (e.g. a Finder .DS_Store) doesn't trip
#    composer's "directory not empty" guard.
if [ ! -f artisan ]; then
  log "installing Laravel (composer create-project)…"
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2086 # base_flags is a deliberate word list
  composer create-project laravel/laravel "$tmp" $base_flags
  cp -a "$tmp"/. /var/www/html/
  rm -rf "$tmp"
fi

# 2. Ensure PHP dependencies are present.
if [ ! -f vendor/autoload.php ]; then
  log "composer install…"
  # shellcheck disable=SC2086 # install_flags is a deliberate word list
  composer install $install_flags
fi

# 3. Ensure Octane (Swoole) is installed. The guard is a filesystem check, not
#    `composer show`: the prod image ships no composer, and a "command not
#    found" would read as "not installed" and send us into `composer require`.
if [ ! -d vendor/laravel/octane ]; then
  log "requiring laravel/octane…"
  # base_flags, not install_flags: `require` rejects --no-dev, so in prod this
  # pulls the root package's dev dependencies back in. Harmless vendor bloat —
  # `artisan optimize` and Octane don't care — and worth far more than a hard
  # failure on an unsupported flag.
  # shellcheck disable=SC2086 # base_flags is a deliberate word list
  composer require laravel/octane $base_flags
fi

# 3b. Everything above this line is composer; everything below is artisan or the
#     database. That split is the whole point of bootstrap-only mode: a
#     throwaway container supplies the composer half (the runtime image may ship
#     none), then exits, and the real container — which has the app's PHP, its
#     extensions, and the stack's network — does the rest on first boot.
#     Running artisan here would mean booting Laravel on a foreign PHP, and
#     waiting on a database we aren't attached to.
#
#     We run as root to write into the root-owned bind mount, so hand the tree
#     to whatever user the runtime image runs as on the way out.
if [ "$XDEV_BOOTSTRAP_ONLY" = "1" ]; then
  if [ -n "$XDEV_APP_UID" ]; then
    log "chown -R $XDEV_APP_UID:${XDEV_APP_GID:-$XDEV_APP_UID}…"
    chown -R "$XDEV_APP_UID:${XDEV_APP_GID:-$XDEV_APP_UID}" /var/www/html
  fi
  log "bootstrap complete"
  exit 0
fi

# 3c. Laravel's create-project scripts normally copy .env.example to .env, but
#     the bootstrap container ran with --no-scripts, so do it here. key:generate
#     below writes into this file and fails outright without it. Values here are
#     only defaults: compose passes DB_*/REDIS_* as real environment variables,
#     and Laravel's dotenv does not override those.
if [ ! -f .env ] && [ -f .env.example ]; then
  log "seeding .env from .env.example…"
  cp .env.example .env
fi

if [ ! -f config/octane.php ]; then
  log "octane:install --server=swoole…"
  php artisan octane:install --server=swoole --no-interaction
fi

# 4. Generate the app key once.
if ! grep -q '^APP_KEY=base64:' .env 2>/dev/null; then
  log "key:generate…"
  php artisan key:generate --no-interaction
fi

# 5. Wait for the database to accept connections (the db healthcheck can report
#    ready before MariaDB finishes startup, so probe over TCP from here).
log "waiting for database…"
i=0
until php -r '
  try {
    new PDO(
      "mysql:host=".getenv("DB_HOST").";port=".(getenv("DB_PORT")?:"3306").";dbname=".getenv("DB_DATABASE"),
      getenv("DB_USERNAME"), getenv("DB_PASSWORD")
    );
    exit(0);
  } catch (Throwable $e) { exit(1); }
' >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 30 ]; then
    log "database still not reachable after $i tries — continuing"
    break
  fi
  sleep 2
done

# 6. Run migrations (non-fatal so a migration hiccup doesn't block serving).
log "migrate…"
php artisan migrate --force || true

# 6b. Production: cache config/routes/views. Non-fatal — a cacheable-config
#     complaint (a closure in a config file) must not stop the app from serving.
if [ "$prod" = 1 ]; then
  log "optimize (config/route/view cache)…"
  php artisan optimize --no-interaction || true
fi

# 7. Serve via Octane/Swoole (becomes the container's main process).
log "starting Octane (Swoole) on :8000…"
exec php artisan octane:start --server=swoole --host=0.0.0.0 --port=8000
