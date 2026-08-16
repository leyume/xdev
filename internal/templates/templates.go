// Package templates renders the per-app compose stack and any scaffold files
// from embedded templates. Each app type lives under files/<type>/ with a
// compose.yml.tmpl and an optional scaffold/ directory whose contents are
// dropped into the app's bind-mounted app/ directory on creation.
//
// The generated layout mirrors the bizepp sample: a compose file under _/ and
// application content under app/.
package templates

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"text/template"
)

//go:embed all:files
var filesFS embed.FS

// Data is everything a compose template needs. The apps service fills it in.
type Data struct {
	ProjectSlug string
	NetworkName string
	AppSlug     string
	AppType     string
	Env         string // local | prod (selects compose.prod.yml.tmpl when prod)
	// Server selects a laravel app's PHP server: "" or "swoole" for
	// Octane/Swoole, "fpm" for php-fpm. It picks a template the same way Env
	// does, so an unset value renders exactly what it rendered before the
	// option existed. Ignored by every other app type.
	Server      string
	AppImage    string // image the app service runs; "" lets RenderCompose resolve it
	HostPort    int
	AdminerPort int     // published host port for the secondary HTTP UI (laravel Adminer, mail admin); 0 = none
	CPULimit    float64 // cores; 0 = unlimited
	MemLimit    int64   // bytes; 0 = unlimited

	// Shared-database mode (PLANx §B): the app uses the platform xdev-db MariaDB
	// on the external xdev_shared network instead of its own db service.
	SharedDB bool
	DBName   string // database AND user name on xdev-db (<project>_<app>)
	DBPass   string // generated password for that user
}

// HasLimits reports whether any resource limit is set (drives the deploy block).
func (d Data) HasLimits() bool { return d.CPULimit > 0 || d.MemLimit > 0 }

// CPUStr formats the CPU limit for compose (e.g. "1.5").
func (d Data) CPUStr() string { return strconv.FormatFloat(d.CPULimit, 'f', -1, 64) }

// MemStr formats the memory limit in compose-friendly units (e.g. "512m", "1g").
func (d Data) MemStr() string { return humanBytes(d.MemLimit) }

// TypeInfo describes an app type for the UI catalog. Tagline is the one-line
// version shown on the type cards in the add-app form; Description is the full
// text behind them (tooltip / docs).
type TypeInfo struct {
	Type        string
	Name        string
	Tagline     string
	Description string
	Available   bool // false types are shown but not yet selectable
}

// Catalog lists the app types offered in the UI. The single "static" type runs
// on the host's system Node (or is file-served by Caddy) with no container; the
// rest are container/compose stacks.
func Catalog() []TypeInfo {
	return []TypeInfo{
		{"static", "Static", "Files or a Node dev server", "Static site/app served by xdev with your system Node — no container. Code lives directly in the app folder.", true},
		{"go", "Go", "Built + run with system Go", "Go app built and run on the host with your system Go toolchain — no container. Code lives directly in the app folder.", true},
		{"proxy", "Proxy", "Forward to another server", "Forward this domain to another server — just a Caddy route to an upstream URL. No container, process, or files.", true},
		{"compose", "Compose", "Bring your own compose.yml", "Bring your own docker-compose.yml / compose.yml — paste or upload it and xdev runs the stack as-is, routing a domain to each port it publishes.", true},
		{"wordpress", "WordPress", "Shared PHP host or its own stack", "WordPress + MariaDB, code in app/.", true},
		{"laravel", "Laravel", "Octane + MariaDB + Redis", "Laravel on Octane/Swoole + MariaDB + Redis (drop your app in app/).", true},
		{"mail", "Mail", "Stalwart SMTP/IMAP + webmail", "Full mail server — Stalwart (SMTP/IMAP, admin UI for domains, mailboxes, DKIM) + SnappyMail webmail. Prod needs port 25 open and MX/SPF/DKIM DNS records.", true},
	}
}

// IsValidType reports whether t is a currently-creatable app type.
func IsValidType(t string) bool {
	for _, ti := range Catalog() {
		if ti.Type == t && ti.Available {
			return true
		}
	}
	return false
}

