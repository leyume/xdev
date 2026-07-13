-- Proxy apps are just a Caddy route forwarding a domain to another server:
-- no container, no process, no host port. upstream holds the target URL
-- (http(s)://host[:port]); blank for every other app type.
ALTER TABLE apps ADD COLUMN upstream TEXT NOT NULL DEFAULT '';
