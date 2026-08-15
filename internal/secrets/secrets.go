// Package secrets encrypts the small number of values xdev must store but must
// not store in the clear — today, the private half of an app's git deploy key.
//
// The threat it addresses is a copy of the database leaving the host: a backup,
// an scp, a file accidentally committed. Those carry the rows but not the key
// file, so the ciphertext in them is inert. It is deliberately *not* protection
// against someone who is already root here — the key sits next to the database
// and that user reads both. Anything stronger (a passphrase typed at boot, a
// KMS) would mean xdev could not start unattended, which is the wrong trade for
// a tool whose whole job is to bring apps back up after a reboot.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// keySize is AES-256.
const keySize = 32

// Box seals and unseals values with one key.
type Box struct{ aead cipher.AEAD }

// New loads the key at path, creating it (0600) with fresh random bytes if it
// does not exist. The parent directory is created if needed.
//
// A key file that exists but is the wrong size is an error rather than
// something to regenerate: replacing it would silently orphan every value
// already encrypted under the old one.
func New(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		key = make([]byte, keySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		// O_EXCL so two processes starting together cannot both write a key and
		// leave one of them encrypting under a file that is no longer there.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return New(path) // the other process won; read what it wrote
			}
			return nil, err
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("%s: expected a %d-byte key, found %d bytes — refusing to replace it", path, keySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and returns it as base64(nonce || ciphertext), safe
// to put in a TEXT column.
func (b *Box) Seal(plain []byte) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Unseal reverses Seal. A value encrypted under a different key fails here
// rather than returning garbage — GCM authenticates.
func (b *Box) Unseal(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode secret: %w", err)
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return nil, errors.New("secret is too short to be valid")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w (wrong or replaced secret.key?)", err)
	}
	return plain, nil
}
