-- Deploying a container app (Laravel) from a repository.
--
-- A container deploy runs migrations, and a migration cannot be rolled back by
-- renaming a directory the way files can. So the default is to dump the app's
-- database before applying any pending migration, and this column is the opt
-- out for the apps where that dump is too slow or too large to be worth it.
--
-- Default 0 = take the dump. The safe behaviour is the one you get by not
-- thinking about it.
ALTER TABLE apps ADD COLUMN skip_db_dump INTEGER NOT NULL DEFAULT 0;
