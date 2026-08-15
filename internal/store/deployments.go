package store

import "strings"

// The deploy endpoints are served on each app's own hostname, under a prefix
// no site is likely to want. Putting them there rather than on a hostname of
// xdev's own means there is nothing extra to point at the server: the domain
// already resolves and already has a certificate. Caddy proxies exactly these
// two paths to the control plane and nothing else, so publishing them does not
// publish the admin UI behind them.
//
// They live here, beside the columns that enable them, because both the
// reconciler (which routes the paths) and the server (which serves and prints
// them) need the same answer.
const (
	// HookPathPrefix is completed by the app's unguessable hook id.
	HookPathPrefix = "/_xdev/hook/"
	// PushPath takes a bearer token and a .tar.gz of a built site.
	PushPath = "/_xdev/deploy"
)

// HookPath is the path GitHub posts to for this app ("" when no webhook).
func (a App) HookPath() string {
	if a.HookID == "" {
		return ""
	}
	return HookPathPrefix + a.HookID
}

// EndpointPaths are the request paths Caddy must route to the control plane for
// this app — only the ones actually switched on.
func (a App) EndpointPaths() []string {
	var paths []string
	if a.HookID != "" {
		paths = append(paths, a.HookPath())
	}
	if a.PushTokenHash != "" {
		paths = append(paths, PushPath)
	}
	return paths
}

// Deploy triggers and statuses.
const (
	DeployManual  = "manual"
	DeployWebhook = "webhook"
	DeployPush    = "push"
	DeployCreate  = "create" // the first build, run while the app was being added

	DeployRunning = "running"
	DeployOK      = "ok"
	DeployFailed  = "failed"
)

// keepDeployments is how many rows are kept per app. Enough to see a pattern
// ("every deploy since Tuesday has failed"), not so many that the table becomes
// a log store — the build output of each is in here too.
const keepDeployments = 20

// Deployment is one attempt to bring an app up to date, from whichever of the
// three directions started it. Deploys are asynchronous, so this row *is* the
// progress indicator: it exists as `running` from the moment work starts.
type Deployment struct {
	ID         int64
	AppID      int64
	Trigger    string // create | manual | webhook | push
	Status     string // running | ok | failed
	SHA        string // the commit deployed ("" for a push, which carries no commit)
	Message    string // one line: what happened, or why it didn't
	Log        string // build output — the reason a failure is diagnosable
	CreatedAt  string
	FinishedAt string
}

// Running reports whether this deploy is still in progress.
func (d Deployment) Running() bool { return d.Status == DeployRunning }

// DeployStep is one command a deploy ran, with what it printed.
type DeployStep struct {
	Name   string
	Output string
	Failed bool // the step the deploy stopped on
}

