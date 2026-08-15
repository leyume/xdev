package server

// The per-app settings page: everything the add-app form collected, editable
// after the fact. One page per app rather than a field here and a field there,
// because most changes come in groups — renaming an app and moving its domain,
// or editing a compose file and adding the domain its new service needs.

import (
	"net/http"
	"strconv"
	"strings"

	"xdev/internal/apps"
	"xdev/internal/store"
)

// appSettingsForm is the page's inputs, filled from the app on GET and from the
// request on POST. Keeping it in one struct is what lets a rejected save
// re-render exactly what was typed — a hand-edited compose file is not
// something to make anyone type twice.
type appSettingsForm struct {
	Name          string
	Domain        string
	ServeMode     string
	RootDir       string
	SourceDir     string
	GitURL        string
	GitRef        string
	GitSubdir     string
	BuildCmd      string
	StartCmd      string
	Upstream      string
	ServiceDomain string
	ExtraDomains  []string
	Compose       string
}

// handleAppSettingsForm shows the settings page.
func (s *Server) handleAppSettingsForm(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	s.renderAppSettings(w, r, app, proj, s.settingsFormFor(app), r.URL.Query().Get("error"))
}

// handleAppSettingsSave validates and applies the form, then restarts the app if
// it was running — settings only reach a live stack on restart. A rejected save
// re-renders the page with the submitted values and changes nothing.
func (s *Server) handleAppSettingsSave(w http.ResponseWriter, r *http.Request) {
	app, proj, ok := s.appAndProject(w, r)
	if !ok {
		return
	}
	form := settingsFormFrom(r)
	updated, err := s.apps.Update(app.ID, apps.EditOpts{
		Name:      form.Name,
		Domain:    form.Domain,
		ServeMode: form.ServeMode,
		RootDir:   form.RootDir,
		SourceDir: form.SourceDir,
		// Read through the same helper as the add-app form. Spelling the fields
		// out here once meant KeyID was silently dropped, so an app re-pointed
		// at a private repository could never be given a key.
		Git:           gitOptsFrom(r),
		BuildCmd:      form.BuildCmd,
		StartCmd:      form.StartCmd,
		Upstream:      form.Upstream,
		ServiceDomain: form.ServiceDomain,
		ExtraDomains:  form.ExtraDomains,
		ComposeFile:   form.Compose,
	})
	if err != nil {
		s.renderAppSettings(w, r, app, proj, form, err.Error())
		return
	}
	s.store.AddEvent(proj.ID, app.ID, "info", "Updated settings for app "+updated.Name)

	// Stop-then-start rather than a bare start: an app that switched from
	// serving files to running a command has a process to retire, and a compose
	// stack whose file changed should come up from the new one.
	target := "/apps/" + strconv.FormatInt(app.ID, 10) + "/settings"
	if app.Status == store.AppRunning {
		_ = s.apps.Stop(app.ID)
		if err := s.apps.Start(app.ID); err != nil {
			// The settings are saved; say so, and report why it didn't come back.
			s.reconcile()
			redirectWithError(w, r, target, err)
			return
		}
	}
	s.reconcile()
	http.Redirect(w, r, target+"?saved=1", http.StatusSeeOther)
}

// actionResult is the output of a maintenance command run inside a container
// app, shown on the page that started it. Nil when the page was not reached
// that way.
type actionResult struct {
	Label  string
	Output string
	Failed bool
}

// renderAppSettings draws the page around a form — the app's current settings on
// a GET, the rejected ones on a failed save.
func (s *Server) renderAppSettings(w http.ResponseWriter, r *http.Request, app store.App, proj store.Project, form appSettingsForm, errMsg string) {
	s.renderAppSettingsWith(w, r, app, proj, form, errMsg, nil)
}

