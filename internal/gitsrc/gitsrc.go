// Package gitsrc gives an app a git repository as its source of truth: clone it
// once, then deploy by fetching and resetting to the tracked branch. It wraps
// the `git` CLI rather than a Go implementation — git is already on every host
// that can build a Node app, and the CLI is the thing whose behavior people can
// reproduce by hand when a deploy goes wrong.
//
// Credentials never touch disk in a form that outlives the command. A private
// repository is read over SSH with a deploy key written to a 0600 file in a
// per-call temp directory that is removed before the function returns; the key
// is passed via GIT_SSH_COMMAND, so it appears in no config file, no remote
// URL, and no `git remote -v` output. Nothing is written into .git/config that
// a later `git push` could leak.
package gitsrc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Timeouts are the caller's to set via context; these are the defaults the app
// service uses. A clone of a large repo on a slow link is the long pole.
const (
	// DefaultHost is assumed when a repo is given as bare "owner/name".
	DefaultHost = "github.com"
)

// githubHostKey is GitHub's SSH host key, pinned so the very first clone is
// verified rather than trusted blindly. Fingerprint SHA256:+DiY3wvvV6TuJJhbpZis
// F/zLDA0zPMSvHdkr4UvCOqU, as published at
// https://docs.github.com/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
//
// Other hosts (GitLab, Bitbucket, a self-hosted server) are accepted on first
// use and pinned from then on — see knownHostsFile.
const githubHostKey = "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"

// Repo is a parsed repository reference. The same repository can be written
// several ways; this is the one shape the rest of xdev stores and compares.
type Repo struct {
	Host  string // github.com
	Owner string // leyume
	Name  string // xdev (no .git suffix)
}

// String renders the canonical form xdev stores: https://host/owner/name.
// The transport is chosen per-operation from whether a key is supplied, so the
// stored URL does not have to change when a repo goes from public to private.
func (r Repo) String() string { return "https://" + r.Host + "/" + r.Owner + "/" + r.Name }

// HTTPS is the anonymous clone URL, used when there is no deploy key.
func (r Repo) HTTPS() string { return r.String() + ".git" }

// SSH is the clone URL used with a deploy key.
func (r Repo) SSH() string { return "git@" + r.Host + ":" + r.Owner + "/" + r.Name + ".git" }

// WebURL links to the repository in a browser.
func (r Repo) WebURL() string { return r.String() }

// KeysURL is where a deploy key is added for this repository — the page the UI
// sends people to with the public key in hand.
func (r Repo) KeysURL() string { return r.String() + "/settings/keys" }

// ParseRepo accepts the forms people actually paste — the browser URL, the SSH
// remote, the "owner/name" shorthand — and returns one canonical Repo.
//
// It deliberately does not accept a URL carrying credentials
// (https://token@github.com/...): a secret in a field that is displayed back,
// logged, and stored in the clear is exactly the mistake this package's design
// is trying to prevent. The deploy key is where auth belongs.
func ParseRepo(raw string) (Repo, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Repo{}, errors.New("repository is required")
	}
	if strings.ContainsAny(s, " \t\n\r") {
		return Repo{}, fmt.Errorf("invalid repository %q — it must not contain spaces", raw)
	}

	// git@host:owner/name.git → host/owner/name
	if rest, ok := strings.CutPrefix(s, "git@"); ok {
		host, p, found := strings.Cut(rest, ":")
		if !found {
			return Repo{}, fmt.Errorf("invalid repository %q — expected git@host:owner/name", raw)
		}
		s = host + "/" + p
	} else {
		for _, scheme := range []string{"https://", "http://", "ssh://"} {
			if rest, ok := strings.CutPrefix(s, scheme); ok {
				s = rest
				break
			}
		}
		// ssh://git@host/owner/name — drop the user, it is always git.
		if _, rest, found := strings.Cut(s, "@"); found {
			if strings.HasPrefix(s, "git@") {
				s = rest
			} else {
				return Repo{}, fmt.Errorf("invalid repository %q — remove the credentials from the URL and use a deploy key instead", raw)
			}
		}
	}
	s = strings.Trim(s, "/")
	s = strings.TrimSuffix(s, ".git")
	// Strip anything a browser appends past the repo: /tree/main, ?tab=…, #x.
	s, _, _ = strings.Cut(s, "?")
	s, _, _ = strings.Cut(s, "#")

	parts := strings.Split(s, "/")
	// "owner/name" with no host means the default (GitHub).
	if len(parts) == 2 && !strings.Contains(parts[0], ".") {
		parts = []string{DefaultHost, parts[0], parts[1]}
	}
	if len(parts) < 3 {
		return Repo{}, fmt.Errorf("invalid repository %q — use owner/name or https://github.com/owner/name", raw)
	}
	r := Repo{Host: strings.ToLower(parts[0]), Owner: parts[1], Name: parts[2]}
	for _, f := range []string{r.Host, r.Owner, r.Name} {
		if f == "" {
			return Repo{}, fmt.Errorf("invalid repository %q — use owner/name or https://github.com/owner/name", raw)
		}
	}
	return r, nil
}

