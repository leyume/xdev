package apps

// Host apps whose code comes from a git repository. The repository is the
// source of truth: xdev clones it into the app's managed directory, builds it
// there, and serves the build output. A deploy is `fetch` + `reset --hard` +
// build, which is why a git app may never point at a directory of the user's
// own — see validSourceDir's counterpart check in layoutStatic.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"xdev/internal/gitsrc"
	"xdev/internal/store"
)

// cloneTimeout bounds a clone or a fetch. Generous: a first clone of a big repo
// over a slow uplink is the longest thing here, and failing at two minutes
// would be worse than waiting.
const cloneTimeout = 10 * time.Minute

// GitOpts is the repository half of the add-app form, shared by Create and
// Update so both validate it the same way.
type GitOpts struct {
	URL    string // anything ParseRepo accepts; "" = not a git app
	Ref    string // branch or tag; "" = the remote's default
	Subdir string // build from this subdirectory of the repo
	// KeyID binds an already-generated deploy key to the app (private repos).
	// The key is generated before the app exists so its public half can be added
	// to GitHub first — see store.DeployKey.
	KeyID int64
}

// resolveGit validates the repository fields and returns them in the canonical
// form the app row stores. A blank URL means the app is not git-backed and the
// other fields are ignored rather than rejected: switching a git app back to a
// plain folder should not first require clearing the branch.
func resolveGit(opts GitOpts) (url, ref, subdir string, err error) {
	if strings.TrimSpace(opts.URL) == "" {
		return "", "", "", nil
	}
	repo, err := gitsrc.ParseRepo(opts.URL)
	if err != nil {
		return "", "", "", err
	}
	ref = strings.TrimSpace(opts.Ref)
	if strings.ContainsAny(ref, " \t\\^~:?*[") {
		return "", "", "", fmt.Errorf("invalid branch %q", opts.Ref)
	}
	// Normalized here rather than only at build time, so the stored value is the
	// one the settings page shows back and SubdirOf never sees a surprise.
	subdir = strings.Trim(strings.TrimSpace(opts.Subdir), "/")
	if subdir != "" {
		clean := path.Clean(subdir)
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return "", "", "", fmt.Errorf("invalid subdirectory %q — it must be inside the repository", opts.Subdir)
		}
		subdir = clean
	}
	return repo.String(), ref, subdir, nil
}

// gitSource assembles what a clone or fetch needs, decrypting the app's deploy
// key when it has one. An app with no key reads the repository anonymously over
// HTTPS, which is all a public repository needs.
func (s *Service) gitSource(app store.App) (gitsrc.Source, error) {
	repo, err := gitsrc.ParseRepo(app.GitURL)
	if err != nil {
		return gitsrc.Source{}, err
	}
	src := gitsrc.Source{Repo: repo, Ref: app.GitRef}
	key, err := s.store.DeployKeyForApp(app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return src, nil // public repo
	}
	if err != nil {
		return gitsrc.Source{}, err
	}
	if s.keys == nil {
		return gitsrc.Source{}, errors.New("this app has a deploy key but secret storage is not configured")
	}
	plain, err := s.keys.Unseal(key.PrivateKey)
	if err != nil {
		return gitsrc.Source{}, err
	}
	src.Key = string(plain)
	return src, nil
}

// buildDir is where an app's build command runs and, for command mode, where
// its process starts: the app directory, or the subdirectory of the repository
// the user named. Everything else about an app still points at the app
// directory — the .env it reads, the logs, the backups.
func (s *Service) buildDir(app store.App) string {
	dir := s.appDir(app)
	if app.GitSubdir == "" {
		return dir
	}
	sub, err := gitsrc.SubdirOf(dir, app.GitSubdir)
	if err != nil {
		return dir // validated on the way in; fall back rather than build nowhere
	}
	return sub
}

