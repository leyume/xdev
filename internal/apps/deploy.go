package apps

// Deploying an app: bringing what is served up to date with what the repository
// (or CI) says it should be. Three things start one — the Deploy button, a
// GitHub webhook, a push from CI — and they all end up here.
//
// A deploy is slow (a fetch, then `npm ci` on a cold cache) and the two remote
// triggers cannot wait: GitHub gives a webhook ten seconds before it calls the
// delivery failed. So a deploy runs in the background and its progress lives in
// the `deployments` table, which is what the UI reads.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"xdev/internal/gitsrc"
	"xdev/internal/store"
)

// maxLogBytes caps what a failed build leaves in the database. A webpack error
// is a few kilobytes; a dependency resolver in a bad mood is megabytes. The tail
// is kept, because the error is at the end.
const maxLogBytes = 16 << 10

// ErrDeployInProgress is returned when a deploy is asked for while one is
// already running for that app. Deploys of one app must not overlap: two builds
// writing the same output directory produce a mixture of both.
var ErrDeployInProgress = errors.New("a deploy is already running for this app")

// inflight tracks the apps currently deploying, so a second trigger is refused
// rather than queued. Queueing would mean a burst of five pushes builds five
// times to reach the state the last one describes.
type inflight struct {
	mu  sync.Mutex
	ids map[int64]bool
}

func (f *inflight) claim(id int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = map[int64]bool{}
	}
	if f.ids[id] {
		return false
	}
	f.ids[id] = true
	return true
}

func (f *inflight) release(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ids, id)
}

// Deploying reports whether a deploy is running for this app right now.
func (s *Service) Deploying(id int64) bool {
	s.deploying.mu.Lock()
	defer s.deploying.mu.Unlock()
	return s.deploying.ids[id]
}

// DeployAsync starts a deploy in the background and returns the id of the
// `deployments` row tracking it. The caller answers immediately; the outcome
// shows up on the app's settings page.
//
// trigger is one of store.DeployManual / DeployWebhook / DeployPush.
func (s *Service) DeployAsync(id int64, trigger string) (int64, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return 0, err
	}
	if !app.IsGit() {
		return 0, errors.New("this app has no repository — Deploy applies to git-backed apps")
	}
	if !s.deploying.claim(id) {
		return 0, ErrDeployInProgress
	}
	depID, err := s.store.StartDeployment(id, trigger)
	if err != nil {
		s.deploying.release(id)
		return 0, err
	}
	go func() {
		defer s.deploying.release(id)
		sha, buildLog, err := s.deployNow(app)
		status, msg := store.DeployOK, "deployed "+shortSHA(sha)
		if err != nil {
			status, msg = store.DeployFailed, firstLine(err.Error())
		}
		if ferr := s.store.FinishDeployment(depID, status, sha, msg, tailLog(buildLog, err)); ferr != nil {
			log.Printf("finish deployment %d: %v", depID, ferr)
		}
		if perr := s.store.PruneDeployments(id); perr != nil {
			log.Printf("prune deployments for app %d: %v", id, perr)
		}
		if err != nil {
			log.Printf("deploy app %d (%s): %v", id, trigger, err)
		}
	}()
	return depID, nil
}

// Deploy runs a deploy synchronously and returns the commit deployed and the
// build output. Used by tests and by anything that genuinely wants to wait;
// everything the UI and the endpoints do goes through DeployAsync.
func (s *Service) Deploy(id int64) (string, string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", "", err
	}
	if !app.IsGit() {
		return "", "", errors.New("this app has no repository — Deploy applies to git-backed apps")
	}
	if !s.deploying.claim(id) {
		return "", "", ErrDeployInProgress
	}
	defer s.deploying.release(id)
	return s.deployNow(app)
}

// deployNow is the deploy itself: fetch, reset, rebuild, and — for a
// command-mode app that was running — restart it onto the new build.
//
// A stopped command-mode app is rebuilt but not started. Deploying is "make the
// files current"; starting something the user stopped is a different decision
// and belongs to the Start button.
func (s *Service) deployNow(app store.App) (sha string, buildLog string, err error) {
	dir := s.codeDir(app)
	if !gitsrc.IsRepo(dir) {
		return "", "", fmt.Errorf("%s is not a git checkout — the app's files were replaced or the clone never finished", dir)
	}
	src, err := s.gitSource(app)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()
	sha, err = gitsrc.Update(ctx, dir, src)
	if err != nil {
		repo, _ := gitsrc.ParseRepo(app.GitURL)
		return "", "", cloneAdvice(err, repo, src.Key != "")
	}

	// A container app's build happens inside its own container — the host has
	// no PHP, and the versions that matter are the image's. Everything up to
	// here (fetch, hard reset) was identical; from here the two diverge
	// completely.
	if !app.IsHostProc() {
		out, err := s.deployContainer(app, nil)
		if err != nil {
			s.store.SetAppStatus(app.ID, store.AppError)
			return sha, out, err
		}
		if err := s.store.SetDeployed(app.ID, sha); err != nil {
			return sha, out, err
		}
		return sha, out, nil
	}

	wasRunning := app.Status == store.AppRunning
	if cmd := strings.TrimSpace(app.BuildCmd); cmd != "" {
		out, err := s.sup.RunBuild(s.buildDir(app), cmd, s.staticEnv(app, dir))
		buildLog = out
		if err != nil {
			s.store.SetAppStatus(app.ID, store.AppError)
			return sha, buildLog, err
		}
	}
	if app.ServeMode == store.ServeCommand && wasRunning {
		if err := s.startStatic(app); err != nil {
			return sha, buildLog, err
		}
	} else if app.ServeMode == store.ServeStatic {
		s.store.SetAppStatus(app.ID, store.AppRunning) // its files are what "running" means
	}
	if err := s.store.SetDeployed(app.ID, sha); err != nil {
		return sha, buildLog, err
	}
	return sha, buildLog, nil
}

