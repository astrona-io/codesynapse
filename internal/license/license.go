// Package license identifies the licences of a repository and its
// dependencies and explains, per dependency, whether its licence fits the
// repository's own.
//
// The verdicts are guidance for a human, not legal advice: each one carries
// the reason and where the licence was read from, so a reviewer can check it.
package license

import (
	"strings"
)

// Category groups licences by the obligations they put on a project that
// distributes them.
type Category string

// Licence categories, from least to most restrictive (see rank).
const (
	PublicDomain Category = "public-domain"
	Permissive   Category = "permissive"
	// WeakCopyleft covers licences whose copyleft stops at the file or
	// library boundary (LGPL, MPL, EPL).
	WeakCopyleft Category = "weak-copyleft"
	// StrongCopyleft extends to the whole distributed work (GPL).
	StrongCopyleft Category = "strong-copyleft"
	// NetworkCopyleft also triggers on network use (AGPL).
	NetworkCopyleft Category = "network-copyleft"
	// Restricted covers source-available and non-commercial licences that are
	// not open source (BUSL, SSPL, Elastic, Commons Clause, CC *-NC).
	Restricted Category = "restricted"
	Unknown    Category = "unknown"
)

// rank orders categories from least to most restrictive. Unknown sorts
// between copyleft and restricted: it needs a look, not a verdict.
var rank = map[Category]int{PublicDomain: 0, Permissive: 1, WeakCopyleft: 2, StrongCopyleft: 3, NetworkCopyleft: 4, Unknown: 5, Restricted: 6}

// categories maps SPDX identifiers (upper-cased, without -only/-or-later) to
// their category.
var categories = map[string]Category{
	"0BSD": PublicDomain, "CC0-1.0": PublicDomain, "UNLICENSE": PublicDomain, "WTFPL": PublicDomain, "PUBLIC-DOMAIN": PublicDomain,

	"MIT": Permissive, "MIT-0": Permissive, "APACHE-2.0": Permissive, "BSD-2-CLAUSE": Permissive, "BSD-3-CLAUSE": Permissive,
	"BSD-3-CLAUSE-CLEAR": Permissive, "ISC": Permissive, "ZLIB": Permissive, "BSL-1.0": Permissive, "PYTHON-2.0": Permissive,
	"PSF-2.0": Permissive, "BLUEOAK-1.0.0": Permissive, "CC-BY-3.0": Permissive, "CC-BY-4.0": Permissive, "X11": Permissive,
	"POSTGRESQL": Permissive, "NCSA": Permissive, "UPL-1.0": Permissive, "ARTISTIC-2.0": Permissive, "UNICODE-DFS-2016": Permissive,
	"UNICODE-3.0": Permissive, "OFL-1.1": Permissive, "MS-PL": Permissive, "AFL-3.0": Permissive, "HPND": Permissive,

	"LGPL-2.0": WeakCopyleft, "LGPL-2.1": WeakCopyleft, "LGPL-3.0": WeakCopyleft, "MPL-1.1": WeakCopyleft, "MPL-2.0": WeakCopyleft,
	"EPL-1.0": WeakCopyleft, "EPL-2.0": WeakCopyleft, "CDDL-1.0": WeakCopyleft, "CDDL-1.1": WeakCopyleft, "MS-RL": WeakCopyleft,
	"CC-BY-SA-4.0": WeakCopyleft, "CC-BY-SA-3.0": WeakCopyleft, "EUPL-1.2": StrongCopyleft, "OSL-3.0": StrongCopyleft,

	"GPL-2.0": StrongCopyleft, "GPL-3.0": StrongCopyleft, "GPL-1.0": StrongCopyleft,
	"AGPL-3.0": NetworkCopyleft, "AGPL-1.0": NetworkCopyleft, "SSPL-1.0": Restricted,

	"BUSL-1.1": Restricted, "ELASTIC-2.0": Restricted, "COMMONS-CLAUSE": Restricted, "CC-BY-NC-4.0": Restricted,
	"CC-BY-NC-SA-4.0": Restricted, "CC-BY-NC-ND-4.0": Restricted, "CC-BY-ND-4.0": Restricted, "POLYFORM-NONCOMMERCIAL-1.0.0": Restricted,
	"POLYFORM-SHIELD-1.0.0": Restricted, "CONFLUENT-COMMUNITY-1.0": Restricted, "PROPRIETARY": Restricted, "UNLICENSED": Restricted,
}

