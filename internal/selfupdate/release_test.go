package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// newTestClient points a client at a stub standing in for both api.github.com
// and github.com.
func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := NewClient("owner/repo")
	c.apiBase = srv.URL
	c.downloadBase = srv.URL
	return c, srv
}

// The asset name has to match what release.yml builds and install.sh fetches.
// Getting it wrong is a 404 on a release that is actually fine.
func TestAssetNameMatchesPublishedArtifacts(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"linux", "amd64", "xdev-linux-amd64"},
		{"linux", "arm64", "xdev-linux-arm64"},
		{"darwin", "amd64", "xdev-darwin-amd64"},
		{"darwin", "arm64", "xdev-darwin-arm64"},
	} {
		got, err := AssetName(tc.goos, tc.goarch)
		if err != nil {
			t.Errorf("AssetName(%s,%s): %v", tc.goos, tc.goarch, err)
		}
		if got != tc.want {
			t.Errorf("AssetName(%s,%s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// An unpublished platform must be an error, not a URL that 404s later or a
// binary that dies with "exec format error".
func TestAssetNameRejectsUnpublishedPlatforms(t *testing.T) {
	for _, tc := range []struct{ goos, goarch string }{
		{"windows", "amd64"},
		{"linux", "386"},
		{"freebsd", "arm64"},
	} {
		if _, err := AssetName(tc.goos, tc.goarch); err == nil {
			t.Errorf("AssetName(%s,%s) returned no error", tc.goos, tc.goarch)
		}
	}
}

func TestResolveLatestReadsTheAPI(t *testing.T) {
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/releases/latest" {
			t.Errorf("unexpected request path %q", r.URL.Path)
		}
		w.Write([]byte(`{"tag_name":"v9.9.9","name":"v9.9.9"}`))
	}))

	rel, err := c.Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rel.Tag != "v9.9.9" {
		t.Errorf("tag = %q, want v9.9.9", rel.Tag)
	}
	asset, _ := AssetName(runtime.GOOS, runtime.GOARCH)
	if want := srv.URL + "/owner/repo/releases/download/v9.9.9/" + asset; rel.AssetURL != want {
		t.Errorf("asset URL = %q, want %q", rel.AssetURL, want)
	}
	if !strings.HasSuffix(rel.ChecksumURL, "/checksums.txt") {
		t.Errorf("checksum URL = %q", rel.ChecksumURL)
	}
}

// A pinned version must not consult the API at all — that is what makes
// `--version` a working escape hatch when the API is rate limited.
func TestResolvePinnedSkipsTheAPI(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("pinned resolve called the API: %s", r.URL.Path)
	}))

	rel, err := c.Resolve(context.Background(), "v1.2.3")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rel.Tag != "v1.2.3" {
		t.Errorf("tag = %q, want v1.2.3", rel.Tag)
	}
}

// Rate limiting is the failure people will actually hit, and it must not read
// as "the network is down" or "there are no releases".
func TestResolveExplainsRateLimiting(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limit exceeded", http.StatusForbidden)
	}))

	_, err := c.Resolve(context.Background(), "latest")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("error does not mention the rate limit: %v", err)
	}
}

func TestResolveReportsNoReleases(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	}))

	_, err := c.Resolve(context.Background(), "latest")
	if err == nil || !strings.Contains(err.Error(), "no published releases") {
		t.Errorf("unhelpful error for a repo with no releases: %v", err)
	}
}

func TestCompareOrdersReleases(t *testing.T) {
	for _, tc := range []struct {
		installed, release string
		want               Comparison
	}{
		{"v0.2.6", "v0.2.7", Newer},
		{"v0.2.6", "v0.3.0", Newer},
		{"v0.2.6", "v1.0.0", Newer},
		{"v0.2.7", "v0.2.7", Same},
		{"0.2.7", "v0.2.7", Same}, // the v is cosmetic
		{"v0.2.7", "v0.2.6", Older},
		{"v0.10.0", "v0.9.0", Older}, // numeric, not lexical
		{"v0.9.0", "v0.10.0", Newer},

		// Development builds are not points on the release line.
		{"dev", "v0.2.7", Unknown},
		{"v0.2.6-14-g2dbfafc", "v0.2.7", Unknown},
		{"v0.2.6-dirty", "v0.2.6", Unknown},
	} {
		if got := Compare(tc.installed, tc.release); got != tc.want {
			t.Errorf("Compare(%q, %q) = %v, want %v", tc.installed, tc.release, got, tc.want)
		}
	}
}

// `xdev version` prints a banner, not a bare tag. Compare has to cope, or every
// caller has to remember to split the string first.
func TestCompareAcceptsTheVersionBanner(t *testing.T) {
	if got := Compare("v0.2.6 (linux/amd64, go1.26)", "v0.2.7"); got != Newer {
		t.Errorf("Compare on a version banner = %v, want Newer", got)
	}
}
