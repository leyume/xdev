#!/bin/sh
# Check a built image actually has what Laravel needs, before it is pushed.
#
#   ./verify.sh [image]     default: leyume/php-fpm-alpine:1.0.0
#
# This exists because the image that looked like the obvious choice for this job
# (serversideup/php:8.5-fpm-nginx-alpine) advertises Laravel support and ships
# only `sodium` on its 8.5 tag — no pdo_mysql, no redis, no opcache. Nothing in
# the image's labels, env or history says so; the extension list is a build ARG.
# The only way to know is to ask the built image, which is what this does.
set -e
IMG="${1:-leyume/php-fpm-alpine:1.0.0}"

# What the Swoole image ships, minus swoole itself. An fpm app must be able to
# do everything a Swoole app can, or switching servers silently breaks it.
#
# One name per word, deliberately: this list is word-split by the loop below, so
# a two-word module name here would be tested as two nonexistent ones and fail a
# perfectly good image. Zend OPcache is the only such name, and it is checked
# separately further down.
NEED="bcmath intl mbstring pcntl pdo_mysql redis sodium zip"

echo "image  $IMG"
echo

if ! docker image inspect "$IMG" >/dev/null 2>&1; then
  echo "!! no such image locally. Build it first:  ./build.sh" >&2
  exit 1
fi

echo "php:   $(docker run --rm "$IMG" php -r 'echo PHP_VERSION;')"
echo "fpm:   $(docker run --rm "$IMG" php-fpm -v 2>/dev/null | head -1)"
echo

MODS=$(docker run --rm "$IMG" php -m)
fail=0
for m in $NEED; do
  if echo "$MODS" | grep -qix "$m"; then
    printf '  ok    %s\n' "$m"
  else
    printf '  MISS  %s\n' "$m"
    fail=1
  fi
done

# Matched loosely, not as a whole line: php -m prints this one as "Zend OPcache"
# and lists it under both [PHP Modules] and [Zend Modules].
if echo "$MODS" | grep -qi "zend opcache"; then
  printf '  ok    Zend OPcache\n'
else
  printf '  MISS  Zend OPcache\n'
  fail=1
fi

echo
# opcache being present is not the same as opcache being on: the image that
# failed this check had it compiled in and disabled by default.
ENABLED=$(docker run --rm "$IMG" php -r 'echo ini_get("opcache.enable") ? "on" : "off";')
CLI=$(docker run --rm "$IMG" php -r 'echo ini_get("opcache.enable_cli") ? "on" : "off";')
MEM=$(docker run --rm "$IMG" php -r 'echo ini_get("opcache.memory_consumption");')
echo "opcache.enable      $ENABLED   (must be on, or every request re-compiles)"
echo "opcache.enable_cli  $CLI  (should be off; each CLI process would get its own pool)"
echo "opcache.memory_consumption  ${MEM}M"

PM=$(docker run --rm "$IMG" sh -c 'grep -h "^pm = " /usr/local/etc/php-fpm.d/*.conf | tail -1')
echo "fpm process manager $PM  (must be ondemand for the idle-memory saving)"

echo
if [ "$fail" = "1" ] || [ "$ENABLED" != "on" ]; then
  echo "FAILED — do not push this image."
  echo
  echo "Everything the image actually loads, for comparison:"
  echo "$MODS" | sed 's/^/  /'
  exit 1
fi
echo "PASSED — safe to push."
