// Package selfupdate replaces the running xdev binary with one published to
// GitHub Releases, then restarts the service and checks it came back.
//
// It is the engine behind `xdev update`. The same job can be done by
// deploy/update.sh (from a checkout) or deploy/upgrade.sh (piped from curl);
// this exists so a server that has only the binary — no repo, no Go, no
// installer on disk — can still move forward, which is every server installed
// the way the README describes.
//
// The design goal is that a failed update is not an outage. Nothing is
// destructive until a verified binary is on local disk, the old binary is kept,
// the swap is a rename, and a service that does not come back is rolled back
// automatically.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo is where releases are published. Overridable so a fork, or a test,
// can point somewhere else without patching code.
const DefaultRepo = "leyume/xdev"

const (
	defaultAPIBase      = "https://api.github.com"
	defaultDownloadBase = "https://github.com"
)

// checksumsAsset is the manifest release.yml uploads beside the binaries. Its
// absence is not fatal — a release predating it, or a hand-made one, still
// installs — but a mismatch is.
const checksumsAsset = "checksums.txt"

// Release is one published version and where to fetch this host's binary from.
type Release struct {
	Tag         string
	Asset       string
	AssetURL    string
	ChecksumURL string
}

// Client fetches release metadata and assets. The zero value is not usable;
// call NewClient.
type Client struct {
	HTTP *http.Client
	Repo string

	// apiBase and downloadBase are split out so tests can serve both from an
	// httptest server. Production code never sets them.
	apiBase      string
	downloadBase string
}

// NewClient returns a client with timeouts suited to downloading a ~20 MB
// binary over a slow link.
func NewClient(repo string) *Client {
	if repo == "" {
		repo = DefaultRepo
	}
	return &Client{
		HTTP:         &http.Client{Timeout: 5 * time.Minute},
		Repo:         repo,
		apiBase:      defaultAPIBase,
		downloadBase: defaultDownloadBase,
	}
}

// AssetName is the release asset for a platform, matching what release.yml
// cross-compiles and what install.sh downloads. Those three must agree; a
// mismatch shows up as a 404 on an otherwise healthy release.
//
// An unsupported platform is an error rather than a guess, because the guess
// would download something that fails at exec with "exec format error" — a
// message that looks nothing like the actual problem.
func AssetName(goos, goarch string) (string, error) {
	switch goos {
	case "linux", "darwin":
	default:
		return "", fmt.Errorf("no xdev binaries are published for %s — build from source", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("no xdev binaries are published for %s/%s — build from source", goos, goarch)
	}
	return fmt.Sprintf("xdev-%s-%s", goos, goarch), nil
}

// Resolve returns the release to install. tag is a version like "v0.2.7", or
// "latest"/"" to ask GitHub what the newest one is.
func (c *Client) Resolve(ctx context.Context, tag string) (Release, error) {
	asset, err := AssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return Release{}, err
	}
	if tag == "" || tag == "latest" {
		if tag, err = c.latestTag(ctx); err != nil {
			return Release{}, err
		}
	}
	base := fmt.Sprintf("%s/%s/releases/download/%s", c.downloadBase, c.Repo, tag)
	return Release{
		Tag:         tag,
		Asset:       asset,
		AssetURL:    base + "/" + asset,
		ChecksumURL: base + "/" + checksumsAsset,
	}, nil
}

// latestTag asks the API rather than following the /releases/latest/download
// redirect, because the tag itself is what makes "already up to date" and
// "this would be a downgrade" answerable. Without it the only way to know what
// you fetched is to run it.
func (c *Client) latestTag(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", c.apiBase, c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "xdev-selfupdate")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask GitHub for the latest release: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("%s has no published releases", c.Repo)
	case http.StatusForbidden, http.StatusTooManyRequests:
		// The unauthenticated API allows 60 requests an hour per IP. Say so,
		// because the alternative reading — "my server is blocked" — sends
		// people debugging the wrong thing.
		return "", fmt.Errorf("GitHub API rate limit reached (%s); retry later, or name a version explicitly", resp.Status)
	default:
		return "", fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var body struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("parse the GitHub release response: %w", err)
	}
	if body.Tag == "" {
		return "", fmt.Errorf("the latest release of %s has no tag name", c.Repo)
	}
	return body.Tag, nil
}

// Comparison of the installed version against a release.
type Comparison int

const (
	// Unknown means the two can't be ordered — a dev build, a dirty tree, or a
	// tag this doesn't know how to parse.
	Unknown Comparison = iota
	Same
	Newer // the release is newer than what's installed: the normal update
	Older // the release is older: installing it would be a downgrade
)

// Compare orders an installed version against a release tag.
//
// `xdev version` prints whatever -ldflags stamped in, which for a release is
// "v0.2.7" but for a local build is `git describe` output like
// "v0.2.6-14-g2dbfafc-dirty", or plain "dev". Anything that isn't a clean
// vX.Y.Z is reported Unknown rather than forced into an order, since a
// development build is not meaningfully "behind" or "ahead" of a release.
func Compare(installed, release string) Comparison {
	a, aok := parseVersion(installed)
	b, bok := parseVersion(release)
	if !aok || !bok {
		if installed == release && installed != "" {
			return Same
		}
		return Unknown
	}
	for i := range a {
		switch {
		case a[i] < b[i]:
			return Newer
		case a[i] > b[i]:
			return Older
		}
	}
	return Same
}

// parseVersion reads a clean vMAJOR.MINOR.PATCH. Any suffix at all — a
// pre-release, a `git describe` commit count, "-dirty" — makes it unparseable
// on purpose: those builds are not points on the release line.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimSpace(v)
	// `xdev version` prints a banner; accept the bare tag or the first field.
	if i := strings.IndexByte(v, ' '); i >= 0 {
		v = v[:i]
	}
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