// RenderCompose renders the compose template for an app type. When d.Env is
// "prod" and a compose.prod.yml.tmpl exists, it is preferred over the dev one.
func RenderCompose(appType string, d Data) (string, error) {
	// Resolve the app image unless the caller pinned one. Doing it here keeps
	// every caller — the apps service, tests, anything later — on the same
	// architecture-aware choice instead of hardcoding a tag in the template.
	if d.AppImage == "" && appType == "laravel" {
		if d.Server == "fpm" {
			d.AppImage = ResolveLaravelFPMImage(d.Env)
		} else {
			d.AppImage, _ = ResolveLaravelImage(d.Env)
		}
	}
	candidates := composeCandidates(appType, d.Env, d.Server)

	var raw []byte
	var err error
	for _, p := range candidates {
		raw, err = filesFS.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		return "", fmt.Errorf("no compose template for type %q: %w", appType, err)
	}
	tmpl, err := parseWithPartials(appType, string(raw))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// composeCandidates lists the compose templates to try, most specific first.
//
// Two independent dimensions select a template: the environment (prod has its
// own file) and, for laravel, the PHP server. They are ordered server-then-env
// because a server is a different stack — an fpm app in a prod project wants
// the fpm template far more than it wants the Swoole prod one — while env is a
// variation within a stack. Each dimension falls back to the unqualified name,
// so adding compose.fpm.yml.tmpl alone is enough to support fpm everywhere,
// and a type with neither still resolves compose.yml.tmpl exactly as before.
func composeCandidates(appType, env, server string) []string {
	base := "files/" + appType + "/compose"
	prod := env == "prod"

	var out []string
	if server != "" && server != "swoole" {
		if prod {
			out = append(out, base+"."+server+".prod.yml.tmpl")
		}
		out = append(out, base+"."+server+".yml.tmpl")
	}
	if prod {
		out = append(out, base+".prod.yml.tmpl")
	}
	return append(out, base+".yml.tmpl")
}

// RenderPartial renders one named block from a type's partials on its own —
// the same text RenderCompose would have inlined for the same Data.
//
// It exists so a service that can be switched on and off after an app is
// created (the Laravel Adminer) is written from the same definition that the
// compose file was rendered from, rather than from a second copy in Go that
// would drift the first time either changed.
func RenderPartial(appType, name string, d Data) (string, error) {
	tmpl, err := parseWithPartials(appType, "")
	if err != nil {
		return "", err
	}
	part := tmpl.Lookup(name)
	if part == nil {
		return "", fmt.Errorf("no partial %q for type %q", name, appType)
	}
	var buf bytes.Buffer
	if err := part.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// parseWithPartials parses body as the main template and associates every
// {{define}} under files/<type>/partials/ with it. A type with no partials
// directory just gets the body.
func parseWithPartials(appType, body string) (*template.Template, error) {
	tmpl, err := template.New(appType).Parse(body)
	if err != nil {
		return nil, err
	}
	names, err := fs.Glob(filesFS, "files/"+appType+"/partials/*.tmpl")
	if err != nil {
		return nil, err
	}
	for _, p := range names {
		raw, err := filesFS.ReadFile(p)
		if err != nil {
			return nil, err
		}
		// A file of nothing but {{define}} blocks has an empty body, which Parse
		// leaves the existing one alone for — so this only adds definitions.
		if tmpl, err = tmpl.Parse(string(raw)); err != nil {
			return nil, fmt.Errorf("parse partial %s: %w", p, err)
		}
	}
	return tmpl, nil
}

// AdminerCSS returns the bundled Adminer stylesheet (the Pepa Linha dark theme
// the laravel apps ship in their db/), so the platform's shared Adminer can
// mount the same design instead of the image's older built-in one.
func AdminerCSS() ([]byte, error) {
	return filesFS.ReadFile("files/laravel/infra/db/adminer.css")
}

// ScaffoldFiles returns the relative path -> contents of every file under
// files/<type>/scaffold/, to be written into the app's content directory.
// Returns an empty map if the type has no scaffold.
func ScaffoldFiles(appType string) (map[string][]byte, error) {
	return embeddedFiles(appType, "scaffold")
}

// InfraFiles returns the relative path -> contents of every file under
// files/<type>/infra/, to be written into the app's _/ directory (compose
// support files like init.sh and db config). Empty map if the type has none.
func InfraFiles(appType string) (map[string][]byte, error) {
	return embeddedFiles(appType, "infra")
}

// embeddedFiles walks files/<type>/<sub>/ and returns each file's path (relative
// to that root) -> contents. A missing subdir yields an empty map.
func embeddedFiles(appType, sub string) (map[string][]byte, error) {
	root := "files/" + appType + "/" + sub
	out := map[string][]byte{}
	err := fs.WalkDir(filesFS, root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			// No such dir for this type is fine.
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if de.IsDir() {
			return nil
		}
		data, err := filesFS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		out[rel] = data
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

// humanBytes converts a byte count to the smallest clean compose unit.
func humanBytes(b int64) string {
	const k = 1024
	switch {
	case b == 0:
		return "0"
	case b%(k*k*k) == 0:
		return strconv.FormatInt(b/(k*k*k), 10) + "g"
	case b%(k*k) == 0:
		return strconv.FormatInt(b/(k*k), 10) + "m"
	case b%k == 0:
		return strconv.FormatInt(b/k, 10) + "k"
	default:
		return strconv.FormatInt(b, 10)
	}
}
