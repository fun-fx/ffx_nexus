// Package crypto provides secret encryption and key hashing for the control
// plane. Provider secrets are encrypted with AES-256-GCM under a master key
// supplied via env/KMS; virtual keys are hashed (not encrypted) since they only
// need verification, not recovery.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// ErrNoMasterKey is returned when encryption is attempted without a master key.
var ErrNoMasterKey = errors.New("crypto: master key not configured")

// Cipher encrypts and decrypts provider secrets using AES-256-GCM.
//
// This is the data-encryption layer of an envelope-encryption scheme: the
// master key acts as the key-encryption key (KEK). In production the master key
// is injected from a secret manager / KMS (env NEXUS_MASTER_KEY); rotating it
// re-wraps stored ciphertext. For now the master key directly encrypts secrets.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a master key. The key must decode (base64 or
// hex) to exactly 32 bytes for AES-256. An empty key disables encryption and
// returns a nil Cipher (callers treat nil as "credential store unavailable").
func NewCipher(masterKey string) (*Cipher, error) {
	if masterKey == "" {
		return nil, nil
	}
	raw, err := decodeKey(masterKey)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("crypto: master key must be 32 bytes (got %d); use a base64/hex 256-bit key", len(raw))
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals plaintext, returning nonce-prefixed ciphertext.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil {
		return nil, ErrNoMasterKey
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	// Output layout: nonce || ciphertext+tag.
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt opens nonce-prefixed ciphertext produced by Encrypt.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if c == nil {
		return nil, ErrNoMasterKey
	}
	ns := c.aead.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("crypto: ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	return c.aead.Open(nil, nonce, ct, nil)
}

func decodeKey(s string) ([]byte, error) {
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil && len(raw) == 32 {
		return raw, nil
	}
	if raw, err := hex.DecodeString(s); err == nil && len(raw) == 32 {
		return raw, nil
	}
	// Fall back to raw bytes (e.g. a 32-char passphrase).
	return []byte(s), nil
}

// HashKey returns the hex-encoded SHA-256 of a virtual key. Virtual keys are
// high-entropy random tokens, so SHA-256 gives constant-time-lookup-friendly
// hashing without the cost/format issues of bcrypt for table lookups.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
