#!/usr/bin/env bash
# release.sh — orchestrates a tag-anchored, per-mode release. Called from the
# manual GitLab release job for vm or k8s.
#
# Inputs (all via env, supplied by the GitLab job):
#   MODE                vm | k8s                    (required)
#   RELEASE_SHA         commit to release           (default: HEAD)
#   DRY_RUN             true | false                (default: false)
#   GHCR_REGISTRY       e.g. ghcr.io/crusoecloud/crusoe-watch-agent-v2
#   GHCR_TOKEN          ghcr push token
#   GHCR_USERNAME       ghcr user
#   GITHUB_REPO         e.g. crusoecloud/crusoe-watch-agent-v2
#   GITHUB_TOKEN        used by gh CLI
#   COSIGN_PRIVATE_KEY_B64
#   GIT_PUSH_URL        e.g. https://<user>:<token>@gitlab.com/<group>/<proj>.git
#
# What it does, in order:
#   1. Resolve RELEASE_SHA, compute next per-mode version (e.g. v1.4).
#   2. Check out a worktree at RELEASE_SHA (clean, isolated).
#   3. Render templates from RELEASE_SHA's dependencies.yaml.
#   4. Publish artifacts: cosign-sign; for k8s, push chart to ghcr.io and
#      move the per-mode "latest" pointer.
#   5. Generate notes from conventional-commit subjects in the tag range.
#   6. Create the GitHub Release (materializes the tag on the GitHub mirror).
#   7. Push the tag to GitLab — done last so a mid-flight failure leaves no
#      orphan tag, and the next attempt is a clean re-run at the same version.

set -euo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }
log() { echo "==> $*" >&2; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPTS="${REPO_ROOT}/ci/scripts"

MODE_PATHS_vm="vm/ dependencies.yaml"
MODE_PATHS_k8s="k8s/ dependencies.yaml"

mode_paths() {
    local v="MODE_PATHS_${1}"
    echo "${!v}"
}

# Write release notes to a temp file and echo the path.
generate_notes() {
    local new="$1"
    local out
    out=$(mktemp -t cwa-notes.XXXXXX)
    "${WORK}/ci/scripts/generate-release-notes.sh" "$MODE" "$new" > "$out"
    echo "$out"
}

publish_vm() {
    log "Signing VM script"
    local key=/tmp/cosign.key
    echo "$COSIGN_PRIVATE_KEY_B64" | base64 -d > "$key"
    chmod 600 "$key"

    local script="${RENDER_OUT}/crusoe_watch_agent.sh"
    cosign sign-blob --yes --key "$key" \
        --new-bundle-format=false \
        --bundle "${script}.bundle" \
        --output-signature "${script}.sig" \
        "$script"
    rm -f "$key"

    # Build the cwa-manager binary for native-mode installs.
    log "Building cwa-manager-linux-amd64"
    local binary="${RENDER_OUT}/cwa-manager-linux-amd64"
    ( cd "$WORK" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -ldflags "-X 'gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version.Version=${NEW_VERSION}'" \
        -o "$binary" \
        ./cmd/cwa-manager ) || die "go build cwa-manager failed"

    local notes_file
    notes_file=$(generate_notes "$NEW_TAG")

    log "Creating GitHub Release ${NEW_TAG}"
    local -a assets=(
        "${script}#crusoe_watch_agent.sh"
        "${script}.sig#crusoe_watch_agent.sh.sig"
        "${script}.bundle#crusoe_watch_agent.sh.bundle"
        "${binary}#cwa-manager-linux-amd64"
        "${RENDER_OUT}/VERSION#VERSION"
    )
    # Compose + systemd + config files travel with the script as assets so a
    # downloader doesn't need to clone the repo.
    for f in "${RENDER_OUT}/docker/"*.yaml \
             "${RENDER_OUT}/systemctl/"*.service \
             "${RENDER_OUT}/config/"*; do
        [[ -e "$f" ]] && assets+=("$f")
    done

    GH_REPO="$GITHUB_REPO" gh release create "$NEW_TAG" \
        --target "$RELEASE_SHA" \
        --title "VM Agent ${NEW_VERSION}" \
        ${notes_file:+--notes-file "$notes_file"} \
        --latest \
        "${assets[@]}"
}

publish_k8s() {
    local chart_dir="${RENDER_OUT}/helm-chart"
    local chart_version="${NEW_VERSION#v}"
    log "Packaging chart ${chart_version}"
    helm package "$chart_dir" -d "$RENDER_OUT"
    local tgz
    tgz=$(ls "${RENDER_OUT}/crusoe-watch-agent-${chart_version}"*.tgz)

    log "Pushing chart to ${GHCR_REGISTRY}"
    helm registry login -u "$GHCR_USERNAME" --password-stdin ghcr.io <<<"$GHCR_TOKEN"
    helm push "$tgz" "oci://${GHCR_REGISTRY}/charts"

    # docker login is needed for the `docker buildx imagetools create` below
    # that moves the :latest pointer. cosign sign uses its own auth flow.
    echo "$GHCR_TOKEN" | docker login -u "$GHCR_USERNAME" --password-stdin ghcr.io

    log "Signing chart digest"
    local key=/tmp/cosign.key
    echo "$COSIGN_PRIVATE_KEY_B64" | base64 -d > "$key"
    chmod 600 "$key"
    cosign sign --yes --key "$key" \
        "${GHCR_REGISTRY}/charts/crusoe-watch-agent:${chart_version}"
    rm -f "$key"

    # Move the K8s "latest" pointer to this chart version.
    docker buildx imagetools create \
        --tag "${GHCR_REGISTRY}/charts/crusoe-watch-agent:latest" \
        "${GHCR_REGISTRY}/charts/crusoe-watch-agent:${chart_version}"

    local notes_file
    notes_file=$(generate_notes "$NEW_TAG")

    log "Creating GitHub Release ${NEW_TAG}"
    GH_REPO="$GITHUB_REPO" gh release create "$NEW_TAG" \
        --target "$RELEASE_SHA" \
        --title "K8s Agent ${NEW_VERSION}" \
        ${notes_file:+--notes-file "$notes_file"} \
        "${tgz}#crusoe-watch-agent-${chart_version}.tgz"
}

###############################################################################
# Main
###############################################################################
MODE="${MODE:-}"
RELEASE_SHA="${RELEASE_SHA:-}"
DRY_RUN="${DRY_RUN:-false}"

case "$MODE" in vm|k8s) ;; *) die "MODE must be vm or k8s" ;; esac