// cloneRepo populates a new app's directory from its repository and fills in
// the build settings the repository implies. Called from layoutStatic, before
// the app row exists, so everything it decides lands in the row that is about
// to be written.
//
// It runs at create — meaning a private repository whose deploy key has not
// been added to GitHub yet fails here, with git's own "Repository not found"
// message wrapped in advice. That is the right moment to fail: the alternative
// is an app that exists but has never had a single file.
func (s *Service) cloneRepo(app *store.App, opts *CreateOpts, appDir string) error {
	if !gitsrc.Available() {
		return errors.New("git is not installed on this host — install it, or add the app from a folder instead")
	}
	url, ref, subdir, err := resolveGit(opts.Git)
	if err != nil {
		return err
	}
	app.GitURL, app.GitRef, app.GitSubdir = url, ref, subdir

	repo, err := gitsrc.ParseRepo(url)
	if err != nil {
		return err
	}
	src := gitsrc.Source{Repo: repo, Ref: ref}
	if opts.Git.KeyID > 0 {
		key, err := s.store.DeployKeyByID(opts.Git.KeyID)
		if err != nil {
			return errors.New("that deploy key no longer exists — generate a new one")
		}
		if s.keys == nil {
			return errors.New("secret storage is not configured, so a private repository cannot be used")
		}
		plain, err := s.keys.Unseal(key.PrivateKey)
		if err != nil {
			return err
		}
		src.Key = string(plain)
	}

	// Clone into the app directory. git insists on an empty (or missing) target,
	// and layoutStatic has just created it, so hand it a path git can make.
	if err := os.RemoveAll(appDir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()
	if err := s.cloneFn()(ctx, appDir, src); err != nil {
		return cloneAdvice(err, repo, src.Key != "")
	}
	return nil
}

// cloneFn is how this service reaches git. It is indirected through a field so
// a test can create an app from a repository on disk, which keeps ParseRepo
// free to reject the local paths a user should never be able to type into the
// Repository field.
func (s *Service) cloneFn() func(context.Context, string, gitsrc.Source) error {
	if s.clone != nil {
		return s.clone
	}
	return gitsrc.Clone
}

// cloneAdvice turns git's failure into the sentence that says what to do about
// it. The three that actually happen are a private repo whose key is not on the
// repository yet (GitHub answers "not found" rather than "forbidden", to avoid
// confirming the repo exists), a private repo reached over HTTPS because the app
// has no key at all, and a branch that is spelled wrong.
func cloneAdvice(err error, repo gitsrc.Repo, hadKey bool) error {
	msg := err.Error()
	switch {
	// No key means an HTTPS remote, and git asks for a username it can never be
	// given (GIT_TERMINAL_PROMPT=0). This is what a public app pointed at a
	// private repository looks like, and it says nothing useful on its own.
	case strings.Contains(msg, "could not read Username"),
		strings.Contains(msg, "Authentication failed"),
		strings.Contains(msg, "terminal prompts disabled"):
		return fmt.Errorf("%s asked for credentials xdev does not have — it is private, and this app has no deploy key. Add one under Deploy key on this page, put the public half at %s, then deploy again.\n\n%w",
			repo.String(), repo.KeysURL(), err)
	case strings.Contains(msg, "Repository not found"), strings.Contains(msg, "not found"),
		strings.Contains(msg, "Permission denied"), strings.Contains(msg, "access rights"):
		if hadKey {
			return fmt.Errorf("%s could not be read with this deploy key — add the public key at %s (Allow write access is not needed), then try again.\n\n%w",
				repo.String(), repo.KeysURL(), err)
		}
		return fmt.Errorf("%s could not be read anonymously — if it is private, choose \"Private repository\" so xdev can generate a deploy key.\n\n%w",
			repo.String(), err)
	case strings.Contains(msg, "Remote branch"):
		return fmt.Errorf("that branch does not exist in %s — leave the branch blank to track the repository's default.\n\n%w", repo.String(), err)
	}
	return err
}

// nodeBuild is what a repository's package.json implies about building it: the
// command to run and where the result lands. Both are only defaults — the form
// shows them and the user can overrule either.
type nodeBuild struct {
	Command string // "" = nothing to build (a plain HTML repo)
	OutDir  string // "" = the folder itself; "dist" for most bundlers
}

// detectNodeBuild reads package.json and works out how to build the app.
//
// The package manager comes from the lockfile, because using the wrong one on a
// repository that has a lockfile is how you get a build that works locally and
// not here. The output directory is a guess from the framework in
// devDependencies: it cannot be read from config without running the build, and
// a wrong guess is visible and one field away from being fixed.
func detectNodeBuild(dir string) nodeBuild {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nodeBuild{} // no package.json: nothing to build, serve the files
	}
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nodeBuild{}
	}
	if _, ok := pkg.Scripts["build"]; !ok {
		return nodeBuild{}
	}

	// Install with whatever the lockfile says, then run the build script.
	install, run := "npm ci", "npm run build"
	switch {
	case exists(filepath.Join(dir, "pnpm-lock.yaml")):
		install, run = "pnpm install --frozen-lockfile", "pnpm run build"
	case exists(filepath.Join(dir, "yarn.lock")):
		install, run = "yarn install --frozen-lockfile", "yarn build"
	case exists(filepath.Join(dir, "bun.lockb")), exists(filepath.Join(dir, "bun.lock")):
		install, run = "bun install --frozen-lockfile", "bun run build"
	case !exists(filepath.Join(dir, "package-lock.json")):
		install = "npm install" // `npm ci` requires a lockfile and fails without one
	}

	has := func(name string) bool {
		_, a := pkg.Dependencies[name]
		_, b := pkg.DevDependencies[name]
		return a || b
	}
	out := "dist" // vite, vue-cli, svelte, astro, parcel, angular (dist/<name>)
	switch {
	case has("react-scripts"), has("@craco/craco"):
		out = "build"
	case has("next"):
		out = "out" // only correct for `next export`; a server build is not a static app
	case has("nuxt"), has("nuxt3"):
		out = ".output/public"
	case has("@docusaurus/core"), has("gatsby"):
		out = "build"
	}
	return nodeBuild{Command: install + " && " + run, OutDir: out}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
