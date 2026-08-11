package apps

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

// appDir returns an app's on-disk root: <project.Dir>/<app-slug>. For container
// apps this is the parent of _/ and app/; for static apps it holds the code
// directly; shared-WP sites live under data/wp/sites instead. Derived from the
// compose path when present, else from the project.
func (s *Service) appDir(app store.App) string {
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

// envPath is the app's editable .env file. Container apps keep it in the
// bind-mounted app/ dir; static apps keep it at the app root (no app/ subdir);
// compose apps keep it next to their compose file, which is the copy the engine
// actually reads (${VAR} substitution and env_file).
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
	return os.WriteFile(p, []byte(content), 0o644)
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
		if _, err := s.dumpSharedDB(ctx, engine, app, sharedDBName(proj.Slug, app.Slug), backupsRoot); err != nil {
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
