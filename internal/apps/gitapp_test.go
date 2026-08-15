package apps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xdev/internal/gitsrc"
	"xdev/internal/store"
)

// TestResolveGit pins what the repository fields become in the row: one
// canonical URL whatever was pasted, and a subdirectory that cannot climb out
// of the checkout.
func TestResolveGit(t *testing.T) {
	url, ref, sub, err := resolveGit(GitOpts{URL: "git@github.com:leyume/site.git", Ref: " main ", Subdir: "/apps/web/"})
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/leyume/site" {
		t.Errorf("url = %q, want the canonical https form", url)
	}
	if ref != "main" || sub != "apps/web" {
		t.Errorf("ref/subdir = %q/%q, want main/apps/web", ref, sub)
	}

	// A blank URL is "not a git app", not an error — clearing the field is how
	// an app is disconnected from its repository.
	if url, _, _, err := resolveGit(GitOpts{Ref: "main"}); err != nil || url != "" {
		t.Errorf("blank URL: got %q (%v), want it accepted as not-a-git-app", url, err)
	}

	for _, bad := range []GitOpts{
		{URL: "not a repo"},
		{URL: "https://token@github.com/o/r"}, // a secret in a field that is displayed back
		{URL: "leyume/site", Subdir: "../../etc"},
		{URL: "leyume/site", Ref: "main branch"},
	} {
		if _, _, _, err := resolveGit(bad); err == nil {
			t.Errorf("resolveGit(%+v) should be rejected", bad)
		}
	}
}

// TestDetectNodeBuild covers the defaults a cloned repository implies. Each
// case is a repository shape people actually have; the point is that the add-app
// form comes back filled in rather than blank.
func TestDetectNodeBuild(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		wantCmd string
		wantOut string
	}{{
		name:    "vite with a lockfile",
		files:   map[string]string{"package.json": `{"scripts":{"build":"vite build"},"devDependencies":{"vite":"^5"}}`, "package-lock.json": "{}"},
		wantCmd: "npm ci && npm run build",
		wantOut: "dist",
	}, {
		name:    "no lockfile — npm ci would fail",
		files:   map[string]string{"package.json": `{"scripts":{"build":"vite build"}}`},
		wantCmd: "npm install && npm run build",
		wantOut: "dist",
	}, {
		name:    "pnpm",
		files:   map[string]string{"package.json": `{"scripts":{"build":"vite build"}}`, "pnpm-lock.yaml": ""},
		wantCmd: "pnpm install --frozen-lockfile && pnpm run build",
		wantOut: "dist",
	}, {
		name:    "yarn",
		files:   map[string]string{"package.json": `{"scripts":{"build":"vite build"}}`, "yarn.lock": ""},
		wantCmd: "yarn install --frozen-lockfile && yarn build",
		wantOut: "dist",
	}, {
		name:    "create-react-app builds into build/",
		files:   map[string]string{"package.json": `{"scripts":{"build":"react-scripts build"},"dependencies":{"react-scripts":"5"}}`, "package-lock.json": "{}"},
		wantCmd: "npm ci && npm run build",
		wantOut: "build",
	}, {
		name:    "nuxt builds into .output/public",
		files:   map[string]string{"package.json": `{"scripts":{"build":"nuxt build"},"devDependencies":{"nuxt":"^3"}}`, "package-lock.json": "{}"},
		wantCmd: "npm ci && npm run build",
		wantOut: ".output/public",
	}, {
		name:    "no build script — nothing to run",
		files:   map[string]string{"package.json": `{"scripts":{"test":"vitest"}}`},
		wantCmd: "",
	}, {
		name:    "plain html repo — no package.json at all",
		files:   map[string]string{"index.html": "<h1>hi</h1>"},
		wantCmd: "",
	}, {
		name:    "unreadable package.json is not a crash",
		files:   map[string]string{"package.json": "{oops"},
		wantCmd: "",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range c.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := detectNodeBuild(dir)
			if got.Command != c.wantCmd {
				t.Errorf("command = %q, want %q", got.Command, c.wantCmd)
			}
			if got.OutDir != c.wantOut {
				t.Errorf("out dir = %q, want %q", got.OutDir, c.wantOut)
			}
		})
	}
}

