package store

import (
	"database/sql"
	"errors"
)

// DeployKey is the SSH keypair one app reads its private repository with. The
// private half is stored encrypted (see internal/secrets); this package never
// looks inside it, it just carries the ciphertext.
//
// AppID is 0 while the key is unbound. The add-app form generates a key before
// the app exists so the public half can be added to GitHub first — a private
// repo cannot be cloned before its deploy key is in place, so there is no
// workable order that creates the app first.
type DeployKey struct {
	ID          int64
	AppID       int64
	PublicKey   string // "ssh-ed25519 AAAA... xdev-<app>"
	PrivateKey  string // encrypted; opaque here
	Fingerprint string // "SHA256:..." — matches what GitHub displays
	CreatedAt   string
}

// CreateDeployKey stores a generated keypair, unbound unless AppID is set.
func (s *Store) CreateDeployKey(k DeployKey) (DeployKey, error) {
	res, err := s.db.Exec(
		`INSERT INTO deploy_keys (app_id, public_key, private_key, fingerprint)
		 VALUES (?, ?, ?, ?)`,
		k.AppID, k.PublicKey, k.PrivateKey, k.Fingerprint)
	if err != nil {
		return DeployKey{}, err
	}
	id, _ := res.LastInsertId()
	return s.DeployKeyByID(id)
}

// DeployKeyByID looks up one key.
func (s *Store) DeployKeyByID(id int64) (DeployKey, error) {
	return scanDeployKey(s.db.QueryRow(deployKeySelect+` WHERE id = ?`, id))
}

// DeployKeyForApp returns the key an app reads its repository with, or
// ErrNotFound when it has none (a public repo needs no key). The newest wins,
// so rotating leaves the old row harmless if a delete ever failed.
func (s *Store) DeployKeyForApp(appID int64) (DeployKey, error) {
	if appID == 0 {
		return DeployKey{}, ErrNotFound
	}
	return scanDeployKey(s.db.QueryRow(
		deployKeySelect+` WHERE app_id = ? ORDER BY id DESC LIMIT 1`, appID))
}

// BindDeployKey attaches an unbound key to a newly created app. Binding an
// already-bound key is refused rather than silently moved: it would take a
// repository's access away from the app that has it.
func (s *Store) BindDeployKey(id, appID int64) error {
	res, err := s.db.Exec(`UPDATE deploy_keys SET app_id = ? WHERE id = ? AND app_id = 0`, appID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("that deploy key is not available — generate a new one")
	}
	return nil
}

// DeleteDeployKeysForApp removes an app's keys, called when the app is deleted
// or its key is rotated. The key stays on GitHub until the user removes it
// there; it just no longer opens anything, since the private half is gone.
func (s *Store) DeleteDeployKeysForApp(appID int64) error {
	if appID == 0 {
		return nil
	}
	_, err := s.db.Exec(`DELETE FROM deploy_keys WHERE app_id = ?`, appID)
	return err
}

// PruneUnboundDeployKeys drops keys generated for an add-app form that was
// never submitted. Called on startup — a key nobody bound within a day is a
// dialog somebody closed.
func (s *Store) PruneUnboundDeployKeys() error {
	_, err := s.db.Exec(
		`DELETE FROM deploy_keys WHERE app_id = 0 AND created_at < datetime('now', '-1 day')`)
	return err
}

const deployKeySelect = `SELECT id, app_id, public_key, private_key, fingerprint, created_at
	FROM deploy_keys`

func scanDeployKey(row *sql.Row) (DeployKey, error) {
	var k DeployKey
	err := row.Scan(&k.ID, &k.AppID, &k.PublicKey, &k.PrivateKey, &k.Fingerprint, &k.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DeployKey{}, ErrNotFound
	}
	return k, err
}
