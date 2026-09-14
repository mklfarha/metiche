#!/bin/sh
# helm-deploy.sh — `helm upgrade --install` one or all of the three charts.
#
#   deploy/scripts/helm-deploy.sh [mysql|backend|web|all] [--dry-run]
#
# Environment:
#   METICHE_TAG           image tag to deploy (required for backend and web).
#                         Passed with --set-string, never --set: tags are
#                         timestamps (20260912213026) and Helm types an
#                         unquoted number as one. Do not `helm upgrade
#                         --reuse-values` by hand either — the stored number
#                         comes back as 2.0260912213026e+13 (InvalidImageName).
#   METICHE_NAMESPACE     default: metiche
#   METICHE_INGRESS_CLASS override ingress.className on every chart
#   METICHE_ISSUER        override ingress.clusterIssuer on every chart
#   METICHE_STORAGE_CLASS override persistence.storageClass on metiche-mysql
#   METICHE_VALUES_<NAME> path to an extra -f values file per chart, e.g.
#                         METICHE_VALUES_BACKEND=/etc/metiche/backend.yaml
#   METICHE_ALLOW_NETPOL_REMOVAL=1
#                         let a metiche-mysql upgrade remove the live
#                         NetworkPolicy (see below). Refused otherwise.
#
# THE metiche-mysql NETWORKPOLICY. `nuzur-agent-setup.sh netpol` enables it by
# writing /etc/metiche/metiche-mysql.networkpolicy.yaml (METICHE_CRED_DIR). A
# metiche-mysql upgrade here adds `-f` for that file whenever it exists, before
# METICHE_VALUES_MYSQL so an explicit file still wins. And before upgrading, it
# compares the live release (`helm get manifest`) with the new render (`helm
# template`, same flags): if the live release has a NetworkPolicy and the new
# render does not, it stops, unless METICHE_ALLOW_NETPOL_REMOVAL=1. So neither a
# missing file nor a run without sudo (the directory is 0700) can quietly
# reopen MySQL to every pod in the cluster.
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
: "${METICHE_ALLOW_NETPOL_REMOVAL:=0}"

WHAT="${1:-all}"
DRY=""
case "${2:-}" in
    --dry-run) DRY="--dry-run" ;;
    "") ;;
    *) die "unknown argument: $2" ;;
esac
case "${WHAT}" in
    -h|--help) sed -n '2,38p' "$0"; exit 0 ;;
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

# The -f files for metiche-mysql, in order: the NetworkPolicy values when the
# file exists, then METICHE_VALUES_MYSQL (later -f wins). The same file named
# twice is passed once, which is what `nuzur-agent-setup.sh netpol` used to do.
mysql_values_files() {
    if [ -f "${METICHE_MYSQL_NETPOL_VALUES}" ]; then
        printf ' -f %s' "${METICHE_MYSQL_NETPOL_VALUES}"
    fi
    if [ -n "${METICHE_VALUES_MYSQL}" ] && [ "${METICHE_VALUES_MYSQL}" != "${METICHE_MYSQL_NETPOL_VALUES}" ]; then
        printf ' -f %s' "${METICHE_VALUES_MYSQL}"
    fi
}

# Whether a manifest on stdin contains a NetworkPolicy object.
has_networkpolicy() { grep -Eq '^kind:[[:space:]]*NetworkPolicy[[:space:]]*$'; }

# Refuse an upgrade that would silently delete the live NetworkPolicy.
# $@ are exactly the flags the upgrade will be given.
guard_mysql_netpol() {
    # 2>&1: on failure the message decides between "no release yet" and
    # "could not ask", and those must not be confused — the second is not
    # permission to proceed.
    # shellcheck disable=SC2086
    if ! _live=$(${HELM} get manifest metiche-mysql --namespace "${METICHE_NAMESPACE}" 2>&1); then
        case "${_live}" in
            *"release: not found"*)
                log "no live metiche-mysql release, so no NetworkPolicy to keep"
                return 0 ;;
            *)
                die "could not read the live metiche-mysql release, so cannot tell whether this upgrade removes its NetworkPolicy:
