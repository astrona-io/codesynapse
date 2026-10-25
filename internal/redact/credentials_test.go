package redact

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestCredentials(t *testing.T) {
	// Put together at run time so this file holds no credential.
	token := "gh" + "p_" + "Zx9Qw3Er7Ty1Ui5Op2As8Df4Gh6Jk0LmNbVcXz"
	for text, want := range map[string]string{
		"token = " + token:                                     "GitHub token",
		"AKIA" + "IOSFODNN7EXAMPLE":                            "",
		"gh" + "p_" + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx": "",
		"gh" + "p_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "",
		"api_key = 'hunter2hunter2'":                           "", // generic assignments are not credentials here
	} {
		got := Credentials(text)
		if want == "" {
			assert.Len(t, got, 0, text)
			continue
		}
		if assert.Len(t, got, 1, text) {
			assert.Equal(t, got[0].Kind, want)
			assert.Equal(t, got[0].Value, token)
		}
	}
}
