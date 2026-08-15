package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xdev/internal/apps"
	"xdev/internal/auth"
	"xdev/internal/config"
	"xdev/internal/platform"
	"xdev/internal/projects"
	"xdev/internal/proxy"
	"xdev/internal/runtime"
	"xdev/internal/secrets"
	"xdev/internal/store"
)

// deployFixture builds a Server backed by a temporary database, plus one static
// app with a webhook and a deploy token already switched on. The app is not a
// real checkout, so a deploy that gets as far as git will fail — which is fine:
// these tests are about who is allowed to *start* one.
func deployFixture(t *testing.T) (*Server, store.App, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "xdev.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	box, err := secrets.New(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DataDir: dir, ProjectsDir: filepath.Join(dir, "projects"), Addr: "127.0.0.1:0"}
	engine := runtime.NewSelector(runtime.Detect(), "")
	appSvc := apps.New(st, engine, nil, "", box)
	pm := proxy.NewManager("127.0.0.1:1", 443, 80, "", "")
	recon := platform.NewReconciler(st, pm, filepath.Join(dir, "hosts"), false)
	srv, err := New(st, auth.New(st, false), engine, cfg, projects.New(st, cfg, engine), appSvc, recon, 443)
	if err != nil {
		t.Fatal(err)
	}

	proj, err := st.CreateProject(store.Project{
		Name: "Demo", Slug: "demo", BaseDomain: "demo.test", Environment: "local",
		NetworkName: "xdev_demo", Engine: "docker", Dir: filepath.Join(dir, "projects", "demo"),
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "site", Slug: "site", Type: store.TypeStatic,
		Domain: "site.demo.test", ServeMode: store.ServeStatic, RootDir: "dist",
		GitURL: "https://github.com/leyume/site", GitRef: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAppDomains(app.ID, []string{"site.demo.test"}, nil, true, "internal"); err != nil {
		t.Fatal(err)
	}

	secret := "s3cret-webhook-key"
	sealed, err := box.Seal([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAppHook(app.ID, "hook123", sealed); err != nil {
		t.Fatal(err)
	}
	token := "xdp_test-token"
	if err := st.SetAppPushToken(app.ID, hashPushToken(token), token[:12]); err != nil {
		t.Fatal(err)
	}
	app, _ = st.AppByID(app.ID)
	return srv, app, secret, token
}

// sign produces the header GitHub sends.
func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func pushBody(ref string) string {
	return `{"ref":"refs/heads/` + ref + `","after":"abc123","deleted":false}`
}

// hookRequest posts a webhook delivery the way GitHub would.
func hookRequest(hookID, event, body, signature string) *http.Request {
	r := httptest.NewRequest("POST", store.HookPathPrefix+hookID, strings.NewReader(body))
	r.Host = "site.demo.test"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GitHub-Event", event)
	if signature != "" {
		r.Header.Set("X-Hub-Signature-256", signature)
	}
	return r
}

// TestWebhookRequiresAValidSignature is the security boundary of the whole
// feature: this endpoint is on the public internet with no session behind it,
// so an unsigned or wrongly-signed delivery must not start anything.
func TestWebhookRequiresAValidSignature(t *testing.T) {
	srv, app, secret, _ := deployFixture(t)
	body := pushBody("main")

	for _, c := range []struct {
		name, sig string
		want      int
	}{
		{"no signature at all", "", http.StatusUnauthorized},
		{"signed with the wrong secret", sign("not-the-secret", body), http.StatusUnauthorized},
		{"signature of a different body", sign(secret, pushBody("other")), http.StatusUnauthorized},
		{"empty signature value", "sha256=", http.StatusUnauthorized},
		{"not even the right scheme", "sha1=abcdef", http.StatusUnauthorized},
	} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, hookRequest(app.HookID, "push", body, c.sig))
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
	}

	// And nothing was recorded as a deploy.
	if d, _ := srv.store.AppDeployments(app.ID, 10); len(d) != 0 {
		t.Errorf("%d deployments recorded from rejected deliveries", len(d))
	}
}

// TestWebhookUnknownIDIs404 — a wrong id must look exactly like a wrong path.
// Anything else confirms which ids exist.
func TestWebhookUnknownIDIs404(t *testing.T) {
	srv, _, secret, _ := deployFixture(t)
	body := pushBody("main")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, hookRequest("nosuchhook", "push", body, sign(secret, body)))
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// TestWebhookOnlyActsOnTheTrackedBranch: a repository's webhook fires for every
// branch. Deploying a feature branch over production because someone pushed it
// is the failure this prevents.
func TestWebhookOnlyActsOnTheTrackedBranch(t *testing.T) {
	srv, app, secret, _ := deployFixture(t)

	body := pushBody("feature/x")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, hookRequest(app.HookID, "push", body, sign(secret, body)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ignored") {
		t.Errorf("push to another branch: %d %s, want an ignored 200", w.Code, w.Body.String())
	}
	if d, _ := srv.store.AppDeployments(app.ID, 10); len(d) != 0 {
		t.Errorf("a push to another branch started %d deploys", len(d))
	}

	// A ping is answered without deploying — it is how GitHub tests the hook.
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, hookRequest(app.HookID, "ping", `{"zen":"hi"}`, sign(secret, `{"zen":"hi"}`)))
	if w.Code != http.StatusOK {
		t.Errorf("ping: %d %s", w.Code, w.Body.String())
	}
	if d, _ := srv.store.AppDeployments(app.ID, 10); len(d) != 0 {
		t.Errorf("a ping started %d deploys", len(d))
	}

	// An event xdev does not act on is acknowledged, not treated as an error —
	// a red delivery in GitHub's list should mean something is actually wrong.
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, hookRequest(app.HookID, "issues", `{}`, sign(secret, `{}`)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ignored") {
		t.Errorf("issues event: %d %s, want an ignored 200", w.Code, w.Body.String())
	}
}

// TestWebhookOnTheTrackedBranchDeploys checks the accepted path: a correctly
// signed push to the right branch is acknowledged immediately (GitHub gives it
// ten seconds) and leaves a deployment row behind.
func TestWebhookOnTheTrackedBranchDeploys(t *testing.T) {
	srv, app, secret, _ := deployFixture(t)
	body := pushBody("main")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, hookRequest(app.HookID, "push", body, sign(secret, body)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202 accepted (%s)", w.Code, w.Body.String())
	}
	// The deploy runs in a goroutine; the row is created before the response.
	deploys, err := srv.store.AppDeployments(app.ID, 10)
	if err != nil || len(deploys) != 1 {
		t.Fatalf("deployments = %v (%v), want exactly one", deploys, err)
	}
	if deploys[0].Trigger != store.DeployWebhook {
		t.Errorf("trigger = %q, want %q", deploys[0].Trigger, store.DeployWebhook)
	}
	// It fails (the app directory is not a checkout) — the point is that it ran
	// and said so rather than staying "running" forever.
	waitForDeploy(t, srv, app.ID)
}

// TestPushDeployNeedsTheToken: the upload endpoint is on the public internet
// too, and it *writes files*. A missing or wrong bearer token must stop it
// before the body is read.
func TestPushDeployNeedsTheToken(t *testing.T) {
	srv, _, _, token := deployFixture(t)

	for _, c := range []struct {
		name, header string
		want         int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer xdp_nope", http.StatusUnauthorized},
		{"right token, wrong scheme", "Basic " + token, http.StatusUnauthorized},
		{"token of another app", "Bearer xdp_test-tokenX", http.StatusUnauthorized},
	} {
		r := httptest.NewRequest("POST", store.PushPath, strings.NewReader("not-a-tarball"))
		r.Host = "site.demo.test"
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.want)
		}
	}
}

