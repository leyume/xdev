package apps

import (
	"net"
	"strings"
	"testing"

	"xdev/internal/store"
	"xdev/internal/templates"
)

// TestValidUpstream exercises the proxy-app upstream validator:
// http(s)://host[:port] with an optional path prefix passes (normalized);
// anything else is rejected.
func TestValidUpstream(t *testing.T) {
	good := map[string]string{
		"http://10.0.0.5:3000":      "http://10.0.0.5:3000",
		"https://coolify.example":   "https://coolify.example",
		" https://h.example:8443/ ": "https://h.example:8443", // trimmed, trailing slash dropped
		// An app under a prefix on the other server keeps its path.
		"https://mail.example.com/snappymail": "https://mail.example.com/snappymail",
		"https://mail.example.com/webmail/":   "https://mail.example.com/webmail", // trailing slash dropped
		"http://10.0.0.5:3000/a/b":            "http://10.0.0.5:3000/a/b",
		"https://h.example/one%20two":         "https://h.example/one%20two", // stays escaped
	}
	for in, want := range good {
		got, err := validUpstream(in)
		if err != nil || got != want {
			t.Errorf("validUpstream(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "example.com", "ftp://x.example", "http://",
		"http://h.example?x=1", "http://h.example/path?x=1", "http://user:pw@h.example",
		"http://h.example/path#frag",
	} {
		if _, err := validUpstream(in); err == nil {
			t.Errorf("validUpstream(%q) should be rejected", in)
		}
	}
}

// TestGoScaffoldMatchesDefaults checks the go app's shipped scaffold and its
// default build/start commands agree: the build writes the binary the start
// command runs, and the scaffold binds the $PORT xdev allocates.
func TestGoScaffoldMatchesDefaults(t *testing.T) {
	files, err := templates.ScaffoldFiles(store.TypeGo)
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	main, ok := files["main.go.tpl"]
	if !ok {
		t.Fatalf("go scaffold missing main.go.tpl, has %v", files)
	}
	if _, ok := files["go.mod.tpl"]; !ok {
		t.Fatalf("go scaffold missing go.mod.tpl (go build needs a module)")
	}
	if !strings.Contains(string(main), `os.Getenv("PORT")`) {
		t.Error("scaffold does not read $PORT — xdev's route would hit a dead port")
	}
	// `go build -o app .` then `./app`: the names must line up.
	bin := strings.TrimPrefix(defaultGoStartCmd, "./")
	if !strings.Contains(defaultGoBuildCmd, "-o "+bin) {
		t.Errorf("build %q does not produce the binary start %q runs", defaultGoBuildCmd, defaultGoStartCmd)
	}
}

// TestPortFreeSeesLoopbackBind guards the port allocator against handing out a
// port something already holds on 127.0.0.1 only (podman's gvproxy does this):
// SO_REUSEADDR lets the wildcard bind succeed anyway, so portFree must probe
// loopback too.
func TestPortFreeLoopbackBind(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	p := ln.Addr().(*net.TCPAddr).Port
	if portFree(p) {
		t.Errorf("portFree(%d) = true while 127.0.0.1:%d is bound", p, p)
	}
}

// TestDBEnvFile checks the generated .env parses the way staticEnv reads it
// (KEY=VALUE lines) and that the DSN agrees with the individual DB_* vars — a
// mismatch would hand the app two different sets of credentials.
func TestDBEnvFile(t *testing.T) {
	got := map[string]string{}
	for _, line := range strings.Split(dbEnvFile("bpm_gosvc", "s3cr3t", 20021), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("line is not KEY=VALUE: %q", line)
		}
		got[k] = v
	}
	want := map[string]string{
		"DB_HOST": "127.0.0.1", "DB_PORT": "20021", "DB_NAME": "bpm_gosvc",
		"DB_USER": "bpm_gosvc", "DB_PASSWORD": "s3cr3t",
		"DB_DSN": "bpm_gosvc:s3cr3t@tcp(127.0.0.1:20021)/bpm_gosvc?parseTime=true",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestPickPortsSkipsTaken covers the allocation rules that a port collision
// would violate: taken ports (stored ones plus any picked earlier in the same
// create, which aren't in the store or bound yet) are skipped, and a pass never
// repeats a port.
func TestPickPortsSkipsTaken(t *testing.T) {
	free := func(int) bool { return true }
	got := pickPorts(3, []int{portMin, portMin + 2}, free)
	want := []int{portMin + 1, portMin + 3, portMin + 4}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("pickPorts = %v, want %v", got, want)
		}
	}
	// A port the host says is busy is skipped even when nothing claims it.
	busy := portMin + 1
	got = pickPorts(1, nil, func(p int) bool { return p != busy })
	if got[0] == busy {
		t.Errorf("handed out busy port %d", busy)
	}
}
