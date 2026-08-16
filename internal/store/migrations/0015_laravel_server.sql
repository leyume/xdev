-- Which PHP server a laravel app runs: Octane/Swoole or php-fpm.
--
-- Empty string means Swoole, so every app created before this migration keeps
-- the stack it already has without a backfill. The two differ in what they cost
-- at idle far more than in what they can serve: Swoole holds the whole framework
-- resident in each worker (fast, ~130 MiB floor on a 2-core host), php-fpm
-- rebuilds it per request and reaps idle workers (slower per request, ~40 MiB).
ALTER TABLE apps ADD COLUMN laravel_server TEXT NOT NULL DEFAULT '';
