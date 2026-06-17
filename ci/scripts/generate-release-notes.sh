#!/usr/bin/env bash
# generate-release-notes.sh — emit Markdown release notes for the range
# <previous-tag>..<current-tag> in this repo, scoped to one mode (vm or k8s),
# by grouping commits by their conventional-commit type prefix. Writes to
# stdout only; the release pipeline captures the output and attaches it to
# the GitHub Release body. We never commit a CHANGELOG.md back to the repo.
#
# Usage:
#   generate-release-notes.sh <mode> <new-tag>            (auto-finds prev tag)
#   generate-release-notes.sh <mode> <prev-tag> <new-tag>
#
# Conventional-commit grouping:
#   feat:                              -> **Features**
#   fix:                               -> **Bug Fixes**
#   perf:, refactor:                   -> **Improvements**
#   chore:, docs:, ci:, test:, build:  -> skipped (not customer-facing)
#   anything without a type prefix     -> **Other**
#
# Scope suffixes like `feat(vm):` are honored; the scope is dropped from the
# output. Breaking-change markers (`feat!:`, `BREAKING CHANGE:`) are surfaced
# as a top-level **Breaking Changes** section above the regular groups.

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }

# Commit links in rendered notes point at the public GitHub mirror.
COMMIT_URL_BASE="https://github.com/crusoecloud/crusoe-watch-agent-v2/commit/"

# Per-mode path scope. dependencies.yaml counts for both modes since external
# pin bumps land there.
mode_paths() {
    case "$1" in
        vm)  echo "vm/ dependencies.yaml" ;;
        k8s) echo "k8s/ dependencies.yaml" ;;
        *)   die "unknown mode: $1 (expected vm or k8s)" ;;
    esac
}

SOURCE_PATHS=""

# Find the predecessor tag for this mode by version sort, excluding the new one.
prev_tag_for() {
    local mode="$1" new="$2"
    git tag -l "${mode}/v*" \
        | grep -v -F "$new" \
        | sort -V \
        | awk 'BEGIN{p=""} { p=$0 } END{ if (p) print p }'
}

# Print `<sha> <subject>` lines for commits in $prev..$new touching SOURCE_PATHS.
list_commits() {
    local prev="$1" new="$2"
    git log --first-parent --format='%H %s' "${prev}..${new}" -- ${SOURCE_PATHS}
}

# Group commits by their conventional-commit type and print one Markdown
# section per non-empty group. Reads `<sha> <subject>` lines from stdin.
emit_grouped() {
    local feats="" fixes="" improvements="" other="" breaking=""

    while IFS= read -r line; do
        [[ -n "$line" ]] || continue
        local sha="${line%% *}" subject="${line#* }"
        local short="${sha:0:7}"
        local link="(${short})"
        [[ -n "$COMMIT_URL_BASE" ]] && link="([${short}](${COMMIT_URL_BASE}${sha}))"

        # type[(scope)][!]: subject — parse with bash regex. Assign the
        # pattern to a variable so bash doesn't try to interpret the parens
        # as quoting in the [[ =~ ]] expression.
        local type="" bang="" message="$subject"
        local cc_re='^([a-zA-Z]+)(\([^)]+\))?(!)?:[[:space:]]*(.*)$'
        if [[ "$subject" =~ $cc_re ]]; then
            type=$(printf '%s' "${BASH_REMATCH[1]}" | tr '[:upper:]' '[:lower:]')
            bang="${BASH_REMATCH[3]}"
            message="${BASH_REMATCH[4]}"
        fi

        # Capitalize first letter so bullets look polished.
        message="$(printf '%s' "${message:0:1}" | tr '[:lower:]' '[:upper:]')${message:1}"
        local bullet="- ${message} ${link}"

        if [[ -n "$bang" ]]; then
            breaking+="${bullet}"$'\n'
        fi

        case "$type" in
            feat)               feats+="${bullet}"$'\n' ;;
            fix)                fixes+="${bullet}"$'\n' ;;
            perf|refactor)      improvements+="${bullet}"$'\n' ;;
            chore|docs|ci|test|build|style)
                                ;; # skipped — not customer-facing
            *)                  other+="${bullet}"$'\n' ;;
        esac
    done

    local emitted=0
    if [[ -n "$breaking" ]];     then echo "**Breaking Changes**"; echo;     echo -n "$breaking";     echo; emitted=1; fi
    if [[ -n "$feats" ]];        then echo "**Features**";         echo;     echo -n "$feats";        echo; emitted=1; fi
    if [[ -n "$improvements" ]]; then echo "**Improvements**";     echo;     echo -n "$improvements"; echo; emitted=1; fi
    if [[ -n "$fixes" ]];        then echo "**Bug Fixes**";        echo;     echo -n "$fixes";        echo; emitted=1; fi
    if [[ -n "$other" ]];        then echo "**Other**";            echo;     echo -n "$other";        echo; emitted=1; fi

    if [[ $emitted -eq 0 ]]; then
        echo "Internal improvements and maintenance."
    fi
}

usage() {
    cat >&2 <<EOF
Usage:
  $0 <mode> <new-tag>             # auto-detect previous tag for <mode>
  $0 <mode> <prev-tag> <new-tag>  # explicit range

Modes: vm | k8s.
Output: Markdown release notes on stdout, grouped by conventional-commit type.
EOF
    exit 1
}

main() {
    [[ $# -ge 2 ]] || usage
    local mode="$1"; shift
    SOURCE_PATHS=$(mode_paths "$mode")

    local prev new
    if [[ $# -eq 1 ]]; then
        new="$1"
        prev=$(prev_tag_for "$mode" "$new")
        # First release for this mode — diff from the repo's root commit.
        [[ -n "$prev" ]] || prev=$(git rev-list --max-parents=0 HEAD | head -n1)
    elif [[ $# -eq 2 ]]; then
        prev="$1"; new="$2"
    else
        usage
    fi

    echo "Generating notes for ${mode}: ${prev}..${new}" >&2
    list_commits "$prev" "$new" | emit_grouped
}

main "$@"
