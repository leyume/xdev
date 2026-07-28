package templates

import (
	"strings"
	"testing"
)

// TestRenderAllAvailableTypes renders every selectable app type and checks the
// output is non-empty, wires the per-app container name and host port, and joins
// the shared project network. This catches template syntax/field errors without
// needing a container engine.
func TestRenderAllAvailableTypes(t *testing.T) {
	d := Data{
		ProjectSlug: "demo",
		NetworkName: "xdev_demo",
		AppSlug:     "site",
		HostPort:    20000,
	}
	for _, ti := range Catalog() {
		if !ti.Available {
			continue
		}
		// Static/go apps run on the host and proxy apps are just a Caddy route —
		// none of them has a compose template to render.
		if ti.Type == "static" || ti.Type == "go" || ti.Type == "proxy" {
			continue
		}
		d.AppType = ti.Type
		out, err := RenderCompose(ti.Type, d)
		if err != nil {
			t.Fatalf("%s: render error: %v", ti.Type, err)
		}
		for _, want := range []string{
			"demo_site",       // container_name prefix
			"20000",           // host port
			"name: xdev_demo", // external project network
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: rendered compose missing %q\n%s", ti.Type, want, out)
			}
		}
	}
}

// TestProdComposeSelection verifies that a prod environment selects the prod
// compose variant when one exists (laravel), and falls back otherwise.
func TestProdComposeSelection(t *testing.T) {
	d := Data{ProjectSlug: "p", NetworkName: "xdev_p", AppSlug: "api", AppType: "laravel", HostPort: 8000, Env: "prod"}
	out, err := RenderCompose("laravel", d)
	if err != nil {
		t.Fatalf("render laravel prod: %v", err)
	}
	// Both variants now serve from the same image (the -dev tag is arm64-only),
	// so anchor on something the prod stack alone has.
	if !strings.Contains(out, "APP_ENV: production") {
		t.Errorf("prod laravel should select the prod compose variant:\n%s", out)
	}
	local := Data{ProjectSlug: "p", NetworkName: "xdev_p", AppSlug: "api", AppType: "laravel", HostPort: 8000}
	lout, err := RenderCompose("laravel", local)
	if err != nil {
		t.Fatalf("render laravel local: %v", err)
	}
	if strings.Contains(lout, "APP_ENV: production") {
		t.Errorf("local laravel should select the dev compose variant:\n%s", lout)
	}

	// A type without a prod variant falls back to the dev template.
	d2 := Data{ProjectSlug: "p", NetworkName: "xdev_p", AppSlug: "blog", AppType: "wordpress", HostPort: 8001, Env: "prod"}
	if _, err := RenderCompose("wordpress", d2); err != nil {
		t.Errorf("wordpress prod should fall back to dev template, got: %v", err)
	}
}

// TestLaravelProdBootable guards the regression that made a Laravel app in a
// prod project fail `compose up -d` outright: the code mount was read-only with
// named volumes carved out of it, so on a freshly created (empty) app/ the
// nested mountpoints couldn't be created and the app container never started.
// The prod stack must mount app/ writable and boot through init.sh, exactly
// like the local one — only the image and hardening differ.
func TestLaravelProdBootable(t *testing.T) {
	d := Data{ProjectSlug: "demo", NetworkName: "xdev_demo", AppSlug: "api",
		AppType: "laravel", Env: "prod", HostPort: 20000, AdminerPort: 20001,
		SharedDB: true, DBName: "demo_api", DBPass: "p4ss"}
	out, err := RenderCompose("laravel", d)
	if err != nil {
		t.Fatalf("render laravel prod: %v", err)
	}
	for _, want := range []string{
		"- ../app:/var/www/html\n", // writable code mount
		"./init.sh:/init.sh:ro",    // bootstrap entrypoint mount
		`command: ["sh", "/init.sh"]`,
		"APP_ENV: production", // init.sh installs --no-dev and caches config
		"DB_PORT: 3306",       // parity with the local stack
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prod laravel compose missing %q\n%s", want, out)
		}
	}
	for _, not := range []string{
		"../app:/var/www/html:ro",             // ro code mount + nested volumes = unstartable
		"storage:/var/www/html/storage",       //
		"cache:/var/www/html/bootstrap/cache", //
	} {
		if strings.Contains(out, not) {
			t.Errorf("prod laravel compose must not contain %q\n%s", not, out)
		}
	}
}

