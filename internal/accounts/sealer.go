package accounts

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Sealer encrypts organisation secrets (Git tokens) with AES-256-GCM. The
// additional data binds a ciphertext to its organisation and host, so a
// sealed token copied into another row does not open.
type Sealer struct {
	aead cipher.AEAD
	// KeyID names the key (a hash prefix, not the key), so tokens sealed
	// with another key are reported instead of failing obscurely.
	KeyID string
}

// ParseSecretKey reads 32 bytes given as base64 (standard or URL) or hex.
func ParseSecretKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, dec := range []func(string) ([]byte, error){
		hex.DecodeString, base64.StdEncoding.DecodeString, base64.RawStdEncoding.DecodeString,
		base64.URLEncoding.DecodeString, base64.RawURLEncoding.DecodeString,
	} {
		if b, err := dec(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("the secret key must be 32 bytes, base64 or hex encoded (openssl rand -base64 32)")
}

// NewSealer returns a sealer for a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("the secret key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(append([]byte("codesynapse-sealer:"), key...))
	return &Sealer{aead: aead, KeyID: hex.EncodeToString(sum[:4])}, nil
}

func tokenAAD(orgID, host string) []byte { return sealAAD("git-token", orgID, host) }

// sealAAD binds a ciphertext to what it is for (a purpose such as
// "git-token" or "oidc-client-secret"), its organisation and a scope (a Git
// host, an issuer), so a secret copied to another row, organisation or use
// does not open.
func sealAAD(purpose, orgID, scope string) []byte {
	return []byte(purpose + "\x00" + orgID + "\x00" + scope)
}

// Seal encrypts plaintext for orgID and host: nonce || ciphertext.
func (s *Sealer) Seal(plaintext, orgID, host string) ([]byte, error) {
	return s.seal(plaintext, tokenAAD(orgID, host))
}

// Open decrypts what Seal produced for the same organisation and host.
func (s *Sealer) Open(sealed []byte, keyID, orgID, host string) (string, error) {
	return s.open(sealed, keyID, tokenAAD(orgID, host))
}

// SealFor encrypts plaintext for a purpose other than Git tokens.
func (s *Sealer) SealFor(purpose, plaintext, orgID, scope string) ([]byte, error) {
	return s.seal(plaintext, sealAAD(purpose, orgID, scope))
}

// OpenFor decrypts what SealFor produced for the same purpose, organisation
// and scope.
func (s *Sealer) OpenFor(purpose string, sealed []byte, keyID, orgID, scope string) (string, error) {
	return s.open(sealed, keyID, sealAAD(purpose, orgID, scope))
}

func (s *Sealer) seal(plaintext string, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(plaintext), aad), nil
}

func (s *Sealer) open(sealed []byte, keyID string, aad []byte) (string, error) {
	if keyID != s.KeyID {
		return "", fmt.Errorf("secret was sealed with key %s, the API has key %s: set it again", keyID, s.KeyID)
	}
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", errors.New("sealed secret is truncated")
	}
	b, err := s.aead.Open(nil, sealed[:n], sealed[n:], aad)
	if err != nil {
		return "", errors.New("sealed secret does not open (wrong key, organisation, scope or purpose)")
	}
	return string(b), nil
}

// Preview shows the first and last five characters of a token, as the
// settings page does for installation tokens.
func Preview(token string) string {
	if len(token) <= 14 {
		return strings.Repeat("*", len(token))
	}
	return token[:5] + "***" + token[len(token)-5:]
}
