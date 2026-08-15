package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSealUnseal is the round trip, plus the two properties the storage relies
// on: the ciphertext is not the plaintext, and encrypting the same value twice
// does not produce the same row (a nonce is used).
func TestSealUnseal(t *testing.T) {
	box, err := New(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	plain := "-----BEGIN OPENSSH PRIVATE KEY-----\nnot really\n"
	sealed, err := box.Seal([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if sealed == plain {
		t.Fatal("Seal returned the plaintext")
	}
	got, err := box.Unseal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != plain {
		t.Errorf("round trip = %q, want %q", got, plain)
	}
	again, err := box.Seal([]byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	if again == sealed {
		t.Error("the same value sealed twice produced the same ciphertext — the nonce is not random")
	}
}

// TestKeyFile checks the file xdev creates on first use: it is created with
// owner-only permissions, and reopening uses it rather than making a new one —
// a regenerated key would orphan every value already stored.
func TestKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "secret.key")
	box, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", st.Mode().Perm())
	}
	if st.Size() != keySize {
		t.Errorf("key file is %d bytes, want %d", st.Size(), keySize)
	}

	sealed, err := box.Seal([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Unseal(sealed)
	if err != nil {
		t.Fatalf("a value sealed before the restart no longer opens: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestUnsealRejectsForeignCiphertext is the point of encrypting at all: a
// database copied without its key file gives up nothing.
func TestUnsealRejectsForeignCiphertext(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "a.key"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(filepath.Join(t.TempDir(), "b.key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.Seal([]byte("private key"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Unseal(sealed); err == nil {
		t.Errorf("another key opened the ciphertext, got %q", got)
	}

	// Tampering is detected too — GCM authenticates, so a flipped byte fails
	// rather than decrypting to something.
	bad := []byte(sealed)
	bad[len(bad)-2] ^= 'A' ^ 'B'
	if _, err := a.Unseal(string(bad)); err == nil {
		t.Error("a modified ciphertext was accepted")
	}
	for _, in := range []string{"", "not base64!!", "c2hvcnQ="} {
		if _, err := a.Unseal(in); err == nil {
			t.Errorf("Unseal(%q) should fail", in)
		}
	}
}

// TestRefusesWrongSizedKeyFile: a truncated or replaced key file must stop
// startup, not be quietly overwritten — overwriting it would make every stored
// secret permanently unreadable while looking like a clean start.
func TestRefusesWrongSizedKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil {
		t.Fatal("a wrong-sized key file was accepted")
	}
	// And it is still there — New did not replace it.
	if b, err := os.ReadFile(path); err != nil || string(b) != "too short" {
		t.Errorf("key file = %q (%v), want it left untouched", b, err)
	}
}
