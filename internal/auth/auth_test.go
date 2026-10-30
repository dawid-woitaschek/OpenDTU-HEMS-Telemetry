package auth

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestTokenGenerationAndBearer(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		token, err := GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatal("repeated token")
		}
		seen[token] = true
		if got, ok := Bearer("bEaReR " + token); !ok || got != token {
			t.Fatal("generated token rejected")
		}
	}
	for _, header := range []string{"", "Basic abc", "Bearer", "Bearer short", "Bearer " + strings.Repeat("x", 48), "Bearer hems_" + strings.Repeat("!", 43), "Bearer a b"} {
		if _, ok := Bearer(header); ok {
			t.Fatal("invalid header accepted")
		}
	}
}

func TestEncryption(t *testing.T) {
	c, err := NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token := "secret test value"
	encrypted, err := c.Encrypt(token, 42)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(token)) {
		t.Fatal("plaintext present in ciphertext")
	}
	if got, err := c.Decrypt(encrypted, 42); err != nil || got != token {
		t.Fatalf("roundtrip failed: %v", err)
	}
	if _, err := c.Decrypt(encrypted, 43); err == nil {
		t.Fatal("ciphertext was not bound to source")
	}
	other, _ := NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	if _, err := other.Decrypt(encrypted, 42); err == nil {
		t.Fatal("incorrect key accepted")
	}
	encrypted[len(encrypted)-1] ^= 1
	if _, err := c.Decrypt(encrypted, 42); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := c.Decrypt([]byte{1}, 42); err == nil {
		t.Fatal("truncated ciphertext accepted")
	}
	for _, key := range []string{"", "not base64", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := NewCipher(key); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
}
