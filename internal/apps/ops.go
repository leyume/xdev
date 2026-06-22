package apps

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

// appDir returns an app's on-disk root: <project.Dir>/<app-slug>. For container
// apps this is the parent of _/ and app/; for static apps it holds the code
// directly. Derived from the compose path when present, else from the project.
func (s *Service) appDir(app store.App) string {
	if app.ComposePath != "" {
		return filepath.Dir(filepath.Dir(app.ComposePath))
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return ""
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
	if app.IsStatic() {
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
// bind-mounted app/ dir; static apps keep it at the app root (no app/ subdir).
func (s *Service) envPath(id int64) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	if app.IsStatic() {
		return filepath.Join(s.appDir(app), ".env"), nil
	}
	return filepath.Join(s.appDir(app), "app", ".env"), nil
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

// Backup writes a timestamped .tar.gz of the app's directory (compose + content)
// under backupsRoot and returns the archive path. Named volumes (e.g. databases)
// are not included — back those up separately.
func (s *Service) Backup(id int64, backupsRoot string) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
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