// TestPushDeployUnknownHostIs404 — the app is identified by the Host header, so
// a token cannot be used against a hostname that is not its app's.
func TestPushDeployUnknownHostIs404(t *testing.T) {
	srv, _, _, token := deployFixture(t)
	r := httptest.NewRequest("POST", store.PushPath, strings.NewReader("x"))
	r.Host = "somewhere-else.example"
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

// TestValidSignature covers the comparison itself, including the shapes GitHub
// and people actually send.
func TestValidSignature(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	good := sign("key", string(body))
	for _, c := range []struct {
		name, header, secret string
		want                 bool
	}{
		{"exact", good, "key", true},
		// The digest may be sent in either case; the "sha256=" prefix may not —
		// GitHub always sends it lowercase, so there is nothing to be lenient about.
		{"uppercase hex", "sha256=" + strings.ToUpper(strings.TrimPrefix(good, "sha256=")), "key", true},
		{"uppercase prefix", strings.ToUpper(good), "key", false},
		{"surrounding space", " " + good + " ", "key", true},
		{"wrong secret", good, "other", false},
		{"empty secret", good, "", false},
		{"no prefix", strings.TrimPrefix(good, "sha256="), "key", false},
		{"empty header", "", "key", false},
	} {
		if got := validSignature(c.header, c.secret, body); got != c.want {
			t.Errorf("%s: validSignature = %v, want %v", c.name, got, c.want)
		}
	}
	// A body that differs by one byte must not verify.
	if validSignature(good, "key", []byte(`{"ref":"refs/heads/mainX"}`)) {
		t.Error("a modified body verified against the original signature")
	}
}

// TestPushTokenHashing: the database holds a hash, so a copy of it cannot
// deploy — and the comparison still accepts the real token.
func TestPushTokenHashing(t *testing.T) {
	token := "xdp_abcdefghijklmnop"
	hash := hashPushToken(token)
	if strings.Contains(hash, token) || len(hash) != 64 {
		t.Fatalf("hash = %q, want a 64-char digest that does not contain the token", hash)
	}
	if !validPushToken(token, hash) {
		t.Error("the real token was rejected")
	}
	for _, bad := range []string{"", "xdp_abcdefghijklmno", "xdp_abcdefghijklmnopq", hash} {
		if validPushToken(bad, hash) {
			t.Errorf("token %q was accepted", bad)
		}
	}
	if validPushToken(token, "") {
		t.Error("any token was accepted for an app with no token set")
	}
}

// waitForDeploy blocks until the app's newest deploy has finished, so a test
// does not leave a goroutine writing to a database it is about to close.
func waitForDeploy(t *testing.T, srv *Server, appID int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		deploys, err := srv.store.AppDeployments(appID, 1)
		if err == nil && len(deploys) > 0 && !deploys[0].Running() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the deploy never finished")
}
