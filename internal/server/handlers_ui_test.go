package server

import (
	"regexp"
	"strings"
	"testing"

	"xdev/internal/gitsrc"
	"xdev/internal/store"
	"xdev/internal/templates"
)

// gitApp is a git-backed Laravel app with both deploy endpoints switched on —
// the state in which the settings page has every generated value to hand over.
func gitApp() store.App {
	return store.App{
		ID: 1, Name: "web", Slug: "web", Type: "laravel", Status: store.AppRunning,
		Domain: "web.demo.test", Port: 20001, ComposePath: "/p/demo/web/_/compose.yml",
		GitURL: "https://github.com/o/r", GitRef: "main",
	}
}

func gitViewData() viewData {
	return viewData{
		"Git": &gitInfo{
			Repo: gitsrc.Repo{Owner: "o", Name: "r"}, URL: "https://github.com/o/r",
			KeysURL: "https://github.com/o/r/settings/keys", Ref: "main",
			PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 xdev-web", Fingerprint: "SHA256:abc",
		},
		"Deploy": &deployInfo{
			HookURL: "https://web.demo.test/_xdev/hook/hk", HookSecret: "the-signing-secret",
			Ref: "main", PushURL: "https://web.demo.test/_xdev/deploy", PushHint: "xdp_abc",
			PushTarget: "/p/demo/web", NewToken: "xdp_freshly-issued-token", Repo: "o/r",
		},
	}
}

// Every value xdev generates for you to paste somewhere else gets a Copy button
// beside it. Selecting a three-line key or a 43-character secret by hand is
// where a half-copied value comes from, and a half-copied deploy key fails as
// "repository not found" an hour later.
func TestSettingsPageCopiesEveryGeneratedValue(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web", Domain: "web.demo.test"}, gitViewData())

	for _, want := range []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 xdev-web", // deploy key
		"https://web.demo.test/_xdev/hook/hk",       // payload URL
		"the-signing-secret",                        // webhook secret
		"xdp_freshly-issued-token",                  // the token shown once
		"https://web.demo.test/_xdev/deploy",        // push endpoint
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("settings page does not show %q at all", want)
		}
		if !inCopyRow(out, want) {
			t.Errorf("%q is shown without a Copy button beside it", want)
		}
	}

	// The GitHub Actions sample is copyable too, and its ${{ }} expressions must
	// reach the page as GitHub's syntax rather than as Go template escaping.
	if !strings.Contains(out, "${{ secrets.XDEV_DEPLOY_TOKEN }}") {
		t.Error("the workflow sample's ${{ secrets… }} expression is mangled")
	}
}

// inCopyRow reports whether value appears inside a .copy-row that also carries a
// [data-copy] button — i.e. the markup layout.html's click handler acts on.
func inCopyRow(html, value string) bool {
	for _, row := range strings.Split(html, `class="copy-row"`)[1:] {
		end := strings.Index(row, "</div>")
		if end < 0 {
			end = len(row)
		}
		block := row[:end]
		if strings.Contains(block, value) && strings.Contains(block, "data-copy") {
			return true
		}
	}
	return false
}

// The read-only fields holding those values must not be a 20-column box you
// have to scroll sideways to read: the stylesheet gives every textarea the full
// width, not only the .code ones the compose editor uses.
func TestReadOnlyTextareasAreFullWidth(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web", Domain: "web.demo.test"}, gitViewData())
	rule := regexp.MustCompile(`(?m)^input, select, textarea \{`)
	if !rule.MatchString(out) {
		t.Error("the width rule still names textarea.code only, so .mono textareas render at their default 20 columns")
	}
}

// Deploys sits in the sticky side column. Pressing "Deploy latest" used to
// scroll its own progress out of sight, which is exactly when anyone wants to
// watch it.
func TestDeploysAreInTheSideColumn(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web", Domain: "web.demo.test"}, gitViewData())
	i := strings.Index(out, `class="side-col is-sticky"`)
	if i < 0 {
		t.Fatal("no sticky side column on the settings page")
	}
	rest := out[i:]
	if !strings.Contains(rest[:min(len(rest), 400)], "Deploys") {
		t.Error("the side column does not open with the Deploys card")
	}
	// And the main column is still the one holding the settings form.
	if strings.Index(out, `class="main-col"`) > i {
		t.Error("the main column comes after the side column")
	}
}

