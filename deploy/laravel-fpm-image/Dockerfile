# leyume/php-fpm-alpine — the php-fpm counterpart to leyume/swoole, for
# Laravel apps xdev creates with the "fpm" server option.
#
# Deliberately the same PHP major.minor and the same extension set as the Swoole
# image reports, so switching an app between the two servers changes only how
# requests are served, never what PHP can do. Verified against the running
# Swoole image, which ships:
#   bcmath intl mbstring pcntl pdo_mysql redis sodium swoole zip
# (sodium is built into the official base; swoole is what this image replaces.)
#
# Measured against that Swoole stack serving the same Laravel app, idle:
#   php-fpm + nginx   48.7 MiB
#   Octane/Swoole    168.6 MiB
# The gap is three things: no Octane supervisor process, one opcache pool
# instead of two, and worker heaps that are reaped instead of resident.
ARG PHP_VERSION=8.5
FROM php:${PHP_VERSION}-fpm-alpine

# mlocati's installer resolves each extension's alpine build dependencies
# itself. Hand-rolling `apk add` for intl and zip is where this usually breaks:
# they need icu-dev and libzip-dev at build time and only the runtime libs after.
COPY --from=mlocati/php-extension-installer /usr/bin/install-php-extensions /usr/local/bin/
RUN install-php-extensions bcmath intl mbstring pcntl pdo_mysql redis zip opcache

# opcache mirrors the Swoole image's ini (256M / 32M interned / 20000 files), so
# an app moved between servers gets the same compilation budget.
#
# enable_cli is off on purpose. Under Swoole the server itself is a CLI process
# and needs it; under fpm the only CLI processes are artisan one-shots during
# deploy, and giving each of those its own 256M opcache pool is exactly the
# duplication that makes the Swoole stack expensive.
#
# No JIT: the Swoole image reserves a 100 MiB JIT buffer that measures 0 bytes
# resident, so it buys nothing here either.
RUN { \
      echo "opcache.enable = 1"; \
      echo "opcache.enable_cli = 0"; \
      echo "opcache.memory_consumption = 256"; \
      echo "opcache.interned_strings_buffer = 32"; \
      echo "opcache.max_accelerated_files = 20000"; \
      echo "opcache.validate_timestamps = 1"; \
      echo "opcache.revalidate_freq = 2"; \
      echo "memory_limit = ${PHP_MEMORY_LIMIT:-256M}"; \
    } > /usr/local/etc/php/conf.d/99-xdev.ini

# pm=ondemand is the entire memory argument for this image: workers are forked
# per request and reaped after the idle timeout, so a site with no traffic costs
# the master plus opcache and nothing else. pm=dynamic keeps spare workers
# resident and gives most of the saving straight back.
#
# max_requests recycles a worker after 500 requests, which is what stops PHP's
# allocator holding pages a long-lived worker has grown into.
RUN { \
      echo "[www]"; \
      echo "user = www-data"; \
      echo "group = www-data"; \
      echo "listen = 9000"; \
      echo "pm = ondemand"; \
      echo "pm.max_children = \${PHP_FPM_MAX_CHILDREN}"; \
      echo "pm.process_idle_timeout = \${PHP_FPM_IDLE_TIMEOUT}"; \
      echo "pm.max_requests = 500"; \
      echo "clear_env = no"; \
      echo "catch_workers_output = yes"; \
      echo "decorate_workers_output = no"; \
    } > /usr/local/etc/php-fpm.d/zz-xdev.conf

# Defaults for the two knobs above, so the file works with no compose env set.
ENV PHP_FPM_MAX_CHILDREN=20 \
    PHP_FPM_IDLE_TIMEOUT=10s

# composer, so xdev's deploy can run `composer install` in this container the
# same way it does for the Swoole image.
COPY --from=composer:2 /usr/bin/composer /usr/local/bin/composer

WORKDIR /var/www/html
EXPOSE 9000
CMD ["php-fpm", "-F"]
