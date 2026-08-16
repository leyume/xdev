package templates

import (
	"strings"
	"testing"
)

func renderFPM(t *testing.T, d Data) string {
	t.Helper()
	d.Server = "fpm"
	out, err := RenderCompose("laravel", d)
	if err != nil {
		t.Fatalf("render fpm: %v", err)
	}
	return out
}

func fpmData() Data {
	return Data{
		ProjectSlug: "demo", NetworkName: "demo_net", AppSlug: "api",
		AppType: "laravel", Env: "prod", HostPort: 20005,
		SharedDB: true, DBName: "demo_api", DBPass: "s3cret",
	}
}

// serviceBlock returns the lines of one top-level compose service, so an
// assertion about "the web service" cannot accidentally match a line belonging
// to another one. Services are two-space indented under `services:`.
func serviceBlock(yaml, name string) string {
	lines := strings.Split(yaml, "\n")
	start := -1
	for i, l := range lines {
		if l == "  "+name+":" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	for i := start; i < len(lines); i++ {
		l := lines[i]
		if strings.TrimSpace(l) == "" || strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "  #") {
			continue
		}
		return strings.Join(lines[start:i], "\n")
	}
	return strings.Join(lines[start:], "\n")
}

// The fpm stack has the services the design depends on, and specifically not
// the single `app` service that is the Swoole shape.
func TestFPMComposeShape(t *testing.T) {
	out := renderFPM(t, fpmData())

	for _, want := range []string{"php", "web", "redis"} {
		if serviceBlock(out, want) == "" {
			t.Errorf("missing %q service:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\n  app:\n") {
		t.Error("fpm stack still has an 'app' service — that is the Swoole shape")
	}
	if !strings.Contains(serviceBlock(out, "php"), "XDEV_SERVER: fpm") {
		t.Error("php service does not set XDEV_SERVER=fpm; init.sh would start Octane")
	}
}

// php-fpm speaks FastCGI, which is unauthenticated: publishing :9000 to the
// host would expose arbitrary script execution. Only nginx gets a port.
func TestFPMDoesNotPublishFastCGI(t *testing.T) {
	out := renderFPM(t, fpmData())

	if php := serviceBlock(out, "php"); strings.Contains(php, "ports:") {
		t.Errorf("php service publishes a port; FastCGI must stay on the internal network:\n%s", php)
	}
	web := serviceBlock(out, "web")
	if !strings.Contains(web, `"20005:80"`) {
		t.Errorf("web service does not publish the app's host port:\n%s", web)
	}
	if strings.Contains(out, ":9000\"") {
		t.Error("something publishes :9000 to the host")
	}
}

// nginx serves public/ and has no reason to write to the checkout; php-fpm must
// be able to, or composer and storage/ fail on first boot.
func TestFPMMountPermissions(t *testing.T) {
	out := renderFPM(t, fpmData())

	if web := serviceBlock(out, "web"); !strings.Contains(web, "../app:/var/www/html:ro") {
		t.Errorf("nginx does not mount the app read-only:\n%s", web)
	}
	php := serviceBlock(out, "php")
	if !strings.Contains(php, "- ../app:/var/www/html\n") {
		t.Errorf("php-fpm has no writable app mount:\n%s", php)
	}
	// State stays outside the checkout, same as the Swoole stack: a git deploy
	// resets the working tree hard and must not take uploads or the key with it.
	for _, want := range []string{"../_volumes/storage:/var/www/html/storage", "./laravel.env:/var/www/html/.env"} {
		if !strings.Contains(php, want) {
			t.Errorf("php service missing state mount %q", want)
		}
	}
}

// The server choice must not quietly change where data lives.
func TestFPMHonoursDatabaseMode(t *testing.T) {
	shared := renderFPM(t, fpmData())
	if serviceBlock(shared, "db") != "" {
		t.Error("shared-DB app rendered a db service")
	}
	if !strings.Contains(shared, "xdev_shared") {
		t.Error("shared-DB app is not on the shared network")
	}
	if !strings.Contains(serviceBlock(shared, "php"), "DB_PASSWORD: s3cret") {
		t.Error("shared-DB app did not get its database password")
	}

	d := fpmData()
	d.SharedDB, d.DBName, d.DBPass = false, "", ""
	dedicated := renderFPM(t, d)
	if serviceBlock(dedicated, "db") == "" {
		t.Error("dedicated-DB app rendered no db service")
	}
	if strings.Contains(dedicated, "xdev_shared") {
		t.Error("dedicated-DB app was put on the shared network")
	}
}

// The whole point of the option: choosing fpm changes the image, and leaving it
// unset does not.
func TestFPMSelectsTheFPMImage(t *testing.T) {
	if php := serviceBlock(renderFPM(t, fpmData()), "php"); !strings.Contains(php, LaravelFPMImage) {
		t.Errorf("fpm stack does not run %s:\n%s", LaravelFPMImage, php)
	}

	swoole, err := RenderCompose("laravel", fpmData()) // Server unset
	if err != nil {
		t.Fatalf("render swoole: %v", err)
	}
	if strings.Contains(swoole, LaravelFPMImage) {
		t.Error("the default (Swoole) stack picked up the fpm image")
	}
	if !strings.Contains(swoole, "\n  app:\n") {
		t.Error("the default stack is no longer the Swoole shape")
	}
}

// Adminer is bundled the same way in both stacks, so switching server does not
// drop a service the user turned on, or move the port they bookmarked.
func TestFPMKeepsAdminer(t *testing.T) {
	d := fpmData()
	d.AdminerPort = 20006
	out := renderFPM(t, d)

	if serviceBlock(out, "adminer") == "" {
		t.Error("fpm stack dropped the adminer service")
	}
	if !strings.Contains(out, "20006:") {
		t.Error("fpm stack did not publish adminer on its allocated port")
	}
}

// Resource limits are the app's, not the server's.
func TestFPMCarriesLimits(t *testing.T) {
	d := fpmData()
	d.CPULimit, d.MemLimit = 1.5, 512*1024*1024
	php := serviceBlock(renderFPM(t, d), "php")

	if !strings.Contains(php, `cpus: "1.5"`) {
		t.Errorf("cpu limit not applied:\n%s", php)
	}
	if !strings.Contains(php, "memory: 512m") {
		t.Errorf("memory limit not applied:\n%s", php)
	}
}

// init.sh is one script for both servers. These assert the fpm branch does not
// do Octane's work and ends up exec'ing php-fpm, because getting it wrong means
// a container that either installs Octane into an fpm app or never serves.
func TestInitScriptServesBothServers(t *testing.T) {
	raw, err := filesFS.ReadFile("files/laravel/infra/init.sh")
	if err != nil {
		t.Fatalf("read init.sh: %v", err)
	}
	sh := string(raw)

	// The two Octane steps must both be behind the server check, or an fpm app
	// pulls laravel/octane into its composer.json on first boot.
	for _, guarded := range []string{
		`if [ "$XDEV_SERVER" != "fpm" ] && [ ! -d vendor/laravel/octane ]`,
		`if [ "$XDEV_SERVER" != "fpm" ] && [ ! -f config/octane.php ]`,
	} {
		if !strings.Contains(sh, guarded) {
			t.Errorf("init.sh does not guard an Octane step:\n  want %s", guarded)
		}
	}

	// The fpm exec has to come before the Octane one, or it is unreachable.
	fpmExec := strings.Index(sh, "exec php-fpm -F")
	octaneExec := strings.Index(sh, "exec php artisan octane:start")
	switch {
	case fpmExec < 0:
		t.Error("init.sh never execs php-fpm")
	case octaneExec < 0:
		t.Error("init.sh no longer execs Octane")
	case fpmExec > octaneExec:
		t.Error("the php-fpm exec is after the Octane one, so it can never run")
	}

	// php-fpm binds no HTTP port; nginx does. A --host/--port on that line
	// would mean the wrong server was copied.
	line := sh[fpmExec:]
	if i := strings.IndexByte(line, '\n'); i > 0 {
		line = line[:i]
	}
	if strings.Contains(line, "--port") || strings.Contains(line, "--host") {
		t.Errorf("php-fpm exec carries HTTP flags it cannot use: %q", line)
	}
}

// The nginx config has to ship, since the fpm compose mounts it.
func TestFPMNginxConfShips(t *testing.T) {
	raw, err := filesFS.ReadFile("files/laravel/infra/nginx.conf")
	if err != nil {
		t.Fatalf("nginx.conf is not embedded: %v", err)
	}
	conf := string(raw)
	for _, want := range []string{
		"root /var/www/html/public", // Laravel's docroot, not the project root
		"fastcgi_pass php:9000",     // the service name the compose file uses
		"try_files $uri $uri/ /index.php?$query_string",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("nginx.conf missing %q", want)
		}
	}
}
