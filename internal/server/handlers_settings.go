package server

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"xdev/internal/config"
)

// envKeyRE is the key shape systemd's EnvironmentFile parser accepts.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// handleEnvSettings shows the xdev.env behind this install, so the settings a
// running service was started with are visible without SSH.
func (s *Server) handleEnvSettings(w http.ResponseWriter, r *http.Request) {
	path := config.EnvFilePath()
	data := viewData{
		"Title":   "Configuration · xdev",
		"Path":    path,
		"Msg":     r.URL.Query().Get("msg"),
		"Err":     r.URL.Query().Get("err"),
		"DataDir": s.cfg.DataDir,
	}
	switch {
	case path == "":
		data["Err"] = "No xdev.env found. It's written by the installer; set XDEV_ENV_FILE if yours lives elsewhere."
	default:
		b, err := os.ReadFile(path)
		if err != nil {
			data["Err"] = "Could not read " + path + ": " + err.Error()
		} else {
			data["Content"] = string(b)
			data["Editable"] = writableFile(path) == nil
		}
	}
	s.render(w, r, "settings_env", data)
}

// handleEnvSettingsSave writes the edited file back, keeping a .bak of what was
// there. xdev reads its configuration at startup, so the change takes effect on
// the next restart — the handler deliberately does not restart the service it is
// running inside.
func (s *Server) handleEnvSettingsSave(w http.ResponseWriter, r *http.Request) {
	path := config.EnvFilePath()
	if path == "" {
		s.redirectEnv(w, r, "", "No xdev.env to write to.")
		return
	}
	content := strings.ReplaceAll(r.FormValue("content"), "\r\n", "\n")
	if err := validateEnvFile(content); err != nil {
		s.redirectEnv(w, r, "", err.Error())
		return
	}
	// Keep the previous mode (0640 from the installer — it can hold an ACME
	// account email and any secrets an operator adds).
	mode := os.FileMode(0o640)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if prev, err := os.ReadFile(path); err == nil {
		if err := os.WriteFile(path+".bak", prev, mode); err != nil {
			s.redirectEnv(w, r, "", "Could not write a backup, so nothing was changed: "+err.Error())
			return
		}
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		s.redirectEnv(w, r, "", "Could not write "+path+": "+err.Error())
		return
	}
	s.store.AddEvent(0, 0, "info", "Edited "+path)
	s.redirectEnv(w, r, "Saved "+path+" (previous version kept as .bak). Restart xdev for it to take effect.", "")
}

// validateEnvFile rejects content the service manager's EnvironmentFile parser
// would choke on. This is the guard that matters: a malformed line makes systemd
// refuse to start xdev, and since this file is edited through the UI that xdev
// itself serves, a bad save would lock the admin out of the only way to fix it.
func validateEnvFile(content string) error {
	if strings.ContainsRune(content, 0) {
		return fmt.Errorf("file contains a NUL byte")
	}
	for i, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		key, _, ok := strings.Cut(t, "=")
		if !ok || !envKeyRE.MatchString(strings.TrimSpace(key)) {
			return fmt.Errorf("line %d is neither KEY=VALUE nor a # comment: %q", i+1, strings.TrimSpace(line))
		}
	}
	return nil
}

func (s *Server) redirectEnv(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	target := "/settings/env"
	switch {
	case errMsg != "":
		target += "?err=" + url.QueryEscape(errMsg)
	case msg != "":
		target += "?msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// writableFile reports whether path can be opened for writing, without changing
// it — so the UI can show the editor read-only rather than failing on save.
func writableFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}
