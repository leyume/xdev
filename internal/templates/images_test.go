package templates

import (
	"strings"
	"testing"
)

// TestLaravelImageSelection covers the choice on both architectures, including
// the ones this test isn't running on — the whole point of the table is hosts
// we can't execute against.
func TestLaravelImageSelection(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		goarch   string
		override string
		want     string
		fallback bool
	}{
		// arm64: the dev tag is published, so a local project gets what it asked
		// for and a prod project falls back.
		{"local on arm64 gets dev", "local", "arm64", "", LaravelImageDev, false},
		{"prod on arm64 falls back to dev", "prod", "arm64", "", LaravelImageDev, true},
		// amd64: the mirror image of the above. This is the case that shipped
		// broken — a local project naming an arm64-only image.
		{"prod on amd64 gets prod", "prod", "amd64", "", LaravelImageProd, false},
		{"local on amd64 falls back to prod", "local", "amd64", "", LaravelImageProd, true},
		// An architecture neither tag is built for: say so rather than pretend.
		{"local on s390x warns", "local", "s390x", "", LaravelImageDev, true},
		// An override is taken at face value on any architecture.
		{"override wins", "local", "amd64", "ghcr.io/me/swoole:x", "ghcr.io/me/swoole:x", false},
		{"override wins in prod", "prod", "s390x", "ghcr.io/me/swoole:x", "ghcr.io/me/swoole:x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := LaravelImage(tc.env, tc.goarch, tc.override)
			if got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
			if tc.fallback && reason == "" {
				t.Error("expected a reason explaining the substitution, got none")
			}
			if !tc.fallback && reason != "" {
				t.Errorf("unexpected fallback reason: %q", reason)
			}
		})
	}
}

// TestLaravelImageAlwaysRunnableHere is the invariant that matters at runtime:
// whatever this host is, neither environment may be handed an image that can't
// exec on it. Getting this wrong doesn't fail at pull time — it crash-loops
// with "exec format error", which reads like a broken entrypoint.
func TestLaravelImageAlwaysRunnableHere(t *testing.T) {
	for _, env := range []string{"local", "prod"} {
		image, _ := ResolveLaravelImage(env)
		if !imageRunsOn(image, goarch) {
			t.Errorf("%s on %s resolves to %s, which isn't published for it", env, goarch, image)
		}
	}
}

// TestRenderUsesResolvedImage checks the wiring: a rendered compose names the
// resolved image, and an explicitly pinned one is passed through untouched.
func TestRenderUsesResolvedImage(t *testing.T) {
	for _, env := range []string{"local", "prod"} {
		d := Data{ProjectSlug: "p", NetworkName: "xdev_p", AppSlug: "api",
			AppType: "laravel", Env: env, HostPort: 20000, AdminerPort: 20001}
		out, err := RenderCompose("laravel", d)
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		want, _ := ResolveLaravelImage(env)
		if !strings.Contains(out, "image: "+want) {
			t.Errorf("%s: compose should name %q\n%s", env, want, out)
		}

		d.AppImage = "ghcr.io/me/pinned:1"
		out, err = RenderCompose("laravel", d)
		if err != nil {
			t.Fatalf("%s pinned: %v", env, err)
		}
		if !strings.Contains(out, "image: ghcr.io/me/pinned:1") {
			t.Errorf("%s: a pinned image must be used verbatim\n%s", env, out)
		}
	}
}

// TestRenderNeverLeavesImageBlank guards the failure mode of templating the
// image: a Data with no AppImage must not render `image:` with nothing after
// it, which compose rejects with a confusing schema error.
func TestRenderNeverLeavesImageBlank(t *testing.T) {
	for _, ti := range Catalog() {
		if !ti.Available || ti.Type == "static" || ti.Type == "go" || ti.Type == "proxy" {
			continue
		}
		out, err := RenderCompose(ti.Type, Data{ProjectSlug: "p", NetworkName: "n",
			AppSlug: "a", AppType: ti.Type, HostPort: 20000})
		if err != nil {
			t.Fatalf("%s: %v", ti.Type, err)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == "image:" || strings.HasSuffix(strings.TrimRight(line, " "), "image: ") {
				t.Errorf("%s: rendered an empty image line: %q", ti.Type, line)
			}
		}
	}
}

