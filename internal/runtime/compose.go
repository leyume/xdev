package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Logs returns the last `tail` lines of the stack's combined container logs.
func Logs(ctx context.Context, engine Engine, workdir, project, file string, tail int) (string, error) {
	return Compose(ctx, engine, workdir, project, file,
		"logs", "--no-color", "--tail", strconv.Itoa(tail))
}

// Compose runs `<engine> compose -p <project> -f <file> <args...>` with its
// working directory set to workdir (so relative bind-mount paths in the compose
// file resolve correctly). It returns combined stdout+stderr either way; on
// failure the output is wrapped into the error for surfacing in the UI.
func Compose(ctx context.Context, engine Engine, workdir, project, file string, args ...string) (string, error) {
	full := append([]string{"compose", "-p", project, "-f", file}, args...)
	cmd := exec.CommandContext(ctx, string(engine), full...)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s compose %s failed: %w\n%s",
			engine, strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}

// Up brings the stack up in the background (detached).
func Up(ctx context.Context, engine Engine, workdir, project, file string) (string, error) {
	return Compose(ctx, engine, workdir, project, file, "up", "-d")
}

// Down stops and removes the stack's containers and default network.
func Down(ctx context.Context, engine Engine, workdir, project, file string) (string, error) {
	return Compose(ctx, engine, workdir, project, file, "down")
}

// Start starts existing (stopped) containers without recreating them.
func Start(ctx context.Context, engine Engine, workdir, project, file string) (string, error) {
	return Compose(ctx, engine, workdir, project, file, "start")
}

// Stop stops containers but leaves them around for a quick start.
func Stop(ctx context.Context, engine Engine, workdir, project, file string) (string, error) {
	return Compose(ctx, engine, workdir, project, file, "stop")
}

// Running reports whether the stack has at least one running container. Used to
// reconcile the stored status with reality.
func Running(ctx context.Context, engine Engine, workdir, project, file string) bool {
	out, err := Compose(ctx, engine, workdir, project, file, "ps", "--status=running", "--quiet")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// NetworkCreate creates a named network if it doesn't already exist. Both
// podman and docker error with an "already exists" message we treat as success,
// keeping the call idempotent across engines.
func NetworkCreate(ctx context.Context, engine Engine, name string) error {
	out, err := exec.CommandContext(ctx, string(engine), "network", "create", name).CombinedOutput()
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(string(out)), "already") {
		return nil
	}
	return fmt.Errorf("%s network create %s: %w\n%s", engine, name, err, string(out))
}

// NetworkRemove deletes a network, ignoring "not found".
func NetworkRemove(ctx context.Context, engine Engine, name string) error {
	out, err := exec.CommandContext(ctx, string(engine), "network", "rm", name).CombinedOutput()
	if err == nil {
		return nil
	}
	low := strings.ToLower(string(out))
	if strings.Contains(low, "not found") || strings.Contains(low, "no such") {
		return nil
	}
	return fmt.Errorf("%s network rm %s: %w\n%s", engine, name, err, string(out))
}