// renderAppSettingsWith is renderAppSettings plus the output of a container
// action, which is rendered in place rather than redirected away — a redirect
// would lose the only thing the user pressed the button for.
func (s *Server) renderAppSettingsWith(w http.ResponseWriter, r *http.Request, app store.App, proj store.Project, form appSettingsForm, errMsg string, result *actionResult) {
	svc, _ := s.store.AppServiceDomains(app.ID)
	deploys, _ := s.store.AppDeployments(app.ID, 8)
	// Ports for the compose rows the form is showing: an existing slot keeps
	// its allocated port, one added by this edit gets its number on save.
	ports := make([]int, len(form.ExtraDomains))
	for i := range form.ExtraDomains {
		if i < len(svc) {
			ports[i] = svc[i].Port
		}
	}
	s.render(w, r, "app_settings", viewData{
		"Title":        app.Name + " settings · xdev",
		"App":          app,
		"Project":      proj,
		"Form":         form,
		"SlotPorts":    ports,
		"ServicePort":  servicePort(svc),
		"ServiceLabel": serviceLabel(app.Type),
		"DB":           appDB(app, proj, svc),
		"MaxDomains":   maxComposeDomains,
		"HasCompose":   app.ComposePath != "",
		"ComposeName":  composeFileLabel(app),
		"EnvPath":      s.apps.EnvLocation(app.ID),
		"Git":          s.appGit(app),
		"Deploy":       s.appDeploys(app, newPushToken(r)),
		"Deploys":      deploys,
		"Deploying":    s.apps.Deploying(app.ID),
		"Actions":      apps.ContainerActions(app),
		"ActionResult": result,
		// Whether the bundled Adminer can be switched at all, and whether it is
		// on now. "On" is the presence of its service-domain row — the same fact
		// appDB above reads for the hostname, not a second copy of it.
		"CanToggleAdminer": apps.CanToggleAdminer(app),
		"AdminerOn":        len(svc) > 0,
		"Error":            errMsg,
		"Saved":            r.URL.Query().Get("saved") != "",
		"JustStarted":      r.URL.Query().Get("deploying") != "",
		"Rotated":          r.URL.Query().Get("rotated") != "",
		"HookState":        r.URL.Query().Get("hook"),
	})
}

// settingsFormFor fills the form from an app's current state (the compose file
// is read from disk — it is the app's real configuration, not a copy in the DB).
func (s *Server) settingsFormFor(app store.App) appSettingsForm {
	// The domain field holds every hostname the app answers on, in entry order.
	// app.Domain is only the first of them, so seeding the field from that alone
	// would drop the rest the next time the form is saved.
	hosts, err := s.store.AppHostnames(app.ID)
	if err != nil || len(hosts) == 0 {
		hosts = []string{app.Domain} // no domain rows: fall back to the app row
	}
	form := appSettingsForm{
		Name:      app.Name,
		Domain:    strings.Join(hosts, ", "),
		ServeMode: app.ServeMode,
		RootDir:   app.RootDir,
		SourceDir: app.SourceDir,
		GitURL:    app.GitURL,
		GitRef:    app.GitRef,
		GitSubdir: app.GitSubdir,
		BuildCmd:  app.BuildCmd,
		StartCmd:  app.StartCmd,
		Upstream:  app.Upstream,
	}
	if svc, err := s.store.AppServiceDomains(app.ID); err == nil && len(svc) > 0 {
		if app.IsCompose() {
			for _, d := range svc {
				form.ExtraDomains = append(form.ExtraDomains, d.Host)
			}
		} else {
			form.ServiceDomain = svc[0].Host
		}
	}
	if app.ComposePath != "" {
		form.Compose, _ = s.apps.ReadCompose(app.ID)
	}
	return form
}

