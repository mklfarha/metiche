#!/bin/sh
# helm-deploy.sh — `helm upgrade --install` one or all of the three charts.
#
#   deploy/scripts/helm-deploy.sh [mysql|backend|web|all] [--dry-run]
#
# Environment:
#   METICHE_TAG           image tag to deploy (required for backend and web)
#   METICHE_NAMESPACE     default: metiche
#   METICHE_INGRESS_CLASS override ingress.className on every chart
#   METICHE_ISSUER        override ingress.clusterIssuer on every chart
#   METICHE_STORAGE_CLASS override persistence.storageClass on metiche-mysql
#   METICHE_VALUES_<NAME> path to an extra -f values file per chart, e.g.
#                         METICHE_VALUES_BACKEND=/etc/metiche/backend.yaml
#
# NOTHING SENSITIVE IS PASSED HERE. No --set carries a password, and there is
# no values file in this repo that contains one. The credentials reach the
# pods only through the two Secrets that gen-credentials.sh created, which the
# charts reference by name. If you ever find yourself wanting `--set
# db.password=...`, stop: it would land in the release's stored values, which
# `helm get values` prints to anyone with namespace read.

set -eu
. "$(dirname -- "$0")/lib.sh"

# Optional overrides, defaulted so `set -u` and the ${VAR:+...} forms below
# behave the same whether or not the caller exported them.
: "${METICHE_STORAGE_CLASS:=}"
: "${METICHE_VALUES_MYSQL:=}"
: "${METICHE_VALUES_BACKEND:=}"
: "${METICHE_VALUES_WEB:=}"

WHAT="${1:-all}"
DRY=""
case "${2:-}" in
    --dry-run) DRY="--dry-run" ;;
    "") ;;
    *) die "unknown argument: $2" ;;
esac
case "${WHAT}" in
    -h|--help) sed -n '2,25p' "$0"; exit 0 ;;
    mysql|backend|web|all) ;;
    *) die "expected one of: mysql backend web all" ;;
esac

need_cmd "${HELM}" "Install helm 3, or set HELM='microk8s helm3'."
need_cmd "${KUBECTL}" "Install kubectl, or set KUBECTL='microk8s kubectl'."

if [ -z "${DRY}" ]; then ensure_namespace; fi

common_flags() {
    printf '%s' "--namespace ${METICHE_NAMESPACE}"
    if [ -n "${METICHE_INGRESS_CLASS:-}" ]; then
        printf ' --set ingress.className=%s' "${METICHE_INGRESS_CLASS}"
    fi
    if [ -n "${METICHE_ISSUER:-}" ]; then
        printf ' --set ingress.clusterIssuer=%s' "${METICHE_ISSUER}"
    fi
}

require_secret() {
    kube get secret "$1" >/dev/null 2>&1 \
        || die "secret/$1 is missing in ${METICHE_NAMESPACE}. Run: sudo deploy/scripts/gen-credentials.sh"
}

require_tag() {
    [ -n "${METICHE_TAG:-}" ] \
        || die "METICHE_TAG is not set. Build first: deploy/scripts/build-images.sh
The charts use imagePullPolicy: Never, so a tag that is not in containerd
gives ErrImageNeverPull rather than a pull attempt."
}

deploy_mysql() {
    step "metiche-mysql"
    if [ -z "${DRY}" ]; then require_secret "${METICHE_DB_SECRET}"; fi
    # shellcheck disable=SC2046,SC2086
    ${HELM} upgrade --install metiche-mysql "${METICHE_CHART_DIR}/metiche-mysql" \
        $(common_flags) \
        --set auth.existingSecret="${METICHE_DB_SECRET}" \
        ${METICHE_STORAGE_CLASS:+--set persistence.storageClass=${METICHE_STORAGE_CLASS}} \
        ${METICHE_VALUES_MYSQL:+-f ${METICHE_VALUES_MYSQL}} \
        --wait --timeout 10m ${DRY}
}

deploy_backend() {
    step "metiche (backend)"
    require_tag
    if [ -z "${DRY}" ]; then require_secret "${METICHE_CONFIG_SECRET}"; fi
    # shellcheck disable=SC2046,SC2086
    ${HELM} upgrade --install metiche "${METICHE_CHART_DIR}/metiche" \
        $(common_flags) \
        --set image.tag="${METICHE_TAG}" \
        --set config.secretName="${METICHE_CONFIG_SECRET}" \
        ${METICHE_VALUES_BACKEND:+-f ${METICHE_VALUES_BACKEND}} \
        --wait --timeout 5m ${DRY}
}

deploy_web() {
    step "metiche-web (board)"
    require_tag
    # backend.serviceName must match the backend release's fullname. It is
    # "metiche" here because deploy_backend installs the release under that
    # name; with the api/mcp split it becomes "metiche-api".
    # shellcheck disable=SC2046,SC2086
    ${HELM} upgrade --install metiche-web "${METICHE_CHART_DIR}/metiche-web" \
        $(common_flags) \
        --set image.tag="${METICHE_TAG}" \
        --set backend.serviceName=metiche \
        ${METICHE_VALUES_WEB:+-f ${METICHE_VALUES_WEB}} \
        --wait --timeout 5m ${DRY}
}

case "${WHAT}" in
    mysql)   deploy_mysql ;;
    backend) deploy_backend ;;
    web)     deploy_web ;;
    all)     deploy_mysql; deploy_backend; deploy_web ;;
esac

if [ -z "${DRY}" ]; then
    step "Releases in ${METICHE_NAMESPACE}"
    ${HELM} list --namespace "${METICHE_NAMESPACE}" | sed 's/^/  /'
fi
