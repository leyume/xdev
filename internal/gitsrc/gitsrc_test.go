package gitsrc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestParseRepo pins the grammar of the Repository field: every way a person
// might name the same repository has to land on one canonical value, because
// that value is what gets stored, compared, and linked to.
func TestParseRepo(t *testing.T) {
	for in, want := range map[string]string{
		"leyume/xdev":                                "https://github.com/leyume/xdev",
		"https://github.com/leyume/xdev":             "https://github.com/leyume/xdev",
		"https://github.com/leyume/xdev.git":         "https://github.com/leyume/xdev",
		"https://github.com/leyume/xdev/":            "https://github.com/leyume/xdev",
		"git@github.com:leyume/xdev.git":             "https://github.com/leyume/xdev",
		"ssh://git@github.com/leyume/xdev.git":       "https://github.com/leyume/xdev",
		"github.com/leyume/xdev":                     "https://github.com/leyume/xdev",
		"  https://github.com/leyume/xdev  ":         "https://github.com/leyume/xdev",
		"https://GitHub.com/leyume/xdev":             "https://github.com/leyume/xdev",
		"https://github.com/leyume/xdev/tree/main":   "https://github.com/leyume/xdev",
		"https://github.com/leyume/xdev?tab=readme":  "https://github.com/leyume/xdev",
		"https://gitlab.com/group/proj":              "https://gitlab.com/group/proj",
		"git@git.example.com:team/site.git":          "https://git.example.com/team/site",
		"https://github.com/leyume/xdev#readme":      "https://github.com/leyume/xdev",
		"https://github.com/leyume/xdev/blob/x/y.md": "https://github.com/leyume/xdev",
	} {
		got, err := ParseRepo(in)
		if err != nil {
			t.Errorf("ParseRepo(%q): %v", in, err)
			continue
		}
		if got.String() != want {
			t.Errorf("ParseRepo(%q) = %q, want %q", in, got.String(), want)
		}
	}

	for _, in := range []string{
		"",
		"   ",
		"xdev",                                   // no owner
		"github.com",                             // no repo at all
		"https://x:token@github.com/leyume/xdev", // a secret in a stored field
		"https://ghp_abc123@github.com/o/r",      // ditto, the pasted-token shape
		"https://github.com/leyume/xdev extra",   // two things in one field
		"git@github.com",                         // no colon, no path
	} {
		if got, err := ParseRepo(in); err == nil {
			t.Errorf("ParseRepo(%q) should be rejected, got %q", in, got.String())
		}
	}
}

