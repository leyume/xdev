package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"xdev/internal/auth"
	"xdev/internal/config"
	"xdev/internal/store"
)

// runCreateAdmin creates the first (admin) account. It resolves the data dir
// the same way the server does, opens the store (running migrations), and reads
// the password from $XDEV_ADMIN_PASSWORD or, failing that, prompts twice on the
// TTY with no echo.
//
// It is idempotent by default: if an admin already exists it prints a friendly
// message and succeeds (exit 0), so installers can re-run it safely. Pass
// --fail-if-exists to make an existing admin an error instead.
func runCreateAdmin(args []string) error {
	fs := flag.NewFlagSet("xdev create-admin", flag.ContinueOnError)
	dataDir := fs.String("data", envOr("XDEV_DATA", ""), "data directory (sqlite db + state)")
	failIfExists := fs.Bool("fail-if-exists", false, "exit non-zero if an admin already exists")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: xdev create-admin <email> [-data dir] [--fail-if-exists]")
		fmt.Fprintln(fs.Output(), "  password is read from $XDEV_ADMIN_PASSWORD, else prompted twice (hidden).")
	}
	// Accept the email either before or after the flags. Go's flag package stops
	// at the first non-flag argument, so if the email leads (the documented
	// form, `create-admin you@example.com -data …`), pull it off before parsing
	// the rest as flags.
	email := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		email, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if email == "" {
		email = fs.Arg(0)
	}
	if email == "" {
		fs.Usage()
		return fmt.Errorf("create-admin: an email argument is required")
	}

	// Only the data dir matters here; projects dir / addr default harmlessly.
	cfg, err := config.Load(*dataDir, "", "")
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	authsvc := auth.New(st, false)

	// Idempotency: if an admin is already present, no-op (or fail if asked).
	if need, err := authsvc.NeedsSetup(); err != nil {
		return err
	} else if !need {
		if *failIfExists {
			return fmt.Errorf("an admin account already exists")
		}
		fmt.Println("admin account already exists — nothing to do")
		return nil
	}

	password, err := readAdminPassword()
	if err != nil {
		return err
	}

	if _, err := authsvc.CreateAdmin(email, password); err != nil {
		return err
	}
	fmt.Printf("created admin %s\n", strings.TrimSpace(strings.ToLower(email)))
	return nil
}

// readAdminPassword returns the admin password: from $XDEV_ADMIN_PASSWORD if
// set (non-interactive installs), otherwise prompted twice on the TTY with no
// echo. Enforces the same 8-char minimum as auth.CreateAdmin so we fail before
// touching the store.
func readAdminPassword() (string, error) {
	if pw := os.Getenv("XDEV_ADMIN_PASSWORD"); pw != "" {
		if len(pw) < 8 {
			return "", errors.New("XDEV_ADMIN_PASSWORD must be at least 8 characters")
		}
		return pw, nil
	}

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no TTY for password prompt; set XDEV_ADMIN_PASSWORD instead")
	}

	fmt.Print("Admin password (min 8 chars): ")
	first, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", err
	}
	if len(first) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	fmt.Print("Confirm password: ")
	second, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	return string(first), nil
}
