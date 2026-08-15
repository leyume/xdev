package server

// The two endpoints GitHub talks to. They are the only handlers in xdev that
// are reachable from the internet without a session, so everything about them
// is deliberately narrow:
//
//   - They are mounted under /_xdev/ on each app's **own** hostname, and Caddy
//     routes only those exact paths to xdev (see store.HookPathPrefix and
//     platform.Reconciler.endpointRoutes). Nothing else of the control plane is
//     published by turning one on.
//   - They exist only for apps where somebody switched them on. An app with no
//     webhook and no push token has no route and answers nothing.
//   - Neither trusts anything in the URL beyond finding which app is meant. The
//     webhook is verified by HMAC over the raw body; the push endpoint by a
//     bearer token compared against a stored hash. Both comparisons are
//     constant-time.
//   - They are exempt from CSRF because they carry no cookie and no ambient
//     authority: a browser cannot make one of these requests succeed.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"xdev/internal/apps"
	"xdev/internal/gitsrc"
	"xdev/internal/store"
)

// maxHookBody caps a webhook delivery. GitHub's push payloads are tens of
// kilobytes; the cap is here so an unauthenticated request cannot make xdev
// read an unbounded body before it has verified anything.
const maxHookBody = 1 << 20 // 1 MiB

// maxPushBody caps an uploaded build. A static site's dist/ is a few megabytes;
// 256 MiB is far past anything reasonable and still refuses a disk-filler.
const maxPushBody = 256 << 20