// TestLaravelInfra checks the laravel stack renders its bizepp-style infra
// (Adminer, _volumes bind-mounts, the init.sh entrypoint) and that its infra
// support files are embedded.
func TestLaravelInfra(t *testing.T) {
	d := Data{ProjectSlug: "demo", NetworkName: "xdev_demo", AppSlug: "api",
		AppType: "laravel", HostPort: 20000, AdminerPort: 20001}
	out, err := RenderCompose("laravel", d)
	if err != nil {
		t.Fatalf("render laravel: %v", err)
	}
	for _, want := range []string{
		"demo_api_adminer",                 // adminer service
		"20001:8080",                       // adminer published port
		"ADMINER_DEFAULT_SERVER: db",       // adminer prefilled server
		"../_volumes/mysql:/var/lib/mysql", // bizepp-format db volume
		"../_volumes/redis:/data",          // redis volume
		"./init.sh:/init.sh:ro",            // bootstrap entrypoint mount
		"DB_CONNECTION: mysql",             // not Laravel's sqlite default
	} {
		if !strings.Contains(out, want) {
			t.Errorf("laravel compose missing %q\n%s", want, out)
		}
	}

	infra, err := InfraFiles("laravel")
	if err != nil {
		t.Fatalf("InfraFiles(laravel): %v", err)
	}
	for _, want := range []string{"init.sh", "db/my.cnf", "db/adminer.css"} {
		if _, ok := infra[want]; !ok {
			t.Errorf("laravel infra missing %q", want)
		}
	}
}