// PushDeploy replaces an app's served files with the contents of a .tar.gz that
// CI built and uploaded. It is the other half of the deploy story: the build
// happens where the private registry credentials and the fast cache are, and
// xdev only receives the result.
//
// Unlike a git deploy this is synchronous — the archive is the request body,
// so there is nothing to return early to. It is also the reason the swap is
// done through a staging directory: an upload that dies halfway must not leave
// the site as half of two builds.
func (s *Service) PushDeploy(id int64, archive io.Reader, trigger string) (target string, err error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	target, err = s.pushTarget(app)
	if err != nil {
		return "", err
	}
	if !s.deploying.claim(id) {
		return "", ErrDeployInProgress
	}
	defer s.deploying.release(id)

	depID, err := s.store.StartDeployment(id, trigger)
	if err != nil {
		return "", err
	}
	defer func() {
		status, msg := store.DeployOK, "uploaded build published to "+target
		if err != nil {
			status, msg = store.DeployFailed, firstLine(err.Error())
		}
		s.store.FinishDeployment(depID, status, "", msg, "")
		s.store.PruneDeployments(id)
	}()

	// Stage beside the target, on the same filesystem, so the swap is a rename
	// and the site is never serving a half-unpacked directory.
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".xdev-push-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := untarGz(archive, staging); err != nil {
		return "", fmt.Errorf("unpack upload: %w", err)
	}
	if empty, err := isEmptyDir(staging); err != nil {
		return "", err
	} else if empty {
		return "", errors.New("the uploaded archive is empty — nothing to publish")
	}

	// Rename the live directory out of the way first: two renames, so the window
	// where the path does not exist is as short as the filesystem can make it.
	retired := ""
	if _, err := os.Stat(target); err == nil {
		retired = target + ".retiring"
		os.RemoveAll(retired)
		if err := os.Rename(target, retired); err != nil {
			return "", err
		}
	}
	if err := os.Rename(staging, target); err != nil {
		if retired != "" {
			os.Rename(retired, target) // put the old build back rather than serving nothing
		}
		return "", err
	}
	if retired != "" {
		os.RemoveAll(retired)
	}

	// A command-mode app *is* the uploaded files, so it has to be restarted onto
	// them; a serve-mode app is already serving the new directory.
	if app.ServeMode == store.ServeCommand && app.Status == store.AppRunning {
		if err := s.startStatic(app); err != nil {
			return target, err
		}
	} else if app.ServeMode == store.ServeStatic {
		s.store.SetAppStatus(app.ID, store.AppRunning)
	}
	return target, nil
}

// PushTarget reports which directory an uploaded build would replace, or why
// this app cannot take one. The settings page calls it before issuing a deploy
// token, so a token that could never be used is refused at the moment somebody
// asks for it rather than the first time CI runs.
func (s *Service) PushTarget(id int64) (string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return "", err
	}
	return s.pushTarget(app)
}

// pushTarget is the directory a pushed build replaces, and the place the rules
// about what xdev may overwrite are enforced.
func (s *Service) pushTarget(app store.App) (string, error) {
	if !app.IsHostProc() {
		return "", errors.New("pushed builds apply to static and Go apps — a container app is deployed by its compose file")
	}
	if app.IsExternalDir() {
		// The whole point of pointing an app at your own folder is that xdev does
		// not rewrite it. A push replaces the directory wholesale.
		return "", errors.New("this app runs from a folder of your own, which xdev will not replace — move it to a folder xdev manages to deploy by upload")
	}
	dir := s.appDir(app)
	if app.RootDir == "" {
		if app.IsGit() {
			// Replacing the app folder would delete the checkout — .git and all.
			return "", errors.New("set \"Folder to serve\" first (e.g. dist), so an uploaded build replaces the build output rather than the checkout")
		}
		return dir, nil
	}
	return filepath.Join(dir, app.RootDir), nil
}

func isEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// tailLog is what a deploy leaves behind: for a failure the whole error and
// then the tail of the build output; for a success the output alone, which the
// settings page renders as a list of steps.
//
// The error goes in whole because the message column only gets its first line
// (firstLine), and git puts the reason on the second — without this, a deploy
// that never reached the build step records "exit status 128" and no cause
// anywhere. The tail is kept rather than the head because that is where a
// build's error is.
func tailLog(out string, err error) string {
	if len(out) > maxLogBytes {
		out = "…(trimmed)…\n" + out[len(out)-maxLogBytes:]
	}
	if err == nil {
		return out // kept, so the settings page can show what the deploy did
	}
	if out == "" {
		return err.Error()
	}
	return err.Error() + "\n\n" + out
}

// shortSHA abbreviates a commit the way git does, for a one-line message.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// firstLine reduces an error to the sentence that fits in a table cell; the
// build log beside it carries the rest.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return strings.TrimSpace(s)
}
