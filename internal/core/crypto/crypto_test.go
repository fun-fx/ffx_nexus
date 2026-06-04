package crypto_test

import (
	"bytes"
	"testing"

	"github.com/ffxnexus/nexus/internal/core/crypto"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := crypto.NewCipher(string(key))
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("sk-test-secret-value")
	ct, err := c.Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, plain) {
		t.Fatal("ciphertext must not contain plaintext")
	}
	got, err := c.Decrypt(ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("decrypt mismatch: %q vs %q", got, plain)
	}
}

func TestNewCipherHexKey(t *testing.T) {
	hexKey := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	c, err := crypto.NewCipher(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("expected cipher")
	}
}

func TestNewCipherEmptyDisables(t *testing.T) {
	c, err := crypto.NewCipher("")
	if err != nil {
		t.Fatal(err)
}
	if c != nil {
		t.Fatal("empty master key should return nil cipher")
	}
}

func TestHashKeyDeterministic(t *testing.T) {
	a := crypto.HashKey("nxs_live_testkey")
	b := crypto.HashKey("nxs_live_testkey")
	if a != b || len(a) != 64 {
		t.Fatalf("hash should be 64-char hex, got %q", a)
	}
	if crypto.HashKey("other") == a {
		t.Fatal("different keys should hash differently")
	}
}

func TestEncryptWithoutCipher(t *testing.T) {
	var c *crypto.Cipher
	if _, err := c.Encrypt([]byte("x")); err != crypto.ErrNoMasterKey {
		t.Fatalf("want ErrNoMasterKey, got %v", err)
	}
}