// TestGitAndFolderAreExclusive: a deploy is a hard reset, and xdev's standing
// promise about a folder you already have is that it never rewrites it. The two
// cannot both be true, so asking for both is refused before anything is written.
func TestGitAndFolderAreExclusive(t *testing.T) {
	svc, st, proj := editFixture(t)
	own := t.TempDir()

	_, err := svc.Create(proj.ID, CreateOpts{
		Name: "site", Type: store.TypeStatic, ServeMode: store.ServeStatic,
		SourceDir: own, Git: GitOpts{URL: "leyume/site"},
	})
	if err == nil {
		t.Fatal("a repository and an existing folder should not be accepted together")
	}
	if !strings.Contains(err.Error(), "one source") {
		t.Errorf("error = %v, want it to explain the two sources conflict", err)
	}
	// Nothing was created, and the folder is untouched.
	if apps, _ := st.ListAppsByProject(proj.ID); len(apps) != 0 {
		t.Errorf("%d apps exist after a rejected create", len(apps))
	}
	entries, err := os.ReadDir(own)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the user's folder has %d entries — a rejected create must write nothing", len(entries))
	}
}

// TestUpdateRefusesToAttachARepo: adding a repository to an app that already
// has files would mean clearing them to make room for a clone. That is a new
// app, and the settings page is not where anything gets deleted.
func TestUpdateRefusesToAttachARepo(t *testing.T) {
	svc, st, proj := editFixture(t)
	app, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "site", Slug: "site",
		Type: store.TypeStatic, Domain: "site.demo.test", ServeMode: store.ServeStatic})
	if err != nil {
		t.Fatal(err)
	}
	st.ReplaceAppDomains(app.ID, []string{"site.demo.test"}, nil, true, "internal")

	_, err = svc.Update(app.ID, EditOpts{
		Name: "site", Domain: "site.demo.test", ServeMode: store.ServeStatic,
		Git: GitOpts{URL: "leyume/site"},
	})
	if err == nil {
		t.Fatal("attaching a repository to a non-git app should be rejected")
	}
	after, _ := st.AppByID(app.ID)
	if after.GitURL != "" {
		t.Errorf("git_url = %q after a rejected save, want it unchanged", after.GitURL)
	}
}

// TestDeployNeedsARepo: the Deploy action is only meaningful for a git app, and
// says so rather than failing somewhere inside git.
func TestDeployNeedsARepo(t *testing.T) {
	svc, st, proj := editFixture(t)
	app, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "site", Slug: "site",
		Type: store.TypeStatic, Domain: "site.demo.test", ServeMode: store.ServeStatic})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Deploy(app.ID); err == nil || !strings.Contains(err.Error(), "no repository") {
		t.Errorf("Deploy on a non-git app: %v, want it to say the app has no repository", err)
	}
}

// TestGitFieldsSurviveARoundTrip guards the column list: a field added to the
// struct but forgotten in CreateApp/UpdateApp/appSelect reads back blank, which
// silently turns a private repo app into one that deploys from nowhere.
func TestGitFieldsSurviveARoundTrip(t *testing.T) {
	_, st, proj := editFixture(t)
	app, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "site", Slug: "site", Type: store.TypeStatic,
		Domain: "site.demo.test", ServeMode: store.ServeStatic,
		GitURL: "https://github.com/leyume/site", GitRef: "main", GitSubdir: "apps/web",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !app.IsGit() || app.GitURL != "https://github.com/leyume/site" ||
		app.GitRef != "main" || app.GitSubdir != "apps/web" {
		t.Fatalf("after create: %+v", app)
	}

	app.GitRef = "release"
	if err := st.UpdateApp(app); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeployed(app.ID, "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	got, err := st.AppByID(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GitRef != "release" {
		t.Errorf("git_ref = %q, want the updated value", got.GitRef)
	}
	if got.DeployedSHA != "0123456789abcdef" || got.DeployedAt == "" {
		t.Errorf("deployed = %q at %q, want both recorded", got.DeployedSHA, got.DeployedAt)
	}
	// The list query has its own column list — the same omission hides there.
	list, err := st.ListAppsByProject(proj.ID)
	if err != nil || len(list) != 1 || list[0].GitURL != app.GitURL {
		t.Errorf("ListAppsByProject lost the repository: %+v (%v)", list, err)
	}
}

