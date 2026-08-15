package apps

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	goruntime "runtime" // stdlib; xdev/internal/runtime owns the plain name here
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

// manifestTimeout bounds a registry manifest lookup. It is short on purpose:
// the answer only refines a choice that already has a usable offline default,
// so a slow or unreachable registry must not hold up creating an app.
const manifestTimeout = 15 * time.Second

// laravelImage resolves the image a laravel app in this environment should run,
// asking the registry which architectures each candidate is actually published
// for. Detection is what lets a newly published multi-arch tag take effect
// without a code change; when it can't answer — offline, private registry, an
// engine that reports manifests differently — the choice falls back to what the
// package recorded, so app creation never depends on reaching a registry.
func laravelImage(ctx context.Context, engine runtime.Engine, env string) (image, reason string) {
	lookup := func(img string) ([]string, bool) {
		lctx, cancel := context.WithTimeout(ctx, manifestTimeout)
		defer cancel()
		arches, err := runtime.ImagePlatforms(lctx, engine, img)
		if err != nil {
			return nil, false
		}
		return arches, true
	}
	return templates.LaravelImageDetected(env, goruntime.GOARCH,
		os.Getenv("XDEV_LARAVEL_IMAGE"), lookup)
}

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
	serveImage, _ := laravelImage(ctx, engine, proj.Environment)
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

// codeDir is where an app's *code* lives — the directory a repository is cloned
// into and reset against.
//
// For a host app that is the app directory itself. For a container app it is
// the app/ subdirectory, because the app directory also holds the things xdev
// generates (_/compose.yml, _/laravel.env) and the state it keeps
// (_volumes/), none of which a deploy may touch. That separation already
// existed for the bind mount; making it the checkout root is what lets the
// ordinary git deploy path work on a container app unchanged.
func (s *Service) codeDir(app store.App) string {
	dir := s.appDir(app)
	if dir == "" || app.IsHostProc() {
		return dir
	}
	return filepath.Join(dir, "app")
}

// canDeployFromGit reports whether an app type has a deploy path a repository
// can drive. Static and go build on the host; laravel builds inside its own
// container. WordPress and a bring-your-own compose stack have neither, so a
// repository attached to one would be cloned and then never looked at again.
func canDeployFromGit(appType string) bool {
	switch appType {
	case store.TypeStatic, store.TypeGo, "laravel":
		return true
	}
	return false
}

// laravelStorageDirs are the subdirectories Laravel expects to exist under
// storage/. A stock install creates them; an app cloned from a repository does
// not, because they are gitignored (only the .gitignore files inside them are
// committed). Mounting an empty storage/ over the checkout therefore has to
// bring them along or the framework fails to boot.
var laravelStorageDirs = []string{
	"app/public",
	"framework/cache/data",
	"framework/sessions",
	"framework/testing",
	"framework/views",
	"logs",
}

