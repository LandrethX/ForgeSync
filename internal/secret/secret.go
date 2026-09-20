// Package secret seals the values ForgeSync has to store but must not
// store in the clear.
//
// There is one of those: a node's API token, which is site-admin on that
// Forgejo. Tokens used to be files on the controller's disk, read at
// startup and never written anywhere. Keeping nodes in the database means
// the token is written down, and a database is copied in ways a 0600 file
// is not: a dump, a nightly backup, a streaming standby, a restore into a
// scratch database to verify it. None of those should hand somebody
// site-admin on every node.
//
// So the token is sealed with a key each controller holds on its own disk
// and the database never sees. Someone who has the database has ciphertext;
// someone who has a controller already has the node tokens anyway, because
// it is using them. The key therefore has to be the same on every
// controller, and it has to be kept: losing it means entering the tokens
// again, which is recoverable, while losing the database is not.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the key length in bytes: AES-256.
const KeySize = 32

// ErrWrongKey says a value could not be opened with this key. It is
// deliberately the same error whether the key is wrong, the value is
// truncated or the value was tampered with, because the caller can do
// nothing different about any of them.
var ErrWrongKey = errors.New("sealed value could not be opened with this key")

// Key seals and opens values. It is safe for concurrent use.
type Key struct{ aead cipher.AEAD }

// NewKey makes a Key from raw bytes.
func NewKey(raw []byte) (*Key, error) {
	if len(raw) != KeySize {
		return nil, fmt.Errorf("key is %d bytes, want %d", len(raw), KeySize)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Key{aead: aead}, nil
}

// LoadKey reads a key from a file. The file holds the key as hex or
// base64, with surrounding whitespace ignored, so it can be written by
// `openssl rand -hex 32` as well as by GenerateKey.
func LoadKey(path string) (*Key, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("node key: %w", err)
	}
	raw, err := decodeKey(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("node key %s: %w", path, err)
	}
	return NewKey(raw)
}

func decodeKey(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("the file is empty")
	}
	if raw, err := hex.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return nil, errors.New("the file is neither hex nor base64")
}

// GenerateKey writes a new random key to path, readable only by its owner,
// and refuses to overwrite one that is already there: replacing a key
// silently would leave every sealed token in the database unopenable.
func GenerateKey(path string) error {
	raw := make([]byte, KeySize)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("node key: %w", err)
	}
	defer f.Close()
	if _, err := fmt.Fprintln(f, hex.EncodeToString(raw)); err != nil {
		return err
	}
	return f.Close()
}

// Seal encrypts a value. The nonce is fresh for every call and is carried
// in front of the ciphertext, so sealing the same token twice gives two
// different values and neither says anything about the other.
func (k *Key) Seal(plaintext string) ([]byte, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open decrypts a value sealed by Seal with the same key.
func (k *Key) Open(sealed []byte) (string, error) {
	n := k.aead.NonceSize()
	if len(sealed) < n {
		return "", ErrWrongKey
	}
	out, err := k.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return "", ErrWrongKey
	}
	return string(out), nil
}