${_live}" ;;
        esac
    fi
    if ! printf '%s\n' "${_live}" | has_networkpolicy; then
        log "the live metiche-mysql release has no NetworkPolicy"
        return 0
    fi
    # shellcheck disable=SC2086
    _new=$(${HELM} template metiche-mysql "${METICHE_CHART_DIR}/metiche-mysql" "$@") \
        || die "helm template metiche-mysql failed (above); not upgrading"
    if printf '%s\n' "${_new}" | has_networkpolicy; then
        log "NetworkPolicy: live, and kept by this upgrade"
        return 0
    fi
    if [ "${METICHE_ALLOW_NETPOL_REMOVAL}" = "1" ]; then
        warn "METICHE_ALLOW_NETPOL_REMOVAL=1: this upgrade REMOVES the live NetworkPolicy."
        warn "Every pod in the cluster will reach metiche-mysql:3306 again."
        return 0
    fi
    die "the live metiche-mysql release has a NetworkPolicy and this upgrade would remove it.

It admits only the backend and the nuzur agent to 3306 (docs/NUZUR_AGENT.md §7.1).
The upgrade renders none because ${METICHE_MYSQL_NETPOL_VALUES}
is not in its values: the file is missing, or this shell cannot see it (the
directory is root-owned 0700; run with sudo), or it now says enabled: false.

  keep the policy:   sudo deploy/scripts/helm-deploy.sh mysql, with that file in place
                     (deploy/scripts/nuzur-agent-setup.sh netpol recreates it)
  remove it on purpose:
                     METICHE_ALLOW_NETPOL_REMOVAL=1 deploy/scripts/helm-deploy.sh mysql

Nothing was changed."
}

deploy_mysql() {
    step "metiche-mysql"
    if [ -z "${DRY}" ]; then require_secret "${METICHE_DB_SECRET}"; fi
    if [ -f "${METICHE_MYSQL_NETPOL_VALUES}" ]; then
        [ -r "${METICHE_MYSQL_NETPOL_VALUES}" ] \
            || die "${METICHE_MYSQL_NETPOL_VALUES} exists but is not readable by $(id -un). Re-run with sudo."
        log "NetworkPolicy values: adding -f ${METICHE_MYSQL_NETPOL_VALUES} (written by nuzur-agent-setup.sh netpol)"
    elif [ -d "${METICHE_CRED_DIR}" ] && [ ! -x "${METICHE_CRED_DIR}" ]; then
        warn "cannot look inside ${METICHE_CRED_DIR} as $(id -un), so ${METICHE_MYSQL_NETPOL_VALUES} is not added even if it exists; sudo sees it"
    fi
    _mysql_flags="$(common_flags) --set auth.existingSecret=${METICHE_DB_SECRET}${METICHE_STORAGE_CLASS:+ --set persistence.storageClass=${METICHE_STORAGE_CLASS}}$(mysql_values_files)"
    # shellcheck disable=SC2086
    guard_mysql_netpol ${_mysql_flags}
    # shellcheck disable=SC2086
    ${HELM} upgrade --install metiche-mysql "${METICHE_CHART_DIR}/metiche-mysql" \
        ${_mysql_flags} \
        --wait --timeout 10m ${DRY}
}

deploy_backend() {
    step "metiche (backend)"
    require_tag
    if [ -z "${DRY}" ]; then require_secret "${METICHE_CONFIG_SECRET}"; fi
    # shellcheck disable=SC2046,SC2086
    ${HELM} upgrade --install metiche "${METICHE_CHART_DIR}/metiche" \
        $(common_flags) \
        --set-string image.tag="${METICHE_TAG}" \
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
        --set-string image.tag="${METICHE_TAG}" \
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