// The Adminer toggle: shown for Laravel in both directions, absent for a type
// that has no Adminer to switch.
func TestAdminerToggle(t *testing.T) {
	app := gitApp()

	on := renderSettings(t, app, appSettingsForm{Name: "web", Domain: "web.demo.test"},
		viewData{"CanToggleAdminer": true, "AdminerOn": true})
	if !strings.Contains(on, `action="/apps/1/adminer"`) {
		t.Fatal("no Adminer toggle on a Laravel settings page")
	}
	if !strings.Contains(on, "Turn off") || strings.Contains(on, `name="enable"`) {
		t.Error("an app that has Adminer is offered anything but turning it off")
	}

	off := renderSettings(t, app, appSettingsForm{Name: "web", Domain: "web.demo.test"},
		viewData{"CanToggleAdminer": true, "AdminerOn": false})
	if !strings.Contains(off, `name="enable"`) || !strings.Contains(off, "Turn on") {
		t.Error("an app without Adminer is not offered it")
	}
	if !strings.Contains(off, "adminer.web.demo.test") {
		t.Error("the page does not say which hostname turning it on would publish")
	}

	// A compose app bundles nothing of xdev's, so there is nothing to switch.
	compose := store.App{ID: 2, Name: "byo", Slug: "byo", Type: store.TypeCompose,
		Domain: "byo.demo.test", ComposePath: "/p/demo/byo/_/compose.yml"}
	out := renderSettings(t, compose, appSettingsForm{Name: "byo", Domain: "byo.demo.test"}, nil)
	if strings.Contains(out, `/adminer"`) {
		t.Error("a compose app is offered an Adminer toggle")
	}
}

// renderProject draws the project page, whose add-app dialog is the biggest
// form in xdev and the one this test is really about.
func renderProject(t *testing.T, apps []store.App) string {
	t.Helper()
	return renderNamed(t, "project", viewData{
		"Title":   "Demo · xdev",
		"Project": store.Project{Name: "Demo", Slug: "demo", BaseDomain: "demo.test", Dir: "/p/demo"},
		"Apps":    apps, "AppsRunning": len(apps), "Catalog": templates.Catalog(),
		"MaxDomains": maxComposeDomains,
		"ExtraHosts": map[int64][]string{}, "AdminerDomains": map[int64]string{},
		"ServiceDomains": map[int64][]store.ServiceDomain{},
		"ProxyEnabled":   true, "HTTPSPort": 443,
	})
}

// The add-app dialog asks for one thing at a time: a type, then what that type
// needs, then the options every type shares.
func TestAddAppDialogIsStepped(t *testing.T) {
	out := renderProject(t, nil)
	for _, want := range []string{
		`class="steps-bar"`, `x-show="step===1"`, `x-show="step===2"`, `x-show="step===3"`,
		`class="wizard-foot"`, `@click="next()"`, `@click="back()"`, `@click="pick(`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("add-app dialog missing %q", want)
		}
	}

	// Every required field on the setup step is required only while that step is
	// on screen. A hidden required field is one the browser cannot focus, and it
	// refuses to submit the whole form rather than say so — which would make the
	// Create button do nothing at all.
	if n := strings.Count(out, `:required="step===2"`); n < 5 {
		t.Errorf("only %d fields bind required to the step; the rest would block submit when hidden", n)
	}
	for _, bad := range []string{
		`name="name" x-model="name" required`,
		`name="upstream" required`,
		`name="git_url" x-model="gitUrl" required`,
		`name="source_dir" required`,
	} {
		if strings.Contains(out, bad) {
			t.Errorf("a step-2 field is unconditionally required: %s", bad)
		}
	}
}

// Adminer is a database client on a public hostname, so whether a new Laravel
// app gets one is asked rather than assumed.
func TestAddAppDialogOffersAdminerChoice(t *testing.T) {
	out := renderProject(t, nil)
	if !strings.Contains(out, `<select name="adminer"`) {
		t.Fatal("the add-app dialog does not ask whether to include Adminer")
	}
	if !strings.Contains(out, `<option value="0">`) {
		t.Error("Adminer cannot be declined")
	}
	// The hostname field is only meaningful when there is an Adminer to name.
	if !strings.Contains(out, `x-show="adminer==='1'"`) {
		t.Error("the Adminer hostname field is shown even when Adminer is declined")
	}
}

// An app's facts are labelled rather than run together in one grey line, so a
// hostname, a port and a repository URL are told apart by reading, not guessing.
func TestAppCardMetaIsLabelled(t *testing.T) {
	app := store.App{ID: 1, Name: "web", Slug: "web", Type: "laravel", Status: store.AppRunning,
		Domain: "web.demo.test", Port: 20001, GitURL: "https://github.com/o/r",
		DeployedSHA: "abcdef1234567", CPULimit: 1.5, MemLimit: 512 * 1024 * 1024,
		DBMode: store.DBShared, ComposePath: "/p/demo/web/_/compose.yml"}
	out := renderProject(t, []store.App{app})
	for _, want := range []string{
		`class="meta-k">Address<`, `class="meta-k">Host port<`, `class="meta-k">Repository<`,
		`class="meta-k">Database<`, `class="meta-k">Limits<`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("app card meta missing %q", want)
		}
	}
	if !strings.Contains(out, "abcdef1") || strings.Contains(out, "abcdef1234567") {
		t.Error("the deployed commit is not abbreviated the way git abbreviates it")
	}
}
