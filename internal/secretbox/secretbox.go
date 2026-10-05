// Package secretbox encrypts small secrets (an integration's API token) before
// they are stored in the database, so a copy of the database alone — a backup,
// a read-only check, a leaked dump — does not hand out the token.
//
// AES-256-GCM with a random nonce; the key is derived from a server secret that
// lives only in the environment. Rotating that secret makes stored values
// unreadable: Open then fails and the operator re-enters the token, which is
// the intended behaviour, not data loss.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

// ErrUnreadable means the stored value cannot be decrypted with the current
// key: tampered with, truncated, or sealed under a previous server secret.
var ErrUnreadable = errors.New("secretbox: stored secret cannot be decrypted with the current key")

// Box seals and opens secrets under one derived key.
type Box struct {
	aead cipher.AEAD
}

// New derives a key for `purpose` from the server secret. Different purposes
// get unrelated keys, so a value sealed for one use cannot be opened as another.
func New(serverSecret, purpose string) (*Box, error) {
	if serverSecret == "" {
		return nil, errors.New("secretbox: empty server secret")
	}
	key := sha256.Sum256([]byte("ffm-secretbox:" + purpose + ":" + serverSecret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext and returns base64(nonce || ciphertext).
func (b *Box) Seal(plaintext string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Open reverses Seal.
func (b *Box) Open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", ErrUnreadable
	}
	nonce, body := raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():]
	plain, err := b.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", ErrUnreadable
	}
	return string(plain), nil
}
