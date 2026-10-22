package accounts

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestSealer(t *testing.T) {
	raw := bytes.Repeat([]byte{0x42}, 32)
	for _, enc := range []string{hex.EncodeToString(raw), base64.StdEncoding.EncodeToString(raw), base64.RawURLEncoding.EncodeToString(raw)} {
		k, err := ParseSecretKey(" " + enc + "\n")
		require.NoError(t, err, enc)
		assert.Equal(t, k, raw)
	}
	_, err := ParseSecretKey("too-short")
	assert.Error(t, err)

	s, err := NewSealer(raw)
	require.NoError(t, err)
	a, err := s.Seal("ghp_secret_value_123", "org1", "github.com")
	require.NoError(t, err)
	b, err := s.Seal("ghp_secret_value_123", "org1", "github.com")
	require.NoError(t, err)
	assert.False(t, bytes.Equal(a, b), "a fresh nonce every time")
	got, err := s.Open(a, s.KeyID, "org1", "github.com")
	require.NoError(t, err)
	assert.Equal(t, got, "ghp_secret_value_123")

	_, err = s.Open(a, s.KeyID, "org1", "gitlab.com")
	assert.Error(t, err, "bound to the host")
	tampered := bytes.Clone(a)
	tampered[len(tampered)-1] ^= 1
	_, err = s.Open(tampered, s.KeyID, "org1", "github.com")
	assert.Error(t, err, "tampering is detected")
	other, _ := NewSealer(bytes.Repeat([]byte{0x43}, 32))
	_, err = other.Open(a, s.KeyID, "org1", "github.com")
	assert.Contains(t, err.Error(), "sealed with key", "a changed key is named, not a mystery")
	assert.False(t, bytes.Contains([]byte(s.KeyID), raw[:4]), "the key ID is a hash, not key material")

	assert.Equal(t, Preview("ghp_abcdefghijklmnop"), "ghp_a***lmnop")
	assert.Equal(t, Preview("shorttoken"), "**********")
}
