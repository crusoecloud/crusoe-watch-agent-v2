#!/usr/bin/env bash
# compute-next-version.sh — autobump the per-mode release version from the
# most recent matching git tag. Writes ONLY the new version string to stdout.
#
# Usage:
#   compute-next-version.sh <vm|k8s|updater> [patch|minor]   (default: patch)

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

MODE="${1:?usage: compute-next-version.sh <vm|k8s|updater> [patch|minor]}"
BUMP="${2:-patch}"
case "$MODE" in vm|k8s|updater) ;; *) die "unknown mode: ${MODE}" ;; esac
case "$BUMP" in patch|minor) ;; *) die "unknown bump: ${BUMP}" ;; esac

# Pull tags so a brand-new clone (or shallow CI checkout) sees them.
git fetch --tags --quiet origin 2>/dev/null || true

# Pick the highest-numbered <mode>/v* tag using version sort.
latest=$(git tag -l "${MODE}/v*" | sort -V | tail -n1 || true)

# First release: vm/k8s continue the v1 agent line at v2; cwa-updater is its own line at v1.
if [[ -z "$latest" ]]; then
    case "$MODE" in
        updater) echo "v1.0.0" ;;
        *)       echo "v2.0.0" ;;
    esac
    exit 0
fi

if [[ ! "$latest" =~ ^${MODE}/v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    die "tag ${latest} does not match expected pattern ${MODE}/vX.Y.Z"
fi

major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"

if [[ "$BUMP" == minor ]]; then
    echo "v${major}.$((minor + 1)).0"
else
    echo "v${major}.${minor}.$((patch + 1))"
fi
