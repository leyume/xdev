// Shared MariaDB platform service (PLANx §B): one xdev-managed `xdev-db`
// container on the external `xdev_shared` network serves every shared-mode
// wordpress/laravel app, instead of a ~200MB MariaDB per app. SQL runs through
// `<engine> exec xdev-db mariadb ...` — no Go MySQL driver needed.
//
// ponytail: one MariaDB is a shared blast radius — a restart/crash/upgrade
// touches every shared-mode site at once. Acceptable at this scale; per-app
// "dedicated" mode is the escape hatch for a site that needs isolation. The
// server also lives on whichever engine first opted in — mixing docker and
// podman shared-mode apps would need one server per engine.
package apps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

const (
	sharedDBContainer = "xdev-db"
	sharedDBNetwork   = "xdev_shared"
	sharedDBImage     = "docker.io/library/mariadb:11"
	sharedDBVolume    = "xdev_db_data" // engine-managed named volume
	sharedDBRootKey   = "shared_db_root_password"
)

// sharedDBName is the database AND user name for an app on the shared server:
// <project>_<app>, with slug dashes flattened to underscores (MySQL-identifier
// safe without quoting games).
func sharedDBName(projectSlug, appSlug string) string {
	return strings.ReplaceAll(projectSlug+"_"+appSlug, "-", "_")
}

// ensureSharedDB lazily brings up the shared MariaDB: creates the xdev_shared
// network, generates + persists the root password on first use, starts (or
// creates) the xdev-db container, and waits until it answers SQL. Idempotent.
func (s *Service) ensureSharedDB(ctx context.Context, engine runtime.Engine) (rootPass string, err error) {
	rootPass, err = s.store.GetSetting(sharedDBRootKey)
	if err != nil {
		return "", err
	}
	if rootPass == "" {
		rootPass = randHex(16)
		if err := s.store.SetSetting(sharedDBRootKey, rootPass); err != nil {
			return "", err
		}
	}
	if err := runtime.NetworkCreate(ctx, engine, sharedDBNetwork); err != nil {
		return "", err
	}
	// Missing container -> run it (first use pulls the image); stopped -> start.
	out, err := runtime.Exec(ctx, engine, "container", "inspect",
		"--format", "{{.State.Running}}", sharedDBContainer)
	switch {
	case err != nil:
		if _, err := runtime.Exec(ctx, engine, "run", "-d",
			"--name", sharedDBContainer, "--restart", "unless-stopped",
			"--network", sharedDBNetwork,
			"-e", "MARIADB_ROOT_PASSWORD="+rootPass,
			"-v", sharedDBVolume+":/var/lib/mysql",
			sharedDBImage); err != nil {
			return "", err
		}
	case strings.TrimSpace(out) != "true":
		if _, err := runtime.Exec(ctx, engine, "start", sharedDBContainer); err != nil {
			return "", err
		}
	}
	// A fresh MariaDB takes a few seconds to initialize; poll until SQL answers.
	deadline := time.Now().Add(90 * time.Second)
	for {
		_, err = s.sharedSQL(ctx, engine, rootPass, "SELECT 1")
		if err == nil {
			return rootPass, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("shared MariaDB (%s) did not become ready: %w", sharedDBContainer, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// sharedSQL runs statements against the shared server via `<engine> exec`. The
// root password travels in the MYSQL_PWD env of the exec'd process, not argv,
// so it doesn't show up in host process lists.
func (s *Service) sharedSQL(ctx context.Context, engine runtime.Engine, rootPass, stmts string) (string, error) {
	return runtime.Exec(ctx, engine, "exec", "-e", "MYSQL_PWD="+rootPass,
		sharedDBContainer, "mariadb", "-uroot", "-e", stmts)
}

// provisionSharedDB creates the app's database and user (name db, generated
// password) on the shared server, bringing the server up first if needed, and
// returns the password for injection into the app's compose env.
func (s *Service) provisionSharedDB(ctx context.Context, engine runtime.Engine, db string) (string, error) {
	rootPass, err := s.ensureSharedDB(ctx, engine)
	if err != nil {
		return "", err
	}
	pass := randHex(16)
	// IF NOT EXISTS + ALTER keeps a retried create idempotent while ensuring the
	// password we return is the one that's live.
	stmts := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%[1]s`;"+
		" CREATE USER IF NOT EXISTS '%[1]s'@'%%' IDENTIFIED BY '%[2]s';"+
		" ALTER USER '%[1]s'@'%%' IDENTIFIED BY '%[2]s';"+
		" GRANT ALL PRIVILEGES ON `%[1]s`.* TO '%[1]s'@'%%';"+
		" FLUSH PRIVILEGES;", db, pass)
	if _, err := s.sharedSQL(ctx, engine, rootPass, stmts); err != nil {
		return "", err
	}
	return pass, nil
}

// dumpSharedDB writes a timestamped mariadb-dump of the app's shared database
// into its backups dir (next to the .tar.gz app backups, so the existing
// list/download flow picks it up) and returns the file path.
func (s *Service) dumpSharedDB(ctx context.Context, engine runtime.Engine, app store.App, db, backupsRoot string) (string, error) {
	rootPass, err := s.store.GetSetting(sharedDBRootKey)
	if err != nil || rootPass == "" {
		return "", fmt.Errorf("shared db root password not found: %v", err)
	}
	dir, err := s.backupsDirFor(app, backupsRoot)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, time.Now().Format("20060102-150405")+"-db.sql")
	err = runtime.ExecToFile(ctx, engine, dest, "exec", "-e", "MYSQL_PWD="+rootPass,
		sharedDBContainer, "mariadb-dump", "--databases", db)
	if err != nil {
		return "", err
	}
	return dest, nil
}

// archiveSharedDB dumps a shared-mode app's database into the backups dir and,
// only when the dump landed, drops it — so a hiccup (shared server down, disk
// full) can't silently lose the data. Failures are logged, not fatal: it runs
// on the app-delete path, which proceeds regardless.
func (s *Service) archiveSharedDB(ctx context.Context, engine runtime.Engine, app store.App, projSlug, backupsRoot string) {
	db := sharedDBName(projSlug, app.Slug)
	if _, err := s.dumpSharedDB(ctx, engine, app, db, backupsRoot); err != nil {
		log.Printf("dump shared db %s: %v (leaving database in place)", db, err)
	} else if err := s.dropSharedDB(ctx, engine, db); err != nil {
		log.Printf("drop shared db %s: %v", db, err)
	}
}

// dropSharedDB removes the app's database and user from the shared server.
func (s *Service) dropSharedDB(ctx context.Context, engine runtime.Engine, db string) error {
	rootPass, err := s.store.GetSetting(sharedDBRootKey)
	if err != nil || rootPass == "" {
		return fmt.Errorf("shared db root password not found: %v", err)
	}
	_, err = s.sharedSQL(ctx, engine, rootPass,
		fmt.Sprintf("DROP DATABASE IF EXISTS `%[1]s`; DROP USER IF EXISTS '%[1]s'@'%%';", db))
	return err
}

// randHex returns n random bytes as hex (crypto/rand; 2n chars).
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("xdev: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
