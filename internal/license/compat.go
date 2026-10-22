package license

import (
	"fmt"
	"strings"
)

// Verdict levels.
const (
	OK       = "ok"
	Review   = "review"
	Conflict = "conflict"
)

// Verdict says whether a dependency's licence fits the project's and why.
type Verdict struct {
	Level  string `json:"level"`
	Reason string `json:"reason"`
}

// Check compares a dependency licence expression with the project's. dev
// marks dependencies used only to build or test, which are normally not
// distributed with the product: their conflicts become reviews.
//
// The model is distribution of a combined work: what a project may ship
// together with its dependencies under its own licence.
func Check(project, dep string, dev bool) Verdict {
	v := check(project, dep)
	if dev && v.Level == Conflict {
		return Verdict{Review, v.Reason + " It is a development dependency, so this only matters if it ends up in what you ship (bundled, vendored or copied)."}
	}
	return v
}

func check(project, dep string) Verdict {
	dc := Classify(dep)
	pc := Classify(project)
	dl := display(dep)
	pl := display(project)
	if project == "" {
		pl = "this project (no licence found, so all rights reserved)"
		pc = Restricted
	}

	switch dc {
	case Unknown:
		if dep == "" {
			return Verdict{Review, "No licence found for this dependency. Without one, you have no right to use or ship it; check the package's repository."}
		}
		return Verdict{Review, fmt.Sprintf("%q is not a licence CodeSynapse recognises; check its terms by hand.", dep)}
	case Restricted:
		return Verdict{Conflict, fmt.Sprintf("%s is not an open-source licence: it restricts use (commercial use, competing services or production use). It is not compatible with %s without a separate agreement.", dl, pl)}
	}

	// GPL-2.0-only cannot be combined with Apache-2.0 or GPL-3.0 code.
	if gpl2Only(project) {
		for _, id := range ids(dep) {
			switch key(id) {
			case "APACHE-2.0":
				if !hasAlternative(dep, "APACHE-2.0") {
					return Verdict{Conflict, "Apache-2.0 is incompatible with GPL-2.0-only: its patent and indemnity terms are extra restrictions GPL-2.0 does not allow."}
				}
			case "GPL-3.0", "LGPL-3.0", "AGPL-3.0":
				if !hasAlternative(dep, key(id)) {
					return Verdict{Conflict, fmt.Sprintf("%s cannot be combined with GPL-2.0-only code; only GPL-2.0-or-later projects can move to version 3.", id)}
				}
			}
		}
	}

	switch pc {
	case StrongCopyleft, NetworkCopyleft:
		switch dc {
		case NetworkCopyleft:
			if pc == NetworkCopyleft {
				return Verdict{OK, fmt.Sprintf("%s is network copyleft like %s.", dl, pl)}
			}
			return Verdict{Review, fmt.Sprintf("%s may be combined with GPL-3.0 code, but anyone who offers the result as a network service must publish the source of the AGPL part.", dl)}
		case StrongCopyleft:
			return Verdict{OK, fmt.Sprintf("%s is copyleft like %s; check the versions match (GPL-2.0-only and GPL-3.0 do not mix).", dl, pl)}
		default:
			return Verdict{OK, fmt.Sprintf("%s can be included in a %s project; keep its copyright notice.", dl, pl)}
		}
	}

	// The project is permissive, weak copyleft, proprietary or restricted:
	// it cannot absorb strong copyleft.
	switch dc {
	case PublicDomain:
		return Verdict{OK, fmt.Sprintf("%s places no conditions on use.", dl)}
	case Permissive:
		return Verdict{OK, fmt.Sprintf("%s is permissive and compatible with %s; keep its copyright notice (and NOTICE file, for Apache-2.0).", dl, pl)}
	case WeakCopyleft:
		return Verdict{Review, weakCopyleftReason(dep, dl)}
	case StrongCopyleft:
		return Verdict{Conflict, fmt.Sprintf("%s is strong copyleft: distributing it as part of %s requires releasing the whole work under the GPL.", dl, pl)}
	case NetworkCopyleft:
		return Verdict{Conflict, fmt.Sprintf("%s is network copyleft: using it in %s requires releasing the whole work under the AGPL, even if it is only offered as a service.", dl, pl)}
	}
	return Verdict{Review, "Unclassified licence; check by hand."}
}

func gpl2Only(expr string) bool {
	if Classify(expr) != StrongCopyleft {
		return false
	}
	for _, id := range ids(expr) {
		u := strings.ToUpper(id)
		if u == "GPL-2.0-ONLY" || u == "GPL-2.0" {
			return true
		}
	}
	return false
}

// hasAlternative reports whether expr offers an OR choice other than id.
func hasAlternative(expr, id string) bool {
	alts := splitTop(expr, " OR ")
	if len(alts) < 2 {
		return false
	}
	for _, a := range alts {
		if !strings.Contains(strings.ToUpper(a), id) {
			return true
		}
	}
	return false
}

func display(expr string) string {
	if expr == "" {
		return "an unknown licence"
	}
	return Normalize(expr)
}

// weakCopyleftReason explains the obligation of each weak-copyleft family;
// they differ in where the copyleft stops.
func weakCopyleftReason(dep, dl string) string {
	for _, id := range ids(dep) {
		switch k := key(id); {
		case strings.HasPrefix(k, "LGPL"):
			return dl + " is weak copyleft: you may use it unmodified, but users must be able to replace the LGPL library with their own version. That is easy with dynamic linking and hard for statically linked binaries such as Go, which then need object files or source to relink."
		case strings.HasPrefix(k, "CC-BY-SA"):
			return dl + " is share-alike: the work itself may be used, but adaptations of it must be published under the same licence."
		case strings.HasPrefix(k, "GPL"):
			return dl + " carries a linking exception: you may link it into a differently licensed program, but changes to the library itself stay under the GPL."
		}
	}
	return dl + " is weak copyleft at the file level: you may use it unmodified in any project; if you change its files, you must publish those changes under " + dl + "."
}
