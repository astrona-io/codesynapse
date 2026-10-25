package imagescan

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const trivy = `{"ArtifactName":"ghcr.io/o/api:1","Results":[
 {"Target":"debian","Vulnerabilities":[
  {"VulnerabilityID":"CVE-2025-0001","PkgName":"libssl3","InstalledVersion":"3.0.1","FixedVersion":"3.0.2","Severity":"HIGH","Title":"overflow"},
  {"VulnerabilityID":"CVE-2025-0001","PkgName":"libssl3","InstalledVersion":"3.0.1","FixedVersion":"3.0.2","Severity":"HIGH","Title":"overflow"},
  {"VulnerabilityID":"bad id with spaces","PkgName":"x","InstalledVersion":"1","Severity":"LOW"},
  {"VulnerabilityID":"CVE-2025-0002","PkgName":"zlib","InstalledVersion":"1.2","Severity":"weird"}]},
 {"Target":"none"}]}`

const grype = `{"matches":[
 {"vulnerability":{"id":"CVE-2025-0003","severity":"Critical","fix":{"versions":["2.1","2.2"]}},"artifact":{"name":"busybox","version":"1.0"}},
 {"vulnerability":{"id":"GHSA-aaaa-bbbb-cccc","severity":"Medium","fix":{}},"artifact":{"name":"","version":"1"}}],
 "source":{"target":{"userInput":"alpine:3"}}}`

func TestParse(t *testing.T) {
	tests := []struct {
		name, in string
		want     []Finding
	}{
		{"trivy", trivy, []Finding{
			{Image: "ghcr.io/o/api:1", Package: "libssl3", Version: "3.0.1", ID: "CVE-2025-0001", Severity: "high", Fixed: "3.0.2", Title: "overflow"},
			{Image: "ghcr.io/o/api:1", Package: "zlib", Version: "1.2", ID: "CVE-2025-0002", Severity: "unknown"}}},
		{"grype", grype, []Finding{
			{Image: "alpine:3", Package: "busybox", Version: "1.0", ID: "CVE-2025-0003", Severity: "critical", Fixed: "2.1, 2.2"}}},
		{"empty trivy", `{"ArtifactName":"x","Results":null}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(strings.NewReader(tt.in))
			require.NoError(t, err)
			assert.Equal(t, len(got), len(tt.want))
			for i := range tt.want {
				assert.Equal(t, got[i], tt.want[i])
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	for name, in := range map[string]string{"not json": "nope", "other json": `{"a":1}`, "array": `[1]`} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(in))
			assert.Error(t, err)
		})
	}
}

func TestClip(t *testing.T) {
	assert.Equal(t, len(clip(strings.Repeat("é", 400))) <= maxFieldLen, true)
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(trivy))
	f.Add([]byte(grype))
	f.Add([]byte(`{"matches":[{"vulnerability":null}],"source":{"target":7}}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := Parse(strings.NewReader(string(b)))
		if err != nil {
			return
		}
		for _, x := range got {
			if x.Package == "" || !advisoryID.MatchString(x.ID) || len(x.Package) > maxFieldLen {
				t.Fatalf("bad finding %+v", x)
			}
		}
	})
}
