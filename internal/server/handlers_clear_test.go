package server

import (
	"regexp"
	"strings"
	"testing"

	"xdev/internal/store"
)

// deployViewData is a settings page with a history to clear: one finished
// deploy and one still running.
func deployViewData() viewData {
	return viewData{
		"Deploys": []store.Deployment{
			{ID: 7, AppID: 1, Trigger: store.DeployWebhook, Status: store.DeployOK,
				Message: "deployed abc1234", CreatedAt: "2026-08-15 14:02",
				Log: "$ composer install\n86 installs\n$ php artisan migrate --force\nnothing to migrate"},
			{ID: 8, AppID: 1, Trigger: store.DeployManual, Status: store.DeployRunning,
				CreatedAt: "2026-08-15 14:40"},
		},
		"Deploying": true,
	}
}

// A deploy history is read by date: which build ran at 14:02, and what it said.
// Twenty past builds should cost twenty lines, not twenty walls of output — so
// each one is a collapsed entry headed by when it ran.
func TestDeployHistoryCollapsesPerDeploy(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, deployViewData())

	rows := regexp.MustCompile(`<details class="deploy-row" data-deploy="(\d+)"([^>]*)>`).FindAllStringSubmatch(out, -1)
	if len(rows) != 2 {
		t.Fatalf("found %d collapsible deploy entries, want 2 — each deploy should be its own <details>", len(rows))
	}

	byID := map[string]string{}
	for _, m := range rows {
		byID[m[1]] = m[2]
	}
	// The finished one is closed: it is history, and the date line is the handle.
	if strings.Contains(byID["7"], "open") {
		t.Errorf("the finished deploy starts expanded (%q) — the point is that old builds stay collapsed", byID["7"])
	}
	// The running one is open: it was just triggered, and hiding a build in
	// progress behind a click is the opposite of watching it.
	if !strings.Contains(byID["8"], "open") {
		t.Errorf("the running deploy starts collapsed (%q) — a build in flight should be on screen", byID["8"])
	}
	if !strings.Contains(byID["8"], `data-running="1"`) {
		t.Error("the running deploy has no data-running marker, so the page cannot know to keep polling")
	}

	// The date is in the summary, which is what you read when everything is shut.
	if !regexp.MustCompile(`(?s)<summary class="deploy-head">.*?2026-08-15 14:02.*?</summary>`).MatchString(out) {
		t.Error("the collapsed handle does not carry the deploy's date")
	}
	// Steps keep their own identity so an expanded one can be restored after a poll.
	if !strings.Contains(out, `data-step="0"`) {
		t.Error("deploy steps are not individually identified (data-step), so the poll cannot restore them")
	}
}

// Polling replaces the whole list, and a replaced element is a new element with
// default state. Without explicitly putting the expanded ones back, opening a
// build's output while it runs closes it again within three seconds — exactly
// when the output is the thing you are trying to read.
func TestDeployPollRestoresExpandedEntries(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, deployViewData())
	for _, want := range []string{"openKeys()", "restore(keys)", "details[open]"} {
		if !strings.Contains(out, want) {
			t.Errorf("the deploy poll does not preserve open panels: missing %q", want)
		}
	}
	// The order matters: read the open set before the innerHTML swap, not after.
	keys := strings.Index(out, "const keys = this.openKeys()")
	swap := strings.Index(out, "this.$refs.list.innerHTML = html")
	if keys < 0 || swap < 0 || keys > swap {
		t.Error("open panels are read after the list is replaced, which is too late to record them")
	}
}

// Every log surface xdev owns can be emptied from the page that shows it.
func TestClearButtonsExist(t *testing.T) {
	settings := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, deployViewData())
	if !strings.Contains(settings, `action="/apps/1/deploys/clear"`) {
		t.Error("the Deploys card has no Clear button")
	}

	// A host app's log file is xdev's own, so it can be cleared.
	hostApp := store.App{ID: 2, Name: "site", Slug: "site", Type: "static", Status: store.AppRunning}
	out := renderNamed(t, "app_logs", viewData{
		"Title": "logs", "App": hostApp,
		"Project":  store.Project{Name: "Demo", Slug: "demo"},
		"Logs":     "hello",
		"CanClear": true,
	})
	if !strings.Contains(out, `action="/apps/2/logs/clear"`) {
		t.Error("a host app's logs page has no Clear button")
	}

	// A container app's output belongs to the engine. Better no button than one
	// that exists only to fail.
	out = renderNamed(t, "app_logs", viewData{
		"Title": "logs", "App": gitApp(),
		"Project":  store.Project{Name: "Demo", Slug: "demo"},
		"Logs":     "hello",
		"CanClear": false,
	})
	if strings.Contains(out, "/logs/clear") {
		t.Error("a container app offers a Clear button for logs xdev does not own")
	}
}

