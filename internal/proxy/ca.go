package proxy

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// caddyDataDir mirrors Caddy's own default storage location so xdev can find the
// local CA files. Like Caddy, it honors XDG_DATA_HOME first.
func caddyDataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "caddy")
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Caddy")
	case "windows":
		if ad := os.Getenv("AppData"); ad != "" {
			return filepath.Join(ad, "Caddy")
		}
	}
	return filepath.Join(home, ".local", "share", "caddy")
}

// IntermediateRemaining reports how long the local CA's intermediate is still
// valid, and whether one exists yet.
func IntermediateRemaining(caName string) (time.Duration, bool) {
	path := filepath.Join(caddyDataDir(), "pki", "authorities", caName, "intermediate.crt")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return 0, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0, false
	}
	return time.Until(cert.NotAfter), true
}

// RefreshStaleIntermediate removes the local CA's intermediate (keeping the
// root) when it has less than minRemaining validity left, so the next issuance
// mints a fresh, longer-lived one. Because only the intermediate is removed, the
// trusted root is preserved and no re-trust is needed. Returns the prior
// remaining lifetime and whether it regenerated. Call before Caddy starts.
func RefreshStaleIntermediate(caName string, minRemaining time.Duration) (time.Duration, bool) {
	remaining, ok := IntermediateRemaining(caName)
	if !ok || remaining >= minRemaining {
		return remaining, false
	}
	base := filepath.Join(caddyDataDir(), "pki", "authorities", caName)
	os.Remove(filepath.Join(base, "intermediate.crt"))
	os.Remove(filepath.Join(base, "intermediate.key"))
	return remaining, true
}
