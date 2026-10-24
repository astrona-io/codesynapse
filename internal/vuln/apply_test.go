package vuln

import (
	"slices"
	"testing"
)

// Every installed version is kept, in the same order on every run, with the
// preferred one (direct before indirect, then the highest) first of its
// package. The same version found twice is one module.
func TestInstalledVersionsKeepsEveryVersionInOrder(t *testing.T) {
	mods := []Module{
		{Path: "pkg", Version: "0.6.1", Ecosystem: EcosystemNPM, Indirect: true},
		{Path: "pkg", Version: "0.10.0", Ecosystem: EcosystemNPM, Indirect: true},
		{Path: "pkg", Version: "0.10.0", Ecosystem: EcosystemNPM, Indirect: true}, // nested twice
		{Path: "lib", Version: "1.0.0", Ecosystem: EcosystemNPM, Indirect: true},
		{Path: "lib", Version: "0.9.0", Ecosystem: EcosystemNPM},
	}
	want := []string{"lib@0.9.0", "lib@1.0.0", "pkg@0.10.0", "pkg@0.6.1"}
	for range 3 {
		var got []string
		for _, m := range installedVersions(mods) {
			got = append(got, m.Path+"@"+m.Version)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("installed versions = %v; want %v (direct lib first, highest pkg first, no duplicates)", got, want)
		}
		slices.Reverse(mods)
	}
}
