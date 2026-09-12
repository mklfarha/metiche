#!/bin/sh
# build-images.sh — build the two metiche images ON THIS MACHINE and import
# them into microk8s's containerd.
#
#   deploy/scripts/build-images.sh [TAG]
#
# TAG defaults to the short git commit if this is a checkout, otherwise a UTC
# timestamp. It is printed at the end; deploy.sh passes it to helm.
#
# ─────────────────────────────────────────────────────────────────────────────
# WHY THIS RUNS ON THE BOX
#
# The box is amd64. A laptop is very likely arm64, and there are two ways to
# get that wrong:
#
#   * Build natively on the laptop and push. The image is arm64; every pod
#     dies instantly with "exec format error", which reads like a corrupt
#     image rather than an architecture mistake.
#
#   * Build with --platform linux/amd64 on the laptop. This works, and it is
#     the trap: every compile step runs under qemu, a Go build that takes 40
#     seconds takes 15 minutes, and you will do it again on every deploy.
#
# So there is no --platform anywhere in this script or in either Dockerfile,
# and the script refuses outright to run on an architecture that is not the
# target. Get the source onto the box (git clone, or rsync the checkout) and
# run it there.
#
# NO REGISTRY. The images go straight from the local builder into containerd
# via `docker save | ctr image import`, which is why the charts set
# imagePullPolicy: Never. Pointing this at a registry instead is a two-line
# change, and it buys nothing on a single-node cluster.
# ─────────────────────────────────────────────────────────────────────────────

set -eu
. "$(dirname -- "$0")/lib.sh"

case "${1:-}" in
    -h|--help) sed -n '2,45p' "$0"; exit 0 ;;
esac

assert_native_arch
need_cmd "${METICHE_BUILDER}" "Set METICHE_BUILDER=podman or =nerdctl if you use one of those."

# ── Tag ──────────────────────────────────────────────────────────────────────
TAG="${1:-${METICHE_TAG:-}}"
if [ -z "${TAG}" ]; then
    if command -v git >/dev/null 2>&1 && git -C "${METICHE_REPO_ROOT}" rev-parse --short HEAD >/dev/null 2>&1; then
        TAG=$(git -C "${METICHE_REPO_ROOT}" rev-parse --short HEAD)
        # A dirty tree would otherwise produce two different images with the
        # same tag, and imagePullPolicy: Never makes that impossible to spot.
        if ! git -C "${METICHE_REPO_ROOT}" diff --quiet HEAD 2>/dev/null; then
            TAG="${TAG}-dirty-$(date -u +%H%M%S)"
        fi
    else
        TAG=$(date -u +%Y%m%d%H%M%S)
    fi
fi
log "tag: ${TAG}"

BACKEND_CONTEXT="${METICHE_REPO_ROOT}/code/backend/metiche"
WEB_CONTEXT="${METICHE_REPO_ROOT}/code/frontend"
WEB_DOCKERFILE="${METICHE_DEPLOY_DIR}/docker/metiche-web.Dockerfile"

[ -f "${BACKEND_CONTEXT}/Dockerfile" ] || die "no Dockerfile at ${BACKEND_CONTEXT} — is this a full checkout?"
[ -f "${WEB_DOCKERFILE}" ] || die "missing ${WEB_DOCKERFILE}"
[ -d "${WEB_CONTEXT}" ] || die "no frontend at ${WEB_CONTEXT}"

TMPDIR_IMG=$(mktemp -d)
cleanup() { rm -rf "${TMPDIR_IMG}"; }
trap cleanup EXIT INT TERM

# ── Import into containerd ───────────────────────────────────────────────────
# Via a file rather than a pipe: `docker save | ctr import` hides a failure on
# the left-hand side unless pipefail is set, and pipefail is not POSIX. A
# temporary file costs a few hundred MB for a few seconds and turns a silent
# half-import into an error.
import_image() {
    _ref=$1
    _tar="${TMPDIR_IMG}/$(printf '%s' "${_ref}" | tr ':/' '__').tar"
    log "saving ${_ref}"
    "${METICHE_BUILDER}" save -o "${_tar}" "${_ref}" >&2
    log "importing ${_ref} into containerd"
    # microk8s' ctr wrapper already targets the k8s.io containerd namespace,
    # which is the one the kubelet looks in. Plain containerd needs
    # `ctr --namespace k8s.io`; set CTR accordingly if you are not on microk8s.
    #
    # >&2 on every command in this script, deliberately: the LAST line of
    # stdout is the tag and nothing else, so deploy.sh can capture it with
    # $(...). `ctr image import` and `docker pull` both chatter on stdout and
    # would otherwise end up inside the tag.
    # shellcheck disable=SC2086
    ${CTR} image import "${_tar}" >&2
    rm -f "${_tar}"
}

# ── Backend ──────────────────────────────────────────────────────────────────
step "Building ${METICHE_BACKEND_IMAGE}:${TAG} (code/backend/metiche)"
"${METICHE_BUILDER}" build \
    -t "${METICHE_BACKEND_IMAGE}:${TAG}" \
    -f "${BACKEND_CONTEXT}/Dockerfile" \
    "${BACKEND_CONTEXT}" >&2
import_image "${METICHE_BACKEND_IMAGE}:${TAG}"

# ── Board ────────────────────────────────────────────────────────────────────
step "Building ${METICHE_WEB_IMAGE}:${TAG} (code/frontend)"
"${METICHE_BUILDER}" build \
    -t "${METICHE_WEB_IMAGE}:${TAG}" \
    -f "${WEB_DOCKERFILE}" \
    "${WEB_CONTEXT}" >&2
import_image "${METICHE_WEB_IMAGE}:${TAG}"

# ── MySQL ────────────────────────────────────────────────────────────────────
# Pre-pulled and imported so that a deploy — or a pod rescheduled at 3am —
# does not depend on Docker Hub being up and on the rate limit not being hit.
step "Pre-pulling ${METICHE_MYSQL_IMAGE}"
if "${METICHE_BUILDER}" image inspect "${METICHE_MYSQL_IMAGE}" >/dev/null 2>&1; then
    log "already present locally"
else
    "${METICHE_BUILDER}" pull "${METICHE_MYSQL_IMAGE}" >&2
fi
import_image "${METICHE_MYSQL_IMAGE}"

step "Built and imported"
log "${METICHE_BACKEND_IMAGE}:${TAG}"
log "${METICHE_WEB_IMAGE}:${TAG}"
log "${METICHE_MYSQL_IMAGE}"
log ""
log "Verify containerd actually has them:"
log "  ${CTR} images ls | grep -E '${METICHE_BACKEND_IMAGE}|${METICHE_WEB_IMAGE}'"
log ""
log "Deploy with this tag:"
log "  METICHE_TAG=${TAG} sudo deploy/deploy.sh"

# The last line of stdout is the tag and nothing else, so deploy.sh can
# capture it. Everything above went to stderr.
printf '%s\n' "${TAG}"
