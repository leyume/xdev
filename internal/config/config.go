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
