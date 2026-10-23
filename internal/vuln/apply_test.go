package vuln

import (
	"slices"
	"testing"
)

func TestPreferredModule(t *testing.T) {
	mods := []Module{
		{Path: "pkg", Version: "0.6.1", Ecosystem: EcosystemNPM, Indirect: true},
		{Path: "pkg", Version: "0.10.0", Ecosystem: EcosystemNPM, Indirect: true},
		{Path: "lib", Version: "1.0.0", Ecosystem: EcosystemNPM, Indirect: true},
		{Path: "lib", Version: "0.9.0", Ecosystem: EcosystemNPM},
	}
	for range 3 {
		got := preferred(mods)
		if len(got) != 2 || got[0].Path != "lib" || got[0].Version != "0.9.0" || got[1].Version != "0.10.0" {
			t.Fatalf("preferred = %+v; want the direct lib 0.9.0 and the highest pkg 0.10.0", got)
		}
		slices.Reverse(mods)
	}
}
