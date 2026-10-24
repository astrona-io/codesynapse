#!/usr/bin/env bash
# Reviews the checked-out change against its base branch with
# `codesynapse review --offline` and writes the Markdown report. The
# review never runs the code it reads: go list runs offline, git hooks and
# diff drivers are off (gitx).
set -euo pipefail

base="${BASE_REF:-main}"
report="${RUNNER_TEMP:-/tmp}/codesynapse-review.md"

# The base must be known and the history deep enough to find where the
# change left it.
if [ "$(git rev-parse --is-shallow-repository)" = "true" ]; then
  git fetch --no-tags --quiet --unshallow origin
fi
git fetch --no-tags --quiet origin "+refs/heads/${base}:refs/remotes/origin/${base}"

args=(review . --offline --base "origin/${base}" --head HEAD -o markdown)
if [ -n "${GITHUB_REPOSITORY:-}" ]; then
  args+=(--repo "$GITHUB_REPOSITORY")
fi
IFS=',' read -r -a files <<< "${COVERAGE:-}"
for f in "${files[@]}"; do
  [ -n "$f" ] && args+=(--coverage "$f")
done
IFS=',' read -r -a files <<< "${JUNIT:-}"
for f in "${files[@]}"; do
  [ -n "$f" ] && args+=(--junit "$f")
done
if [ -n "${FAIL_ON_NEW:-}" ]; then
  args+=(--fail-on-new "$FAIL_ON_NEW")
fi

status=0
"$CODESYNAPSE_BIN" "${args[@]}" > "$report" || status=$?
{
  echo "report=$report"
  if [ "$status" -ne 0 ] && grep -q "this change introduces" "$report"; then
    echo "introduced=true"
  else
    echo "introduced=false"
  fi
} >> "$GITHUB_OUTPUT"
cat "$report" >> "$GITHUB_STEP_SUMMARY"
exit "$status"
