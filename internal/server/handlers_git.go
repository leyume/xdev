package server

// Git-backed apps: generating the deploy key a private repository needs, and
// deploying an app to the tip of its branch.
//
// The key is generated *before* the app exists. A private repository cannot be
// cloned until its deploy key is on GitHub, and the clone happens during create
// — so the add-app dialog asks for a key, shows the public half to paste into
// the repository's settings, and submits the key's id along with the form.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"xdev/internal/apps"
	"xdev/internal/gitsrc"
	"xdev/internal/naming"
	"xdev/internal/store"
)

// handleDeployKeyNew generates an unbound keypair and returns its public half.
// The private half is encrypted before it is stored and is never sent anywhere
// — not to this response, not to a log, not to the page.
func (s *Server) handleDeployKeyNew(w http.ResponseWriter, r *http.Request) {
	box := s.apps.Secrets()
	if box == nil {
		writeJSONError(w, errors.New("secret storage is unavailable, so private repositories cannot be used"), http.StatusServiceUnavailable)
		return
	}
	// The comment is what shows next to the key in GitHub's list, so make it say
	// which xdev app it belongs to.
	label := naming.Slugify(strings.TrimSpace(r.FormValue("label")))
	comment := "xdev"
	if label != "" {
		comment += "-" + label
	}
	key, err := gitsrc.NewDeployKey(comment)
	if err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	sealed, err := box.Seal([]byte(key.Private))
	if err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	saved, err := s.store.CreateDeployKey(store.DeployKey{
		PublicKey:   key.Public,
		PrivateKey:  sealed,
		Fingerprint: key.Fingerprint,
	})
	if err != nil {
		writeJSONError(w, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"id":          saved.ID,
		"public_key":  saved.PublicKey,
		"fingerprint": saved.Fingerprint,
	})
}

// handleAppDeploy starts a deploy and returns immediately. The build runs in the
// background — `npm ci` on a cold cache outlasts anyone's patience with a
// spinning tab — and the settings page follows it in the deployments table.
func (s *Server) handleAppDeploy(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/settings"
	if _, err := s.apps.DeployAsync(app.ID, store.DeployManual); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Deploy of "+app.Name+" started")
	http.Redirect(w, r, target+"?deploying=1", http.StatusSeeOther)
}

// handleAppDeployKeyRotate gives an app a deploy key, whether or not it had one.
//
// Replacing: the old private half is destroyed, so the key still listed on
// GitHub stops working immediately — the point of rotating when one may have
// leaked. Adding a first key: the app was created against a public repository
// and has since been pointed at a private one, which the add-app form's key
// step cannot help with because that ran before the change. Either way deploys
// fail until the public half is added to the repository.
func (s *Server) handleAppDeployKeyRotate(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/settings"
	_, keyErr := s.store.DeployKeyForApp(app.ID)
	hadKey := keyErr == nil
	box := s.apps.Secrets()
	if box == nil {
		redirectWithError(w, r, target, errors.New("secret storage is unavailable"))
		return
	}
	key, err := gitsrc.NewDeployKey("xdev-" + app.Slug)
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	sealed, err := box.Seal([]byte(key.Private))
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	if err := s.store.DeleteDeployKeysForApp(app.ID); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	if _, err := s.store.CreateDeployKey(store.DeployKey{
		AppID:       app.ID,
		PublicKey:   key.Public,
		PrivateKey:  sealed,
		Fingerprint: key.Fingerprint,
	}); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	verb := "Added a deploy key for "
	if hadKey {
		verb = "Replaced the deploy key for "
	}
	s.store.AddEvent(proj.ID, app.ID, "warn", verb+app.Name+" — add the public half to the repository")
	http.Redirect(w, r, target+"?rotated=1", http.StatusSeeOther)
}

// gitInfo is what the settings page shows about an app's repository: where it
// deploys from, what is deployed now, and the key to paste into GitHub.
type gitInfo struct {
	Repo        gitsrc.Repo
	URL         string // canonical https://host/owner/name
	KeysURL     string // where to add the deploy key for this repository
	Ref         string // branch tracked ("" = the repo's default)
	Subdir      string
	SHA         string // deployed commit, short
	FullSHA     string
	DeployedAt  string
	PublicKey   string // "" = public repository, no key
	Fingerprint string
}

// appGit describes a git-backed app, or nil for every other kind.
func (s *Server) appGit(app store.App) *gitInfo {
	if !app.IsGit() {
		return nil
	}
	repo, err := gitsrc.ParseRepo(app.GitURL)
	if err != nil {
		return nil
	}
	info := &gitInfo{
		Repo: repo, URL: repo.WebURL(), KeysURL: repo.KeysURL(),
		Ref: app.GitRef, Subdir: app.GitSubdir,
		SHA: shortSHA(app.DeployedSHA), FullSHA: app.DeployedSHA, DeployedAt: app.DeployedAt,
	}
	if key, err := s.store.DeployKeyForApp(app.ID); err == nil {
		info.PublicKey, info.Fingerprint = key.PublicKey, key.Fingerprint
	}
	return info
}

