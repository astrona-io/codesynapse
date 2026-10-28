#!/usr/bin/env sh
# Checks the alert rules in deploy/alerts (`just lint-alerts`, part of
# `just checks`): vmalert parses every rule file (-dryRun), vmalert-tool runs
# the unit tests in deploy/alerts/tests, and no rule can put a tenant,
# repository, path, user or request content into an alert. Both images are
# the vmalert release docker-compose.yml ships (compose-image.sh), so the
# rules are checked by the version that evaluates them.
# usage: check-alerts.sh   (CONTAINER_ENGINE defaults to docker)
set -eu
cd "$(dirname "$0")/../.."
engine=${CONTAINER_ENGINE:-docker}
vmalert=$(sh .github/scripts/compose-image.sh vmalert)
tool=$(printf '%s\n' "$vmalert" | sed 's#/vmalert:#/vmalert-tool:#')

"$engine" run --rm -v "$PWD/deploy/alerts:/alerts:ro" "$vmalert" -dryRun -rule='/alerts/*.yaml'
"$engine" run --rm -v "$PWD/deploy/alerts:/alerts:ro" "$tool" unittest \
    --external.url=https://codesynapse.example.com --files=/alerts/tests/alerts.test.yaml

# Alerts group only by these labels and name only these in their text; a
# rule that needs another must be reviewed against AGENTS.md (Telemetry).
python3 - deploy/alerts/*.yaml <<'PY'
import re, sys
allowed = {"metric", "reason", "db_system", "le", "kind"}
bad = []
for path in sys.argv[1:]:
    text = open(path).read()
    if re.search(r"\bwithout\s*\(", text):
        bad.append(f"{path}: 'without' keeps every other label, tenant_id included")
    for group in re.findall(r"\bby\s*\(([^)]*)\)", text):
        for label in (l.strip() for l in group.split(",")):
            if label and label not in allowed:
                bad.append(f"{path}: groups by {label}")
    for label in re.findall(r"\$labels\.(\w+)", text):
        if label not in allowed:
            bad.append(f"{path}: names $labels.{label}")
    # A bare metric with no aggregation passes all its labels through.
    for expr in re.findall(r"expr:\s*(\|[^\n]*\n(?:\s{10,}.*\n?)+|.*)", text):
        if not re.search(r"\b(sum|max|min|avg|count|histogram_quantile)\b", expr):
            bad.append(f"{path}: an expression without aggregation keeps every label: {expr.strip()[:60]}")
if bad:
    print("alert rules:\n  " + "\n  ".join(bad), file=sys.stderr)
    sys.exit(1)
print("alert rules: aggregated, labelled only by " + ", ".join(sorted(allowed)))
PY
