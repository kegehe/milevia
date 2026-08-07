package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// profileSecretStore encrypts managed API keys before they reach SQLite. The
// master key is derived from a random file that lives beside the database, so
// the ciphertext is not recoverable by reading the database alone.
type profileSecretStore struct {
	db        *sql.DB
	masterKey []byte
}

// secretQueryer lets secret rows be read/written on the caller's connection.
// SQLite runs with SetMaxOpenConns(1), so issuing a second connection while a
// write transaction holds the only connection would deadlock. Methods therefore
// accept either *sql.DB or an in-progress *sql.Tx.
type secretQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func newProfileSecretStore(db *sql.DB, keyPath string) (*profileSecretStore, error) {
	masterKey, err := loadOrCreateMasterKey(keyPath)
	if err != nil {
		return nil, err
	}
	return &profileSecretStore{db: db, masterKey: masterKey}, nil
}

func loadOrCreateMasterKey(keyPath string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(keyPath)
	if err == nil && len(raw) > 0 {
		key := sha256.Sum256(raw)
		return key[:], nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		return nil, err
	}
	key := sha256.Sum256(raw)
	return key[:], nil
}

// Store encrypts apiKey into a fresh secret row and returns its opaque id. It
// never reuses an existing row: each revision must reference its own immutable
// credential snapshot so rotating a key never aliases two revisions to one
// secret.
func (s *profileSecretStore) Store(q secretQueryer, ctx context.Context, apiKey string) (string, error) {
	ciphertext, err := s.encrypt([]byte(apiKey))
	if err != nil {
		return "", err
	}
	id := "sec_" + uuid.NewString()
	now := time.Now().UTC()
	if _, err := q.ExecContext(ctx, `insert into profile_secrets (id,ciphertext,created_at,updated_at) values (?,?,?,?)`, id, ciphertext, now, now); err != nil {
		return "", err
	}
	return id, nil
}

// Load decrypts the api key addressed by a secret id.
func (s *profileSecretStore) Load(q secretQueryer, ctx context.Context, secretID string) (string, error) {
	if secretID == "" {
		return "", nil
	}
	var ciphertext []byte
	err := q.QueryRowContext(ctx, `select ciphertext from profile_secrets where id=? and revoked_at=''`, secretID).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("managed credential is unavailable")
	}
	if err != nil {
		return "", err
	}
	plaintext, err := s.decrypt(string(ciphertext))
	if err != nil {
		return "", errors.New("managed credential could not be decrypted")
	}
	return plaintext, nil
}

// Revoke marks a secret unusable (and purges the ciphertext).
func (s *profileSecretStore) Revoke(q secretQueryer, ctx context.Context, secretID string) error {
	if secretID == "" {
		return nil
	}
	_, err := q.ExecContext(ctx, `update profile_secrets set ciphertext='',revoked_at=? where id=? and revoked_at=''`, time.Now().UTC(), secretID)
	return err
}

func (s *profileSecretStore) encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(s.masterKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *profileSecretStore) decrypt(encoded string) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(s.masterKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(sealed) < nonceSize {
		return "", errors.New("bad ciphertext")
	}
	plaintext, err := gcm.Open(nil, sealed[:nonceSize], sealed[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
