#!/usr/bin/env sh
# Prints the image docker-compose.yml ships for a store, e.g.
#   compose-image.sh postgres   ->  docker.io/library/postgres:18-alpine
# The integration recipes in the justfile test exactly these, so a version bump in
# docker-compose.yml changes what the contracts run against (docs: "Integration
# images"). The argument is matched against the repository name of the image.
set -eu
file="$(dirname "$0")/../../docker-compose.yml"
img=$(sed -n "s#^[[:space:]]*image:[[:space:]]*\\(.*/$1:[^[:space:]]*\\)[[:space:]]*\$#\\1#p" "$file" | head -n 1)
if [ -z "$img" ]; then
    echo "compose-image.sh: no image named $1 in docker-compose.yml" >&2
    exit 1
fi
printf '%s\n' "$img"
