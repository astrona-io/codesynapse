package asklocal

import (
	"path"
	"regexp"
	"strings"
)

// A real session (PLAN item 18, case h, 2026-10-29) asked to mark every page
// of the documentation site as a draft until verified, naming mkdocs.yml and
// the site's Python hook. The plan took "page" for the user interface and
// "docs", "status" and "verified" for Go names, and the brief read
// internal/metrics/metrics.go. A prompt about the documentation is about the
// pages, the site configuration and its hooks, whatever language they are in.

// docsWords are what a prompt about the documentation talks about.
var docsWords = regexp.MustCompile(`(?i)\b(documentation|docs|mkdocs|docusaurus|sphinx|readme|markdown|docs? ?site|antora|vitepress|hugo)\b`)

// AboutDocs reports whether a prompt is about the documentation.
func AboutDocs(prompt string) bool {
	return docsWords.MatchString(prompt)
}

// docsDirs are top-level directories that hold a documentation site.
var docsDirs = map[string]bool{"docs": true, "doc": true, "documentation": true, "website": true, "site": true}

// siteConfigs are the configuration files of documentation site generators.
var siteConfigs = map[string]bool{
	"mkdocs.yml": true, "mkdocs.yaml": true, "docusaurus.config.js": true, "docusaurus.config.ts": true,
	"antora.yml": true, "book.toml": true, "hugo.toml": true, "hugo.yaml": true, "_config.yml": true,
}

// DocsFile reports whether a repository path belongs to the documentation:
// a markup page anywhere, any file under a top-level documentation directory
// (its pages, hooks and assets), or a site generator's configuration. Code
// that only processes Markdown (internal/ingest/docs) is not documentation.
func DocsFile(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".md", ".mdx", ".rst", ".adoc":
		return true
	}
	if siteConfigs[path.Base(p)] {
		return true
	}
	first, _, nested := strings.Cut(p, "/")
	return nested && docsDirs[first]
}
