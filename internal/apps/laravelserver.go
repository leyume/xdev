package apps

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/internal/templates"
)

// CanSwitchLaravelServer reports whether an app can move between Octane/Swoole
// and php-fpm. Only laravel apps have a server to switch, and only one xdev
// generated a compose file for.
func CanSwitchLaravelServer(app store.App) bool {
	return app.Type == "laravel" && app.ComposePath != ""
}

// SetLaravelServer moves a laravel app between Octane/Swoole and php-fpm.
//
// The two stacks share nothing at the container level — Swoole is one `_app`
// container serving HTTP itself, fpm is `_php` behind an nginx `_web` — so this
// regenerates the compose file from the template for the requested server
// rather than editing services in place.
//
// The app is left stopped. Starting it is the caller's (the user's) move: the
// switch replaces every container in the stack, and doing that silently to a
// serving app is not something a settings form should decide.
func (s *Service) SetLaravelServer(id int64, server string) error {
	app, err := s.store.AppByID(id)
	if err != nil {
		return err
	}
	if !CanSwitchLaravelServer(app) {
		return fmt.Errorf("%s apps have no PHP server to switch", app.Type)
	}
	want := store.LaravelServerOr(server)
	if want == store.LaravelServerOr(app.LaravelServer) {
		return nil
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return err
	}

	// Take the stack down while the compose file still describes it. Once the
	// file names different services the engine no longer knows the old
	// containers belong to this stack, and neither `down` nor `up` will ever
	// remove them — the same trap the Adminer toggle documents, except here it
	// would orphan the app's own container rather than a helper.
	//
	// Best effort: a stopped stack has nothing to remove, and a container that
	// refuses to die is not a reason to leave the app on a server the user has
	// asked it to leave.
	if _, engine, workdir, pname, file, cerr := s.composeCtx(app.ID); cerr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
		if out, derr := runtime.Down(ctx, engine, workdir, pname, file); derr != nil {
			log.Printf("app %d: compose down before server switch: %v: %s", app.ID, derr, firstLine(out))
		}
		cancel()
	}

	data, err := s.laravelRenderData(app, proj, want)
	if err != nil {
		return err
	}
	yaml, err := templates.RenderCompose(app.Type, data)
	if err != nil {
		return err
	}
	if err := writeComposeEdit(app.ComposePath, yaml); err != nil {
		return err
	}

	app.LaravelServer = want
	app.Status = store.AppStopped
	if err := s.store.UpdateApp(app); err != nil {
		return err
	}
	return s.store.SetAppStatus(app.ID, store.AppStopped)
}

// laravelRenderData reconstructs the template inputs for an app that already
// exists, so its compose file can be regenerated for a different server.
//
// Everything here is read back from what the app already has rather than
// recomputed: the ports it was allocated, the database it was given. The one
// value with nowhere else to live is the shared-database password, which is
// only ever written to _/laravel.env and the compose file — so that file is
// where it is read from. Getting it wrong would hand the app a stack it cannot
// authenticate with, which is why a missing password is an error rather than a
// blank.
func (s *Service) laravelRenderData(app store.App, proj store.Project, server string) (templates.Data, error) {
	underscore := filepath.Dir(app.ComposePath)

	d := templates.Data{
		ProjectSlug: proj.Slug,
		NetworkName: proj.NetworkName,
		AppSlug:     app.Slug,
		AppType:     app.Type,
		Env:         proj.Environment,
		Server:      server,
		HostPort:    app.Port,
		CPULimit:    app.CPULimit,
		MemLimit:    app.MemLimit,
		SharedDB:    app.DBMode == store.DBShared,
	}

	if server == store.LaravelFPM {
		d.AppImage = templates.ResolveLaravelFPMImage(proj.Environment)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), composeTimeout)
		engine := runtime.Engine(app.Runtime)
		if engine == "" {
			engine = s.sel.Current()
		}
		image, reason := laravelImage(ctx, engine, proj.Environment)
		cancel()
		if reason != "" {
			log.Printf("app %s/%s: %s", proj.Slug, app.Slug, reason)
		}
		d.AppImage = image
	}

	// Adminer keeps whatever port it was allocated, so the switch does not move
	// a URL the user may have bookmarked.
	svc, err := s.store.AppServiceDomains(app.ID)
	if err != nil {
		return d, err
	}
	for _, sd := range svc {
		if sd.Port > 0 {
			d.AdminerPort = sd.Port
			break
		}
	}

	if d.SharedDB {
		d.DBName = SharedDBName(proj.Slug, app.Slug)
		pass, err := envValue(filepath.Join(underscore, "laravel.env"), "DB_PASSWORD")
		if err != nil {
			return d, fmt.Errorf("read this app's database password: %w", err)
		}
		if pass == "" {
			return d, errors.New("this app's _/laravel.env has no DB_PASSWORD — regenerating its stack would lock it out of its database")
		}
		d.DBPass = pass
	}
	return d, nil
}

// envValue reads one KEY=value out of a dotenv-style file. Values are taken
// literally apart from surrounding quotes: these files are written by xdev, so
// there is no shell expansion to honour and guessing at any would be a way to
// corrupt a password rather than read it.
func envValue(path, key string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	prefix := key + "="
	val := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, prefix) {
			continue
		}
		val = strings.TrimPrefix(line, prefix)
		// Last assignment wins, matching how dotenv parsers resolve duplicates.
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	val = strings.TrimSpace(val)
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
	}
	return val, nil
}