// Steps splits a deploy's log into the commands that produced it, so the UI can
// show a list of collapsed steps rather than one wall of text.
//
// The log is written as "$ <command>" followed by that command's output, so the
// marker is a line beginning with "$ ". Anything before the first marker (or a
// log with no markers at all — a host app's build is just the build's stdout) is
// returned as a single unnamed step, which renders the same as it always did.
//
// The last step of a failed deploy is marked failed: a deploy stops at its first
// failure, so the step it stopped on is the one worth opening.
func (d Deployment) Steps() []DeployStep {
	if strings.TrimSpace(d.Log) == "" {
		return nil
	}
	var steps []DeployStep
	var cur *DeployStep
	var body strings.Builder
	flush := func() {
		if cur != nil {
			cur.Output = strings.TrimRight(body.String(), "\n")
			steps = append(steps, *cur)
		}
		body.Reset()
	}
	for _, line := range strings.Split(d.Log, "\n") {
		if name, ok := strings.CutPrefix(line, "$ "); ok {
			flush()
			cur = &DeployStep{Name: strings.TrimSpace(name)}
			continue
		}
		if cur == nil {
			cur = &DeployStep{} // preamble, or a log with no step markers
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	flush()
	if d.Status == DeployFailed && len(steps) > 0 {
		steps[len(steps)-1].Failed = true
	}
	return steps
}

// StartDeployment opens a deploy record and returns its id.
func (s *Store) StartDeployment(appID int64, trigger string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO deployments (app_id, trigger, status) VALUES (?, ?, ?)`,
		appID, trigger, DeployRunning)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// FinishDeployment closes a deploy record with its outcome.
//
// The log is kept for successes as well as failures. It used to be dropped on
// success — nobody reads the output of a build that worked — but the settings
// page now renders it as a list of collapsed steps, which is how anyone sees
// what a deploy actually did. It is trimmed before it gets here, and only the
// most recent rows are kept (keepDeployments), so the size stays bounded.
func (s *Store) FinishDeployment(id int64, status, sha, message, log string) error {
	_, err := s.db.Exec(
		`UPDATE deployments SET status = ?, sha = ?, message = ?, log = ?,
		                        finished_at = datetime('now') WHERE id = ?`,
		status, sha, message, log, id)
	return err
}

// AppDeployments returns an app's recent deploys, newest first.
func (s *Store) AppDeployments(appID int64, limit int) ([]Deployment, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(deploymentSelect+` WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(&d.ID, &d.AppID, &d.Trigger, &d.Status, &d.SHA,
			&d.Message, &d.Log, &d.CreatedAt, &d.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneDeployments keeps only the most recent rows for an app.
func (s *Store) PruneDeployments(appID int64) error {
	_, err := s.db.Exec(
		`DELETE FROM deployments WHERE app_id = ? AND id NOT IN (
			SELECT id FROM deployments WHERE app_id = ? ORDER BY id DESC LIMIT ?)`,
		appID, appID, keepDeployments)
	return err
}

// ClearDeployments deletes an app's deploy history and returns how many rows
// went.
//
// A deploy that is still running is deliberately kept. Its row is doing two
// jobs beyond being history: it is where the goroutine building right now will
// write its result, and a running row is how a second deploy is refused. Delete
// it and the build carries on invisibly, finishes into a row that no longer
// exists, and nothing stops another deploy starting on top of it.
func (s *Store) ClearDeployments(appID int64) (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM deployments WHERE app_id = ? AND status != ?`, appID, DeployRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReapRunningDeployments marks in-flight deploys as failed. A deploy runs in a
// goroutine, so xdev stopping mid-build leaves a row that would otherwise say
// "running" forever — and block the next deploy, since a running one is how
// concurrency is refused.
func (s *Store) ReapRunningDeployments() error {
	_, err := s.db.Exec(
		`UPDATE deployments SET status = ?, message = ?, finished_at = datetime('now')
		 WHERE status = ?`,
		DeployFailed, "interrupted — xdev restarted while this deploy was running", DeployRunning)
	return err
}

// AppByHookID finds the app a webhook URL belongs to. The id is the unguessable
// part of the path; the request is still HMAC-verified after this.
func (s *Store) AppByHookID(hookID string) (App, error) {
	if hookID == "" {
		return App{}, ErrNotFound
	}
	return s.scanApp(s.db.QueryRow(appSelect+` WHERE hook_id = ?`, hookID))
}

// SetAppHook stores (or clears, with empty strings) an app's webhook id and
// signing secret.
func (s *Store) SetAppHook(id int64, hookID, secret string) error {
	_, err := s.db.Exec(
		`UPDATE apps SET hook_id = ?, hook_secret = ?, updated_at = datetime('now') WHERE id = ?`,
		hookID, secret, id)
	return err
}

// SetAppPushToken stores the hash of a deploy token (or clears it). The token
// itself is never written down — it is shown once, at the moment it is made.
func (s *Store) SetAppPushToken(id int64, hash, hint string) error {
	_, err := s.db.Exec(
		`UPDATE apps SET push_token_hash = ?, push_token_hint = ?, updated_at = datetime('now') WHERE id = ?`,
		hash, hint, id)
	return err
}

// SetSkipDBDump toggles whether a container app's deploy dumps the database
// before applying pending migrations.
func (s *Store) SetSkipDBDump(id int64, skip bool) error {
	_, err := s.db.Exec(
		`UPDATE apps SET skip_db_dump = ?, updated_at = datetime('now') WHERE id = ?`,
		skip, id)
	return err
}

// AppsWithEndpoints returns every app that has a webhook or a push token, so
// the reconciler can publish the /_xdev/ paths those need. Apps without either
// get no such route: an endpoint nobody enabled is not exposed.
func (s *Store) AppsWithEndpoints() ([]App, error) {
	rows, err := s.db.Query(appSelect + ` WHERE hook_id <> '' OR push_token_hash <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		a, err := scanAppRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

const deploymentSelect = `SELECT id, app_id, trigger, status, sha, message, log,
	created_at, finished_at FROM deployments`
