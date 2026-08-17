package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoChecksums reports that a release published no checksums.txt. Callers
// decide whether that is fatal; it is not by default, since releases predating
// the manifest are still installable.
var ErrNoChecksums = errors.New("release has no checksums.txt")

// Download fetches the release binary into dir and returns its path.
//
// The file is verified against the release's checksums.txt before it is
// returned, so a caller that gets a path back is holding bytes GitHub vouched
// for. A truncated download — the common failure on a flaky link, and one that
// produces a perfectly valid file — is caught here rather than by the service
// failing to start after the swap.
func (c *Client) Download(ctx context.Context, rel Release, dir string) (string, error) {
	dest := filepath.Join(dir, rel.Asset)
	sum, err := c.fetchTo(ctx, rel.AssetURL, dest)
	if err != nil {
		return "", err
	}

	want, err := c.checksum(ctx, rel)
	switch {
	case errors.Is(err, ErrNoChecksums):
		// Nothing to check against. Not fatal, but the user should know the
		// download was taken on trust.
		return dest, ErrNoChecksums
	case err != nil:
		return "", err
	case !strings.EqualFold(want, sum):
		os.Remove(dest)
		return "", fmt.Errorf("checksum mismatch for %s: expected %s, got %s — the download is corrupt or the release was altered",
			rel.Asset, want, sum)
	}
	return dest, nil
}

// fetchTo streams a URL to a file and returns the sha256 of what was written.
// Hashing during the copy means the bytes are never read twice and never held
// in memory.
func (c *Client) fetchTo(ctx context.Context, url, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "xdev-selfupdate")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%s is not in this release — it may not publish a binary for this platform", filepath.Base(url))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}

	// 0600 while in flight: the file is not executable until it has been
	// verified, so a mid-download crash cannot leave something runnable behind.
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, cerr := io.Copy(io.MultiWriter(f, h), resp.Body)
	closeErr := f.Close()
	if cerr != nil {
		os.Remove(dest)
		return "", fmt.Errorf("download %s: %w", url, cerr)
	}
	if closeErr != nil {
		os.Remove(dest)
		return "", closeErr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checksum returns the expected sha256 of this release's asset.
func (c *Client) checksum(ctx context.Context, rel Release) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.ChecksumURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "xdev-selfupdate")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNoChecksums
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch checksums: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("fetch checksums: %w", err)
	}
	sum := ParseChecksums(string(body), rel.Asset)
	if sum == "" {
		return "", ErrNoChecksums
	}
	return sum, nil
}

// ParseChecksums pulls one asset's hash out of sha256sum output. The leading
// "*" that sha256sum writes for binary mode is tolerated, matching how
// install.sh reads the same file.
func ParseChecksums(body, asset string) string {
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset {
			return fields[0]
		}
	}
	return ""
}