// TestMailPorts checks the mail stack publishes the Stalwart admin on the
// secondary port, real mail ports in prod, and high ports locally (rootless
// podman can't bind <1024).
func TestMailPorts(t *testing.T) {
	d := Data{ProjectSlug: "demo", NetworkName: "xdev_demo", AppSlug: "mail",
		AppType: "mail", HostPort: 20000, AdminerPort: 20001}
	out, err := RenderCompose("mail", d)
	if err != nil {
		t.Fatalf("render mail local: %v", err)
	}
	for _, want := range []string{"20001:443", "18080:8080", `"2525:25"`, "20000:8888", "demo_mail_webmail"} {
		if !strings.Contains(out, want) {
			t.Errorf("local mail compose missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, `"25:25"`) {
		t.Errorf("local mail compose must not bind port 25:\n%s", out)
	}

	d.Env = "prod"
	out, err = RenderCompose("mail", d)
	if err != nil {
		t.Fatalf("render mail prod: %v", err)
	}
	for _, want := range []string{`"25:25"`, `"465:465"`, `"993:993"`} {
		if !strings.Contains(out, want) {
			t.Errorf("prod mail compose missing %q\n%s", want, out)
		}
	}
}

// TestSharedDBCompose verifies shared-database mode: the compose drops its own
// db service, joins the external xdev_shared network, and injects the xdev-db
// host + per-app credentials — while dedicated mode keeps today's per-app db.
func TestSharedDBCompose(t *testing.T) {
	dbHostKey := map[string]string{"wordpress": "WORDPRESS_DB_HOST", "laravel": "DB_HOST"}
	for _, typ := range []string{"wordpress", "laravel"} {
		for _, env := range []string{"local", "prod"} {
			d := Data{ProjectSlug: "demo", NetworkName: "xdev_demo", AppSlug: "site",
				AppType: typ, Env: env, HostPort: 20000, AdminerPort: 20001,
				SharedDB: true, DBName: "demo_site", DBPass: "p4ss"}
			out, err := RenderCompose(typ, d)
			if err != nil {
				t.Fatalf("%s/%s shared: render error: %v", typ, env, err)
			}
			for _, want := range []string{
				dbHostKey[typ] + ": xdev-db", // DB host points at the platform server
				"demo_site",                  // db/user name injected
				"p4ss",                       // generated password injected
				"name: xdev_shared",          // joins the external shared network
			} {
				if !strings.Contains(out, want) {
					t.Errorf("%s/%s shared: missing %q\n%s", typ, env, want, out)
				}
			}
			for _, not := range []string{
				"demo_site_db",          // no per-app db container
				"MARIADB_ROOT_PASSWORD", // no per-app db env
				"MYSQL_ROOT_PASSWORD",   //
				"_volumes/mysql",        // no per-app db volume
			} {
				if strings.Contains(out, not) {
					t.Errorf("%s/%s shared: must not contain %q\n%s", typ, env, not, out)
				}
			}

			// Dedicated mode: today's per-app db service, no shared network.
			d.SharedDB, d.DBName, d.DBPass = false, "", ""
			out, err = RenderCompose(typ, d)
			if err != nil {
				t.Fatalf("%s/%s dedicated: render error: %v", typ, env, err)
			}
			if !strings.Contains(out, "demo_site_db") {
				t.Errorf("%s/%s dedicated: missing per-app db service\n%s", typ, env, out)
			}
			if strings.Contains(out, "xdev_shared") || strings.Contains(out, "xdev-db") {
				t.Errorf("%s/%s dedicated: must not reference the shared server\n%s", typ, env, out)
			}
		}
	}
}

// TestRenderWithLimits verifies the deploy/resources block appears only when
// limits are set.
func TestRenderWithLimits(t *testing.T) {
	base := Data{ProjectSlug: "p", NetworkName: "xdev_p", AppSlug: "a", AppType: "wordpress", HostPort: 21000}

	out, _ := RenderCompose("wordpress", base)
	if strings.Contains(out, "deploy:") {
		t.Errorf("expected no deploy block without limits:\n%s", out)
	}

	withLimits := base
	withLimits.CPULimit = 1.5
	withLimits.MemLimit = 512 * 1024 * 1024
	out, _ = RenderCompose("wordpress", withLimits)
	for _, want := range []string{"deploy:", `cpus: "1.5"`, "memory: 512m"} {
		if !strings.Contains(out, want) {
			t.Errorf("limits: missing %q\n%s", want, out)
		}
	}
}

// TestLaravelInitNoComposerAfterBootstrap guards the prod path: the hardened
// image ships no composer, so once app/ is bootstrapped init.sh must reach
// octane:start without invoking composer at all. Every composer call therefore
// has to sit behind a filesystem guard that a bootstrapped tree already
// satisfies — a `composer show` probe would exit 127 ("not found"), read as
// "not installed", and send the container into a `composer require` it cannot
// run.
func TestLaravelInitNoComposerAfterBootstrap(t *testing.T) {
	files, err := InfraFiles("laravel")
	if err != nil {
		t.Fatalf("InfraFiles: %v", err)
	}
	init, ok := files["init.sh"]
	if !ok {
		t.Fatal("laravel infra has no init.sh")
	}
	script := uncomment(string(init))

	if strings.Contains(script, "composer show") {
		t.Error("init.sh gates octane on `composer show`; the prod image has no composer")
	}
	for _, want := range []string{
		"[ ! -f artisan ]",             // create-project guard
		"[ ! -f vendor/autoload.php ]", // install guard
		"[ ! -d vendor/laravel/octane ]",
		"XDEV_BOOTSTRAP_ONLY",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("init.sh missing %q", want)
		}
	}
}

// TestLaravelInitBootstrapOnlyStopsBeforeDatabase: the bootstrap container runs
// unattached to the stack's network, so it must exit before the database wait
// and hand ownership to the (unprivileged) prod image's user.
func TestLaravelInitBootstrapOnlyStopsBeforeDatabase(t *testing.T) {
	files, _ := InfraFiles("laravel")
	script := uncomment(string(files["init.sh"]))

	guard := strings.LastIndex(script, `if [ "$XDEV_BOOTSTRAP_ONLY" = "1" ]`)
	if guard < 0 {
		t.Fatal("init.sh has no bootstrap-only guard")
	}
	// The bootstrap container has composer but not the app's PHP/extensions, and
	// isn't on the stack's network. Nothing that boots Laravel or talks to the
	// database may run before it exits.
	for _, after := range []string{
		"php artisan octane:install", "key:generate",
		"waiting for database", "php artisan migrate", "octane:start",
	} {
		if at := strings.Index(script, after); at >= 0 && at < guard {
			t.Errorf("%q runs before the bootstrap-only exit; the bootstrap container can't do it", after)
		}
	}
	// ...and every composer call must run before it, since the runtime image
	// may not have composer at all.
	for _, before := range []string{"composer create-project", "composer install", "composer require"} {
		at := strings.Index(script, before)
		if at < 0 {
			t.Errorf("init.sh no longer runs %q", before)
			continue
		}
		if at > guard {
			t.Errorf("%q runs after the bootstrap-only exit; the runtime image may have no composer", before)
		}
	}
	if !strings.Contains(script[guard:], "chown -R") {
		t.Error("bootstrap-only must chown the tree to the runtime image's user")
	}

	// Composer's own scripts are artisan in disguise — Laravel's
	// post-create-project-cmd runs `artisan migrate`, which has no database to
	// reach from the bootstrap container. Turning scripts off is what keeps the
	// split from leaking.
	if !strings.Contains(script[:guard], "--no-scripts") {
		t.Error("bootstrap mode must pass --no-scripts; composer scripts boot artisan")
	}
	// ...which means nothing creates .env, so the runtime half must, before
	// key:generate writes into it.
	env := strings.Index(script, "cp .env.example .env")
	keygen := strings.Index(script, "key:generate")
	if env < 0 {
		t.Error("init.sh must seed .env; --no-scripts skips the script that would")
	} else if keygen >= 0 && env > keygen {
		t.Error(".env must be seeded before key:generate, which writes into it")
	}
}

// uncomment strips shell comment lines so a test asserting on what init.sh
// *does* isn't tripped by prose explaining what it deliberately avoids.
func uncomment(script string) string {
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}
