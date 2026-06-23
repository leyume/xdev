package templates

import (
	"strings"
	"testing"
)

// TestRenderAllAvailableTypes renders every selectable app type and checks the
// output is non-empty, wires the per-app container name and host port, and joins
// the shared project network. This catches template syntax/field errors without
// needing a container engine.
func TestRenderAllAvailableTypes(t *testing.T) {
	d := Data{
		ProjectSlug: "demo",
		NetworkName: "xdev_demo",
		AppSlug:     "site",
		HostPort:    20000,
	}
	for _, ti := range Catalog() {
		if !ti.Available {
			continue
		}
		// Static apps run on the host and have no compose template to render.
		if ti.Type == "static" {
			continue
		}
		d.AppType = ti.Type
		out, err := RenderCompose(ti.Type, d)
		if err != nil {
			t.Fatalf("%s: render error: %v", ti.Type, err)
		}
		for _, want := range []string{
			"demo_site",       // container_name prefix
			"20000",           // host port
			"name: xdev_demo", // external project network
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: rendered compose missing %q\n%s", ti.Type, want, out)
			}
		}
	}
}

// TestProdComposeSelection verifies that a prod environment selects the prod
// compose variant when one exists (laravel), and falls back otherwise.
func TestProdComposeSelection(t *testing.T) {
	d := Data{ProjectSlug: "p", NetworkName: "xdev_p", AppSlug: "api", AppType: "laravel", HostPort: 8000, Env: "prod"}
	out, err := RenderCompose("laravel", d)
	if err != nil {
		t.Fatalf("render laravel prod: %v", err)
	}
	if !strings.Contains(out, "2.0.0-prod") {
		t.Errorf("prod laravel should use the prod image:\n%s", out)
	}

	// A type without a prod variant falls back to the dev template.
	d2 := Data{ProjectSlug: "p", NetworkName: "xdev_p", AppSlug: "blog", AppType: "wordpress", HostPort: 8001, Env: "prod"}
	if _, err := RenderCompose("wordpress", d2); err != nil {
		t.Errorf("wordpress prod should fall back to dev template, got: %v", err)
	}
}

// TestLaravelInfra checks the laravel stack renders its bizepp-style infra
// (Adminer, _volumes bind-mounts, the init.sh entrypoint) and that its infra
// support files are embedded.
func TestLaravelInfra(t *testing.T) {
	d := Data{ProjectSlug: "demo", NetworkName: "xdev_demo", AppSlug: "api",
		AppType: "laravel", HostPort: 20000, AdminerPort: 20001}
	out, err := RenderCompose("laravel", d)
	if err != nil {
		t.Fatalf("render laravel: %v", err)
	}
	for _, want := range []string{
		"demo_api_adminer",                 // adminer service
		"20001:8080",                       // adminer published port
		"ADMINER_DEFAULT_SERVER: db",       // adminer prefilled server
		"../_volumes/mysql:/var/lib/mysql", // bizepp-format db volume
		"../_volumes/redis:/data",          // redis volume
		"./init.sh:/init.sh:ro",            // bootstrap entrypoint mount
		"DB_CONNECTION: mysql",             // not Laravel's sqlite default
	} {
		if !strings.Contains(out, want) {
			t.Errorf("laravel compose missing %q\n%s", want, out)
		}
	}

	infra, err := InfraFiles("laravel")
	if err != nil {
		t.Fatalf("InfraFiles(laravel): %v", err)
	}
	for _, want := range []string{"init.sh", "db/my.cnf", "db/adminer.css"} {
		if _, ok := infra[want]; !ok {
			t.Errorf("laravel infra missing %q", want)
		}
	}
}

// TestRenderWithLimits verifies the deploy/resources block appears only when
// limits are set.
func TestRenderWithLimits(t *testing.T) {
	base := Data{ProjectSlug: "p", NetworkName: "xdev_p", AppSlug: "a", AppType: "wordpress", HostPort: 21000}

	out, _ := RenderCompose("wordpress", base)
	if strings.Contains(out, "deploy:") {
		t.Errorf("expected no deploy block without limits:\n%s", out)
	}

	withLimits := base
	withLimits.CPULimit = 1.5
	withLimits.MemLimit = 512 * 1024 * 1024
	out, _ = RenderCompose("wordpress", withLimits)
	for _, want := range []string{"deploy:", `cpus: "1.5"`, "memory: 512m"} {
		if !strings.Contains(out, want) {
			t.Errorf("limits: missing %q\n%s", want, out)
		}
	}
}
