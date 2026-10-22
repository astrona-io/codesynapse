package cleanupplan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/cleanup"
	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store"
)

// hotspotLimit is how many hotspots are checked for tests.
const hotspotLimit = 10

// Assemble reads the stored reports of repo and builds its plan. r should be
// fenced to the repository (store.ForRepository). A report that was never
// stored (an index from before it existed) is just left out of the plan.
func Assemble(ctx context.Context, r store.Reader, repo string) (Plan, error) {
	n, err := r.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return Plan{}, err
	}
	in := Inputs{Repo: repo}
	if n.Cleanup != "" {
		var c cleanup.Report
		if err := json.Unmarshal([]byte(n.Cleanup), &c); err != nil {
			return Plan{}, fmt.Errorf("decoding cleanup report: %w", err)
		}
		in.Cleanup = &c
	}
	if n.Duplicates != "" {
		d, err := duplicates.Decode(n.Duplicates)
		if err != nil {
			return Plan{}, err
		}
		in.Duplicates = &d
	}
	if n.Licenses != "" {
		var l license.Report
		if err := json.Unmarshal([]byte(n.Licenses), &l); err != nil {
			return Plan{}, fmt.Errorf("decoding licence report: %w", err)
		}
		in.Licences = &l
	}
	sec, err := security.Build(ctx, r, repo, security.Options{MinSeverity: "low"})
	if err != nil {
		return Plan{}, err
	}
	in.Vulns = sec.Vulnerabilities

	svc := query.Service{R: r}
	hs, err := svc.Hotspots(ctx, repo, hotspotLimit)
	if err != nil {
		return Plan{}, err
	}
	for _, h := range hs {
		tests, err := svc.Tests(ctx, h.ID, 3, 1)
		if err != nil {
			return Plan{}, err
		}
		in.Hotspots = append(in.Hotspots, Hotspot{Hotspot: h, Tested: len(tests) > 0})
	}
	p := Build(in)
	if in.Cleanup == nil || in.Duplicates == nil {
		p.Notes = append(p.Notes, "Part of the plan is missing: this repository was indexed before some reports existed. Sync it to get the whole plan.")
	}
	return p, nil
}