// TestRepoURLs checks the three URLs derived from one repo, since a wrong one
// shows up as an authentication failure rather than a broken link.
func TestRepoURLs(t *testing.T) {
	r, err := ParseRepo("leyume/xdev")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ got, want string }{
		{r.HTTPS(), "https://github.com/leyume/xdev.git"},
		{r.SSH(), "git@github.com:leyume/xdev.git"},
		{r.KeysURL(), "https://github.com/leyume/xdev/settings/keys"},
		{Source{Repo: r}.URL(), "https://github.com/leyume/xdev.git"},
		{Source{Repo: r, Key: "x"}.URL(), "git@github.com:leyume/xdev.git"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

// TestSubdirOf covers the one thing that matters here: a subdirectory field
// cannot be used to run the build somewhere outside the checkout.
func TestSubdirOf(t *testing.T) {
	root := "/srv/app"
	for in, want := range map[string]string{
		"":            "/srv/app",
		"apps/web":    "/srv/app/apps/web",
		"/apps/web/":  "/srv/app/apps/web",
		"./apps/web":  "/srv/app/apps/web",
		"apps//web":   "/srv/app/apps/web",
		"apps/x/../y": "/srv/app/apps/y",
		"/etc":        "/srv/app/etc", // repo-relative: the field cannot name a host path
	} {
		got, err := SubdirOf(root, in)
		if err != nil {
			t.Errorf("SubdirOf(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("SubdirOf(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"..", "../etc", "../../root", "apps/../../etc"} {
		if got, err := SubdirOf(root, in); err == nil {
			t.Errorf("SubdirOf(%q) should be rejected, got %q", in, got)
		}
	}
}

// TestNewDeployKey checks the generated key is one OpenSSH actually accepts —
// a key that only looks right would fail at the first clone, on the user's
// repository, with a message about permissions.
func TestNewDeployKey(t *testing.T) {
	k, err := NewDeployKey("xdev-site")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(k.Private))
	if err != nil {
		t.Fatalf("the private half is not a usable OpenSSH key: %v", err)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Public))
	if err != nil {
		t.Fatalf("the public half is not an authorized_keys line: %v", err)
	}
	if string(pub.Marshal()) != string(signer.PublicKey().Marshal()) {
		t.Error("the two halves are not a pair")
	}
	if !strings.HasPrefix(k.Public, "ssh-ed25519 ") {
		t.Errorf("public key = %q, want an ed25519 line", k.Public)
	}
	if !strings.HasSuffix(k.Public, " xdev-site") {
		t.Errorf("public key = %q, want the comment to name the app", k.Public)
	}
	if k.Fingerprint != ssh.FingerprintSHA256(pub) {
		t.Error("fingerprint does not match the key it describes")
	}
	// Two calls must not produce the same key.
	other, err := NewDeployKey("xdev-site")
	if err != nil {
		t.Fatal(err)
	}
	if other.Public == k.Public {
		t.Error("two generated keys are identical")
	}
}

// TestCloneAndUpdate exercises the real git invocations against a repository on
// disk: the first clone, a deploy that picks up a new commit, and the deploy's
// promise that whatever is in the working tree is replaced by what is in the
// repository.
func TestCloneAndUpdate(t *testing.T) {
	if !Available() {
		t.Skip("git is not installed")
	}
	origin := t.TempDir()
	gitInit(t, origin)
	writeFile(t, origin, "index.html", "v1")
	first := gitCommit(t, origin, "first")

	ctx := context.Background()
	dst := filepath.Join(t.TempDir(), "checkout")
	if err := cloneFrom(ctx, dst, origin, "", ""); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if !IsRepo(dst) {
		t.Fatal("the clone is not a git checkout")
	}
	if got := readFile(t, dst, "index.html"); got != "v1" {
		t.Errorf("cloned index.html = %q, want %q", got, "v1")
	}
	if sha, err := HeadSHA(ctx, dst); err != nil || sha != first {
		t.Errorf("HeadSHA = %q (%v), want %q", sha, err, first)
	}

	// A new commit upstream, and a local edit that a deploy must throw away.
	writeFile(t, origin, "index.html", "v2")
	second := gitCommit(t, origin, "second")
	writeFile(t, dst, "index.html", "edited by hand")

	sha, err := updateFrom(ctx, dst, origin, "", "")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if sha != second {
		t.Errorf("deployed %q, want the new commit %q", sha, second)
	}
	if got := readFile(t, dst, "index.html"); got != "v2" {
		t.Errorf("after deploy index.html = %q, want %q — a deploy resets the working tree", got, "v2")
	}
}

// TestUpdateKeepsUntrackedFiles pins the deliberate half of the reset: build
// output and node_modules are untracked, and wiping them on every deploy would
// turn a one-file change into a full reinstall.
func TestUpdateKeepsUntrackedFiles(t *testing.T) {
	if !Available() {
		t.Skip("git is not installed")
	}
	origin := t.TempDir()
	gitInit(t, origin)
	writeFile(t, origin, "index.html", "v1")
	gitCommit(t, origin, "first")

	ctx := context.Background()
	dst := filepath.Join(t.TempDir(), "checkout")
	if err := cloneFrom(ctx, dst, origin, "", ""); err != nil {
		t.Fatalf("clone: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, "node_modules/marker", "kept")

	writeFile(t, origin, "index.html", "v2")
	gitCommit(t, origin, "second")
	if _, err := updateFrom(ctx, dst, origin, "", ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := readFile(t, dst, "node_modules/marker"); got != "kept" {
		t.Errorf("node_modules/marker = %q, want it left alone by the deploy", got)
	}
}

// --- helpers: a throwaway repository on disk -------------------------------

func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init", "-b", "main")
}

func gitCommit(t *testing.T, dir, msg string) string {
	t.Helper()
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", msg)
	return strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// An identity, and none of the developer's own config, so the test behaves
	// the same on a machine with commit signing or hooks configured.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=xdev", "GIT_AUTHOR_EMAIL=xdev@example.com",
		"GIT_COMMITTER_NAME=xdev", "GIT_COMMITTER_EMAIL=xdev@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
