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

# 1. Install Laravel on first boot. Build into a temp dir and copy in (incl.
#    dotfiles) so a stray file in app/ (e.g. a Finder .DS_Store) doesn't trip
#    composer's "directory not empty" guard.
if [ ! -f artisan ]; then
  log "installing Laravel (composer create-project)…"
  tmp="$(mktemp -d)"
  composer create-project laravel/laravel "$tmp" --no-interaction
  cp -a "$tmp"/. /var/www/html/
  rm -rf "$tmp"
fi

# 2. Ensure PHP dependencies are present.
if [ ! -f vendor/autoload.php ]; then
  log "composer install…"
  # shellcheck disable=SC2086 # install_flags is a deliberate word list
  composer install $install_flags
fi

# 3. Ensure Octane (Swoole) is installed.
if ! composer show laravel/octane >/dev/null 2>&1; then
  log "requiring laravel/octane…"
  composer require laravel/octane --no-interaction
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