// Source is everything a clone or a deploy needs: which repo, which branch, and
// (for a private one) the PEM-encoded OpenSSH private key to read it with.
type Source struct {
	Repo Repo
	Ref  string // branch or tag; "" = the remote's default branch
	Key  string // OpenSSH private key, decrypted; "" = public repo over HTTPS
}

// URL is the clone URL for this source: SSH when there is a key, HTTPS when not.
func (s Source) URL() string {
	if s.Key != "" {
		return s.Repo.SSH()
	}
	return s.Repo.HTTPS()
}

// Clone makes dir a checkout of the source. dir must not already be a
// repository; the caller decides whether to Update instead.
//
// The clone is shallow (--depth 1). Deploys only ever build the tip of a
// branch, and a year of history is download time and disk for nothing; Update
// keeps it shallow the same way.
func Clone(ctx context.Context, dir string, src Source) error {
	return cloneFrom(ctx, dir, src.URL(), src.Ref, src.Key)
}

// cloneFrom is Clone with the remote spelled out, so the git invocation can be
// exercised against a local repository in tests without ParseRepo having to
// accept paths it should not accept from a user.
func cloneFrom(ctx context.Context, dir, url, ref, key string) error {
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, url, dir)
	_, err := run(ctx, key, "", args...)
	return err
}

// Update fetches the tracked branch and resets the working tree to it, then
// returns the commit now checked out. Local edits under the app directory are
// discarded — the repository is the source of truth for a git-backed app, which
// is why xdev refuses to point one at a directory the user owns.
//
// Untracked files are left alone, so node_modules and a previous build survive
// to make the next build faster.
func Update(ctx context.Context, dir string, src Source) (string, error) {
	return updateFrom(ctx, dir, src.URL(), src.Ref, src.Key)
}

// updateFrom is Update with the remote spelled out — see cloneFrom.
func updateFrom(ctx context.Context, dir, url, ref, key string) (string, error) {
	// The remote may have been re-pointed on the settings page since the clone.
	if _, err := run(ctx, key, dir, "remote", "set-url", "origin", url); err != nil {
		return "", err
	}
	if ref == "" {
		var err error
		if ref, err = defaultBranch(ctx, key, dir, url); err != nil {
			return "", err
		}
	}
	if _, err := run(ctx, key, dir, "fetch", "--depth", "1", "origin", ref); err != nil {
		return "", err
	}
	// --force because the working tree is expected to differ: a previous build
	// wrote into it, or somebody edited a file on the server. Without it git
	// refuses the checkout to protect changes a deploy is meant to discard.
	if _, err := run(ctx, key, dir, "checkout", "--force", "-B", ref, "FETCH_HEAD"); err != nil {
		return "", err
	}
	if _, err := run(ctx, key, dir, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return "", err
	}
	return HeadSHA(ctx, dir)
}

