package apps

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

// appDir returns an app's on-disk root: <project.Dir>/<app-slug>. For container
// apps this is the parent of _/ and app/; for static apps it holds the code
// directly; shared-WP sites live under data/wp/sites instead. Derived from the
// compose path when present, else from the project — unless the app was pointed
// at a directory of the user's own, which wins over both.
func (s *Service) appDir(app store.App) string {
	if app.SourceDir != "" {
		return app.SourceDir
	}
	if app.ComposePath != "" {
		return filepath.Dir(filepath.Dir(app.ComposePath))
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return ""
	}
	if app.IsSharedWP() {
		return s.wpSiteDir(proj.Slug, app.Slug) // docroot: wp-config.php + wp-content/
	}
	return filepath.Join(proj.Dir, app.Slug)
}

// Logs returns the last `tail` lines of the app's logs: container logs for
// container apps, the supervised process's log file for static command apps.
func (s *Service) Logs(id int64, tail int) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	if app.IsProxy() {
		return "Proxy app — traffic is forwarded upstream; there are no local logs.", nil
	}
	if app.IsSharedWP() {
		return "Shared-host WordPress — PHP runs in the platform xdev-wp container, shared by every site; see `" +
			app.Runtime + " logs " + wpHostContainer + "`.", nil
	}
	if app.IsHostProc() {
		proj, err := s.store.ProjectByID(app.ProjectID)
		if err != nil {
			return "", err
		}
		return s.sup.Logs(proj.Slug+"_"+app.Slug, tail)
	}
	_, engine, workdir, pname, file, err := s.composeCtx(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return runtime.Logs(ctx, engine, workdir, pname, file, tail)
}

// CanClearLogs reports whether an app's log is xdev's to empty. Only host
// processes qualify: xdev opened that file and writes to it, so truncating it
// is bookkeeping on its own data.
//
// A container app's output belongs to the engine's logging driver. Shortening
// it means truncating a file the engine holds open, behind its back — the path
// differs per engine, some drivers (journald, syslog) have no file at all, and
// on a rootless install it is not even readable. That is not something a button
// in a web UI should be doing, so there is no button.
func CanClearLogs(app store.App) bool { return app.IsHostProc() }

// ClearLogs empties a host-process app's log.
func (s *Service) ClearLogs(id int64) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	if !CanClearLogs(app) {
		return errors.New("these logs come from the container engine, not from xdev — there is nothing here to clear")
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return err
	}
	return s.sup.ClearLogs(proj.Slug + "_" + app.Slug)
}

// envPath is the app's editable .env file. Static apps keep it at the app root
// (no app/ subdir); compose apps keep it next to their compose file, which is
// the copy the engine actually reads (${VAR} substitution and env_file); other
// container apps keep it in the bind-mounted app/ dir.
//
// Laravel is the exception, and it matters: since the checkout became something
// a deploy resets hard, its .env is _/laravel.env, mounted over
// /var/www/html/.env. Editing app/.env there would write to a file the mount
// shadows — saved, never read, no error. The file's presence is the test rather
// than the app type, so apps created before that change keep using app/.env.
func (s *Service) envPath(id int64) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	if app.IsHostProc() {
		return filepath.Join(s.appDir(app), ".env"), nil
	}
	if app.IsCompose() {
		return filepath.Join(s.appDir(app), "_", ".env"), nil
	}
	if mounted := filepath.Join(s.appDir(app), "_", "laravel.env"); exists(mounted) {
		return mounted, nil
	}
	return filepath.Join(s.appDir(app), "app", ".env"), nil
}

// EnvLocation returns the app-relative path of the editable .env, so the UI can
// tell the user which file they are editing.
func (s *Service) EnvLocation(id int64) string {
	app, err := s.store.AppByID(id)
	if err != nil {
		return ".env"
	}
	switch {
	case app.IsHostProc():
		return ".env"
	case app.IsCompose():
		return "_/.env"
	}
	if exists(filepath.Join(s.appDir(app), "_", "laravel.env")) {
		return "_/laravel.env"
	}
	return "app/.env"
}

