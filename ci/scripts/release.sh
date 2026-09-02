#!/usr/bin/env bash
# release.sh — orchestrates a tag-anchored, per-mode release. Called from the
# manual GitLab release job for vm or k8s.
#
# Inputs (all via env, supplied by the GitLab job):
#   MODE                vm | k8s | updater          (required)
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
#   4. Publish artifacts: cosign-sign; for k8s/updater, push the mode's chart to
#      ghcr.io and move that chart's "latest" pointer.
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
MODE_PATHS_k8s="k8s/helm-chart/ dependencies.yaml"
MODE_PATHS_updater="k8s/cwa-updater-chart/ cmd/cwa-updater/ dependencies.yaml"

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
        "$script"
    rm -f "$key"

    # Build native-mode binaries for both host architectures and add them as
    # release assets. NVIDIA hosts are amd64 except GB200, which are arm64.
    local -a assets=(
        "${script}#crusoe_watch_agent.sh"
        "${script}.bundle#crusoe_watch_agent.sh.bundle"
        "${RENDER_OUT}/VERSION#VERSION"
    )
    local ldflags="-s -w -X 'gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version.Version=${NEW_VERSION}'"
    local arch cmd out
    for arch in amd64 arm64; do
        for cmd in cwa-manager report-runner; do
            out="${RENDER_OUT}/${cmd}-linux-${arch}"
            log "Building ${cmd}-linux-${arch}"
            ( cd "$WORK" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build \
                -ldflags "$ldflags" \
                -o "$out" \
                "./cmd/${cmd}" ) || die "go build ${cmd} (${arch}) failed"
            assets+=("${out}#${cmd}-linux-${arch}")
        done
    done

    local notes_file
    notes_file=$(generate_notes "$NEW_TAG")

    log "Creating GitHub Release ${NEW_TAG}"
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
    local chart_version="${NEW_VERSION#v}"

    publish_chart crusoe-watch-agent helm-chart "$chart_version" "K8s Agent ${NEW_VERSION}"
}

publish_updater() {
    local chart_version="${NEW_VERSION#v}"

    publish_chart cwa-updater cwa-updater-chart "$chart_version" "cwa-updater ${NEW_VERSION}"
}

# Package, push, sign, and release a single rendered chart.
#   $1 chart name (also the OCI repo name)   $2 rendered dir under RENDER_OUT
#   $3 chart version                         $4 GitHub Release title
publish_chart() {
    local chart="$1" src_dir="$2" chart_version="$3" title="$4"
    local tgz

    log "Packaging ${chart} ${chart_version}"
    helm package "${RENDER_OUT}/${src_dir}" -d "$RENDER_OUT"
    tgz=$(ls "${RENDER_OUT}/${chart}-${chart_version}"*.tgz)

    log "Pushing ${chart} to ${GHCR_REGISTRY}"
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
        "${GHCR_REGISTRY}/charts/${chart}:${chart_version}"
    rm -f "$key"

    # cwa-updater verifies every chart it upgrades to against the public key.
    log "Verifying the signature with the committed public key"
    cosign verify --key "${WORK}/ci/cosign.pub" \
        "${GHCR_REGISTRY}/charts/${chart}:${chart_version}" > /dev/null

    # Move this chart's "latest" pointer to the version just pushed.
    docker buildx imagetools create \
        --tag "${GHCR_REGISTRY}/charts/${chart}:latest" \
        "${GHCR_REGISTRY}/charts/${chart}:${chart_version}"

    local notes_file
    notes_file=$(generate_notes "$NEW_TAG")

    log "Creating GitHub Release ${NEW_TAG}"
    GH_REPO="$GITHUB_REPO" gh release create "$NEW_TAG" \
        --target "$RELEASE_SHA" \
        --title "$title" \
        ${notes_file:+--notes-file "$notes_file"} \
        "${tgz}#${chart}-${chart_version}.tgz"
}

###############################################################################
# Main
###############################################################################
MODE="${MODE:-}"
RELEASE_SHA="${RELEASE_SHA:-}"
DRY_RUN="${DRY_RUN:-false}"

case "$MODE" in vm|k8s|updater) ;; *) die "MODE must be vm, k8s or updater" ;; esac

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
    vm)      publish_vm      ;;
    k8s)     publish_k8s     ;;
    updater) publish_updater ;;
esac

log "Pushing tag ${NEW_TAG} to GitLab"
git tag -a "$NEW_TAG" "$RELEASE_SHA" -m "Release ${NEW_TAG}"
git push "${GIT_PUSH_URL:?GIT_PUSH_URL required}" "$NEW_TAG"

log "Release ${NEW_TAG} complete."