// TestNoSingleArchCustomImages: every image a stack names must be able to run
// on both architectures xdev supports. Our own registry namespace has shipped
// single-arch tags before (leyume/mariadb was amd64-only, leyume/swoole's two
// variants are arm64 and amd64 respectively), and the symptom is never an
// obvious pull failure — the container starts and dies with "exec format
// error". The laravel app image is the deliberate exception: it is resolved
// per host by ResolveLaravelImage, covered above.
func TestNoSingleArchCustomImages(t *testing.T) {
	knownSingleArch := []string{
		"leyume/mariadb", // amd64 only; replaced by library/mariadb:11
	}
	for _, ti := range Catalog() {
		if !ti.Available || ti.Type == "static" || ti.Type == "go" || ti.Type == "proxy" {
			continue
		}
		for _, env := range []string{"local", "prod"} {
			out, err := RenderCompose(ti.Type, Data{ProjectSlug: "p", NetworkName: "n",
				AppSlug: "a", AppType: ti.Type, Env: env, HostPort: 20000})
			if err != nil {
				t.Fatalf("%s/%s: %v", ti.Type, env, err)
			}
			for _, bad := range knownSingleArch {
				if strings.Contains(out, bad) {
					t.Errorf("%s/%s names %q, which isn't published for every architecture",
						ti.Type, env, bad)
				}
			}
		}
	}
}

// TestLaravelImageDetected: live platform data overrides the offline table, and
// the point of the whole exercise is the "multi-arch published" row — the day
// both tags go multi-arch, every environment gets the variant it asked for with
// no code change.
func TestLaravelImageDetected(t *testing.T) {
	lookupFrom := func(m map[string][]string) ArchLookup {
		return func(img string) ([]string, bool) {
			arches, ok := m[img]
			return arches, ok
		}
	}
	multiArch := lookupFrom(map[string][]string{
		LaravelImageDev:  {"amd64", "arm64"},
		LaravelImageProd: {"amd64", "arm64"},
	})
	// Detection disagreeing with the stale table: dev has since been rebuilt
	// for amd64, and only the live answer knows.
	devRepublished := lookupFrom(map[string][]string{
		LaravelImageDev: {"amd64"},
	})

	cases := []struct {
		name     string
		env      string
		goarch   string
		lookup   ArchLookup
		want     string
		fallback bool
	}{
		{"multi-arch: local gets dev on amd64", "local", "amd64", multiArch, LaravelImageDev, false},
		{"multi-arch: local gets dev on arm64", "local", "arm64", multiArch, LaravelImageDev, false},
		{"multi-arch: prod gets prod on arm64", "prod", "arm64", multiArch, LaravelImageProd, false},
		{"detection beats the stale table", "local", "amd64", devRepublished, LaravelImageDev, false},
		// Lookup that can't answer (offline, private registry) must degrade to
		// the recorded table rather than to "runs nowhere".
		{"no lookup falls back to table", "local", "amd64", nil, LaravelImageProd, true},
		{"unknown to lookup falls back to table", "prod", "arm64",
			lookupFrom(map[string][]string{}), LaravelImageDev, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := LaravelImageDetected(tc.env, tc.goarch, "", tc.lookup)
			if got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
			if tc.fallback != (reason != "") {
				t.Errorf("fallback = %v, reason = %q", tc.fallback, reason)
			}
		})
	}

	// An override short-circuits detection entirely — no lookup should happen.
	called := false
	got, reason := LaravelImageDetected("local", "amd64", "ghcr.io/me/x:1",
		func(string) ([]string, bool) { called = true; return nil, true })
	if got != "ghcr.io/me/x:1" || reason != "" {
		t.Errorf("override: got %q / %q", got, reason)
	}
	if called {
		t.Error("override must not consult the registry")
	}
}
