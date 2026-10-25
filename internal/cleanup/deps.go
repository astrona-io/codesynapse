package cleanup

import (
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/vuln"
)

// Packages that do their work without being imported: bundlers, compilers,
// linters, test runners, framework CLIs and servers. A match is never
// reported as unused, even as a runtime dependency; a miss costs a false
// report, so the lists lean wide. Names are lower case.
var (
	npmToolingExact = []string{
		"typescript", "esbuild", "rollup", "vite", "webpack", "parcel", "turbo", "nx", "lerna", "nuxt", "next",
		"jest", "vitest", "mocha", "nyc", "c8", "cypress", "storybook", "ts-node", "ts-jest", "tsx", "nodemon",
		"postcss", "autoprefixer", "tailwindcss", "sass", "less", "husky", "lint-staged", "rimraf", "cross-env",
		"concurrently", "npm-run-all",
		// Companions a framework loads itself (Nuxt wires vue-router, Next react-dom) and helpers compilers inject.
		"vue-router", "react-dom", "tslib", "core-js", "regenerator-runtime", "h3", "ofetch", "unhead", "vue-tsc", "react-scripts", "prettier", "eslint", "stylelint", "tsup",
	}
	npmToolingPrefix = []string{
		"@types/", "@babel/", "@eslint/", "@swc/", "@nuxt/", "@nuxtjs/", "@vitejs/", "@tailwindcss/", "@playwright/",
		"@storybook/", "@nx/", "@vue/cli", "@typescript-eslint/", "@rollup/", "@vitest/", "@testing-library/",
		"eslint-", "prettier-", "babel-", "webpack-", "stylelint-", "vite-plugin", "rollup-plugin", "postcss-",
		"jest-", "vitest-", "playwright-", "@commitlint/", "nuxt-",
	}
	npmToolingSuffix = []string{"-loader", "-cli", "-plugin", "-preset", "-webpack-plugin"}

	pyToolingExact = []string{
		"pytest", "black", "ruff", "flake8", "mypy", "isort", "pylint", "tox", "coverage", "gunicorn", "uvicorn",
		"hypercorn", "daphne", "waitress", "pre-commit", "setuptools", "wheel", "pip", "twine", "build", "poetry",
		"pyright", "bandit", "sphinx", "mkdocs", "nox", "hatch", "hatchling", "flit", "pdm", "uv", "pip-tools",
		"autopep8", "yapf", "pycodestyle", "pydocstyle", "codespell", "commitizen", "towncrier", "honcho",
	}
	pyToolingPrefix = []string{"pytest-", "types-", "sphinx-", "mkdocs-", "flake8-", "pylint-", "mypy-", "black-", "poetry-", "django-debug-toolbar"}
	pyToolingSuffix = []string{"-stubs"}
)

// toolingDependency reports whether a package of the ecosystem is used
// without an import, so that finding no import says nothing about it.
func toolingDependency(ecosystem, name string) bool {
	name = strings.ToLower(name)
	switch ecosystem {
	case vuln.EcosystemNPM:
		return slices.Contains(npmToolingExact, name) || hasAnyPrefix(name, npmToolingPrefix) || hasAnySuffix(name, npmToolingSuffix)
	case vuln.EcosystemPyPI:
		name = strings.ReplaceAll(name, "_", "-")
		return slices.Contains(pyToolingExact, name) || hasAnyPrefix(name, pyToolingPrefix) || hasAnySuffix(name, pyToolingSuffix)
	}
	return false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(s, p) })
}

func hasAnySuffix(s string, suffixes []string) bool {
	return slices.ContainsFunc(suffixes, func(p string) bool { return strings.HasSuffix(s, p) })
}
