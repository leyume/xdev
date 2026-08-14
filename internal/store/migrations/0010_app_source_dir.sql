-- Host apps (static, go) can live in a directory the user already has instead of
-- one xdev creates under projects/<project>/<app>/ — pointing an app at, say,
-- /home/li/ui/xyz rather than copying it in.
--
--   source_dir  ''            -> managed: <project.dir>/<app.slug> (the default)
--               '/abs/path'   -> external: the user's own directory
--
-- The distinction matters beyond locating files: xdev never scaffolds into an
-- external directory and never deletes one when the app is deleted.
ALTER TABLE apps ADD COLUMN source_dir TEXT NOT NULL DEFAULT '';
