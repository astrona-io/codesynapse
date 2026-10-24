#!/usr/bin/env bash
# Posts the review as one pull request comment and updates it on every push,
# found again by its marker. A token that may not comment (pull requests
# from forks) leaves a warning; the report is in the job summary anyway.
set -euo pipefail

marker='<!-- codesynapse-review -->'
if [ -z "${REPORT:-}" ] || [ ! -s "$REPORT" ]; then
  echo "::warning::No CodeSynapse report to post; see the review step."
  exit 0
fi

body="${RUNNER_TEMP:-/tmp}/codesynapse-comment.md"
{
  echo "$marker"
  # GitHub keeps comments under 65536 characters.
  head -c 60000 "$REPORT"
  if [ "$(wc -c < "$REPORT")" -gt 60000 ]; then
    printf '\n\n_The report is longer than a comment allows; the full report is in the job summary._\n'
  fi
} > "$body"

existing=$(gh api --paginate "repos/${REPO}/issues/${PR}/comments" \
  --jq ".[] | select(.body | startswith(\"${marker}\")) | .id" 2>/dev/null | head -n 1 || true)
if [ -n "$existing" ]; then
  gh api -X PATCH "repos/${REPO}/issues/comments/${existing}" -F "body=@${body}" > /dev/null ||
    echo "::warning::Could not update the CodeSynapse comment (the token may not write to pull requests)."
else
  gh api -X POST "repos/${REPO}/issues/${PR}/comments" -F "body=@${body}" > /dev/null ||
    echo "::warning::Could not post the CodeSynapse comment (the token may not write to pull requests)."
fi
