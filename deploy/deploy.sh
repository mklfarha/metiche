#!/bin/sh
# deploy.sh — put metiche on the box, in order, from a checkout ON the box.
#
#   sudo deploy/deploy.sh                 build, credentials, all three charts, schema
#   sudo deploy/deploy.sh --no-build      redeploy with METICHE_TAG's existing images
#   sudo deploy/deploy.sh --charts-only   charts only; no build, no credential work
#       deploy/deploy.sh --preflight      read-only: print every assumption, change nothing
#
# ─────────────────────────────────────────────────────────────────────────────
# WHAT THIS IS, AND WHAT IT IS NOT
#
# The project README states metiche's deployment contract in full: one Go
# binary, one MySQL 8 database it owns, two HTTP surfaces, TLS in front that
# does not buffer and does not cut idle streams, config from outside the
# image, and no egress. A laptop, a VPS with systemd, or two services in a
# docker-compose file satisfy that contract exactly as well as this does.
#
# This script is ONE implementation of it — Kubernetes, on a microk8s box that
# already exists and already runs other projects. Nothing in here is the
# blessed way to run metiche. If you are reading it to learn what metiche
# needs, read the README instead; this file is full of decisions that are
# about microk8s and about this particular box.
#
# WHAT IT ASSUMES ABOUT A BOX IT DID NOT BUILD
#   * microk8s is installed and running, with ingress-nginx and cert-manager,
#     both already serving other projects.
#   * An IngressClass exists. The charts default to "public" (microk8s'
#     addon); upstream ingress-nginx registers "nginx" instead.
#   * A ClusterIssuer exists and is shared. The charts REFERENCE it by name
#     and never create or modify one — it is cluster-scoped.
#   * A StorageClass exists; the charts default to "microk8s-hostpath".
#   * metiche.xyz, api.metiche.xyz and mcp.metiche.xyz resolve to this box.
#
# `deploy/deploy.sh --preflight` prints all five as they actually are.
#
# WHAT IT TOUCHES OUTSIDE THE metiche NAMESPACE: the namespace object itself,
# created by name if absent. Nothing else. No chart here renders a
# ClusterRole, ClusterRoleBinding, IngressClass, StorageClass,
# PersistentVolume, CRD, webhook or PriorityClass, and none of them writes
# into another namespace.
# ─────────────────────────────────────────────────────────────────────────────

set -eu

METICHE_DEPLOY_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
export METICHE_DEPLOY_DIR
. "${METICHE_DEPLOY_DIR}/scripts/lib.sh"

DO_BUILD=1
DO_CREDS=1
DO_SCHEMA=1

case "${1:-}" in
    "")            ;;
    --no-build)    DO_BUILD=0 ;;
    --charts-only) DO_BUILD=0; DO_CREDS=0; DO_SCHEMA=0 ;;
    --preflight)   exec "${METICHE_DEPLOY_DIR}/scripts/preflight.sh" ;;
    -h|--help)     sed -n '2,45p' "$0"; exit 0 ;;
    *)             die "unknown argument: $1 (try --help)" ;;
esac

printf '\n'
printf '  metiche deploy\n'
printf '  namespace: %s\n' "${METICHE_NAMESPACE}"
printf '  repo:      %s\n' "${METICHE_REPO_ROOT}"
printf '\n'

# ── 0. Preflight, always ─────────────────────────────────────────────────────
# Read-only, and it prints the assumptions rather than asserting them. On a
# box somebody else set up, the assumptions are the risk.
"${METICHE_DEPLOY_DIR}/scripts/preflight.sh"

confirm "Proceed with the deployment above?" || die "aborted. Nothing was changed."

# ── 1. Credentials ───────────────────────────────────────────────────────────
# First, because everything downstream consumes the Secrets it creates.
# Idempotent: an existing credentials file is kept, not regenerated.
if [ "${DO_CREDS}" = "1" ]; then
    need_root "deploy.sh (it generates credentials; use --charts-only to skip that)"
    "${METICHE_DEPLOY_DIR}/scripts/gen-credentials.sh"
fi

# ── 2. Images ────────────────────────────────────────────────────────────────
# Built here, natively, and imported straight into containerd. build-images.sh
# refuses to run on an architecture that is not the target, which is the whole
# point of doing this step on the box.
if [ "${DO_BUILD}" = "1" ]; then
    METICHE_TAG=$("${METICHE_DEPLOY_DIR}/scripts/build-images.sh")
    export METICHE_TAG
else
    [ -n "${METICHE_TAG:-}" ] || die "--no-build/--charts-only needs METICHE_TAG set to an image tag that is already in containerd.
List what is there:  ${CTR} images ls | grep metiche"
fi
log "deploying tag ${METICHE_TAG}"

# ── 3. Database, then schema, then the rest ──────────────────────────────────
# The order matters exactly once: the backend's first action is to open a
# connection pool, so MySQL must be up and its tables must exist before the
# backend pod starts, or the first rollout fails its readiness probe and the
# deploy looks broken when it is merely early.
"${METICHE_DEPLOY_DIR}/scripts/helm-deploy.sh" mysql

if [ "${DO_SCHEMA}" = "1" ]; then
    "${METICHE_DEPLOY_DIR}/scripts/apply-schema.sh"
fi

"${METICHE_DEPLOY_DIR}/scripts/helm-deploy.sh" backend
"${METICHE_DEPLOY_DIR}/scripts/helm-deploy.sh" web

# ── 4. What to look at ───────────────────────────────────────────────────────
step "Deployed"
kube get pods,svc,ingress 2>/dev/null | sed 's/^/  /' || true

step "Certificates"
log "cert-manager issues these asynchronously; READY goes True within a minute"
log "or two once DNS and the HTTP-01 challenge have both worked."
kube get certificate 2>/dev/null | sed 's/^/  /' || log "(no Certificate resources yet)"

step "The one check that catches the mistake this deployment is most likely to make"
log "SSE must stream, and must NOT stop after ~60 seconds:"
log ""
log "  curl -N https://metiche.xyz/t/<slug>/stream"
log ""
log "If it dies at a minute, or prints nothing and then a burst, the proxy is"
log "buffering or timing out — look at the Ingress annotations, not the board:"
log ""
log "  kubectl -n ${METICHE_NAMESPACE} get ingress -o yaml | grep -A1 proxy-"