// writeLaravelEnv seeds _/laravel.env — the .env the container sees at
// /var/www/html/.env.
//
// It is written once, at create, and never rewritten: it holds the app key, and
// regenerating that would invalidate every encrypted value and signed URL the
// app has issued. Editing it afterwards is the user's business.
//
// The database credentials are duplicated here from the compose environment
// block on purpose. Laravel reads config from the environment at runtime, so
// the compose block alone is enough to *run* — but `php artisan` invoked by a
// deploy inherits that same environment, while anything reading .env directly
// (some packages, and config:cache before the environment is applied) does not.
// Keeping both in step is cheaper than debugging which one a given command saw.
func writeLaravelEnv(underscore, appName, domain string, shared bool, dbName, dbPass string) error {
	path := filepath.Join(underscore, "laravel.env")
	if _, err := os.Stat(path); err == nil {
		return nil // never clobber an existing key
	}
	key, err := laravelAppKey()
	if err != nil {
		return err
	}
	host, database, user, pass := "db", "laravel", "laravel", "secret"
	if shared {
		host, database, user, pass = "xdev-db", dbName, dbName, dbPass
	}
	url := "http://localhost"
	if domain != "" {
		url = "https://" + domain
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# Laravel environment for %s — generated by xdev at create.
#
# This file is mounted into the container as /var/www/html/.env. It lives here,
# outside app/, so deploying the repository cannot overwrite or delete it.
#
# APP_KEY is generated once and never rotated by xdev: changing it invalidates
# every encrypted column, signed URL and session this app has issued.
APP_NAME=%q
APP_ENV=production
APP_KEY=%s
APP_DEBUG=false
APP_URL=%s

LOG_CHANNEL=stack
LOG_LEVEL=warning

DB_CONNECTION=mysql
DB_HOST=%s
DB_PORT=3306
DB_DATABASE=%s
DB_USERNAME=%s
DB_PASSWORD=%s

CACHE_STORE=redis
QUEUE_CONNECTION=redis
SESSION_DRIVER=redis
REDIS_HOST=redis
REDIS_PORT=6379
`, appName, appName, key, url, host, database, user, pass)
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// laravelAppKey generates an APP_KEY in the form artisan key:generate produces:
// 32 random bytes, base64, prefixed so Laravel knows to decode it.
func laravelAppKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "base64:" + base64.StdEncoding.EncodeToString(buf), nil
}

// ProgressFunc receives each finished step of a long operation, so a caller can
// show it while it happens rather than only in the log afterwards. step is the
// command; output is what it printed.
type ProgressFunc func(step, output string)

// report calls a ProgressFunc if there is one. Progress is always optional —
// nothing about the work should depend on somebody watching.
func (f ProgressFunc) report(step, output string) {
	if f != nil {
		f(step, output)
	}
}

// --- deploying a container app ----------------------------------------------

// containerDeployTimeout bounds the whole in-container sequence. Generous
// because `composer install` on a cold cache is the slow part and failing at
// two minutes would be worse than waiting.
const containerDeployTimeout = 20 * time.Minute

// laravelService is the compose service the app's PHP runs in.
const laravelService = "app"

// deployContainer brings a running container app up to the code now in its
// checkout: dependencies, migrations, config caches, then a worker reload.
//
// The order matters and is not the obvious one. Migrations run *before* the
// config caches are rebuilt, because a failed migration should leave the caches
// describing the code that is actually working. The Octane reload is last
// because Swoole holds the old code in memory until it is told otherwise — a
// deploy that skipped it would report success while serving the previous
// release, which is the most confusing failure this whole path can produce.
//
// It returns the accumulated output either way; that is what lands in the
// deployment's log, and for a failure it is the only diagnosis there is.
func (s *Service) deployContainer(app store.App, progress ProgressFunc) (string, error) {
	var out strings.Builder
	step := func(name, o string) {
		fmt.Fprintf(&out, "$ %s\n%s\n", name, strings.TrimRight(o, "\n"))
		progress.report(name, o)
	}

	_, engine, workdir, pname, file, err := s.composeCtx(app.ID)
	if err != nil {
		return out.String(), err
	}
	// Needed to resolve which image this host actually serves from — local and
	// prod are different images here, and the file permissions below depend on
	// which user that image runs as.
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return out.String(), err
	}
	ctx, cancel := context.WithTimeout(context.Background(), containerDeployTimeout)
	defer cancel()

	running := runtime.Running(ctx, engine, workdir, pname, file)

	// exec runs in the live application container. Only the final reload uses
	// it, because only the final reload has anything to say to a running server.
	exec := func(args ...string) (string, error) {
		full := append([]string{"exec", "-T", laravelService}, args...)
		return runtime.Compose(ctx, engine, workdir, pname, file, full...)
	}

	// artisan runs a command in a *throwaway* container built from the same
	// service definition — same image, mounts, environment and networks — rather
	// than in the live one.
	//
	// That independence is the whole point. A deploy exists to make an app
	// healthy, so it cannot require the app to already be healthy: the container
	// this deploy is fixing is typically crash-looping on the very state being
	// fixed, and `exec` into a restarting container fails with "Container … is
	// restarting, wait until the container is running". A one-off container has
	// no such problem, and writes to the same bind-mounted checkout.
	//
	// --entrypoint php because the image's own entrypoint starts the server;
	// without it these arguments would be handed to Octane.
	artisan := func(args ...string) (string, error) {
		full := append([]string{"run", "--rm", "-T", "--entrypoint", "php", laravelService, "artisan"}, args...)
		return runtime.Compose(ctx, engine, workdir, pname, file, full...)
	}

	// 1. Dependencies. --no-dev because this is a release, and
	//    --optimize-autoloader because the classmap is worth building once here
	//    rather than resolving on every request.
	o, deferredScripts, err := s.composerInstall(ctx, engine, app, running, exec)
	step("composer install --no-dev --optimize-autoloader", o)
	if err != nil {
		return out.String(), fmt.Errorf("composer install failed — the deploy stopped before touching the database: %w", err)
	}

	// An app somebody deliberately stopped gets its code and dependencies and
	// nothing else: migrations and cache warming are changes to make when the
	// app is meant to be live, not to a stack that is switched off.
	if app.Status != store.AppRunning {
		fmt.Fprint(&out, "\nthe app is stopped — code and dependencies are in place; migrations and caches run on the next deploy once it is started\n")
		return out.String(), nil
	}

	// 1a. Hand the writable paths to whoever the image runs as.
	//
	//     xdev creates these directories as root, and composer wrote into
	//     bootstrap/cache as root too — but the application image runs as an
	//     unprivileged user (`sail` in the Swoole images). Laravel then cannot
	//     open storage/logs/laravel.log or write bootstrap/cache, and fails at
	//     boot with a permission error that names a file rather than a cause.
	//     This has to run after composer and before anything artisan does.
	if o, err := s.grantContainerUser(ctx, engine, app, proj); err != nil {
		step("grant the container user its writable paths", o)
		return out.String(), fmt.Errorf("could not give the app's own user access to storage/ and bootstrap/cache: %w", err)
	} else if o != "" {
		step("grant the container user its writable paths", o)
	}

	// 1b. The scripts composer could not run. `package:discover` is Laravel's
	//     post-autoload-dump hook: it boots the framework to find each package's
	//     service providers, so it needs the app's own extensions — which the
	//     toolchain container does not have. Without it, packages installed by
	//     this deploy are on disk but not registered.
	if deferredScripts {
		o, err = artisan("package:discover", "--ansi")
		step("php artisan package:discover", o)
		if err != nil {
			return out.String(), fmt.Errorf("dependencies installed, but discovering their service providers failed: %w", err)
		}
	}

	// 2. Migrations, and the dump that is the only way back from a bad one.
	pending, o := s.pendingMigrations(artisan)
	step("php artisan migrate:status", o)
	if pending {
		if dump, err := s.dumpBeforeMigrate(ctx, engine, app); err != nil {
			// A dump we meant to take and couldn't is a stop, not a warning:
			// proceeding would run an irreversible change with no way back.
			return out.String(), fmt.Errorf("could not dump the database before migrating, so the migration was not run: %w", err)
		} else if dump != "" {
			fmt.Fprintf(&out, "$ database dumped to %s\n\n", dump)
		}
		o, err = artisan("migrate", "--force")
		step("php artisan migrate --force", o)
		if err != nil {
			return out.String(), s.migrateFailure(err, app)
		}
	}

	// 3. Caches. Cleared first: a cached config from the previous release can
	//    make `config:cache` itself read stale values.
	for _, c := range [][]string{
		{"config:clear"},
		{"config:cache"},
		{"route:cache"},
		{"view:cache"},
	} {
		o, err = artisan(c...)
		step("php artisan "+strings.Join(c, " "), o)
		if err != nil {
			return out.String(), fmt.Errorf("%s failed: %w", strings.Join(c, " "), err)
		}
	}

	// 4. Put the workers on the new code.
	//
	// A healthy server is reloaded in place, which drops no requests. Anything
	// else — stopped, or crash-looping on what this deploy just fixed — needs a
	// real start, and `up -d` is idempotent for the case where it was already
	// fine. The reload is attempted first and its failure is not fatal: it means
	// "there was no healthy server to talk to", which `up` then addresses.
	if running {
		if o, err := exec("php", "artisan", "octane:reload"); err == nil {
			step("php artisan octane:reload", o)
			return out.String(), nil
		}
	}
	o, err = runtime.Up(ctx, engine, workdir, pname, file)
	step("compose up -d", o)
	if err != nil {
		return out.String(), fmt.Errorf("the code, dependencies and database are deployed, but the stack did not come up: %w", err)
	}
	if !waitRunning(ctx, engine, workdir, pname, file) {
		// Pull the logs in rather than saying "check the logs". This is the one
		// failure whose cause is never in anything above it — the deploy did its
		// work and the application still refused to start — so the container's
		// own output is the only thing that explains it.
		if lg, lerr := runtime.Logs(ctx, engine, workdir, pname, file, 40); lerr == nil {
			step("compose logs --tail 40", lg)
		}
		return out.String(), errors.New("the code, dependencies and database are deployed, but the app container did not stay up")
	}
	return out.String(), nil
}

// composerInstall installs the app's PHP dependencies.
//
// It prefers the app's own container — that is the fastest path and the one
// with the app's environment — but falls back to a one-off toolchain container
// whenever that is not possible: the app is not running (a fresh clone has no
// vendor/, so the production image cannot boot at all), or the image simply has
// no composer. The fallback bind-mounts the same checkout, so what it writes is
// what the app will run.
// It returns whether composer's scripts were skipped, so the caller can run
// them where they can actually work.
func (s *Service) composerInstall(ctx context.Context, engine runtime.Engine, app store.App,
	running bool, exec func(...string) (string, error)) (out string, deferredScripts bool, err error) {

	args := []string{"install", "--no-dev", "--optimize-autoloader",
		"--no-interaction", "--prefer-dist"}

	// Fast path: a running container that actually has composer. Probing costs
	// one process and saves pulling an image on every dev-image deploy.
	if running {
		if _, err := exec("composer", "--version"); err == nil {
			o, err := exec(append([]string{"composer"}, args...)...)
			return o, false, err
		}
	}

	code := s.codeDir(app)
	if code == "" {
		return "", false, errors.New("cannot locate this app's checkout")
	}
	// Two extra flags, both consequences of building outside the app's image:
	//
	//   --ignore-platform-reqs  the toolchain has no ext-swoole (or whatever
	//     else the app needs), so composer would refuse a lockfile that is
	//     perfectly installable. Safe here *because* composer.lock already
	//     exists: this installs an already-resolved set rather than choosing
	//     one, and the set was resolved against the real platform. The cost is
	//     that a genuine mismatch (repo wants PHP 8.4, runtime has 8.3) is no
	//     longer caught here — it surfaces when the app boots.
	//
	//   --no-scripts  Laravel's post-autoload-dump runs `artisan
	//     package:discover`, which boots the framework and needs the app's
	//     extensions and environment. It cannot run here; deployContainer runs
	//     it in the app's own container instead.
	full := append([]string{"run", "--rm",
		"-e", "COMPOSER_ALLOW_SUPERUSER=1",
		"-v", code + ":/var/www/html",
		"-w", "/var/www/html",
		laravelBootstrapImage, "composer"},
		append(args, "--ignore-platform-reqs", "--no-scripts")...)
	out, err = runtime.Exec(ctx, engine, full...)
	if err != nil {
		return out, false, fmt.Errorf("the app image has no composer, and the %s toolchain container also failed: %w",
			laravelBootstrapImage, err)
	}
	return "(ran in a one-off " + laravelBootstrapImage + " container, with --no-scripts)\n" + out, true, nil
}

// waitRunning polls until the stack reports a running container, or gives up.
// `compose up -d` returns once the container is created, which is before the
// application inside it is up — and an exec that lands in that gap fails for a
// reason that has nothing to do with the deploy.
func waitRunning(ctx context.Context, engine runtime.Engine, workdir, pname, file string) bool {
	for i := 0; i < 30; i++ {
		if runtime.Running(ctx, engine, workdir, pname, file) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}

// pendingMigrations reports whether the app has migrations it has not run.
//
// An error is read as "yes": the usual cause is that the migrations table does
// not exist yet, which is exactly the case where migrating matters most. Being
// wrong in this direction costs a dump nobody needed; being wrong in the other
// direction runs an unbacked migration.
func (s *Service) pendingMigrations(run func(...string) (string, error)) (bool, string) {
	out, err := run("php", "artisan", "migrate:status")
	if err != nil {
		return true, out
	}
	return strings.Contains(strings.ToLower(out), "pending"), out
}

// dumpBeforeMigrate writes a database dump next to the app's backups and
// returns its path, or "" when no dump was taken because the app opted out.
func (s *Service) dumpBeforeMigrate(ctx context.Context, engine runtime.Engine, app store.App) (string, error) {
	if app.SkipDBDump {
		return "", nil
	}
	if s.backups == "" {
		return "", errors.New("no backups directory is configured")
	}
	proj, err := s.store.ProjectByID(app.ProjectID)
	if err != nil {
		return "", err
	}
	if app.DBMode == store.DBShared {
		return s.dumpSharedDB(ctx, engine, app, SharedDBName(proj.Slug, app.Slug), s.backups)
	}
	// Dedicated: dump from the app's own db container. Root, because the
	// application user is not guaranteed the grants a full dump needs.
	dir, err := s.backupsDirFor(app, s.backups)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, time.Now().Format("20060102-150405")+"-predeploy.sql")
	err = runtime.ExecToFile(ctx, engine, dest, "exec", "-e", "MYSQL_PWD=root",
		proj.Slug+"_"+app.Slug+"_db", "mariadb-dump", "-u", "root", "--databases", "laravel")
	if err != nil {
		return "", err
	}
	return dest, nil
}

// migrateFailure is the error a failed migration reports. It names the dump,
// because at this point the schema is in an unknown state and that file is the
// only way back — and it deliberately does not restore anything: rows written
// between the dump and the failure are real, and an automatic restore would
// throw them away to fix a problem a human has not looked at yet.
func (s *Service) migrateFailure(err error, app store.App) error {
	if app.SkipDBDump {
		return fmt.Errorf("migration failed and this app has database dumps switched off, so there is no pre-deploy snapshot to restore from: %w", err)
	}
	return fmt.Errorf("migration failed — the schema may be partly applied. The pre-deploy dump is in this app's backups; restore it deliberately rather than re-deploying: %w", err)
}

// grantContainerUser makes the paths a Laravel app writes at runtime owned by
// the user its image actually runs as.
//
// Everything xdev puts on disk is created by root, because xdev runs as root —
// but the application images run unprivileged (`sail`). The bind mounts carry
// host ownership straight through, so without this the app boots far enough to
// try to log and then dies on "Failed to open stream: Permission denied", which
// names a file and explains nothing.
//
// The uid is read from the image rather than assumed. `sail` is conventionally
// 1000, but hardcoding that would break silently against any image that differs,
// and the cost of asking is one short-lived container.
//
// It returns a description of what it changed, "" when there was nothing to do.
func (s *Service) grantContainerUser(ctx context.Context, engine runtime.Engine, app store.App,
	proj store.Project) (string, error) {

	// Resolve the serving image the same way the compose file did, so the uid
	// belongs to the image that will actually run rather than a guess — on this
	// host the local and prod variants are different images entirely.
	image, _ := laravelImage(ctx, engine, proj.Environment)
	uid, gid, err := imageUser(ctx, engine, image)
	if err != nil {
		return "", err
	}
	if uid == 0 {
		return "", nil // the image runs as root; the files are already its own
	}

	// Chown on the host: these are bind mounts, xdev is root, and doing it here
	// avoids a second container and works whether or not the app can start.
	appDir := s.appDir(app)
	code := s.codeDir(app)
	targets := []string{
		filepath.Join(appDir, "_volumes", "storage"),
		filepath.Join(code, "bootstrap", "cache"),
		filepath.Join(appDir, "_", "laravel.env"), // 0600, and the app must read it
	}
	var done []string
	for _, t := range targets {
		if _, err := os.Stat(t); err != nil {
			continue // not every app has every path
		}
		if err := chownTree(t, uid, gid); err != nil {
			return "", fmt.Errorf("chown %s: %w", t, err)
		}
		done = append(done, t)
	}
	return fmt.Sprintf("uid=%d gid=%d applied to:\n  %s", uid, gid, strings.Join(done, "\n  ")), nil
}

// chownTree chowns a file, or a directory and everything under it.
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}