// settingsFormFrom reads the submitted form. Blank extra-domain rows are kept
// rather than dropped: for a compose app the row's position is its ${PORT_n}
// slot, so the apps service rejects the blank by number instead of silently
// renumbering the domains after it.
func settingsFormFrom(r *http.Request) appSettingsForm {
	return appSettingsForm{
		Name:          strings.TrimSpace(r.FormValue("name")),
		Domain:        strings.TrimSpace(r.FormValue("domain")),
		ServeMode:     r.FormValue("serve_mode"),
		RootDir:       r.FormValue("root_dir"),
		SourceDir:     r.FormValue("source_dir"),
		GitURL:        strings.TrimSpace(r.FormValue("git_url")),
		GitRef:        strings.TrimSpace(r.FormValue("git_ref")),
		GitSubdir:     strings.TrimSpace(r.FormValue("git_subdir")),
		BuildCmd:      r.FormValue("build_cmd"),
		StartCmd:      r.FormValue("start_cmd"),
		Upstream:      r.FormValue("upstream"),
		ServiceDomain: strings.TrimSpace(r.FormValue("service_domain")),
		ExtraDomains:  extraDomains(r),
		Compose:       r.FormValue("compose_yaml"),
	}
}

// appDBInfo is what the settings page says about a db-backed app's database.
// It is shown for both modes — which server the app is on, what its database is
// called there, and which Adminer opens it — and none of it is editable:
// moving between shared and dedicated copies data, so it is a migration.
type appDBInfo struct {
	Shared      bool
	Name        string // the database this app uses
	User        string // the account it connects as
	Server      string // where that database lives, as the app's containers see it
	ManageURL   string // shared only: this database's page under /database
	Adminer     string // hostname of the Adminer bundled with this app ("" = none)
	AdminerPort int
	AdminerOn   string // which server that Adminer opens on
}

// appDB describes the database behind a wordpress/laravel app, or nil for types
// that have none.
//
// Dedicated-mode names are the ones the type's own compose template writes
// (internal/templates/files/{laravel,wordpress}/compose.yml.tmpl) — if those
// change, this has to follow. Shared mode has no such duplication: the name is
// computed by the same function that provisions it.
func appDB(app store.App, proj store.Project, svc []store.ServiceDomain) *appDBInfo {
	if !apps.UsesDB(app.Type) {
		return nil
	}
	info := &appDBInfo{Shared: app.DBMode == store.DBShared || app.IsSharedWP()}
	if info.Shared {
		info.Name = apps.SharedDBName(proj.Slug, app.Slug)
		info.User = info.Name
		info.Server = "xdev-db:3306"
		info.ManageURL = "/database/" + info.Name
	} else {
		info.Server = "db:3306"
		if app.Type == "laravel" {
			info.Name, info.User = "laravel", "laravel"
		} else {
			info.Name, info.User = "wordpress", "wp"
		}
	}
	// The bundled Adminer, when the type ships one (Laravel does, in both DB
	// modes — pointed at whichever server the app uses).
	if serviceLabel(app.Type) == "Adminer" && len(svc) > 0 {
		info.Adminer, info.AdminerPort = svc[0].Host, svc[0].Port
		info.AdminerOn = "the shared xdev-db server"
		if !info.Shared {
			info.AdminerOn = "this app's own db container"
		}
	}
	return info
}

// serviceLabel names the second web UI a type bundles, for the domain row that
// points at it. "" means the type has none.
func serviceLabel(appType string) string {
	switch appType {
	case "laravel":
		return "Adminer"
	case "mail":
		return "Mail admin"
	}
	return ""
}

// servicePort is the host port behind a bundled UI's hostname (0 if none).
func servicePort(svc []store.ServiceDomain) int {
	if len(svc) == 0 {
		return 0
	}
	return svc[0].Port
}

// composeFileLabel describes whose file the compose editor is showing: a
// bring-your-own stack is the user's from the start, a templated one was
// generated at create and is theirs from the first edit.
func composeFileLabel(app store.App) string {
	if app.IsCompose() {
		return "the file you supplied"
	}
	return "generated for this " + app.Type + " app when it was created"
}