if [[ -z "$RELEASE_SHA" ]]; then
    RELEASE_SHA="$(git rev-parse HEAD)"
fi
git cat-file -e "${RELEASE_SHA}^{commit}" 2>/dev/null \
    || die "RELEASE_SHA ${RELEASE_SHA} is not a commit in this repo"

NEW_VERSION="$("${SCRIPTS}/compute-next-version.sh" "$MODE")"
NEW_TAG="${MODE}/${NEW_VERSION}"

log "Mode:          ${MODE}"
log "Release SHA:   ${RELEASE_SHA}"
log "Computed tag:  ${NEW_TAG}"

if git rev-parse "$NEW_TAG" >/dev/null 2>&1; then
    die "tag ${NEW_TAG} already exists — autobump is wrong or this is a re-run; investigate"
fi

if [[ "$DRY_RUN" == "true" ]]; then
    log "DRY_RUN — printing commit range and rendered output, then exiting."
    PREV_TAG=$(git tag -l "${MODE}/v*" | sort -V | tail -n1 || true)
    if [[ -n "$PREV_TAG" ]]; then
        log "Commits in ${PREV_TAG}..${RELEASE_SHA}:"
        git log --first-parent --oneline "${PREV_TAG}..${RELEASE_SHA}" -- \
            $(mode_paths "$MODE") >&2 || true
    else
        log "No previous ${MODE} tag — this would be the first release."
    fi

    OUT="$(mktemp -d)"
    git -C "$REPO_ROOT" archive --format=tar "$RELEASE_SHA" | tar -x -C "$OUT"
    ( cd "$OUT" && "${OUT}/ci/scripts/render.sh" "$MODE" "$NEW_VERSION" "${OUT}/_out" )
    log "Rendered output at ${OUT}/_out"
    exit 0
fi

# Render from the chosen commit in an isolated worktree, never from live main.
# Tag after publish succeeds so a failure mid-flight doesn't leave an orphan tag.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"; git worktree prune' EXIT
git worktree add --detach "$WORK" "$RELEASE_SHA" >&2
RENDER_OUT="${WORK}/_render"
"${WORK}/ci/scripts/render.sh" "$MODE" "$NEW_VERSION" "$RENDER_OUT"

case "$MODE" in
    vm)  publish_vm  ;;
    k8s) publish_k8s ;;
esac

log "Pushing tag ${NEW_TAG} to GitLab"
git tag -a "$NEW_TAG" "$RELEASE_SHA" -m "Release ${NEW_TAG}"
git push "${GIT_PUSH_URL:?GIT_PUSH_URL required}" "$NEW_TAG"

log "Release ${NEW_TAG} complete."
