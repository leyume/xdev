package templates

import (
	"reflect"
	"testing"
)

// The whole point of the fallback chain: adding an fpm template must not
// require adding a prod variant, and an app with no server set must resolve
// exactly what it resolved before the option existed.
func TestComposeCandidates(t *testing.T) {
	cases := []struct {
		name, appType, env, server string
		want                       []string
	}{{
		name:    "unset server is unchanged from before the option existed",
		appType: "laravel", env: "local", server: "",
		want: []string{"files/laravel/compose.yml.tmpl"},
	}, {
		name:    "swoole is spelled out but means the same as unset",
		appType: "laravel", env: "local", server: "swoole",
		want: []string{"files/laravel/compose.yml.tmpl"},
	}, {
		name:    "prod without a server keeps the existing two-step fallback",
		appType: "laravel", env: "prod", server: "",
		want: []string{"files/laravel/compose.prod.yml.tmpl", "files/laravel/compose.yml.tmpl"},
	}, {
		name:    "fpm prefers its own template over the swoole prod one",
		appType: "laravel", env: "prod", server: "fpm",
		want: []string{
			"files/laravel/compose.fpm.prod.yml.tmpl",
			"files/laravel/compose.fpm.yml.tmpl",
			"files/laravel/compose.prod.yml.tmpl",
			"files/laravel/compose.yml.tmpl",
		},
	}, {
		name:    "fpm in a local project skips the prod names entirely",
		appType: "laravel", env: "local", server: "fpm",
		want: []string{"files/laravel/compose.fpm.yml.tmpl", "files/laravel/compose.yml.tmpl"},
	}, {
		name:    "another type ignores server",
		appType: "wordpress", env: "local", server: "fpm",
		want: []string{"files/wordpress/compose.fpm.yml.tmpl", "files/wordpress/compose.yml.tmpl"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composeCandidates(tc.appType, tc.env, tc.server)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %v\nwant %v", got, tc.want)
			}
		})
	}
}

// Every existing app type must still render, which is the regression this
// refactor could plausibly cause.
func TestEveryTypeStillRenders(t *testing.T) {
	for _, ti := range Catalog() {
		switch ti.Type {
		case "static", "go", "proxy":
			continue // host apps have no compose template
		}
		if _, err := RenderCompose(ti.Type, Data{
			ProjectSlug: "demo", AppSlug: "app", NetworkName: "demo_net",
			AppType: ti.Type, Env: "local", HostPort: 20000,
		}); err != nil {
			t.Errorf("render %s: %v", ti.Type, err)
		}
	}
}
