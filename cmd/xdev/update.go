package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"xdev/internal/selfupdate"
)

// backupsKept is how many previous binaries stay on disk. Three is enough to
// step back through a bad release without the directory becoming a museum.
const backupsKept = 3

// serviceStartGrace is how long a restarted xdev has to report healthy before
// the update is judged a failure. It opens sqlite, runs migrations and pushes
// proxy config first, so this is generous on purpose: a needless rollback is
// worse than a slow success.
const serviceStartGrace = 30 * time.Second

// runUpdate implements `xdev update`: fetch the newest published binary, verify
// it, swap it in, restart the service, and put the old one back if the service
// does not come up.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	var (
		check     = fs.Bool("check", false, "report what's available and exit without changing anything")
		want      = fs.String("version", "latest", "release to install, e.g. v0.2.7")
		repo      = fs.String("repo", envOr("XDEV_REPO", selfupdate.DefaultRepo), "GitHub repository to fetch releases from")
		force     = fs.Bool("force", false, "reinstall even when the installed version already matches")
		noRestart = fs.Bool("no-restart", false, "swap the binary but leave the service alone")
		binPath   = fs.String("bin", "", "binary to replace (default: the running one)")
	)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintln(out, "Usage: xdev update [flags]")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Replace this binary with a build published to GitHub Releases and restart")
		fmt.Fprintln(out, "the service. Config, the sqlite database and your projects are untouched.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// -h is a request, not a failure: flag reports it as an error, and
		// passing that up turns `xdev update -h` into a non-zero exit with a
		// log line after the help text.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	target := *binPath
	if target == "" {
		var err error
		if target, err = selfupdate.TargetPath(); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := selfupdate.NewClient(*repo)
	rel, err := client.Resolve(ctx, *want)
	if err != nil {
		return err
	}

	fmt.Printf("installed: %s\n", version)
	fmt.Printf("available: %s\n", rel.Tag)

	switch selfupdate.Compare(version, rel.Tag) {
	case selfupdate.Same:
		if !*force {
			fmt.Println("already up to date")
			return nil
		}
	case selfupdate.Older:
		// Naming an older release explicitly is a legitimate rollback, so this
		// is a warning rather than a refusal — but it is never what `--version
		// latest` should do silently.
		fmt.Printf("note: %s is older than what's installed — this is a downgrade\n", rel.Tag)
	case selfupdate.Unknown:
		fmt.Printf("note: %q is not a released version, so there is nothing to compare\n", version)
	}

	if *check {
		fmt.Printf("run `xdev update` to install %s\n", rel.Tag)
		return nil
	}

	// Fail on permissions before downloading 20 MB, not after.
	if !selfupdate.Writable(target) {
		return fmt.Errorf("cannot replace %s as the current user — re-run with sudo", target)
	}

	tmp, err := os.MkdirTemp("", "xdev-update-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	fmt.Printf("downloading %s (%s)…\n", rel.Asset, rel.Tag)
	src, err := client.Download(ctx, rel, tmp)
	switch {
	case errors.Is(err, selfupdate.ErrNoChecksums):
		fmt.Println("warning: this release publishes no checksums.txt — the download could not be verified")
	case err != nil:
		return err
	default:
		fmt.Println("checksum verified")
	}

	if selfupdate.SameFile(src, target) && !*force {
		fmt.Println("the published binary is byte-for-byte what's installed — nothing to do")
		return nil
	}

	svc := selfupdate.DetectService(ctx)
	backup, err := selfupdate.Install(src, target)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s → %s\n", rel.Tag, target)
	fmt.Printf("previous binary kept at %s\n", backup)

	if *noRestart || !svc.Managed() {
		if !svc.Managed() {
			fmt.Println("no xdev service is installed here — restart xdev yourself to pick this up")
		}
		prune(target)
		return nil
	}

	// Note the process being replaced before asking for the restart. Without
	// it, a restart that fails outright — polkit declining, most often — leaves
	// the *old* process running and every liveness check passes, so the update
	// reports success while the machine goes on serving the old binary.
	prevPID := svc.MainPID(ctx)

	fmt.Printf("restarting %s…\n", svc)
	restartErr := svc.Restart(ctx)
	if restartErr != nil {
		fmt.Printf("restart reported an error: %v\n", restartErr)
	}
	if svc.WaitRestarted(ctx, serviceStartGrace, prevPID) {
		fmt.Printf("xdev is up on %s\n", rel.Tag)
		prune(target)
		return nil
	}

	// A restart that never ran is not a bad binary, so rolling back would be
	// undoing a perfectly good update to work around a permissions problem. Say
	// what happened and leave the new binary in place for the user to start.
	if restartErr != nil && svc.Active(ctx) {
		fmt.Printf("%s is still running the previous process — the new binary is installed but not yet serving\n", svc)
		prune(target)
		return fmt.Errorf("could not restart %s: %w (run `sudo systemctl restart xdev`, or re-run this as root)", svc, restartErr)
	}

	// The new binary does not serve. Put back the one that did — an update is
	// not worth an outage, and a machine left down is much worse than a machine
	// left on an old version.
	fmt.Printf("the service did not come back within %s — rolling back\n", serviceStartGrace)
	if rerr := selfupdate.Restore(backup, target); rerr != nil {
		return fmt.Errorf("rollback failed: %w (the previous binary is at %s)", rerr, backup)
	}
	rollbackPID := svc.MainPID(ctx)
	if rerr := svc.Restart(ctx); rerr != nil {
		fmt.Printf("restart after rollback reported an error: %v\n", rerr)
	}
	if svc.WaitRestarted(ctx, serviceStartGrace, rollbackPID) {
		fmt.Println("rolled back; xdev is up on the previous version")
	} else {
		fmt.Println("rolled back, but the service is still not up — inspect the logs")
	}
	if logs := svc.Logs(ctx, 40); logs != "" {
		fmt.Fprintln(os.Stderr, logs)
	}
	return fmt.Errorf("update to %s failed and was rolled back", rel.Tag)
}

// prune trims old backups, reporting what went. Failures are not worth an error
// exit: the update itself already succeeded.
func prune(target string) {
	for _, old := range selfupdate.PruneBackups(target, backupsKept) {
		fmt.Printf("pruned %s\n", filepath.Base(old))
	}
}
