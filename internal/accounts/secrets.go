package accounts

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP: 19 MiB / 2 iterations minimum; we use more).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

// Password length policy: long passphrases over composition rules.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 256
)

// CheckPassword enforces the length policy.
func CheckPassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLen || n > MaxPasswordLen {
		return fmt.Errorf("%w: passwords need %d to %d characters", ErrInvalidInput, MinPasswordLen, MaxPasswordLen)
	}
	return nil
}

// HashPassword returns an argon2id PHC string.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a PHC string in constant time.
func VerifyPassword(pw, phc string) bool {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil || m > 1<<20 || t > 10 || p == 0 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want))) //nolint:gosec // length from our own hash
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is verified against when a user does not exist, so a login
// takes as long for unknown emails as for wrong passwords.
var dummyHash = sync.OnceValue(func() string {
	h, _ := HashPassword("timing-equaliser-not-a-password") //nolint:errcheck // rand failure leaves an empty hash
	return h
})

// randomToken returns n random bytes, base64url without padding.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is how session tokens and API keys are stored.
func HashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// KeyPrefix starts every API key, so leaked keys are recognisable by secret
// scanners and in logs.
const KeyPrefix = "csk_"

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// newAPIKey returns a key (csk_ + 40 base62 characters, ~238 bits) and the
// prefix shown in lists.
func newAPIKey() (key, prefix string, err error) {
	var b strings.Builder
	b.WriteString(KeyPrefix)
	alphabet := big.NewInt(int64(len(base62)))
	for range 40 {
		n, err := rand.Int(rand.Reader, alphabet)
		if err != nil {
			return "", "", err
		}
		b.WriteByte(base62[n.Int64()])
	}
	key = b.String()
	return key, key[:len(KeyPrefix)+8], nil
}

// newID returns a random identifier for rows.
func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
