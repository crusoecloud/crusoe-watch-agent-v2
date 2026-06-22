#!/usr/bin/env bash
# compute-next-version.sh — autobump the per-mode release version from the
# most recent matching git tag. Writes ONLY the new version string to stdout.
#
# Usage:
#   compute-next-version.sh vm
#   compute-next-version.sh k8s

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

MODE="${1:?usage: compute-next-version.sh <vm|k8s>}"
case "$MODE" in vm|k8s) ;; *) die "unknown mode: ${MODE}" ;; esac

# Pull tags so a brand-new clone (or shallow CI checkout) sees them.
git fetch --tags --quiet origin 2>/dev/null || true

# Pick the highest-numbered <mode>/vX.Y tag using version sort.
latest=$(git tag -l "${MODE}/v*" | sort -V | tail -n1 || true)

if [[ -z "$latest" ]]; then
    echo "v2.0"
    exit 0
fi

# Parse vX.Y. Anything else is treated as an error.
if [[ ! "$latest" =~ ^${MODE}/v([0-9]+)\.([0-9]+)$ ]]; then
    die "tag ${latest} does not match expected pattern ${MODE}/vX.Y"
fi

major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
echo "v${major}.$((minor + 1))"