// aliases maps free-form names seen in package metadata to SPDX identifiers.
var aliases = map[string]string{
	"APACHE 2.0": "Apache-2.0", "APACHE-2": "Apache-2.0", "APACHE 2": "Apache-2.0", "APACHE LICENSE 2.0": "Apache-2.0",
	"APACHE LICENSE, VERSION 2.0": "Apache-2.0", "APACHE SOFTWARE LICENSE": "Apache-2.0", "APACHE2": "Apache-2.0", "ASL 2.0": "Apache-2.0",
	"MIT LICENSE": "MIT", "THE MIT LICENSE": "MIT", "EXPAT": "MIT",
	"BSD": "BSD-3-Clause", "BSD LICENSE": "BSD-3-Clause", "NEW BSD": "BSD-3-Clause", "BSD-3": "BSD-3-Clause", "3-CLAUSE BSD": "BSD-3-Clause",
	"BSD-2": "BSD-2-Clause", "SIMPLIFIED BSD": "BSD-2-Clause", "FREEBSD": "BSD-2-Clause",
	"ISC LICENSE": "ISC", "GPLV2": "GPL-2.0", "GPLV3": "GPL-3.0", "GPL": "GPL-3.0", "LGPL": "LGPL-3.0", "LGPLV3": "LGPL-3.0", "LGPLV2.1": "LGPL-2.1",
	"AGPLV3": "AGPL-3.0", "MPL 2.0": "MPL-2.0", "MOZILLA PUBLIC LICENSE 2.0": "MPL-2.0", "PSF": "PSF-2.0", "PSFL": "PSF-2.0",
	"PYTHON SOFTWARE FOUNDATION LICENSE": "PSF-2.0", "PUBLIC DOMAIN": "Public-Domain", "THE UNLICENSE": "Unlicense",
	"ELASTIC LICENSE 2.0": "Elastic-2.0", "BUSINESS SOURCE LICENSE 1.1": "BUSL-1.1", "SEE LICENSE IN LICENSE": "",
}

// Normalize returns the SPDX identifier for a single licence name, or the
// trimmed input when it is not recognised.
func Normalize(name string) string {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(name), "()"))
	if s == "" {
		return ""
	}
	if id, ok := aliases[strings.ToUpper(s)]; ok {
		return id
	}
	return s
}

// key strips the -only/-or-later/+ suffixes SPDX uses for GPL-family versions.
func key(id string) string {
	k := strings.ToUpper(strings.TrimSpace(id))
	k = strings.TrimSuffix(k, "+")
	k = strings.TrimSuffix(k, "-ONLY")
	return strings.TrimSuffix(k, "-OR-LATER")
}

// Of returns the category of one licence identifier.
func Of(id string) Category {
	if c, ok := categories[key(Normalize(id))]; ok {
		return c
	}
	return Unknown
}

// Classify returns the effective category of an SPDX expression. With OR the
// licensee picks, so the least restrictive choice counts; with AND every
// licence applies, so the most restrictive counts. A WITH exception that
// allows linking (Classpath, LLVM, GCC runtime) turns GPL into weak copyleft.
func Classify(expr string) Category {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return Unknown
	}
	best := Unknown
	for i, alt := range splitTop(expr, " OR ") {
		worst := PublicDomain
		for _, part := range splitTop(alt, " AND ") {
			c := classifyOne(part)
			if rank[c] > rank[worst] {
				worst = c
			}
		}
		if i == 0 || rank[worst] < rank[best] {
			best = worst
		}
	}
	return best
}

func classifyOne(part string) Category {
	part = strings.TrimSpace(part)
	if strings.HasPrefix(part, "(") && strings.HasSuffix(part, ")") {
		return Classify(part[1 : len(part)-1])
	}
	id, exception, hasException := strings.Cut(part, " WITH ")
	c := Of(id)
	if hasException && c == StrongCopyleft {
		switch key(exception) {
		case "CLASSPATH-EXCEPTION-2.0", "GCC-EXCEPTION-3.1", "LLVM-EXCEPTION", "LINKING-EXCEPTION", "UNIVERSAL-FOSS-EXCEPTION-1.0":
			return WeakCopyleft
		}
	}
	return c
}

// splitTop splits s on sep (case-insensitive) outside parentheses.
func splitTop(s, sep string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		// EqualFold on the original bytes: ToUpper can change the length of
		// invalid UTF-8 and shift every index after it.
		if depth == 0 && i+len(sep) <= len(s) && strings.EqualFold(s[i:i+len(sep)], sep) {
			parts = append(parts, s[start:i])
			start = i + len(sep)
			i += len(sep) - 1
		}
	}
	return append(parts, s[start:])
}

// ids returns every licence identifier in an expression.
func ids(expr string) []string {
	r := strings.NewReplacer("(", " ", ")", " ")
	var out []string
	fields := strings.Fields(r.Replace(expr))
	for i := 0; i < len(fields); i++ {
		switch strings.ToUpper(fields[i]) {
		case "OR", "AND":
			continue
		case "WITH":
			i++ // skip the exception
			continue
		}
		out = append(out, fields[i])
	}
	return out
}