// gitOptsFrom reads the repository fields shared by the add-app form and the
// settings page.
func gitOptsFrom(r *http.Request) apps.GitOpts {
	id, _ := strconv.ParseInt(r.FormValue("deploy_key_id"), 10, 64)
	return apps.GitOpts{
		URL:    strings.TrimSpace(r.FormValue("git_url")),
		Ref:    strings.TrimSpace(r.FormValue("git_ref")),
		Subdir: strings.TrimSpace(r.FormValue("git_subdir")),
		KeyID:  id,
	}
}

// shortSHA abbreviates a commit for display, the way git does.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// handleAppAction runs one allowlisted maintenance command inside a container
// app and shows its output.
//
// The request names an action key, never a command: apps.RunContainerAction
// resolves the key against a fixed list, so there is no input here that can
// become something to execute. Output is rendered back on the settings page
// rather than redirected away, because the output is the entire point.
func (s *Server) handleAppAction(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/settings"
	action, out, err := s.apps.RunContainerAction(app.ID, r.FormValue("action"))
	if err != nil && action.Key == "" {
		// An unknown action key: nothing ran, so there is no output to show.
		if wantsJSON(r) {
			writeJSONError(w, err, http.StatusBadRequest)
			return
		}
		redirectWithError(w, r, target, err)
		return
	}

	level, msg := "info", "Ran \""+action.Label+"\" on "+app.Name
	if err != nil {
		level = "warn"
		msg += " — it failed"
		if out == "" {
			out = err.Error()
		}
	}
	s.store.AddEvent(proj.ID, app.ID, level, msg)

	// The page runs these with fetch and opens the output under the row that was
	// clicked, so nothing else on a long settings form is lost or scrolled away.
	// A failed command still answers 200: the request succeeded, the *command*
	// failed, and its output is the point — an error status would send the
	// client down a path that has no body to show.
	if wantsJSON(r) {
		writeJSON(w, map[string]any{
			"label": action.Label, "output": out, "failed": err != nil,
		})
		return
	}
	s.renderAppSettingsWith(w, r, app, proj, s.settingsFormFor(app), "", &actionResult{
		Label:  action.Label,
		Output: out,
		Failed: err != nil,
	})
}

// handleAppDumpToggle switches this app's pre-migration database dump on or
// off. Off is a deliberate choice for a database too large to snapshot on every
// deploy, and it is the only way to reach the state where a failed migration
// has nothing to go back to — so it is recorded as a warning.
func (s *Server) handleAppDumpToggle(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/settings"
	skip := r.FormValue("skip") != ""
	if err := s.store.SetSkipDBDump(app.ID, skip); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	if skip {
		s.store.AddEvent(proj.ID, app.ID, "warn",
			"Turned off pre-migration database dumps for "+app.Name+" — a failed migration will have no snapshot to restore")
	} else {
		s.store.AddEvent(proj.ID, app.ID, "info", "Turned on pre-migration database dumps for "+app.Name)
	}
	http.Redirect(w, r, target+"?saved=1", http.StatusSeeOther)
}

// handleAppAdminerToggle adds or removes the Adminer database client bundled
// with a Laravel app.
//
// Turning it off is a security change, not a cosmetic one — Adminer is a
// database client on a public hostname — so it is recorded, and the hostname is
// released rather than left pointing at nothing. The stack is restarted here
// because the compose file it was started from no longer describes it.
func (s *Server) handleAppAdminerToggle(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/settings"
	on := r.FormValue("enable") != ""
	host, err := s.apps.SetAdminer(app.ID, on)
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	if on {
		s.store.AddEvent(proj.ID, app.ID, "info", "Added Adminer to "+app.Name+" at "+host)
	} else {
		s.store.AddEvent(proj.ID, app.ID, "warn", "Removed Adminer from "+app.Name+" — its hostname no longer routes")
	}
	// Same restart the settings form does, and for the same reason: a compose
	// change reaches a running stack only when the stack is recreated from it.
	if app.Status == store.AppRunning {
		_ = s.apps.Stop(app.ID)
		if err := s.apps.Start(app.ID); err != nil {
			s.reconcile()
			redirectWithError(w, r, target, err)
			return
		}
	}
	s.reconcile()
	http.Redirect(w, r, target+"?saved=1", http.StatusSeeOther)
}
