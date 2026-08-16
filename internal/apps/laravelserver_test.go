package apps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/store"
)

func TestCanSwitchLaravelServer(t *testing.T) {
	cases := []struct {
		app  store.App
		want bool
	}{
		{store.App{Type: "laravel", ComposePath: "/x/_/compose.yml"}, true},
		{store.App{Type: "laravel"}, false}, // shared/no stack of its own
		{store.App{Type: "wordpress", ComposePath: "/x/_/compose.yml"}, false},
		{store.App{Type: "static"}, false},
	}
	for _, tc := range cases {
		if got := CanSwitchLaravelServer(tc.app); got != tc.want {
			t.Errorf("%s (compose %q) = %v, want %v", tc.app.Type, tc.app.ComposePath, got, tc.want)
		}
	}
}

// The switch regenerates the compose file from this password. Reading it wrong
// hands the app a stack that cannot authenticate against its own database, so
// the parser is deliberately literal.
func TestEnvValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "laravel.env")
	body := "" +
		"APP_ENV=production\n" +
		"# DB_PASSWORD=commented-out\n" +
		"DB_PASSWORD=p@ss=with=equals\n" +
		"REDIS_HOST=redis\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := envValue(path, "DB_PASSWORD")
	if err != nil {
		t.Fatalf("envValue: %v", err)
	}
	// A password containing '=' must survive: splitting on every '=' instead of
	// the first would truncate it silently.
	if got != "p@ss=with=equals" {
		t.Errorf("got %q, want the full value including '=' characters", got)
	}
}

func TestEnvValueIgnoresCommentsAndPrefixes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "laravel.env")
	// DB_PASSWORD_EXTRA must not satisfy a lookup of DB_PASSWORD.
	body := "DB_PASSWORD_EXTRA=wrong\n#DB_PASSWORD=alsowrong\nDB_PASSWORD=right\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := envValue(path, "DB_PASSWORD")
	if err != nil {
		t.Fatalf("envValue: %v", err)
	}
	if got != "right" {
		t.Errorf("got %q, want %q", got, "right")
	}
}

func TestEnvValueStripsQuotes(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ raw, want string }{
		{`DB_PASSWORD="quoted"`, "quoted"},
		{`DB_PASSWORD='single'`, "single"},
		{`DB_PASSWORD=bare`, "bare"},
		{`DB_PASSWORD="un'even`, `"un'even`}, // not a matching pair: left alone
	} {
		path := filepath.Join(dir, "e.env")
		os.WriteFile(path, []byte(tc.raw+"\n"), 0o600)
		got, err := envValue(path, "DB_PASSWORD")
		if err != nil {
			t.Fatalf("envValue(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Errorf("envValue(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// A missing key is empty with no error; a missing file is an error. The switch
// treats them differently — the first is a corrupt env it refuses to guess at,
// the second is a broken app.
func TestEnvValueMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "laravel.env")
	os.WriteFile(path, []byte("APP_ENV=production\n"), 0o600)

	got, err := envValue(path, "DB_PASSWORD")
	if err != nil || got != "" {
		t.Errorf("missing key: got %q, %v — want empty and no error", got, err)
	}
	if _, err := envValue(filepath.Join(dir, "nope.env"), "DB_PASSWORD"); err == nil {
		t.Error("missing file did not error")
	}
}

// Blank means Swoole everywhere, so a row written before the column existed
// keeps the stack it was built with.
func TestLaravelServerDefault(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", store.LaravelSwoole},
		{"swoole", store.LaravelSwoole},
		{"fpm", store.LaravelFPM},
		{"nonsense", store.LaravelSwoole},
	} {
		if got := store.LaravelServerOr(tc.in); got != tc.want {
			t.Errorf("LaravelServerOr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A bind mount whose source is missing gets a directory created for it by the
// engine, and the container then cannot start because a directory will not
// mount over a file. Worse, the directory then makes every later write fail
// with "is a directory", so the app stays broken until someone deletes it by
// hand. writeInfra has to clear it.
func TestWriteInfraReplacesAnEngineCreatedDirectory(t *testing.T) {
	svc := &Service{}
	dir := t.TempDir()

	// Exactly what docker leaves behind: an empty directory at the file's path.
	stray := filepath.Join(dir, "nginx.conf")
	if err := os.Mkdir(stray, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := svc.writeInfra("laravel", dir); err != nil {
		t.Fatalf("writeInfra: %v", err)
	}

	fi, err := os.Stat(stray)
	if err != nil {
		t.Fatalf("nginx.conf missing after writeInfra: %v", err)
	}
	if fi.IsDir() {
		t.Fatal("nginx.conf is still a directory; the container would fail to start again")
	}
	body, _ := os.ReadFile(stray)
	if !strings.Contains(string(body), "fastcgi_pass php:9000") {
		t.Error("nginx.conf was replaced with the wrong contents")
	}
}

// A directory with anything in it belongs to somebody, and is not an artifact
// of a failed mount. Refuse rather than delete it.
func TestWriteInfraWillNotDeleteANonEmptyDirectory(t *testing.T) {
	svc := &Service{}
	dir := t.TempDir()

	stray := filepath.Join(dir, "nginx.conf")
	if err := os.Mkdir(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "keepme"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := svc.writeInfra("laravel", dir); err == nil {
		t.Fatal("writeInfra deleted or ignored a directory holding data")
	}
	if _, err := os.Stat(filepath.Join(stray, "keepme")); err != nil {
		t.Errorf("the file inside the directory was destroyed: %v", err)
	}
}

// The support files an fpm stack mounts must all land, and init.sh must be the
// current one — an app carrying the pre-XDEV_SERVER copy would try to start
// Octane inside an image that has no Swoole.
func TestWriteInfraShipsWhatTheFPMStackMounts(t *testing.T) {
	svc := &Service{}
	dir := t.TempDir()
	if err := svc.writeInfra("laravel", dir); err != nil {
		t.Fatalf("writeInfra: %v", err)
	}

	init, err := os.ReadFile(filepath.Join(dir, "init.sh"))
	if err != nil {
		t.Fatalf("init.sh: %v", err)
	}
	if !strings.Contains(string(init), "exec php-fpm -F") {
		t.Error("init.sh cannot serve fpm; a switched app would look for Octane")
	}
	if fi, err := os.Stat(filepath.Join(dir, "init.sh")); err == nil && fi.Mode()&0o111 == 0 {
		t.Error("init.sh is not executable")
	}
	if _, err := os.Stat(filepath.Join(dir, "nginx.conf")); err != nil {
		t.Errorf("nginx.conf not written: %v", err)
	}
}
