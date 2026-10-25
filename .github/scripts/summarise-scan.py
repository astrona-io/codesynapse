"""One line per image: vulnerabilities by severity, and how many have a fix.

Reads a Trivy or Grype JSON report. A missing or unreadable report prints
"no report" instead of failing: scans of upstream images only report.
"""

import collections
import json
import sys

scanner, ref, path = sys.argv[1:4]
found = []  # (severity, fixed?)
try:
    with open(path, encoding="utf-8") as f:
        doc = json.load(f)
    if scanner == "trivy":
        for res in doc.get("Results") or []:
            for v in res.get("Vulnerabilities") or []:
                found.append((v.get("Severity", "UNKNOWN").upper(), bool(v.get("FixedVersion"))))
    else:
        for m in doc.get("matches") or []:
            v = m.get("vulnerability", {})
            found.append((v.get("severity", "Unknown").upper(), bool((v.get("fix") or {}).get("versions"))))
except (OSError, ValueError):
    print(f"{ref}: no report")
    sys.exit(0)

by = collections.Counter(s for s, _ in found)
fixable = sum(1 for _, ok in found if ok)
order = ["CRITICAL", "HIGH", "MEDIUM", "LOW", "NEGLIGIBLE", "UNKNOWN"]
parts = ", ".join(f"{by[s]} {s.lower()}" for s in order if by[s])
print(f"{ref} ({scanner}): {len(found)} total, {fixable} with a fix" + (f" ({parts})" if parts else ""))