// TestDeployKeyLifecycle covers the ordering the feature depends on: a key
// exists before its app does, binds to it on create, and goes when the app does.
func TestDeployKeyLifecycle(t *testing.T) {
	_, st, proj := editFixture(t)

	unbound, err := st.CreateDeployKey(store.DeployKey{
		PublicKey: "ssh-ed25519 AAAA xdev-site", PrivateKey: "sealed", Fingerprint: "SHA256:x"})
	if err != nil {
		t.Fatal(err)
	}
	if unbound.AppID != 0 {
		t.Errorf("a new key is bound to app %d, want unbound", unbound.AppID)
	}

	app, err := st.CreateApp(store.App{ProjectID: proj.ID, Name: "site", Slug: "site",
		Type: store.TypeStatic, Domain: "site.demo.test", GitURL: "https://github.com/leyume/site"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BindDeployKey(unbound.ID, app.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.DeployKeyForApp(app.ID)
	if err != nil || got.ID != unbound.ID {
		t.Fatalf("DeployKeyForApp = %+v (%v), want the bound key", got, err)
	}
	// Binding it again would take a repository's access from the app that has it.
	if err := st.BindDeployKey(unbound.ID, app.ID+1); err == nil {
		t.Error("an already-bound key was re-bound to another app")
	}
	if err := st.DeleteDeployKeysForApp(app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeployKeyForApp(app.ID); err == nil {
		t.Error("the key outlived its app")
	}
}

// TestCreateFromRepo is the feature end to end, against a repository on disk:
// the app's folder is a checkout of it, the canonical URL and the deployed
// commit are recorded, and none of the scaffolding that a blank app gets is
// written over the repository's own files.
func TestCreateFromRepo(t *testing.T) {
	if !gitsrc.Available() {
		t.Skip("git is not installed")
	}
	svc, st, proj := editFixture(t)

	// A repository with content of its own — and no package.json, so the create
	// does not try to run a real npm build.
	origin := t.TempDir()
	testGit(t, origin, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "index.html"), []byte("<h1>from the repo</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, origin, "add", "-A")
	testGit(t, origin, "commit", "-m", "first")
	want := strings.TrimSpace(testGit(t, origin, "rev-parse", "HEAD"))

	// Clone from that path instead of from GitHub; everything else is real.
	svc.clone = func(ctx context.Context, dir string, src gitsrc.Source) error {
		cmd := exec.CommandContext(ctx, "git", "clone", origin, dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("clone: %v\n%s", err, out)
		}
		return nil
	}

	app, err := svc.Create(proj.ID, CreateOpts{
		Name: "site", Type: store.TypeStatic, ServeMode: store.ServeStatic,
		Domain: "site.demo.test",
		Git:    GitOpts{URL: "https://github.com/leyume/site", Ref: "main"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if app.GitURL != "https://github.com/leyume/site" || app.GitRef != "main" {
		t.Errorf("repo = %q @ %q, want the canonical URL and branch", app.GitURL, app.GitRef)
	}
	if !app.IsGit() {
		t.Error("IsGit is false for an app created from a repository")
	}

	dir := filepath.Join(proj.Dir, app.Slug)
	if !gitsrc.IsRepo(dir) {
		t.Fatalf("%s is not a git checkout", dir)
	}
	body, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	// The placeholder a blank static app gets must not have replaced the repo's.
	if string(body) != "<h1>from the repo</h1>" {
		t.Errorf("index.html = %q — xdev wrote over the repository's own file", body)
	}

	saved, err := st.AppByID(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.DeployedSHA != want {
		t.Errorf("deployed sha = %q, want %q", saved.DeployedSHA, want)
	}
	if saved.DeployedAt == "" {
		t.Error("deployed_at was not recorded")
	}
}

// testGit runs git in dir with a fixed identity and none of the developer's own
// configuration, so the test behaves the same on every machine.
func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=xdev", "GIT_AUTHOR_EMAIL=xdev@example.com",
		"GIT_COMMITTER_NAME=xdev", "GIT_COMMITTER_EMAIL=xdev@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestCloneAdviceExplainsAPrivateRepoWithNoKey covers the failure that a git
// app re-pointed at a private repository actually produces. With no deploy key
// the remote is HTTPS, git asks for a username it can never be given, and the
// raw error ("exit status 128" / "could not read Username") says nothing about
// the cause. The advice has to name both the cause and the fix.
func TestCloneAdviceExplainsAPrivateRepoWithNoKey(t *testing.T) {
	repo, err := gitsrc.ParseRepo("qodec/bible")
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Errorf("git ls-remote --symref https://github.com/qodec/bible.git HEAD: exit status 128\n" +
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled")

	got := cloneAdvice(raw, repo, false).Error()
	for _, want := range []string{"deploy key", repo.KeysURL(), "private"} {
		if !strings.Contains(got, want) {
			t.Errorf("advice does not mention %q:\n%s", want, got)
		}
	}
	// The original stays reachable — the advice explains, it does not replace.
	if !errors.Is(cloneAdvice(raw, repo, false), raw) {
		t.Error("advice dropped the underlying error")
	}
	// firstLine is what reaches the deployments table, so the advice has to lead.
	if first := firstLine(got); strings.Contains(first, "exit status 128") {
		t.Errorf("the message a user sees is still the raw git error: %q", first)
	}

	// An app that *has* a key gets different advice: the key is on xdev but not
	// on the repository, which is a different thing to go and do.
	withKey := cloneAdvice(fmt.Errorf("ERROR: Repository not found."), repo, true).Error()
	if !strings.Contains(withKey, "add the public key") {
		t.Errorf("key-holding app got the wrong advice: %s", withKey)
	}
}

// TestTailLogKeepsTheReason pins that a failure records its cause. The message
// column only gets firstLine(err), and git puts the reason on the second line —
// so if the log dropped the error too, a deploy that failed before the build
// step would record no diagnosis anywhere.
func TestTailLogKeepsTheReason(t *testing.T) {
	err := fmt.Errorf("git fetch: exit status 128\nfatal: could not read Username")

	// Nothing was built: the error is the whole log.
	if got := tailLog("", err); !strings.Contains(got, "could not read Username") {
		t.Errorf("git failure lost its reason: %q", got)
	}
	// The build ran and failed: both survive, error first.
	got := tailLog("npm ERR! missing script: build", err)
	if !strings.Contains(got, "could not read Username") || !strings.Contains(got, "npm ERR!") {
		t.Errorf("log dropped one of the two halves: %q", got)
	}
	// A success keeps its output: the settings page renders it as a list of
	// steps, which is how anyone sees what a deploy actually did.
	if got := tailLog("lots of build output", nil); got != "lots of build output" {
		t.Errorf("successful deploy did not keep its log: %q", got)
	}
	// Either way the tail is bounded — the head is dropped, not the end, since
	// that is where a build's error is.
	big := strings.Repeat("x", maxLogBytes+500)
	if got := tailLog(big, nil); len(got) > maxLogBytes+64 || !strings.Contains(got, "trimmed") {
		t.Errorf("oversized log not trimmed: %d bytes", len(got))
	}
}

// TestCreateLaravelFromRepo pins the layout that makes a container app
// deployable from a repository: the clone lands in app/ — not the app directory
// — and the two things a deploy must never destroy sit outside it.
//
// It stops at Create. Everything past that point (composer, migrate, octane)
// runs inside a container, which a unit test has no business starting.
func TestCreateLaravelFromRepo(t *testing.T) {
	if !gitsrc.Available() {
		t.Skip("git is not installed")
	}
	svc, st, proj := editFixture(t)

	origin := t.TempDir()
	testGit(t, origin, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "artisan"), []byte("#!/usr/bin/env php\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, origin, "add", "-A")
	testGit(t, origin, "commit", "-m", "first")

	svc.clone = func(ctx context.Context, dir string, src gitsrc.Source) error {
		out, err := exec.CommandContext(ctx, "git", "clone", origin, dir).CombinedOutput()
		if err != nil {
			return fmt.Errorf("clone: %v\n%s", err, out)
		}
		return nil
	}

	// layoutContainer is the unit under test. Create would also Start the stack,
	// and there is no container engine here — the same reason newComposeApp
	// exists for the compose tests.
	// Runtime is pinned so layoutContainer resolves the image without asking the
	// engine selector, which the fixture does not have.
	app := store.App{ProjectID: proj.ID, Name: "shop", Slug: "shop",
		Type: "laravel", Domain: "shop.demo.test", DBMode: "dedicated", Runtime: "docker"}
	opts := CreateOpts{
		Name: "shop", Type: "laravel", Domain: "shop.demo.test",
		DBMode: "dedicated", // no shared xdev-db to provision in a unit test
		Git:    GitOpts{URL: "https://github.com/leyume/shop", Ref: "main"},
	}
	appDir := filepath.Join(proj.Dir, app.Slug)
	if _, err := svc.layoutContainer(&app, &opts, proj, appDir); err != nil {
		t.Fatalf("layoutContainer: %v", err)
	}
	saved, err := st.CreateApp(app)
	if err != nil {
		t.Fatal(err)
	}

	code := filepath.Join(appDir, "app")

	// The checkout is app/, one level below the app directory.
	if !gitsrc.IsRepo(code) {
		t.Errorf("%s is not a git checkout — the clone went to the wrong place", code)
	}
	if gitsrc.IsRepo(appDir) {
		t.Error("the app directory itself is a checkout — a deploy would reset the compose file and volumes")
	}
	if got := svc.codeDir(app); got != code {
		t.Errorf("codeDir = %q, want %q", got, code)
	}

	// State a deploy must not be able to destroy, all outside the checkout.
	for _, p := range []string{
		filepath.Join(appDir, "_", "laravel.env"),
		filepath.Join(appDir, "_", "compose.yml"),
		filepath.Join(appDir, "_volumes", "storage", "framework", "sessions"),
		filepath.Join(appDir, "_volumes", "storage", "logs"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}

	// The generated .env must carry a real app key — regenerating one later
	// would invalidate every session and encrypted column the app has issued.
	env, err := os.ReadFile(filepath.Join(appDir, "_", "laravel.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "APP_KEY=base64:") {
		t.Errorf("laravel.env has no APP_KEY:\n%s", env)
	}

	if saved, err = st.AppByID(saved.ID); err != nil {
		t.Fatal(err)
	}
	if !saved.IsGit() || saved.IsHostProc() {
		t.Errorf("saved app: IsGit=%v IsHostProc=%v, want a git-backed container app", saved.IsGit(), saved.IsHostProc())
	}
	if saved.SkipDBDump {
		t.Error("skip_db_dump defaulted to on — a deploy would migrate with no snapshot")
	}
}

// TestGitIsRefusedForTypesThatCannotDeployIt keeps a repository from being
// attached to a stack that has no way to act on it.
func TestGitIsRefusedForTypesThatCannotDeployIt(t *testing.T) {
	for _, tc := range []struct {
		appType string
		want    bool
	}{
		{store.TypeStatic, true},
		{store.TypeGo, true},
		{"laravel", true},
		{"wordpress", false},
		{store.TypeCompose, false},
		{store.TypeProxy, false},
	} {
		if got := canDeployFromGit(tc.appType); got != tc.want {
			t.Errorf("canDeployFromGit(%q) = %v, want %v", tc.appType, got, tc.want)
		}
	}
}

// TestComposerInstallFallsBackWhenTheImageHasNone covers the failure that took
// down a real prod app: the production Swoole image is runtime-only and has no
// composer, so exec'ing one into it exits 126. Dependencies have to come from
// somewhere else, and the deploy has to notice by itself.
func TestComposerInstallFallsBackWhenTheImageHasNone(t *testing.T) {
	svc, _, _ := editFixture(t)

	// The app image has composer: use it, and never reach for a toolchain.
	var got []string
	ok := func(args ...string) (string, error) {
		got = append(got, strings.Join(args, " "))
		return "Composer version 2.7.0", nil
	}
	out, deferred, err := svc.composerInstall(context.Background(), "docker", store.App{}, true, ok)
	if err != nil {
		t.Fatalf("in-container install: %v", err)
	}
	if deferred {
		t.Error("in-container install should run composer's scripts itself")
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "composer --version") {
		t.Errorf("calls = %v, want a --version probe then the install", got)
	}
	if !strings.Contains(got[1], "install --no-dev --optimize-autoloader") {
		t.Errorf("install args = %q", got[1])
	}
	if strings.Contains(out, "one-off") {
		t.Error("used a toolchain container even though the image had composer")
	}

	// The probe fails (exit 126, "executable file not found"): fall back rather
	// than reporting the probe's error as the install's. The app here has no
	// project, so the fallback stops at locating the checkout — which is enough
	// to prove the branch was taken without needing a container engine.
	missing := func(args ...string) (string, error) {
		return "", errors.New(`exec: "composer": executable file not found in $PATH`)
	}
	_, _, err = svc.composerInstall(context.Background(), "docker", store.App{}, true, missing)
	if err == nil {
		t.Fatal("expected the fallback to be attempted")
	}
	if !strings.Contains(err.Error(), "checkout") {
		t.Errorf("error = %v, want it to come from the fallback path, not the probe", err)
	}

	// A stopped app skips the probe entirely — there is nothing to exec into.
	probed := false
	_, _, err = svc.composerInstall(context.Background(), "docker", store.App{}, false,
		func(args ...string) (string, error) { probed = true; return "", nil })
	if probed {
		t.Error("probed a container that is not running")
	}
	if err == nil {
		t.Error("expected the fallback path for a stopped app")
	}
}

// TestEnvPathFollowsTheMountedFile: a Laravel app's .env is _/laravel.env,
// mounted over /var/www/html/.env. Editing app/.env there writes to a file the
// mount shadows — saved, never read, and no error to show for it. Apps created
// before that mount existed must keep using app/.env.
func TestEnvPathFollowsTheMountedFile(t *testing.T) {
	svc, st, proj := editFixture(t)
	appDir := filepath.Join(proj.Dir, "shop")
	if err := os.MkdirAll(filepath.Join(appDir, "_"), 0o755); err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateApp(store.App{
		ProjectID: proj.ID, Name: "Shop", Slug: "shop", Type: "laravel",
		Domain: "shop.demo.test", ComposePath: filepath.Join(appDir, "_", "compose.yml"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// No mounted file yet: the old location, so existing apps are unaffected.
	got, err := svc.envPath(app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(appDir, "app", ".env"); got != want {
		t.Errorf("without the mount: envPath = %q, want %q", got, want)
	}
	if loc := svc.EnvLocation(app.ID); loc != "app/.env" {
		t.Errorf("EnvLocation = %q, want app/.env", loc)
	}

	// Once it exists, that is the file the container actually reads.
	mounted := filepath.Join(appDir, "_", "laravel.env")
	if err := os.WriteFile(mounted, []byte("APP_KEY=base64:x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.envPath(app.ID); got != mounted {
		t.Errorf("with the mount: envPath = %q, want %q", got, mounted)
	}
	if loc := svc.EnvLocation(app.ID); loc != "_/laravel.env" {
		t.Errorf("EnvLocation = %q, want _/laravel.env", loc)
	}

	// A round trip goes to the mounted file, and keeps it unreadable to others:
	// it holds the app key and the database password.
	if err := svc.WriteEnv(app.ID, "APP_KEY=base64:y\nDB_PASSWORD=hunter2\n"); err != nil {
		t.Fatal(err)
	}
	back, err := svc.ReadEnv(app.ID)
	if err != nil || !strings.Contains(back, "hunter2") {
		t.Errorf("ReadEnv = %q (%v), want the value just written", back, err)
	}
	info, err := os.Stat(mounted)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("laravel.env mode = %o, want 600 — it holds the app key and DB password", perm)
	}
	// And nothing was written to the shadowed path.
	if _, err := os.Stat(filepath.Join(appDir, "app", ".env")); err == nil {
		t.Error("wrote to app/.env, which the bind mount shadows")
	}
}
