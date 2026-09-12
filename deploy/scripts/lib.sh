#!/bin/sh
# Shared helpers for the metiche deploy scripts. Sourced, not executed.
#
# POSIX sh throughout — the box runs Ubuntu, where /bin/sh is dash, and every
# script here is checked with `sh -n`. No bashisms, no arrays, no [[ ]].
#
# set -e but deliberately NOT pipefail: pipefail is not POSIX, and the one
# pipeline that matters (reading from /dev/urandom into head) relies on the
# left side being killed by SIGPIPE without failing the command.

# ── Defaults. Every one overridable from the environment. ────────────────────
: "${METICHE_NAMESPACE:=metiche}"
: "${METICHE_CRED_DIR:=/etc/metiche}"
: "${METICHE_CRED_FILE:=${METICHE_CRED_DIR}/credentials.env}"
: "${METICHE_CONFIG_FILE:=${METICHE_CRED_DIR}/prod.yaml}"
: "${METICHE_DB_SECRET:=metiche-db}"
: "${METICHE_CONFIG_SECRET:=metiche-config}"
: "${METICHE_TARGET_ARCH:=x86_64}"
: "${METICHE_BACKEND_IMAGE:=metiche}"
: "${METICHE_WEB_IMAGE:=metiche-web}"
: "${METICHE_MYSQL_IMAGE:=mysql:8.0}"
: "${METICHE_BUILDER:=docker}"
: "${KUBECTL:=kubectl}"
: "${HELM:=helm}"
: "${CTR:=microk8s ctr}"

# Repository root, derived from this file's location. No absolute path is
# written down anywhere, so a checkout can live wherever it likes on the box.
# METICHE_DEPLOY_DIR may be set by the caller before sourcing this file — the
# top-level deploy.sh does, because it lives one directory up and deriving the
# path from $0 here would then point one directory too high.
if [ -z "${METICHE_DEPLOY_DIR:-}" ]; then
    METICHE_SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
    METICHE_DEPLOY_DIR=$(CDPATH='' cd -- "${METICHE_SCRIPT_DIR}/.." && pwd)
fi
METICHE_SCRIPT_DIR="${METICHE_DEPLOY_DIR}/scripts"
METICHE_REPO_ROOT=$(CDPATH='' cd -- "${METICHE_DEPLOY_DIR}/.." && pwd)
METICHE_CHART_DIR="${METICHE_DEPLOY_DIR}/.helm"

log()  { printf '  %s\n' "$*" >&2; }
step() { printf '\n==> %s\n' "$*" >&2; }
warn() { printf '  !! %s\n' "$*" >&2; }
die()  { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "$1 is not on PATH. $2"
}

need_root() {
    [ "$(id -u)" = "0" ] || die "$1 must run as root: it reads and writes ${METICHE_CRED_DIR}, which is root-owned 0600 so that a credential is not readable by every account on a shared box. Re-run with sudo."
}

confirm() {
    # Skipped entirely when METICHE_YES=1, so the orchestrator can run
    # unattended once a human has read the plan once.
    [ "${METICHE_YES:-0}" = "1" ] && return 0
    printf '\n%s [y/N] ' "$1" >&2
    read -r _answer || return 1
    case "$_answer" in
        y|Y|yes|YES) return 0 ;;
        *) return 1 ;;
    esac
}

# ── Architecture guard ───────────────────────────────────────────────────────
# The laptop this is written on is arm64 and the box is amd64. A `docker build`
# on the laptop produces an arm64 image that the box will refuse with
# "exec format error" — or worse, a `--platform linux/amd64` build that
# silently runs every compile step under qemu, takes half an hour, and
# occasionally miscompiles. So: build on the box, and refuse to build anywhere
# whose architecture does not match the target.
assert_native_arch() {
    _arch=$(uname -m)
    [ "$_arch" = "${METICHE_TARGET_ARCH}" ] && return 0
    die "this machine is ${_arch}, the deployment target is ${METICHE_TARGET_ARCH}.

Images must be built ON the box, natively. Cross-building here would either
produce an image the box cannot execute, or (with --platform) an emulated
build that is slow and not bit-identical to a native one.

Get the source onto the box and run this there. If you genuinely mean to build
for ${_arch} — a different box, or a local kind cluster — set
METICHE_TARGET_ARCH=${_arch} explicitly."
}

# ── Cluster helpers ──────────────────────────────────────────────────────────
kube() { "${KUBECTL}" -n "${METICHE_NAMESPACE}" "$@"; }

namespace_exists() {
    "${KUBECTL}" get namespace "${METICHE_NAMESPACE}" >/dev/null 2>&1
}

ensure_namespace() {
    if namespace_exists; then
        log "namespace ${METICHE_NAMESPACE} exists"
        return 0
    fi
    # The ONLY cluster-scoped object anything here creates, and it creates it
    # by name so it can never touch another project's namespace on this box.
    log "creating namespace ${METICHE_NAMESPACE}"
    "${KUBECTL}" create namespace "${METICHE_NAMESPACE}"
}

mysql_pod() {
    kube get pod \
        -l app.kubernetes.io/name=metiche-mysql,app.kubernetes.io/part-of=metiche \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# ── Secret helpers ───────────────────────────────────────────────────────────
# Both of these pass FILE PATHS to kubectl, never values. A value on a command
# line is in `ps` output, in the shell history, and in any process listing the
# box's other tenants can read.
#
# The generated manifest goes straight down a pipe into `kubectl apply` and is
# never written to disk or to a terminal.
#
# --namespace on BOTH halves, deliberately: `kubectl create --dry-run=client`
# does stamp metadata.namespace into its output, but relying on that would
# mean a future kubectl that stopped doing so would silently write metiche's
# credentials into whatever namespace the current context happens to point at
# — on a box shared with other projects. Repeating the flag makes a mismatch
# an error instead.
secret_from_env_file() {
    # $1 secret name, $2 env-file path
    [ -f "$2" ] || die "credentials file $2 does not exist. Run gen-credentials.sh first."
    "${KUBECTL}" create secret generic "$1" \
        --namespace "${METICHE_NAMESPACE}" \
        --from-env-file="$2" \
        --dry-run=client -o yaml \
    | "${KUBECTL}" apply --namespace "${METICHE_NAMESPACE}" -f - >/dev/null
    log "secret/$1 applied (${METICHE_NAMESPACE})"
}

secret_from_file() {
    # $1 secret name, $2 key inside the secret, $3 path on disk
    [ -f "$3" ] || die "$3 does not exist. Run gen-credentials.sh first."
    "${KUBECTL}" create secret generic "$1" \
        --namespace "${METICHE_NAMESPACE}" \
        --from-file="$2=$3" \
        --dry-run=client -o yaml \
    | "${KUBECTL}" apply --namespace "${METICHE_NAMESPACE}" -f - >/dev/null
    log "secret/$1 applied (${METICHE_NAMESPACE}, key $2)"
}
