package apps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
	"xdev/internal/templates"
)

// laravelBootstrapImage supplies composer for the one-time scaffold. It is the
// official composer image rather than one of our own Swoole tags on purpose:
// those are single-arch and for opposite architectures, so whichever one a
// given host can serve from, the other dies at exec. Bootstrapping is plain
// dependency resolution, so an arch-portable upstream image does it without
// tying the scaffold to whatever platform our images happen to carry — and it
// makes the scaffold identical on every machine. It never serves traffic:
// init.sh's bootstrap-only mode runs composer, then exits before touching
// artisan.
const laravelBootstrapImage = "docker.io/library/composer:2"

// bootstrapTimeout covers a cold `composer create-project` plus an octane
// install, on a slow network, with the image still to pull.
const bootstrapTimeout = 15 * time.Minute

// ensureLaravelBootstrap scaffolds a Laravel install into an app's empty app/
// before its stack starts.
//
// The image a stack serves from may ship no composer (the hardened variant
// doesn't), in which case it cannot bootstrap itself and an app whose app/ is
// empty would crash-loop on "composer: not found" with nothing to serve. So the
// one-time scaffold happens here, in a throwaway container that definitely has
// composer, and the stack then comes up on code that already exists. This runs
// even when the serving image *could* do it itself, so a Laravel app is
// scaffolded identically no matter which image this host ended up with.
// Deploying built code stays a no-op: app/ already has an artisan, so this is
// skipped.
//
// It runs on every Start, not just Create, so an app left empty by a failed
// create recovers on the next start instead of staying wedged.
func (s *Service) ensureLaravelBootstrap(ctx context.Context, app store.App, proj store.Project) error {
	if app.Type != "laravel" || app.ComposePath == "" {
		return nil
	}
	// _/compose.yml -> the app dir that holds it, whose app/ is the code mount.
	appDir := filepath.Dir(filepath.Dir(app.ComposePath))
	content := filepath.Join(appDir, "app")
	if _, err := os.Stat(filepath.Join(content, "artisan")); err == nil {
		return nil // already bootstrapped (or real code deployed)
	}
	initSh := filepath.Join(filepath.Dir(app.ComposePath), "init.sh")
	if _, err := os.Stat(initSh); err != nil {
		return fmt.Errorf("laravel bootstrap: %w", err)
	}

	engine := runtime.Engine(app.Runtime)
	if engine == "" {
		engine = s.sel.Current()
	}

	// Hand the scaffolded tree to whatever user the serving image runs as; we
	// write it as root (the bind mount is root-owned) so it would otherwise be
	// unwritable by the app that has to serve — and migrate — from it. Resolve
	// the same way the compose file did, so we chown for the image that will
	// actually run rather than a hardcoded guess.
	serveImage, _ := templates.ResolveLaravelImage(proj.Environment)
	uid, gid, err := imageUser(ctx, engine, serveImage)
	if err != nil {
		return fmt.Errorf("laravel bootstrap: %w", err)
	}

	// Mirror the compose file's APP_ENV so the scaffold is resolved the same way
	// the stack will run it (prod installs without dev dependencies).
	appEnv := "local"
	if proj.Environment == "prod" {
		appEnv = "production"
	}

	_, err = runtime.Exec(ctx, engine, "run", "--rm",
		"--user", "0:0",
		"--env", "APP_ENV="+appEnv,
		"--env", "XDEV_BOOTSTRAP_ONLY=1",
		"--env", "XDEV_APP_UID="+strconv.Itoa(uid),
		"--env", "XDEV_APP_GID="+strconv.Itoa(gid),
		"--volume", content+":/var/www/html",
		"--volume", initSh+":/init.sh:ro",
		"--entrypoint", "sh",
		laravelBootstrapImage, "/init.sh")
	if err != nil {
		return fmt.Errorf("laravel bootstrap: %w", err)
	}
	return nil
}

// imageUser reports the uid/gid an image's default user resolves to, by asking
// the image itself — Config.User may be a name ("sail") that only the image's
// own /etc/passwd can resolve. An empty Config.User means root, which `id` in
// the container reports as 0 anyway, so there is no special case here.
func imageUser(ctx context.Context, engine runtime.Engine, image string) (int, int, error) {
	out, err := runtime.Exec(ctx, engine, "run", "--rm", "--entrypoint", "sh",
		image, "-c", "id -u; id -g")
	if err != nil {
		return 0, 0, fmt.Errorf("resolve user of %s: %w", image, err)
	}
	fields := strings.Fields(out)
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("resolve user of %s: unexpected output %q", image, out)
	}
	uid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, fmt.Errorf("resolve user of %s: uid %q: %w", image, fields[0], err)
	}
	gid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("resolve user of %s: gid %q: %w", image, fields[1], err)
	}
	return uid, gid, nil
}
