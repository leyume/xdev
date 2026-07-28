package proxy

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// caddyDataDir mirrors Caddy's own default storage location so xdev can find the
// local CA files. Like Caddy, it honors XDG_DATA_HOME first.
func caddyDataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "caddy")
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Caddy")
	}
	return filepath.Join(home, ".local", "share", "caddy")
}

// LocalCARoot returns the path to the local CA's root certificate and whether it
// exists yet. Caddy mints the CA lazily — on the first internal-CA issuance — so
// a fresh install has no root until the first .test/.localhost site is served.
// That's why trusting the root can't happen at install time.
func LocalCARoot(caName string) (string, bool) {
	path := filepath.Join(caddyDataDir(), "pki", "authorities", caName, "root.crt")
	_, err := os.Stat(path)
	return path, err == nil
}

// LocalCATrusted reports whether the root at path is already accepted by the
// system trust store, by verifying it against the platform root pool (nil Roots
// means "use the system pool"). Browsers with their own store — Firefox
// everywhere, Chrome on Linux — are not covered by this and need a separate
// import.
func LocalCATrusted(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	// A CA root carries no serving EKU, so accept any.
	_, err = cert.Verify(x509.VerifyOptions{KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err == nil
}

// TrustCommand returns the command that installs root into this OS's system
// trust store. The engine and whether Caddy is a container or a binary make no
// difference here — the root always lands at the same host path — but the trust
// store itself is OS-specific.
func TrustCommand(root string) string {
	if runtime.GOOS == "darwin" {
		return "sudo security add-trusted-cert -d -r trustRoot " +
			"-k /Library/Keychains/System.keychain " + strconv.Quote(root)
	}
	return "sudo cp " + strconv.Quote(root) + " /usr/local/share/ca-certificates/xdev-local-ca.crt" +
		" && sudo update-ca-certificates"
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
