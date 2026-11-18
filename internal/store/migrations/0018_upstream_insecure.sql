-- A proxy app can opt out of verifying its upstream's certificate, for an origin
-- that only has a self-signed one (another of your own servers, reached by IP).
-- Off for every existing app: they keep exactly the verification they had.
ALTER TABLE apps ADD COLUMN upstream_insecure INTEGER NOT NULL DEFAULT 0;
