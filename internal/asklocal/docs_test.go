package asklocal

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestAboutDocsAndDocsFile(t *testing.T) {
	for _, p := range []string{
		"Change the documentation site so every page is marked as draft",
		"update mkdocs.yml and the docs pages",
		"the README is out of date",
		"fix the doc site navigation",
	} {
		assert.True(t, AboutDocs(p), p)
	}
	for _, p := range []string{
		"the gateway stats should have its own tab/page",
		"make the Stats struct count tokens too",
		"the docker image is too large",
		"when i write docs / code i lose the benefits",
	} {
		assert.False(t, AboutDocs(p), p)
	}
	for _, p := range []string{
		"docs/index.md", "README.md", "mkdocs.yml", "docs/hooks/doc_status.py",
		"docs/assets/extra.css", "website/docusaurus.config.ts", "guide.rst",
	} {
		assert.True(t, DocsFile(p), p)
	}
	for _, p := range []string{
		"internal/metrics/metrics.go", "internal/ingest/docs/docs.go", "docs",
		"services/web/app/pages/stats.vue", "deploy/helm/codesynapse/values.yaml",
	} {
		assert.False(t, DocsFile(p), p)
	}
}
