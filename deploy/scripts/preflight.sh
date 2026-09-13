#!/bin/sh
# preflight.sh — read only. Prints what this deployment ASSUMES about a box it
# did not build, so the assumptions can be checked before anything is applied.
#
# It changes nothing. Every command here is a get or a describe.
#
#   sudo deploy/scripts/preflight.sh
#
# The box is shared with other projects. Everything metiche adds lives in one
# namespace; the only things it needs from outside that namespace already
# exist and are only ever REFERENCED: an ingress controller, a ClusterIssuer,
# and a StorageClass.

set -eu
. "$(dirname -- "$0")/lib.sh"

FAIL=0
note_fail() { warn "$1"; FAIL=1; }

step "Tools"
# ${KUBECTL} and ${HELM} may be MULTI-WORD ("microk8s kubectl"), which on a
# microk8s box is the only form that exists. Resolve the first word.
for c in "${KUBECTL}" "${HELM}"; do
    c0=${c%% *}
    if command -v "$c0" >/dev/null 2>&1; then
        log "$c        $(command -v "$c0")"
    else
        note_fail "$c is not on PATH"
    fi
done
if command -v "${METICHE_BUILDER}" >/dev/null 2>&1; then
    log "${METICHE_BUILDER}       $(command -v "${METICHE_BUILDER}")"
else
    warn "${METICHE_BUILDER} is not on PATH — needed only by build-images.sh"
fi

step "Architecture"
log "this machine: $(uname -m)   target: ${METICHE_TARGET_ARCH}"
if [ "$(uname -m)" = "${METICHE_TARGET_ARCH}" ]; then
    log "native build OK"
else
    warn "images cannot be built here; build-images.sh will refuse"
fi

step "Cluster reachable"
# shellcheck disable=SC2086
if ${KUBECTL} version -o json >/dev/null 2>&1 || ${KUBECTL} cluster-info >/dev/null 2>&1; then
    log "kubectl can reach the API server"
else
    note_fail "kubectl cannot reach a cluster. On microk8s: microk8s kubectl, or 'microk8s config > ~/.kube/config'."
fi

step "Namespace"
if namespace_exists; then
    log "${METICHE_NAMESPACE} exists"
    log "existing workloads in it:"
    kube get deploy,statefulset,svc,ingress,pvc 2>/dev/null | sed 's/^/    /' || true
else
    log "${METICHE_NAMESPACE} does not exist yet; deploy.sh will create it"
fi

step "Other namespaces on this box (metiche must not touch any of them)"
${KUBECTL} get namespace -o name 2>/dev/null | sed 's/^/    /' || warn "could not list namespaces"

step "IngressClasses  — charts default to ingress.className=public"
${KUBECTL} get ingressclass 2>/dev/null | sed 's/^/    /' || note_fail "no IngressClass found; is an ingress controller installed?"
log "microk8s' ingress addon registers 'public'; upstream ingress-nginx registers 'nginx'."
log "If the name below is not 'public', pass --set ingress.className=<name> to every chart."

step "ClusterIssuers  — charts default to ingress.clusterIssuer=letsencrypt-prod"
if ${KUBECTL} get clusterissuer >/dev/null 2>&1; then
    ${KUBECTL} get clusterissuer 2>/dev/null | sed 's/^/    /'
    log "This deployment REFERENCES one of these by name. It never creates or edits one:"
    log "a ClusterIssuer is cluster-scoped and shared with the other projects here."
else
    warn "no ClusterIssuer API — cert-manager may not be installed, or you lack the RBAC to list them."
    warn "Either install one out of band, or set ingress.clusterIssuer='' and bring your own TLS secret."
fi