// HeadSHA is the commit checked out in dir.
func HeadSHA(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, "", dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// HeadRef is the branch checked out in dir ("" if detached).
func HeadRef(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, "", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out = strings.TrimSpace(out)
	if out == "HEAD" {
		return "", err
	}
	return out, err
}

// IsRepo reports whether dir is the root of a git checkout.
func IsRepo(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && (st.IsDir() || st.Mode().IsRegular())
}

// Available reports whether the git CLI is installed.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

// defaultBranch asks the remote which branch HEAD points at, so an app that
// tracks "whatever the default is" follows a repo that renames master to main.
func defaultBranch(ctx context.Context, key, dir, url string) (string, error) {
	out, err := run(ctx, key, dir, "ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return "", err
	}
	// "ref: refs/heads/main\tHEAD"
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ref: refs/heads/"); ok {
			if name, _, _ := strings.Cut(rest, "\t"); name != "" {
				return name, nil
			}
		}
	}
	return "", errors.New("could not determine the repository's default branch — set one explicitly")
}

// run executes git with a hermetic environment. dir may be "" for commands that
// do not act on a checkout (clone).
//
// The environment is built rather than inherited wholesale so that a deploy
// cannot be steered by whatever git config the host happens to have:
//   - GIT_TERMINAL_PROMPT=0 and BatchMode make a missing credential an error
//     instead of a process that hangs forever waiting on a tty nobody is at.
//   - credential.helper= (empty) stops a system helper from supplying, or
//     worse caching, credentials for these fetches.
func run(ctx context.Context, key, dir string, args ...string) (string, error) {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"LC_ALL=C",
	}
	if key != "" {
		keyDir, err := os.MkdirTemp("", "xdev-key-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(keyDir)
		keyPath := filepath.Join(keyDir, "id")
		if err := os.WriteFile(keyPath, []byte(key), 0o600); err != nil {
			return "", err
		}
		known, err := knownHostsFile(keyDir)
		if err != nil {
			return "", err
		}
		// -F /dev/null is not optional. Without it ssh still reads the invoking
		// user's ~/.ssh/config, and an IdentityFile configured there is offered
		// after this one — so a deploy key that was never added to the repository
		// appears to work, silently authenticating as whoever runs xdev and
		// handing the app that account's access to every repo it can read.
		// IdentityAgent=none does the same for a forwarded agent.
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+keyPath+
			" -F /dev/null -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes"+
			" -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="+known)
	}

	full := append([]string{"-c", "credential.helper="}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w\n%s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// knownHostsFile writes a known_hosts seeded with GitHub's pinned key into the
// per-call temp directory. It lives and dies with the command: pinning what we
// verified out-of-band, and letting any other host be accepted on first use
// (there is nothing persistent to compare a later connection against, so this
// is a real limitation for non-GitHub hosts — documented in GUIDELINE §9.9).
func knownHostsFile(dir string) (string, error) {
	p := filepath.Join(dir, "known_hosts")
	return p, os.WriteFile(p, []byte(githubHostKey), 0o600)
}

// DeployKey is a freshly generated SSH keypair for one app.
type DeployKey struct {
	Public      string // "ssh-ed25519 AAAA... xdev-<comment>"
	Private     string // OpenSSH PEM, to be encrypted before it is stored
	Fingerprint string // "SHA256:..." — what GitHub shows next to the key
}

// NewDeployKey generates an ed25519 keypair for reading one repository.
// ed25519 because GitHub accepts it, it is short enough to paste comfortably,
// and it needs no key-size decision.
func NewDeployKey(comment string) (DeployKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return DeployKey{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return DeployKey{}, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return DeployKey{}, err
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	return DeployKey{
		Public:      authorized + " " + comment,
		Private:     string(pem.EncodeToMemory(block)),
		Fingerprint: ssh.FingerprintSHA256(sshPub),
	}, nil
}

// SubdirOf resolves an app's build directory inside a checkout, rejecting a
// subdir that escapes the repository. A path outside it would put the build —
// and everything the build command does — somewhere the user did not name.
//
// A leading slash is read as repo-relative rather than as the host's root, so
// "/apps/web" means the same as "apps/web": the field asks for a path inside
// the repository, and that is the only thing it can mean.
func SubdirOf(root, subdir string) (string, error) {
	sub := strings.Trim(strings.TrimSpace(subdir), "/")
	if sub == "" {
		return root, nil
	}
	clean := path.Clean(sub)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid repository subdirectory %q — it must be inside the repository", subdir)
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}
