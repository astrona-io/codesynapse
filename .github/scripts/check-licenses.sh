#!/usr/bin/env sh
# Licences of what CodeSynapse distributes must be permissive (Apache-2.0
# project): the Go binaries' dependencies and the npm packages inside the
# built web app (.output; build tools such as Vite are not shipped).
# Run `just web-check` first so .output exists.
set -eu

allowed="Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,0BSD,Unlicense"

echo "Go dependencies of the codesynapse, codesynapse-api and codesynapse-mcp binaries"
log=$(mktemp)
trap 'rm -f "$log"' EXIT
status=0
go run github.com/google/go-licenses@v2.0.1 check \
	./cmd/codesynapse ./services/api/cmd/codesynapse-api ./services/mcp/cmd/codesynapse-mcp \
	--allowed_licenses="$allowed" >"$log" 2>&1 || status=$?
# Assembly files cannot be inspected; go-licenses warns about each one.
grep -v -e 'contains non-Go code' -e '\.s$' "$log" || true
[ "$status" -eq 0 ] || exit "$status"
echo "ok"

out=services/web/.output/server/node_modules
if [ ! -d "$out" ]; then
	echo "no $out: run just web-check first" >&2
	exit 1
fi
echo "npm packages in the built web app"
# shellcheck disable=SC2016 # the quoted text is JavaScript, not shell
ALLOWED="$allowed" node -e '
const fs = require("fs"), path = require("path")
const allowed = new Set(process.env.ALLOWED.split(","))
const bad = []
let n = 0
const walk = (dir) => {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    if (!e.isDirectory()) continue
    const p = path.join(dir, e.name)
    if (e.name.startsWith("@")) { walk(p); continue }
    const pj = path.join(p, "package.json")
    if (fs.existsSync(pj)) {
      const d = JSON.parse(fs.readFileSync(pj, "utf8"))
      const lic = typeof d.license === "string" ? d.license : (d.license && d.license.type) || ""
      // "(A OR B)" is fine when any choice is allowed.
      const ok = lic.replace(/[()]/g, "").split(/\s+OR\s+/).some((l) => allowed.has(l.trim()))
      n++
      if (!ok) bad.push(`${d.name}@${d.version}: ${lic || "none"}`)
    }
    const nested = path.join(p, "node_modules")
    if (fs.existsSync(nested)) walk(nested)
  }
}
walk(process.argv[1])
console.log(`${n} packages checked`)
if (bad.length) { console.error("not on the allowlist:\n  " + bad.join("\n  ")); process.exit(1) }
' "$out"
