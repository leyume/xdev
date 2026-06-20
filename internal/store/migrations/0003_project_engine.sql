-- Pin the container engine per project so a project's network and all its apps
-- stay on one engine even if the global default is switched later.
ALTER TABLE projects ADD COLUMN engine TEXT NOT NULL DEFAULT '';
