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
	HostPort    int
	CPULimit    float64 // cores; 0 = unlimited
	MemLimit    int64   // bytes; 0 = unlimited
}

// HasLimits reports whether any resource limit is set (drives the deploy block).
func (d Data) HasLimits() bool { return d.CPULimit > 0 || d.MemLimit > 0 }

// CPUStr formats the CPU limit for compose (e.g. "1.5").
func (d Data) CPUStr() string { return strconv.FormatFloat(d.CPULimit, 'f', -1, 64) }

// MemStr formats the memory limit in compose-friendly units (e.g. "512m", "1g").
func (d Data) MemStr() string { return humanBytes(d.MemLimit) }

// TypeInfo describes an app type for the UI catalog.
type TypeInfo struct {
	Type        string
	Name        string
	Description string
	Available   bool // false types are shown but not yet selectable
}

// Catalog lists the app types. Only static-prebuilt is wired up in Phase 1; the
// rest are placeholders so the UI can show the roadmap.
func Catalog() []TypeInfo {
	return []TypeInfo{
		{"static-prebuilt", "Static (prebuilt)", "Serve a built dist/ folder (Vue/React/plain HTML).", true},
		{"static-build", "Static (build)", "Run a Vite dev server with HMR from your source.", true},
		{"wordpress", "WordPress", "WordPress + MariaDB, code in app/.", true},
		{"laravel", "Laravel", "Laravel on Octane/Swoole + MariaDB + Redis (drop your app in app/).", true},
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
	candidates := make([]string, 0, 2)
	if d.Env == "prod" {
		candidates = append(candidates, "files/"+appType+"/compose.prod.yml.tmpl")
	}
	candidates = append(candidates, "files/"+appType+"/compose.yml.tmpl")

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
	tmpl, err := template.New(appType).Parse(string(raw))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// ScaffoldFiles returns the relative path -> contents of every file under
// files/<type>/scaffold/, to be written into the app's app/ directory. Returns
// an empty map if the type has no scaffold.
func ScaffoldFiles(appType string) (map[string][]byte, error) {
	root := "files/" + appType + "/scaffold"
	out := map[string][]byte{}
	err := fs.WalkDir(filesFS, root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			// No scaffold dir for this type is fine.
			if strings.Contains(err.Error(), "file does not exist") {
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
	if err != nil && !strings.Contains(err.Error(), "file does not exist") {
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
