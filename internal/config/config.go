// Package config holds the small set of runtime settings xdev needs to start:
// where to store data, where to generate project directories, and which
// address to serve the web UI on. Everything is derived from a single data
// directory so the whole install can be moved or inspected easily.
package config

import (
	"os"
	"path/filepath"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// DataDir holds the sqlite database and any other xdev-owned state.
	DataDir string
	// DBPath is the sqlite file inside DataDir.
	DBPath string
	// ProjectsDir is where per-project / per-app directories are generated.
	// Each app's compose stack lives under projects/<project>/<app>/.
	ProjectsDir string
	// Addr is the host:port the web UI listens on.
	Addr string
}

// EnvFilePath locates the xdev.env the installer wrote — the file a service
// reads its XDEV_* settings from. xdev itself is configured from the
// environment, not this file, so nothing else needs it; the UI shows and edits
// it so the settings behind a running install are visible in one place.
//
// XDEV_ENV_FILE overrides. Otherwise the installer's own locations are checked,
// in the order install.sh picks them. Returns "" when there is no such file
// (a bare binary run, or a container that gets env some other way).
func EnvFilePath() string {
	if p := os.Getenv("XDEV_ENV_FILE"); p != "" {
		return p
	}
	candidates := []string{
		"/etc/xdev/xdev.env",           // linux service install
		"/usr/local/etc/xdev/xdev.env", // macOS root / LaunchDaemon install
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, "Library", "Application Support", "xdev", "xdev.env"))
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// Load builds a Config from explicit values, falling back to sensible
// defaults. Empty arguments mean "use the default". Flag parsing happens in
// main; this keeps the defaulting logic in one place.
func Load(dataDir, projectsDir, addr string) (Config, error) {
	if dataDir == "" {
		dataDir = "./data"
	}
	if projectsDir == "" {
		projectsDir = "./projects"
	}
	if addr == "" {
		addr = "127.0.0.1:7331"
	}

	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return Config{}, err
	}
	projectsDir, err = filepath.Abs(projectsDir)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		DataDir:     dataDir,
		DBPath:      filepath.Join(dataDir, "xdev.db"),
		ProjectsDir: projectsDir,
		Addr:        addr,
	}

	// Ensure the directories exist up front so later code can assume them.
	for _, dir := range []string{cfg.DataDir, cfg.ProjectsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}