// ReadEnv returns the app's .env contents ("" if it doesn't exist yet).
func (s *Service) ReadEnv(id int64) (string, error) {
	p, err := s.envPath(id)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WriteEnv saves the app's .env contents (the caller restarts the app to apply).
func (s *Service) WriteEnv(id int64, content string) error {
	p, err := s.envPath(id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// 0600 for the mounted Laravel .env — it holds the app key and the database
	// password, and unlike the others it is chowned to the container's user
	// rather than read by root. Everything else keeps the mode it always had.
	mode := os.FileMode(0o644)
	if filepath.Base(p) == "laravel.env" {
		mode = 0o600
	}
	if err := writePreservingOwner(p, []byte(content), mode); err != nil {
		return err
	}
	// A Laravel deploy caches the config, so a freshly edited .env changes
	// nothing until that cache is dropped — the edit appears to have been
	// ignored. Best-effort: the app may be stopped, which is fine, since a
	// stopped app rebuilds its caches when it next deploys.
	if app, err := s.store.AppByID(id); err == nil && app.Type == "laravel" {
		go func() {
			if _, _, err := s.RunContainerAction(id, "config-clear"); err != nil {
				log.Printf("clear config cache for app %d after .env change: %v", id, err)
			}
		}()
	}
	return nil
}

// writePreservingOwner writes a file, keeping the uid/gid it already had.
//
// os.WriteFile on an existing file leaves ownership alone, but a file that has
// to be *created* here would end up owned by root — and for the Laravel .env
// that is the difference between the app reading its configuration and dying at
// boot. Rewriting in place keeps whatever grantContainerUser set.
func writePreservingOwner(path string, data []byte, mode os.FileMode) error {
	st, statErr := os.Stat(path)
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	if statErr != nil {
		return nil // newly created; nothing to preserve
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return os.Chown(path, int(sys.Uid), int(sys.Gid))
	}
	return nil
}

// backupsDirFor returns the per-app backups directory under backupsRoot.
func (s *Service) backupsDirFor(app store.App, backupsRoot string) (string, error) {
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(backupsRoot, proj.Slug+"_"+app.Slug), nil
}

// Import restores a .tar.gz backup over an existing app: the app is stopped,
// the archive is unpacked into its directory, and it is started again. Files in
// the archive replace files at the same path; anything already there and not in
// the archive is left alone.
// ponytail: overlay, not a clean replace — the archive can't silently delete an
// app's data, at the cost of leaving stale files behind. Wipe-then-extract is
// the upgrade if exact-mirror restores ever matter.
func (s *Service) Import(id int64, archive io.Reader) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	dir := s.appDir(app)
	if dir == "" {
		return errors.New("app has no directory to import into")
	}
	if err := s.Stop(id); err != nil {
		return err
	}
	if err := extractAppArchive(archive, dir, unpacksAll(app)); err != nil {
		return err
	}
	return s.Start(id)
}

// unpacksAll reports whether an import should restore the whole archive,
// including its _/ directory. True for host apps (which have no _/) and for
// compose apps, whose compose file is user content worth restoring — Start
// re-reads it and follows its port. False for templated container apps, where
// _/ holds xdev-rendered files for *this* app.
func unpacksAll(app store.App) bool { return app.IsHostProc() || app.IsCompose() }

// extractAppArchive unpacks a backup .tar.gz into an app directory. Unless the
// app owns its _/ (see unpacksAll) the archive's _/ is skipped, so the compose
// file xdev rendered for *this* app (its ports, container names and network)
// survives — importing someone else's compose would point the app at the wrong
// stack.
func extractAppArchive(archive io.Reader, dir string, all bool) error {
	if all {
		return untarGz(archive, dir)
	}
	return untarGzFilter(archive, dir, func(name string) bool {
		return name != "_" && !strings.HasPrefix(name, "_/")
	})
}

// Backup writes a timestamped .tar.gz of the app's directory (compose + content)
// under backupsRoot and returns the archive path. Dedicated per-app databases
// (named volumes / _volumes) are not included — back those up separately; a
// shared-mode app additionally gets a mariadb-dump of its database written next
// to the archive.
func (s *Service) Backup(id int64, backupsRoot string) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	if app.DBMode == store.DBShared {
		proj, err := s.store.ProjectByID(app.ProjectID)
		if err != nil {
			return "", err
		}
		engine := s.sel.Current()
		if app.Runtime != "" {
			engine = runtime.Engine(app.Runtime)
		}
		ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
		defer cancel()
		if _, err := s.dumpSharedDB(ctx, engine, app, SharedDBName(proj.Slug, app.Slug), backupsRoot); err != nil {
			return "", err
		}
	}
	dir, err := s.backupsDirFor(app, backupsRoot)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, time.Now().Format("20060102-150405")+".tar.gz")
	if err := targz(s.appDir(app), dest); err != nil {
		return "", err
	}
	return dest, nil
}

// ListBackups returns existing backup filenames for an app, newest first.
func (s *Service) ListBackups(id int64, backupsRoot string) ([]string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return nil, err
	}
	dir, err := s.backupsDirFor(app, backupsRoot)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// BackupPath resolves a backup file path, guarding against path traversal in
// the supplied name.
func (s *Service) BackupPath(id int64, backupsRoot, name string) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	dir, err := s.backupsDirFor(app, backupsRoot)
	if err != nil {
		return "", err
	}
	// filepath.Base strips any directory components from the requested name.
	return filepath.Join(dir, filepath.Base(name)), nil
}

// targz writes a gzip-compressed tar of srcDir to dest, with paths relative to
// srcDir.
func targz(srcDir, dest string) error {
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.Walk(srcDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil // dirs (header only), skip symlinks/sockets contents
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		return err
	})
}
