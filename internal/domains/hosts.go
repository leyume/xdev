// Package domains manages local hostname resolution by maintaining an
// xdev-owned block inside the system hosts file. Each local site gets a
// 127.0.0.1 (and ::1) entry so a browser can resolve e.g. frontend.demo.test.
//
// Writing the real /etc/hosts requires elevated privileges; the path is
// configurable so development can target a writable file and production can be
// pointed at /etc/hosts (edited via sudo).
package domains

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const (
	blockStart = "# >>> xdev (managed) >>>"
	blockEnd   = "# <<< xdev (managed) <<<"
)

// SyncHosts rewrites the managed block at path so it maps each hostname to
// localhost. Everything outside the block is preserved. An empty hostnames list
// removes the block entirely.
func SyncHosts(path string, hostnames []string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	base := strings.TrimRight(stripBlock(string(existing)), "\n")

	var b strings.Builder
	b.WriteString(base)
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	if len(hostnames) > 0 {
		b.WriteString(blockStart + "\n")
		for _, h := range hostnames {
			b.WriteString("127.0.0.1\t" + h + "\n")
			b.WriteString("::1\t" + h + "\n")
		}
		b.WriteString(blockEnd + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// MissingFromHosts returns the hostnames that are not currently mapped in the
// hosts file at path (so the UI only nags about entries that are actually
// missing). Unreadable file => everything is considered missing.
func MissingFromHosts(path string, hostnames []string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return hostnames
	}
	content := string(data)
	var missing []string
	for _, h := range hostnames {
		if !hostPresent(content, h) {
			missing = append(missing, h)
		}
	}
	return missing
}

func hostPresent(content, host string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		for _, f := range fields[1:] { // skip the IP (first field)
			if f == host {
				return true
			}
		}
	}
	return false
}

// SyncHostsElevated writes the hosts file with administrator privileges by
// re-invoking xdev's `write-hosts` subcommand through the OS's GUI elevation
// prompt (osascript on macOS, pkexec on Linux). Used when xdev itself can't
// write the hosts file (i.e. it isn't running as root).
func SyncHostsElevated(path string, hostnames []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	switch runtime.GOOS {
	case "darwin":
		// Build the shell command (single-quoted, safe for our validated
		// hostnames + a normal filesystem path), then escape for AppleScript.
		shellCmd := shQuote(self) + " write-hosts " + shQuote(path)
		for _, h := range hostnames {
			shellCmd += " " + shQuote(h)
		}
		script := `do shell script "` + appleScriptEscape(shellCmd) + `" with administrator privileges`
		if out, err := exec.Command("osascript", "-e", script).CombinedOutput(); err != nil {
			return fmt.Errorf("admin prompt failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		if _, e := exec.LookPath("pkexec"); e == nil {
			args := append([]string{self, "write-hosts", path}, hostnames...)
			if out, err := exec.Command("pkexec", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("pkexec failed: %v: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}
		return fmt.Errorf("no GUI elevation available; run manually: sudo %s write-hosts %s %s",
			self, path, strings.Join(hostnames, " "))
	}
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func appleScriptEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// stripBlock returns content with any existing managed block removed.
func stripBlock(content string) string {
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, ln := range lines {
		switch strings.TrimSpace(ln) {
		case blockStart:
			inBlock = true
			continue
		case blockEnd:
			inBlock = false
			continue
		}
		if !inBlock {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}