step "Ingress X-Forwarded-For  — docs/BOARD_LOGIN.md §6.2 (F4)"
# Read only: gets ConfigMaps, workloads and Services in the controller's
# namespace. Decides how the board's -trust-proxy-hops, the backend's
# METICHE_TRUST_PROXY and the /signin limit-rpm annotation can trust the client
# address. Per ingress-nginx's nginx.tmpl, with the keys below:
#   compute-full-forwarded-for false (default)  X-Forwarded-For is OVERWRITTEN
#       with the address nginx saw: one entry, not client-controlled.
#   compute-full-forwarded-for true             nginx APPENDS that address to
#       the client's header: only the RIGHTMOST entry is trustworthy.
#   use-forwarded-headers true                  nginx believes the incoming
#       X-Forwarded-For from proxy-real-ip-cidr (default 0.0.0.0/0, i.e.
#       anyone): the "client" address, and limit-rpm with it, are spoofable
#       unless proxy-real-ip-cidr names only a trusted load balancer.
#   use-proxy-protocol true                     a load balancer sits in front.
# Check against the controller version on the box; this prints, it decides nothing.
: "${METICHE_INGRESS_NAMESPACES:=ingress ingress-nginx}"
XFF_KEYS='use-forwarded-headers|compute-full-forwarded-for|forwarded-for-header|use-proxy-protocol|proxy-real-ip-cidr|enable-real-ip'
xff_found=0
for ns in ${METICHE_INGRESS_NAMESPACES}; do
    ${KUBECTL} get namespace "$ns" >/dev/null 2>&1 || continue
    xff_found=1
    log "controller namespace: $ns"
    log "controller workloads and the ConfigMap each reads (--configmap):"
    ${KUBECTL} -n "$ns" get daemonset,deployment \
        -o go-template='{{range .items}}{{.kind}}/{{.metadata.name}}{{range .spec.template.spec.containers}}{{range .args}} {{.}}{{end}}{{end}}{{"\n"}}{{end}}' 2>/dev/null \
        | awk '{ cm="(no --configmap arg)"; for (i=2; i<=NF; i++) if ($i ~ /^--configmap=/) cm=$i; print "    " $1 "  " cm }' \
        || warn "could not list workloads in $ns"
    log "Services in front (a LoadBalancer here, or anything outside the box, changes what nginx sees):"
    ${KUBECTL} -n "$ns" get svc 2>/dev/null | sed 's/^/    /' || warn "could not list Services in $ns"
    for cm in $(${KUBECTL} -n "$ns" get configmap -o name 2>/dev/null); do
        set_keys=$(${KUBECTL} -n "$ns" get "$cm" \
            -o go-template='{{range $k, $v := .data}}{{$k}}={{$v}}{{"\n"}}{{end}}' 2>/dev/null \
            | grep -E "^(${XFF_KEYS})=" || true)
        if [ -n "$set_keys" ]; then
            log "$cm sets:"
            printf '%s\n' "$set_keys" | sed 's/^/      /' >&2
        else
            log "$cm sets none of the X-Forwarded-For keys"
        fi
        case "$set_keys" in
            *use-forwarded-headers=true*)
                warn "$cm: use-forwarded-headers=true. The client address is taken from X-Forwarded-For; check proxy-real-ip-cidr before trusting it (limit-rpm included)." ;;
        esac
        case "$set_keys" in
            *compute-full-forwarded-for=true*)
                log "$cm: X-Forwarded-For is appended to: trust only the rightmost entry." ;;
        esac
    done
done
if [ "$xff_found" = "0" ]; then
    warn "no ingress controller namespace among: ${METICHE_INGRESS_NAMESPACES}. Set METICHE_INGRESS_NAMESPACES and re-run."
else
    log "Legend: a ConfigMap that sets none of these keys leaves the controller default,"
    log "X-Forwarded-For overwritten with the address nginx saw (see the comment in this script)."
    log "Settle METICHE_TRUST_PROXY and the board's -trust-proxy-hops from this, not from the docs."
fi

step "StorageClasses  — metiche-mysql defaults to microk8s-hostpath"
${KUBECTL} get storageclass 2>/dev/null | sed 's/^/    /' || note_fail "no StorageClass; MySQL's PVC will stay Pending"

step "DNS — these must already resolve to this box's public address"
for h in metiche.xyz api.metiche.xyz mcp.metiche.xyz; do
    if command -v getent >/dev/null 2>&1; then
        addr=$(getent hosts "$h" 2>/dev/null | awk 'NR==1{print $1}')
    else
        addr=$(host "$h" 2>/dev/null | awk '/has address/{print $NF; exit}')
    fi
    if [ -n "${addr:-}" ]; then
        log "$h -> $addr"
    else
        warn "$h does not resolve. cert-manager's HTTP-01 challenge will fail until it does."
    fi
done

step "Credentials"
if [ -f "${METICHE_CRED_FILE}" ]; then
    log "${METICHE_CRED_FILE} exists   mode $(stat -c '%a %U:%G' "${METICHE_CRED_FILE}" 2>/dev/null || stat -f '%Lp %Su:%Sg' "${METICHE_CRED_FILE}")"
    log "(contents deliberately not printed)"
else
    log "${METICHE_CRED_FILE} does not exist; gen-credentials.sh will create it"
fi

step "Summary"
if [ "$FAIL" = "0" ]; then
    log "no blocking problems found."
    log "Read the warnings above — they are the assumptions, not the errors."
else
    die "preflight found blocking problems (above)."
fi
