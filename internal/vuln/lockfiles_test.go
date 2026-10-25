package vuln

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func byPath(mods []Module) map[string]Module {
	m := map[string]Module{}
	for _, x := range mods {
		m[x.Path] = x
	}
	return m
}

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600))
}

func TestParseCargoLock(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", `[package]
name = "app"

[dependencies]
serde = { version = "1", features = ["derive"] }
tokio.workspace = true

[dev-dependencies]
criterion = "0.5"

[target.'cfg(unix)'.dependencies]
libc = "0.2"

[build-dependencies.cc]
version = "1"
`)
	lock := `version = 3

[[package]]
name = "app"
version = "0.1.0"
dependencies = [
 "serde",
]

[[package]]
name = "serde"
version = "1.0.190"
source = "registry+https://github.com/rust-lang/crates.io-index"
dependencies = [
 "serde_derive",
]

[[package]]
name = "serde_derive"
version = "1.0.190"
source = "registry+https://github.com/rust-lang/crates.io-index"
dependencies = [
 "proc-macro2 1.0.69",
 "syn 2.0.38 (registry+https://github.com/rust-lang/crates.io-index)",
]

[[package]]
name = "criterion"
version = "0.5.1"
source = "registry+https://github.com/rust-lang/crates.io-index"
`
	mods := byPath(parseCargoLock([]byte(lock), dir))
	assert.Equal(t, len(mods), 3, "the workspace's own crate has no source and is skipped")
	assert.Equal(t, mods["serde"].Ecosystem, "crates.io")
	assert.False(t, mods["serde"].Indirect)
	assert.Equal(t, mods["serde"].Requires, []string{"crates.io:serde_derive"})
	assert.True(t, mods["serde_derive"].Indirect)
	assert.Equal(t, mods["serde_derive"].Requires, []string{"crates.io:proc-macro2", "crates.io:syn"})
	assert.True(t, mods["criterion"].Dev, "only [dev-dependencies]")
	assert.False(t, mods["criterion"].Indirect)
}

func TestParseComposerLock(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "composer.json", `{"require":{"monolog/monolog":"^3"},"require-dev":{"phpunit/phpunit":"^10"}}`)
	lock := `{"packages":[
	  {"name":"monolog/monolog","version":"v3.5.0","license":["MIT"],"require":{"php":">=8.1","psr/log":"^2||^3","ext-json":"*"}},
	  {"name":"psr/log","version":"3.0.0","license":["MIT"]}],
	 "packages-dev":[{"name":"phpunit/phpunit","version":"10.5.1","license":["BSD-3-Clause"]}]}`
	mods := byPath(parseComposerLock([]byte(lock), dir))
	require.Equal(t, len(mods), 3)
	m := mods["monolog/monolog"]
	assert.Equal(t, m.Version, "3.5.0", "the leading v is not part of the version")
	assert.Equal(t, m.Ecosystem, "Packagist")
	assert.Equal(t, m.Requires, []string{"Packagist:psr/log"}, "php and extensions are not packages")
	assert.Equal(t, m.License, "MIT")
	assert.False(t, m.Indirect)
	assert.True(t, mods["psr/log"].Indirect)
	assert.True(t, mods["phpunit/phpunit"].Dev)
}

func TestParseGemfileLock(t *testing.T) {
	lock := `GEM
  remote: https://rubygems.org/
  specs:
    actionpack (7.1.2)
      rack (>= 2.2.4)
      rails-dom-testing (~> 2.2)
    nokogiri (1.16.0-x86_64-linux)
      racc (~> 1.4)
    rack (3.0.8)
    racc (1.7.3)
    rails-dom-testing (2.2.0)

PLATFORMS
  x86_64-linux

DEPENDENCIES
  actionpack (~> 7.1)
  nokogiri!

BUNDLED WITH
   2.5.3
`
	mods := byPath(parseGemfileLock([]byte(lock), ""))
	require.Equal(t, len(mods), 5)
	assert.False(t, mods["actionpack"].Indirect)
	assert.False(t, mods["nokogiri"].Indirect)
	assert.True(t, mods["rack"].Indirect)
	assert.Equal(t, mods["nokogiri"].Version, "1.16.0", "the platform is not part of the version")
	assert.Equal(t, mods["actionpack"].Requires, []string{"RubyGems:rack", "RubyGems:rails-dom-testing"})
	assert.Equal(t, mods["nokogiri"].Requires, []string{"RubyGems:racc"})
}

func TestParseNuGetLock(t *testing.T) {
	lock := `{"version":1,"dependencies":{
	  "net8.0":{
	    "Newtonsoft.Json":{"type":"Direct","requested":"[13.0.1, )","resolved":"13.0.1"},
	    "Serilog":{"type":"Direct","resolved":"3.1.1","dependencies":{"System.Diagnostics.DiagnosticSource":"7.0.2"}},
	    "System.Diagnostics.DiagnosticSource":{"type":"Transitive","resolved":"7.0.2"},
	    "MyApp.Core":{"type":"Project"}},
	  "net6.0":{
	    "Serilog":{"type":"Direct","resolved":"3.1.1"}}}}`
	mods := byPath(parseNuGetLock([]byte(lock), ""))
	require.Equal(t, len(mods), 3, "projects are not packages, frameworks are merged")
	assert.False(t, mods["Newtonsoft.Json"].Indirect)
	assert.True(t, mods["System.Diagnostics.DiagnosticSource"].Indirect)
	assert.Equal(t, mods["Serilog"].Requires, []string{"NuGet:System.Diagnostics.DiagnosticSource"})
	assert.Equal(t, mods["Serilog"].Ecosystem, "NuGet")
}

func TestParseGradleLock(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "build.gradle.kts", `dependencies {
    implementation("com.google.guava:guava:32.1.3-jre")
    testImplementation("org.junit.jupiter:junit-jupiter:5.10.1")
}`)
	lock := `# This is a Gradle generated file for dependency locking.
com.google.guava:failureaccess:1.0.1=compileClasspath,runtimeClasspath
com.google.guava:guava:32.1.3-jre=compileClasspath,runtimeClasspath
org.junit.jupiter:junit-jupiter:5.10.1=testCompileClasspath,testRuntimeClasspath
org.junit.platform:junit-platform-engine:1.10.1=testRuntimeClasspath
empty=annotationProcessor
`
	mods := byPath(parseGradleLock([]byte(lock), dir))
	require.Equal(t, len(mods), 4)
	g := mods["com.google.guava:guava"]
	assert.Equal(t, g.Version, "32.1.3-jre")
	assert.Equal(t, g.Ecosystem, "Maven")
	assert.False(t, g.Indirect)
	assert.False(t, g.Dev)
	assert.True(t, mods["com.google.guava:failureaccess"].Indirect)
	assert.True(t, mods["org.junit.jupiter:junit-jupiter"].Dev)
	assert.False(t, mods["org.junit.jupiter:junit-jupiter"].Indirect)
}

func TestManifestModulesReadsTheNewLockfiles(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "svc"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "target", "debug"), 0o750))
	write(t, filepath.Join(root, "svc"), "composer.lock", `{"packages":[{"name":"psr/log","version":"3.0.0"}]}`)
	write(t, filepath.Join(root, "target", "debug"), "Cargo.lock", "[[package]]\nname = \"x\"\nversion = \"1\"\nsource = \"s\"\n")
	mods, err := ManifestModules(root)
	require.NoError(t, err)
	require.Equal(t, len(mods), 1, "build output directories are skipped")
	assert.Equal(t, mods[0].Path, "psr/log")
}

func TestSamePackage(t *testing.T) {
	assert.True(t, samePackage(Module{Path: "Newtonsoft.Json", Ecosystem: EcosystemNuGet}, "newtonsoft.json"))
	assert.False(t, samePackage(Module{Path: "monolog/monolog", Ecosystem: EcosystemPackagist}, "Monolog/Monolog"))
	assert.True(t, samePackage(Module{Path: "my-pkg", Ecosystem: EcosystemPyPI}, "My_Pkg"))
	assert.True(t, samePackage(Module{Path: "golang.org/x/net"}, "golang.org/x/net"))
}

// npm marks a package "dev" when only development needs it and
// "devOptional" when it is both a development dependency and an optional
// dependency of production code. The second can ship, so it is not
// development-only: SBOM scope and licence findings follow Module.Dev.
func TestParseNPMLockDevOptionalCanShip(t *testing.T) {
	lock := `{"lockfileVersion":3,"packages":{
		"":{"name":"app","dependencies":{"prod":"1"},"devDependencies":{"tool":"1"}},
		"node_modules/prod":{"version":"1.0.0","optionalDependencies":{"shared":"1"}},
		"node_modules/tool":{"version":"1.0.0","dev":true,"dependencies":{"shared":"1","only-dev":"1"}},
		"node_modules/shared":{"version":"1.0.0","devOptional":true,"license":"GPL-3.0"},
		"node_modules/only-dev":{"version":"1.0.0","dev":true,"license":"GPL-3.0"},
		"node_modules/opt":{"version":"1.0.0","optional":true}}}`
	mods := byPath(parseNPMLock([]byte(lock), t.TempDir()))
	assert.False(t, mods["shared"].Dev, "devOptional is also reachable through a production optional dependency")
	assert.True(t, mods["only-dev"].Dev)
	assert.True(t, mods["tool"].Dev)
	assert.False(t, mods["prod"].Dev)
	assert.False(t, mods["opt"].Dev)

	// The licence verdict follows: a copyleft package that can ship is a
	// conflict with a permissive project, a development-only one a review.
	assert.Equal(t, license.Check("MIT", "GPL-3.0", mods["shared"].Dev).Level, license.Conflict)
	assert.Equal(t, license.Check("MIT", "GPL-3.0", mods["only-dev"].Dev).Level, license.Review)
}

func TestParsePom(t *testing.T) {
	pom := `<project>
  <version>2.1.0</version>
  <properties><jackson.version>2.15.2</jackson.version><alias>${jackson.version}</alias></properties>
  <dependencyManagement><dependencies>
    <dependency><groupId>org.slf4j</groupId><artifactId>slf4j-api</artifactId><version>2.0.9</version></dependency>
  </dependencies></dependencyManagement>
  <dependencies>
    <dependency><groupId>com.fasterxml.jackson.core</groupId><artifactId>jackson-databind</artifactId><version>${alias}</version></dependency>
    <dependency><groupId>org.slf4j</groupId><artifactId>slf4j-api</artifactId></dependency>
    <dependency><groupId>org.junit.jupiter</groupId><artifactId>junit-jupiter</artifactId><version>5.10.1</version><scope>test</scope></dependency>
    <dependency><groupId>org.example</groupId><artifactId>self</artifactId><version>${project.version}</version></dependency>
    <dependency><groupId>org.springframework</groupId><artifactId>spring-core</artifactId></dependency>
    <dependency><groupId>org.example</groupId><artifactId>ranged</artifactId><version>[1.0,2.0)</version></dependency>
  </dependencies>
</project>`
	mods := byPath(parsePom([]byte(pom), ""))
	require.Equal(t, len(mods), 4, "spring-core has no version here and ranged is a range")
	assert.Equal(t, mods["com.fasterxml.jackson.core:jackson-databind"].Version, "2.15.2")
	assert.Equal(t, mods["org.slf4j:slf4j-api"].Version, "2.0.9")
	assert.Equal(t, mods["org.example:self"].Version, "2.1.0")
	assert.True(t, mods["org.junit.jupiter:junit-jupiter"].Dev)
	assert.False(t, mods["org.slf4j:slf4j-api"].Dev)
	assert.Equal(t, mods["org.slf4j:slf4j-api"].Ecosystem, "Maven")
	assert.Len(t, parsePom([]byte("<project><dependencies>"), ""), 0)
}

func FuzzParsePom(f *testing.F) {
	f.Add([]byte(`<project><dependencies><dependency><groupId>a</groupId><artifactId>b</artifactId><version>${x}</version></dependency></dependencies></project>`))
	f.Fuzz(func(_ *testing.T, data []byte) { parsePom(data, "") })
}
