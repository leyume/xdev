package apps

// Running maintenance commands inside a container app.
//
// This is a fixed allowlist, not a command box, and that is the whole design.
// An arbitrary-command endpoint in the web UI is a root shell in the container,
// reachable from the internet and gated by nothing but a session cookie —
// which turns a stolen cookie from "somebody edits my apps" into "somebody has
// a shell next to my database". A named action can be logged, explained, and
// reasoned about; `sh -c $INPUT` can be none of those.
//
// Adding one is deliberately a code change. If an action is common enough to
// want a button, it is common enough to be reviewed once.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

// actionTimeout bounds one action. Shorter than a deploy: these are single
// commands somebody is watching, not a build.
const actionTimeout = 5 * time.Minute

// ContainerAction is one thing xdev will run inside an app's container.
type ContainerAction struct {
	Key     string   // stable id used by the form
	Label   string   // what the button says
	Help    string   // what it does, in the UI
	Args    []string // argv, run in the app service
	Destroy bool     // ask for confirmation first
}

// containerActions is the allowlist. Ordered as they appear on the page:
// diagnosis first, then the caches people clear most, then the ones that
// change something.
var containerActions = []ContainerAction{{
	Key: "migrate-status", Label: "Migration status",
	Help: "List migrations and whether each has run. Changes nothing.",
	Args: []string{"php", "artisan", "migrate:status"},
}, {
	Key: "about", Label: "About",
	Help: "Laravel, PHP and package versions, and which drivers are configured.",
	Args: []string{"php", "artisan", "about"},
}, {
	Key: "cache-clear", Label: "Clear application cache",
	Help: "Flush the application cache (Redis). Sessions are not affected.",
	Args: []string{"php", "artisan", "cache:clear"},
}, {
	Key: "config-clear", Label: "Clear config & route cache",
	Help: "Drop the cached config and routes so the next request reads them fresh. Use after editing .env.",
	Args: []string{"php", "artisan", "optimize:clear"},
}, {
	Key: "config-cache", Label: "Rebuild caches",
	Help: "Cache config, routes and views again — what a deploy does at the end.",
	Args: []string{"php", "artisan", "optimize"},
}, {
	Key: "storage-link", Label: "Link storage",
	Help: "Create public/storage → storage/app/public, so uploaded files are servable.",
	Args: []string{"php", "artisan", "storage:link"},
}, {
	Key: "queue-restart", Label: "Restart queue workers",
	Help: "Tell queue workers to finish the current job and exit, so they pick up new code.",
	Args: []string{"php", "artisan", "queue:restart"},
}, {
	Key: "octane-reload", Label: "Reload Octane",
	Help: "Swap the Swoole workers onto the code currently on disk, without dropping requests.",
	Args: []string{"php", "artisan", "octane:reload"},
}, {
	Key: "composer-install", Label: "Install dependencies",
	Help: "composer install --no-dev, as a deploy runs it. Use when vendor/ is missing or stale.",
	Args: []string{"composer", "install", "--no-dev", "--optimize-autoloader", "--no-interaction"},
}, {
	Key: "migrate", Label: "Run migrations",
	Help: "Apply pending migrations. Takes a database dump first, unless this app has dumps switched off.",
	Args: []string{"php", "artisan", "migrate", "--force"}, Destroy: true,
}}

// ContainerActions lists the actions available for an app, or nil when the app
// is not one they apply to.
func ContainerActions(app store.App) []ContainerAction {
	if app.Type != "laravel" {
		return nil
	}
	return containerActions
}

// RunContainerAction executes one allowlisted action and returns its output.
//
// The action is looked up by key rather than taken from the request, so no
// request can name a command that is not in the list above.
func (s *Service) RunContainerAction(id int64, key string) (ContainerAction, string, error) {
	app, err := s.store.AppByID(id)
	if err != nil {
		return ContainerAction{}, "", err
	}
	var action ContainerAction
	for _, a := range ContainerActions(app) {
		if a.Key == key {
			action = a
			break
		}
	}
	if action.Key == "" {
		return ContainerAction{}, "", errors.New("unknown action")
	}

	_, engine, workdir, pname, file, err := s.composeCtx(id)
	if err != nil {
		return action, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), actionTimeout)
	defer cancel()
	if !runtime.Running(ctx, engine, workdir, pname, file) {
		return action, "", errors.New("the app is not running — start it first, these commands run inside its container")
	}

	// Migrations get the same protection here as in a deploy. Running one from
	// a button is not less irreversible for having been clicked.
	var pre string
	if action.Key == "migrate" {
		dump, err := s.dumpBeforeMigrate(ctx, engine, app)
		if err != nil {
			return action, "", fmt.Errorf("could not dump the database first, so the migration was not run: %w", err)
		}
		if dump != "" {
			pre = "database dumped to " + dump + "\n\n"
		}
	}

	full := append([]string{"exec", "-T", laravelService}, action.Args...)
	out, err := runtime.Compose(ctx, engine, workdir, pname, file, full...)
	return action, pre + out, err
}
