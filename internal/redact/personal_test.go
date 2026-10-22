package redact

import (
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
)

func TestPersonalMasks(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"visa unbroken", "card 4111111111111111 on file", "card [CARD] on file"},
		{"visa grouped", "paid with 4111 1111 1111 1111.", "paid with [CARD]."},
		{"mastercard dashed", "5555-5555-5555-4444", "[CARD]"},
		{"amex grouped", "3782 822463 10005", "[CARD]"},
		{"card then a number", "4111 1111 1111 1111 12", "[CARD] 12"},
		{"us ssn", "SSN 123-45-6789.", "SSN [NATIONAL-ID]."},
		{"danish cpr", "CPR 010190-1234", "CPR [NATIONAL-ID]"},
		{"swedish personnummer", "pnr 19900101-1234 and 900101+1234", "pnr [NATIONAL-ID] and [NATIONAL-ID]"},
		{"international phone", "call +45 12 34 56 78 today", "call [PHONE] today"},
		{"international phone in brackets", "(+1 (555) 123-4567)", "([PHONE])"},
		{"north american phone", "ring (555) 123-4567 or 555-123-4567", "ring [PHONE] or [PHONE]"},
		{"danish phone in pairs", "tlf. 28 12 34 56", "tlf. [PHONE]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, Personal(tt.in), tt.want)
		})
	}
}

func TestPersonalLeavesCodeAlone(t *testing.T) {
	for _, in := range []string{
		"Parse returns the AST.",
		"commit 4c595a72e89da67ef5e6184a3b2c1d0e9f8a7b6c",
		"sha256 3f1d9a0e5b7c4111111111111111a",
		"see main.go:1234 and internal/api/admin.go:205-275",
		"bump golang.org/x/tools to v0.38.0 and 1.2.3+20240101",
		"created_at 1730000000000 (ms) and 1730000000",
		"id 550e8400-e29b-41d4-a716-446655440000",
		"listen on 192.168.1.10:8080",
		"released 2026-10-22, build 20991022-1234 and 20261322-1234 are not dates of birth",
		"order 4111111111111112 fails Luhn",
		"ticket 000-12-3456 and 666-12-3456 are not issued",
		"@@ -10,7 +10,8 @@ func main() {",
		"bytes 00 01 02 03 and 0x12345678",
		"limit 10000 events, 1 234 567 rows",
	} {
		assert.Equal(t, Personal(in), in, in)
	}
}

func TestFree(t *testing.T) {
	assert.Equal(t, Free("password=supersecret1 card 4111111111111111"), "password=[REDACTED] card [CARD]")
}

func FuzzPersonal(f *testing.F) {
	f.Add("+45 12 34 56 78")
	f.Add("4111 1111 1111 1111")
	f.Add("010190-1234")
	f.Fuzz(func(t *testing.T, s string) {
		out := Personal(s)
		if Personal(out) != out {
			t.Errorf("not idempotent: %q -> %q -> %q", s, out, Personal(out))
		}
	})
}
