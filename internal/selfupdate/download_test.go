package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// releaseServer serves one release: the asset, and a checksums.txt that
// describes it. checksum is what the manifest will claim, so a test can make it
// disagree with the bytes.
func releaseServer(t *testing.T, body []byte, checksum string, withChecksums bool) *Client {
	t.Helper()
	asset, err := AssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("no published asset for this platform: %v", err)
	}

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			w.Write([]byte(`{"tag_name":"v1.0.0"}`))
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(body)
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			if !withChecksums {
				http.Error(w, "Not Found", http.StatusNotFound)
				return
			}
			w.Write([]byte(checksum + "  " + asset + "\n"))
		default:
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := NewClient("owner/repo")
	c.apiBase, c.downloadBase = srv.URL, srv.URL
	return c
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestDownloadVerifiesTheChecksum(t *testing.T) {
	body := []byte("a convincing binary")
	c := releaseServer(t, body, sha256Hex(body), true)

	rel, err := c.Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	path, err := c.Download(context.Background(), rel, t.TempDir())
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("downloaded %q, want %q", got, body)
	}
}

// The whole point of verifying: a truncated or tampered download is a perfectly
// valid file, and the only thing that catches it is the hash.
func TestDownloadRejectsAMismatch(t *testing.T) {
	c := releaseServer(t, []byte("the wrong bytes"), sha256Hex([]byte("the right bytes")), true)

	rel, _ := c.Resolve(context.Background(), "latest")
	dir := t.TempDir()
	_, err := c.Download(context.Background(), rel, dir)
	if err == nil {
		t.Fatal("a corrupt download was accepted")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("unclear error: %v", err)
	}

	// A rejected download must not be left on disk where a later step could
	// pick it up.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Errorf("corrupt download left behind: %s", e.Name())
	}
}

// A release with no manifest still installs — that is how older releases work —
// but the caller has to be able to tell, so it can say the download was taken
// on trust.
func TestDownloadReportsMissingChecksums(t *testing.T) {
	body := []byte("unverifiable but fine")
	c := releaseServer(t, body, "", false)

	rel, _ := c.Resolve(context.Background(), "latest")
	path, err := c.Download(context.Background(), rel, t.TempDir())
	if !errors.Is(err, ErrNoChecksums) {
		t.Fatalf("err = %v, want ErrNoChecksums", err)
	}
	if path == "" {
		t.Fatal("no path returned; the download should still be usable")
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Errorf("downloaded %q, want %q", got, body)
	}
}

// A platform whose asset is missing from an otherwise healthy release should
// say so, not report a generic HTTP failure.
func TestDownloadExplainsAMissingAsset(t *testing.T) {
	c := releaseServer(t, nil, "", true)
	rel, _ := c.Resolve(context.Background(), "latest")
	rel.AssetURL = strings.TrimSuffix(rel.AssetURL, filepath.Base(rel.AssetURL)) + "xdev-nonesuch"

	_, err := c.Download(context.Background(), rel, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not in this release") {
		t.Errorf("unclear error for a missing asset: %v", err)
	}
}

func TestParseChecksums(t *testing.T) {
	manifest := strings.Join([]string{
		"aaa  xdev-linux-amd64",
		"bbb  *xdev-linux-arm64", // sha256sum binary mode writes a leading *
		"ccc  xdev-darwin-arm64",
		"", "# a comment nobody writes but nothing should choke on",
	}, "\n")

	for _, tc := range []struct{ asset, want string }{
		{"xdev-linux-amd64", "aaa"},
		{"xdev-linux-arm64", "bbb"},
		{"xdev-darwin-arm64", "ccc"},
		{"xdev-windows-amd64", ""},
	} {
		if got := ParseChecksums(manifest, tc.asset); got != tc.want {
			t.Errorf("ParseChecksums(%q) = %q, want %q", tc.asset, got, tc.want)
		}
	}
}