// The activity log clears in two scopes from two places, through one handler:
// the project page sends its project id, the all-projects page sends none.
func TestActivityClearIsScoped(t *testing.T) {
	events := renderNamed(t, "events", viewData{
		"Title": "Activity", "Events": []store.Event{{TS: "2026-08-15", Level: "info", Message: "did a thing"}},
	})
	if !strings.Contains(events, `action="/events/clear"`) {
		t.Fatal("the Activity page has no Clear button")
	}
	if strings.Contains(events, `name="project_id"`) {
		t.Error("the all-projects Activity page scopes its clear to a project, so rows with no project would survive")
	}

	proj := renderProjectWith(t, nil, viewData{
		"Activity": []store.Event{{TS: "2026-08-15", Level: "info", Message: "started web"}},
	})
	if !strings.Contains(proj, `action="/events/clear"`) {
		t.Fatal("the project page has no Clear button for its activity")
	}
	if !regexp.MustCompile(`(?s)action="/events/clear".*?name="project_id"`).MatchString(proj) {
		t.Error("the project page's clear does not carry a project id, so it would empty every project's log")
	}
}

// An empty log has no Clear button: a control that can only do nothing should
// not be there to be aimed at.
func TestClearIsHiddenWithNothingToClear(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, viewData{
		"Deploy": gitViewData()["Deploy"],
	})
	if strings.Contains(out, "/deploys/clear") {
		t.Error("Clear is offered on an app with no deploy history")
	}
	events := renderNamed(t, "events", viewData{"Title": "Activity"})
	if strings.Contains(events, "/events/clear") {
		t.Error("Clear all is offered on an empty activity log")
	}
}

// data-confirm has to work on every form that carries it. It used to be read
// only inside the start/stop/delete handler, which returns early for every
// other action — so 13 of the 15 confirmations in the app never appeared,
// including "Run migrate? This changes the database".
func TestConfirmWorksOnAnyForm(t *testing.T) {
	out := renderNamed(t, "events", viewData{"Title": "Activity"})

	confirmAt := strings.Index(out, "f.dataset.confirm")
	if confirmAt < 0 {
		t.Fatal("no data-confirm handling in the layout at all")
	}
	// The generic handler must come before the app-action one, which is the
	// handler that used to own (and swallow) the attribute.
	actionAt := strings.Index(out, `/^\/apps\/\d+\/(start|stop|delete)$/`)
	if actionAt >= 0 && confirmAt > actionAt {
		t.Error("data-confirm is still handled inside the app-action handler, so other forms never prompt")
	}
	if !strings.Contains(out, "if (e.defaultPrevented) return;") {
		t.Error("the app-action handler does not check defaultPrevented, so a declined confirm still submits")
	}
	// A confirmation may sit on the button rather than the form when one form
	// has several submit buttons (the container-action list does exactly that).
	if !strings.Contains(out, "e.submitter.dataset") {
		t.Error("a data-confirm on the submitting button is ignored")
	}
}

// The create dialog polls about once a second and re-renders its step list each
// time. A bound :open re-asserts its own value on every tick, so a panel the
// user opened to watch a slow Laravel build closed itself a second later.
func TestCreateStepsStayOpenAcrossPolls(t *testing.T) {
	out := renderProject(t, nil)
	if strings.Contains(out, `:open="st.failed"`) {
		t.Error("create steps still bind :open directly to st.failed, which re-closes them on every poll")
	}
	for _, want := range []string{`:open="stepOpen(i, st)"`, `@toggle="openSteps[i] = $event.target.open"`} {
		if !strings.Contains(out, want) {
			t.Errorf("create steps do not track what the user opened: missing %q", want)
		}
	}
	// A failed step is still open by default — it is the one worth reading.
	if !strings.Contains(out, "!!st.failed") {
		t.Error("a failed step no longer opens itself")
	}
}
