// Package auth contains the collector's deliberately small credential primitives.
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const tokenPrefix = "hems_"

func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Digest is only a lookup index for high-entropy tokens. Recoverable credentials
// are stored separately using authenticated encryption.
func Digest(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func Bearer(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	token := parts[1]
	if !strings.HasPrefix(token, tokenPrefix) || len(token) != len(tokenPrefix)+43 {
		return "", false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, tokenPrefix))
	return token, err == nil && len(b) == 32
}

// Equal compares fixed-size digests so credential lengths do not affect the
// constant-time comparison.
func Equal(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

type Cipher struct{ aead cipher.AEAD }

func NewCipher(encodedKey string) (*Cipher, error) {
	key, err := base64.StdEncoding.Strict().DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("TOKEN_ENCRYPTION_KEY must be standard base64 encoding of exactly 32 random bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

func tokenAAD(sourceID int64) []byte {
	return []byte(fmt.Sprintf("opendtu-hems/source-token/v1/%d", sourceID))
}

func (c *Cipher) Encrypt(token string, sourceID int64) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(token), tokenAAD(sourceID)), nil
}

func (c *Cipher) Decrypt(encrypted []byte, sourceID int64) (string, error) {
	n := c.aead.NonceSize()
	if len(encrypted) < n+c.aead.Overhead() {
		return "", errors.New("invalid encrypted token")
	}
	plain, err := c.aead.Open(nil, encrypted[:n], encrypted[n:], tokenAAD(sourceID))
	if err != nil {
		return "", errors.New("cannot decrypt source token; check the master key and database")
	}
	return string(plain), nil
}