// handleGitHubHook receives a push event and starts a deploy. It answers before
// the deploy finishes: GitHub gives a webhook ten seconds before it records the
// delivery as failed, and a real build takes longer than that.
func (s *Server) handleGitHubHook(w http.ResponseWriter, r *http.Request) {
	app, err := s.store.AppByHookID(r.PathValue("hook"))
	if err != nil {
		// Same answer as a wrong path — a 401 here would confirm the id is real.
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	secret, err := s.hookSecret(app)
	if err != nil {
		log.Printf("webhook for app %d: %v", app.ID, err)
		http.Error(w, "webhook is not configured", http.StatusInternalServerError)
		return
	}
	if !validSignature(r.Header.Get("X-Hub-Signature-256"), secret, body) {
		s.store.AddEvent(app.ProjectID, app.ID, "warn",
			"Rejected a webhook delivery for "+app.Name+" — the signature did not match the secret")
		http.Error(w, "signature mismatch", http.StatusUnauthorized)
		return
	}

	switch event := r.Header.Get("X-GitHub-Event"); event {
	case "ping":
		// GitHub sends this when the webhook is created. Answering it is how the
		// UI's "green tick" appears next to the hook.
		writeJSON(w, map[string]string{"ok": "xdev is listening for pushes to " + app.Name})
		return
	case "push":
	default:
		writeJSON(w, map[string]string{"ignored": "xdev only acts on push events, not " + event})
		return
	}

	// Only the branch this app tracks. A repository whose webhook fires for every
	// branch would otherwise deploy a feature branch over production.
	var payload struct {
		Ref     string `json:"ref"`
		Deleted bool   `json:"deleted"`
		After   string `json:"after"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "could not read the payload", http.StatusBadRequest)
		return
	}
	pushed := strings.TrimPrefix(payload.Ref, "refs/heads/")
	if want := app.GitRef; want != "" && pushed != want {
		writeJSON(w, map[string]string{"ignored": "this app tracks " + want + ", not " + pushed})
		return
	}
	if payload.Deleted {
		writeJSON(w, map[string]string{"ignored": "branch deleted"})
		return
	}

	if _, err := s.apps.DeployAsync(app.ID, store.DeployWebhook); err != nil {
		if errors.Is(err, apps.ErrDeployInProgress) {
			// Not a failure worth showing red in GitHub's delivery list: the push
			// that is already building will be at least as new as this one.
			writeJSON(w, map[string]string{"ignored": "a deploy is already running"})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.store.AddEvent(app.ProjectID, app.ID, "info", "Webhook push on "+pushed+" — deploying "+app.Name)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]string{"deploying": app.Name})
}

// handlePushDeploy publishes a build that CI made and uploaded: the request body
// is a .tar.gz of the finished site, and it replaces what the app serves.
//
// This one *is* synchronous — the archive is the request body, so there is
// nothing to answer early to — but unpacking a few megabytes is fast, and a CI
// job would rather see the result than a 202 it has to poll.
//
// Which app is meant comes from the Host header, not from anything in the path:
// the endpoint is published on the app's own hostname, so the hostname *is* the
// identifier. A token for one app therefore cannot deploy another even if the
// request is aimed at the wrong site.
func (s *Server) handlePushDeploy(w http.ResponseWriter, r *http.Request) {
	id := s.store.DomainOwner(hostOnly(r.Host))
	if id == 0 {
		http.NotFound(w, r)
		return
	}
	app, err := s.store.AppByID(id)
	if err != nil || app.PushTokenHash == "" {
		http.NotFound(w, r)
		return
	}
	token, ok := bearerToken(r)
	if !ok || !validPushToken(token, app.PushTokenHash) {
		http.Error(w, "invalid deploy token", http.StatusUnauthorized)
		return
	}

	target, err := s.apps.PushDeploy(app.ID, http.MaxBytesReader(w, r.Body, maxPushBody), store.DeployPush)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, apps.ErrDeployInProgress) {
			status = http.StatusConflict
		}
		s.store.AddEvent(app.ProjectID, app.ID, "error", "Uploaded deploy of "+app.Name+" failed: "+err.Error())
		writeJSONError(w, err, status)
		return
	}
	s.store.AddEvent(app.ProjectID, app.ID, "info", "Published an uploaded build of "+app.Name)
	s.reconcile()
	writeJSON(w, map[string]string{"published": target})
}

// hookSecret decrypts the HMAC secret an app shares with GitHub.
func (s *Server) hookSecret(app store.App) (string, error) {
	if app.HookSecret == "" {
		return "", errors.New("no webhook secret stored")
	}
	box := s.apps.Secrets()
	if box == nil {
		return "", errors.New("secret storage is unavailable")
	}
	plain, err := box.Unseal(app.HookSecret)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// validSignature checks GitHub's X-Hub-Signature-256 header: HMAC-SHA256 of the
// exact bytes received, keyed with the shared secret. The comparison is
// constant-time, and the body must be the raw one — re-serializing the JSON
// would change the bytes and every delivery would fail.
func validSignature(header, secret string, body []byte) bool {
	sent, ok := strings.CutPrefix(strings.TrimSpace(header), "sha256=")
	if !ok || sent == "" || secret == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(sent)), []byte(want)) == 1
}

// hostOnly strips any :port from a Host header and lowercases it, so it can be
// looked up against the hostnames in the domains table.
func hostOnly(host string) string {
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// bearerToken reads an Authorization: Bearer header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(tok), true
	}
	// A token in a header of its own, for CI systems that reserve Authorization.
	if tok := strings.TrimSpace(r.Header.Get("X-Deploy-Token")); tok != "" {
		return tok, true
	}
	return "", false
}

// hashPushToken is how a deploy token is stored: only its hash, so a copy of the
// database does not carry the ability to deploy. SHA-256 without a salt is right
// here — the token is 256 bits of randomness xdev generated, not a password
// somebody chose, so there is no dictionary to attack.
func hashPushToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validPushToken(token, hash string) bool {
	if token == "" || hash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashPushToken(token)), []byte(hash)) == 1
}

// --- switching the endpoints on and off ------------------------------------

// handleAppWebhookSet turns an app's webhook on (generating a fresh URL and
// signing secret) or off. Re-running it while one exists rotates both, which is
// the answer to a secret that leaked — the old URL stops existing.
func (s *Server) handleAppWebhookSet(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/settings"
	if r.FormValue("disable") != "" {
		if err := s.store.SetAppHook(app.ID, "", ""); err != nil {
			redirectWithError(w, r, target, err)
			return
		}
		s.store.AddEvent(proj.ID, app.ID, "warn", "Disabled the deploy webhook for "+app.Name)
		s.reconcile() // the route for that path goes away with it
		http.Redirect(w, r, target+"?hook=off", http.StatusSeeOther)
		return
	}
	if !app.IsGit() {
		redirectWithError(w, r, target,
			errors.New("a webhook deploys by pulling, so it needs an app created from a repository"))
		return
	}
	box := s.apps.Secrets()
	if box == nil {
		redirectWithError(w, r, target, errors.New("secret storage is unavailable"))
		return
	}
	hookID, err := randomToken(24)
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	secret, err := randomToken(32)
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	sealed, err := box.Seal([]byte(secret))
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	if err := s.store.SetAppHook(app.ID, hookID, sealed); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Enabled the deploy webhook for "+app.Name)
	s.reconcile() // publish /_xdev/hook/<id> on this app's hostname
	http.Redirect(w, r, target+"?hook=on", http.StatusSeeOther)
}

// handleAppPushTokenSet issues (or revokes) the bearer token CI uses to upload a
// built site. The token is shown once, on the redirect that follows: only its
// hash is stored, so xdev cannot show it again and a stolen database cannot
// deploy.
func (s *Server) handleAppPushTokenSet(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/settings"
	if r.FormValue("disable") != "" {
		if err := s.store.SetAppPushToken(app.ID, "", ""); err != nil {
			redirectWithError(w, r, target, err)
			return
		}
		s.store.AddEvent(proj.ID, app.ID, "warn", "Revoked the deploy token for "+app.Name)
		s.reconcile()
		http.Redirect(w, r, target+"?token=off", http.StatusSeeOther)
		return
	}
	// Refuse before issuing a token that could not be used: pushing needs a
	// directory xdev is allowed to replace.
	if _, err := s.apps.PushTarget(app.ID); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	raw, err := randomToken(32)
	if err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	token := "xdp_" + raw
	if err := s.store.SetAppPushToken(app.ID, hashPushToken(token), token[:12]); err != nil {
		redirectWithError(w, r, target, err)
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Issued a deploy token for "+app.Name)
	s.reconcile() // publish /_xdev/deploy on this app's hostname
	// The token travels in the redirect so the page can show it once. It is not
	// stored anywhere; reload the page and it is gone for good.
	http.Redirect(w, r, target+"?token="+url.QueryEscape(token), http.StatusSeeOther)
}

// handleAppDeploysPartial renders the deployment list on its own, for the
// settings page to poll while one is running.
func (s *Server) handleAppDeploysPartial(w http.ResponseWriter, r *http.Request) {
	app, _, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	deploys, _ := s.store.AppDeployments(app.ID, 8)
	s.renderFragment(w, "app_settings", "deploy_list", viewData{
		"Deploys":   deploys,
		"Deploying": s.apps.Deploying(app.ID),
	})
}

// randomToken returns n bytes of randomness as an unpadded URL-safe string.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- what the settings page shows about deploying --------------------------

// deployInfo is the Deploys card: the two endpoints, whether each is on, and
// what to paste where to use them.
type deployInfo struct {
	HookURL      string // "" = webhook off
	HookSecret   string // the shared secret, shown so it can be pasted into GitHub
	RepoHooksURL string // where to add it on GitHub
	Ref          string // the branch a push has to touch for a deploy to happen
	PushURL      string // "" = push disabled
	PushHint     string // the first characters of the live token
	PushTarget   string // the directory an upload would replace
	PushBlocked  string // why this app cannot take an upload ("" = it can)
	NewToken     string // shown once, straight after being issued
	Repo         string // owner/name, for the sample workflow
}

// newPushToken pulls a just-issued deploy token out of the redirect that
// carried it. "off" is the revocation marker, not a token.
func newPushToken(r *http.Request) string {
	if v := r.URL.Query().Get("token"); v != "off" {
		return v
	}
	return ""
}

// appDeploys builds the Deploys card for an app, or nil for the types that have
// nothing to deploy (a container stack is deployed by its compose file).
func (s *Server) appDeploys(app store.App, newToken string) *deployInfo {
	// Host apps get this card whether or not they have a repository (the upload
	// half works without one). A container app only gets it once it has a
	// repository, because the webhook half is the only part that applies and it
	// deploys by pulling. Everything else — WordPress, a compose stack — has no
	// deploy path either half could drive.
	if !app.IsHostProc() && !app.IsGit() {
		return nil
	}
	info := &deployInfo{Ref: app.GitRef, NewToken: newToken, PushHint: app.PushTokenHint}
	if info.Ref == "" {
		info.Ref = "the default branch"
	}
	base := "https://" + app.Domain
	if s.httpsPort != 443 {
		base += ":" + strconv.Itoa(s.httpsPort)
	}
	if app.HookID != "" {
		info.HookURL = base + app.HookPath()
		if secret, err := s.hookSecret(app); err == nil {
			info.HookSecret = secret
		}
	}
	if app.PushTokenHash != "" {
		info.PushURL = base + store.PushPath
	}
	if repo, err := gitsrc.ParseRepo(app.GitURL); err == nil {
		info.RepoHooksURL = repo.WebURL() + "/settings/hooks"
		info.Repo = repo.Owner + "/" + repo.Name
	}
	if target, err := s.apps.PushTarget(app.ID); err != nil {
		info.PushBlocked = err.Error()
	} else {
		info.PushTarget = target
	}
	return info
}
