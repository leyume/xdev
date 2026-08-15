package server

import (
	"regexp"
	"strings"
	"testing"

	"xdev/internal/apps"
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

// The refresh and clear controls are icon-only. That is only acceptable if two
// things are true of every one of them: a data-tip label for anyone who cannot
// tell what the glyph means, and an aria-label — data-tip is a CSS ::after and
// reaches no screen reader at all, so without it the button is announced as
// nothing but "button".
func TestRefreshAndClearAreLabelledIconButtons(t *testing.T) {
	pages := map[string]string{
		"settings": renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, deployViewData()),
		"logs": renderNamed(t, "app_logs", viewData{
			"Title": "logs", "App": store.App{ID: 2, Name: "site", Slug: "site", Type: "static"},
			"Project": store.Project{Name: "Demo", Slug: "demo"}, "Logs": "x", "CanClear": true,
		}),
		"events": renderNamed(t, "events", viewData{
			"Title":  "Activity",
			"Events": []store.Event{{TS: "2026-08-15", Level: "info", Message: "did a thing"}},
		}),
		"project": renderProjectWith(t, nil, viewData{
			"Activity": []store.Event{{TS: "2026-08-15", Level: "info", Message: "started web"}},
		}),
	}

	// Every icon-only control on the page, then the clear/refresh ones among
	// them — matched on what they say they do rather than on the markup around
	// them, which differs per page (the action sits on the form, not the button).
	icons := regexp.MustCompile(`<(?:button|a)[^>]*\biconbtn\b[^>]*>`)
	wanted := regexp.MustCompile(`(?i)data-tip="[^"]*(clear|refresh)`)
	for page, html := range pages {
		found := 0
		for _, tag := range icons.FindAllString(html, -1) {
			if !wanted.MatchString(tag) {
				continue // some other icon button (start, stop, settings…)
			}
			found++
			if !strings.Contains(tag, "data-tip=") {
				t.Errorf("%s: icon control has no hover label: %s", page, tag)
			}
			if !strings.Contains(tag, "aria-label=") {
				t.Errorf("%s: icon control has no accessible name — data-tip is decoration: %s", page, tag)
			}
		}
		if found == 0 {
			t.Errorf("%s: no icon-only refresh/clear control found at all", page)
		}
	}
}

// The Deploys card heads a sticky column that scrolls (overflow-y: auto), which
// clips a label drawn above it. Those two get the below-the-button variant, and
// the stylesheet has to define it.
func TestStickyCardTooltipsPointDown(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, deployViewData())
	if !strings.Contains(out, ".tip.tip-below::after") {
		t.Error("no tip-below rule in the stylesheet, so the variant does nothing")
	}
	i := strings.Index(out, `class="side-col is-sticky"`)
	if i < 0 {
		t.Fatal("no sticky side column")
	}
	if !strings.Contains(out[i:], "tip-below") {
		t.Error("the Deploys card's icon buttons label upwards, where the scrolling column clips them")
	}
}

// actionsViewData is a settings page with the Laravel maintenance commands.
func actionsViewData(result *actionResult) viewData {
	return viewData{
		"Actions": []apps.ContainerAction{
			{Key: "migrate-status", Label: "Migration status",
				Help: "List migrations and whether each has run. Changes nothing."},
			{Key: "migrate", Label: "Run migrations", Destroy: true,
				Help: "Apply pending migrations. Takes a database dump first."},
		},
		"ActionResult": result,
	}
}

// Eleven maintenance commands are a reference, not something anyone is part-way
// through — open, they pushed the settings form off the screen.
func TestRunCommandSectionIsCollapsed(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, actionsViewData(nil))

	box := regexp.MustCompile(`<details class="action-box"([^>]*)>`).FindStringSubmatch(out)
	if box == nil {
		t.Fatal("the run-command section is not collapsible at all")
	}
	if strings.Contains(box[1], "open") {
		t.Errorf("it starts expanded (%q)", box[1])
	}

	// …except right after running one: the output is the only thing the button
	// was pressed for, and a collapsed section would hide it.
	ran := renderSettings(t, gitApp(), appSettingsForm{Name: "web"},
		actionsViewData(&actionResult{Label: "Migration status", Output: "Ran? Yes"}))
	box = regexp.MustCompile(`<details class="action-box"([^>]*)>`).FindStringSubmatch(ran)
	if box == nil || !strings.Contains(box[1], "open") {
		t.Error("after running a command the section is still collapsed, hiding the output")
	}
	if !strings.Contains(ran, "Ran? Yes") {
		t.Error("the command output is not on the page")
	}
}

// Each command reads as a labelled row — name, what it does underneath, and the
// run control in the same place on every row.
func TestActionRowsAreLabelledWithARunIcon(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, actionsViewData(nil))

	rows := regexp.MustCompile(`(?s)<form class="action-row".*?</form>`).FindAllString(out, -1)
	if len(rows) != 2 {
		t.Fatalf("found %d command rows, want 2", len(rows))
	}
	for _, row := range rows {
		if !strings.Contains(row, `class="action-name"`) || !strings.Contains(row, `class="muted small action-help"`) {
			t.Errorf("a command row has no name/help pair:\n%s", row)
		}
		if !strings.Contains(row, "iconbtn") || !strings.Contains(row, `data-tip="Run"`) {
			t.Errorf("a command row has no run icon:\n%s", row)
		}
		if !strings.Contains(row, "aria-label=\"Run ") {
			t.Errorf("the run icon has no accessible name, so it announces as just \"button\":\n%s", row)
		}
	}
	// The destructive one is marked as such and still asks first.
	migrate := rows[1]
	if !strings.Contains(migrate, "changes data") {
		t.Error("the destructive command is not flagged in its row")
	}
	if !strings.Contains(migrate, "data-confirm=") {
		t.Error("the destructive command no longer asks for confirmation")
	}
}

// Running a command posts with fetch and opens the output under the row that
// was clicked — the page keeps its scroll position, its open panels and
// anything half-typed in the settings form below.
func TestActionsRunInPlace(t *testing.T) {
	out := renderSettings(t, gitApp(), appSettingsForm{Name: "web"}, actionsViewData(nil))

	if !strings.Contains(out, `x-data="actionRow()"`) {
		t.Fatal("command rows have no per-row state, so one running would speak for all of them")
	}
	if !strings.Contains(out, `@submit="run($event)"`) {
		t.Error("the form still submits normally — the page would reload")
	}
	// A declined confirmation must not run the command. Alpine's .prevent
	// modifier does not check defaultPrevented, so run() has to.
	if !strings.Contains(out, "if (e.defaultPrevented) return;") {
		t.Error("run() ignores a declined confirmation and would go ahead anyway")
	}
	// The output element lives inside the row's own container.
	item := regexp.MustCompile(`(?s)<div class="action-item".*?</div>\s*</div>`).FindString(out)
	if item == "" || !strings.Contains(item, `class="action-out"`) {
		t.Error("the output panel is not inside the row it belongs to")
	}
	// It still works without JS: a real action and method, not href="#".
	if !strings.Contains(out, `<form class="action-row" method="post" action="/apps/1/action"`) {
		t.Error("the form has no server-side fallback")
	}
}
