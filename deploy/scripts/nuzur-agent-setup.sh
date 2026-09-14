#!/bin/sh
# nuzur-agent-setup.sh — the box half of the nuzur agent for metiche's database:
# set up, verify, and tear down. READ deploy/scripts/nuzur-agent-setup.md FIRST;
# it is the ordered runbook and says which steps need the owner.
# docs/NUZUR_AGENT.md is the design.
#
#   sudo KUBECTL="microk8s kubectl" HELM="microk8s helm3" deploy/scripts/nuzur-agent-setup.sh <step> [args]
#
# Every step is check → skip or act → verify, and safe to re-run. Nothing here
# pairs. Nothing here prints a password, a token, a DSN or the machine-id.
#
#   status                  read-only: objects, recorded uuids, mode, files present
#   preconditions           read-only checks before anything is created
#   secrets                 nuzur_ro password + Secret nuzur-agent-db; machine-id + Secret
#                           nuzur-agent-machine-id; ConfigMap nuzur-agent-ids (+ mirror file).
#                           Generated once, never regenerated.
#   db                      database metiche_nuzur, nuzur_views@localhost (locked), nuzur_ro
#                           (password taken from Secret nuzur-agent-db)
#   views                   the redacted views (gen-views.sh apply, which verifies)
#   image                   build nuzur-agent:<cli version>-<git sha> here, import into containerd;
#                           prints the tag
#   install-setup TAG       helm install in setup mode; verify hostname, USER, config dir, PVC
#   pair-check              confirm it is safe for the OWNER to pair; print the exact command
#   record-agent UUID       record the agent uuid found by the listLocalAgents diff, after
#                           cross-checking the pod
#   connection [--again]    register metiche-prod with the recorded CONNECTION_UUID; publishes
#                           the catalog as the agent
#   run                     switch to run mode (preflight-gated) and verify it comes online
#   netpol                  enable the metiche-mysql NetworkPolicy; prove a stranger pod is refused
#   p1                      restart x2, delete, crash: the identity snapshot must never change
#   snapshot                print the identity snapshot (non-secret) once
#   drift-timer             install the daily systemd timer running gen-views.sh check-live
#   teardown-cut-access     revocation step 1: DROP USER nuzur_ro, kill its sessions
#   teardown-remove UUID    revocation steps 3-5, after the owner revoked UUID in nuzur;
#                           UUID must equal the recorded AGENT_UUID
#
# Environment (all optional): KUBECTL, HELM, CTR, METICHE_NAMESPACE, METICHE_CRED_DIR,
# METICHE_BUILDER, METICHE_STORAGE_CLASS (as in lib.sh), NUZUR_AGENT_STATE_DIR
# (default /etc/nuzur-agent), NUZUR_RO_HOST (default 10.1.0.0/255.255.0.0),
# NUZUR_AGENT_RESEAL=1 (allow run → setup, for re-seal or PVC loss only).

set -eu
. "$(dirname -- "$0")/lib.sh"

: "${NUZUR_AGENT_STATE_DIR:=/etc/nuzur-agent}"
: "${NUZUR_RO_HOST:=10.1.0.0/255.255.0.0}"
: "${METICHE_STORAGE_CLASS:=}"
export NUZUR_RO_HOST

RO_ENV="${METICHE_CRED_DIR}/nuzur_ro.env"
MID_FILE="${METICHE_CRED_DIR}/nuzur-agent.machine-id"
IDS_FILE="${NUZUR_AGENT_STATE_DIR}/ids.env"
NP_VALUES="${METICHE_MYSQL_NETPOL_VALUES}"   # lib.sh; helm-deploy.sh reads the same path
REL=nuzur-agent
POD=nuzur-agent-0
CHART="${METICHE_CHART_DIR}/nuzur-agent"
GEN="${METICHE_DEPLOY_DIR}/sql/nuzur/gen-views.sh"
DOCKERFILE="${METICHE_DEPLOY_DIR}/docker/nuzur-agent.Dockerfile"
CFG=/var/lib/nuzur/config/nuzur/agent
CONN_NAME=metiche-prod
VIEW_DB=metiche_nuzur
DSN_HOST="metiche-mysql.${METICHE_NAMESPACE}.svc.cluster.local"
UUID_RE='^[0-9a-f]\{8\}-[0-9a-f]\{4\}-[0-9a-f]\{4\}-[0-9a-f]\{4\}-[0-9a-f]\{12\}$'

stop() { printf '\nSTOP: %s\n' "$*" >&2; exit 3; }
ok()   { printf '  ok: %s\n' "$*" >&2; }

# Root is required because the credential files live in root-owned /etc. A
# local test cluster that points METICHE_CRED_DIR and NUZUR_AGENT_STATE_DIR
# somewhere else does not need it.
need_root_paths() {
    if [ "${METICHE_CRED_DIR}" = "/etc/metiche" ] || [ "${NUZUR_AGENT_STATE_DIR}" = "/etc/nuzur-agent" ]; then
        need_root "nuzur-agent-setup.sh $1"
    fi
}

# shellcheck disable=SC2086
helmx() { ${HELM} "$@"; }
podx() { kube exec "${POD}" -c agent -- "$@"; }
exists() { kube get "$1" "$2" >/dev/null 2>&1; }
ids_get() { kube get configmap nuzur-agent-ids -o "jsonpath={.data.$1}" 2>/dev/null || true; }
sha256_file() { if command -v sha256sum >/dev/null 2>&1; then sha256sum < "$1" | cut -d' ' -f1; else shasum -a 256 < "$1" | cut -d' ' -f1; fi; }
new_uuid() {
    if [ -r /proc/sys/kernel/random/uuid ]; then cat /proc/sys/kernel/random/uuid
    else uuidgen | tr 'A-Z' 'a-z'; fi
}
current_mode() {
    kube get statefulset nuzur-agent -o 'jsonpath={.spec.template.metadata.annotations.nuzur-agent\.metiche\.xyz/mode}' 2>/dev/null || true
}
current_tag() {
    _img=$(kube get statefulset nuzur-agent -o 'jsonpath={.spec.template.spec.containers[?(@.name=="agent")].image}' 2>/dev/null || true)
    printf '%s' "${_img#*:}"
}
storage_flags() { [ -n "${METICHE_STORAGE_CLASS}" ] && printf -- '--set-string persistence.storageClass=%s' "${METICHE_STORAGE_CLASS}" || true; }

mysql_root() {
    if [ -n "${NUZUR_MYSQL_ROOT:-}" ]; then sh -c "${NUZUR_MYSQL_ROOT}"; return; fi
    _p=$(mysql_pod)
    [ -n "${_p}" ] || die "no metiche-mysql pod in ${METICHE_NAMESPACE}"
    kube exec -i "${_p}" -c mysql -- sh -c 'export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; exec mysql --user=root --batch --skip-column-names'
}

# The mirror of ConfigMap nuzur-agent-ids on the box, so teardown still knows
# the uuids if the cluster objects are gone. Non-secret, 0644.
mirror_ids() {
    mkdir -p "${NUZUR_AGENT_STATE_DIR}"
    _tmp="${IDS_FILE}.tmp"
    {
        for _k in AGENT_UUID CONNECTION_UUID EXPECT_HOSTNAME EXPECT_USER MACHINE_ID_SHA256; do
            printf '%s=%s\n' "${_k}" "$(ids_get "${_k}")"
        done
    } > "${_tmp}"
    chmod 644 "${_tmp}"
    mv "${_tmp}" "${IDS_FILE}"
}
mirror_get() { [ -f "${IDS_FILE}" ] && sed -n "s/^$1=//p" "${IDS_FILE}" | head -1 || true; }

# Wait for the pod to exist and every container to be running (setup), or for
# the preflight to have completed and the agent container to run (run).
wait_pod_running() {
    _i=0
    while :; do
        _phase=$(kube get pod "${POD}" -o 'jsonpath={.status.phase}' 2>/dev/null || true)
        _ready=$(kube get pod "${POD}" -o 'jsonpath={.status.containerStatuses[?(@.name=="agent")].state.running.startedAt}' 2>/dev/null || true)
        [ "${_phase}" = "Running" ] && [ -n "${_ready}" ] && return 0
        _i=$((_i + 1))
        if [ "${_i}" -ge 90 ]; then
            kube get pod "${POD}" -o wide >&2 || true
            kube logs "${POD}" -c preflight --tail=5 >&2 2>/dev/null || true
            die "${POD} is not running after 180s"
        fi
        sleep 2
    done
}

# "paired and online" in the CURRENT agent container's log (cli/agent/daemon.go).
wait_online() {
    _i=0
    while :; do
        if kube logs "${POD}" -c agent 2>/dev/null | grep -q 'paired and online'; then return 0; fi
        if kube logs "${POD}" -c agent 2>/dev/null | grep -q 'too old\|failed precondition\|not paired'; then
            kube logs "${POD}" -c agent --tail=10 >&2 || true
            die "the agent reports a non-retryable condition (above)"
        fi
        _i=$((_i + 1))
        [ "${_i}" -lt 60 ] || { kube logs "${POD}" -c agent --tail=10 >&2 || true; die "no 'paired and online' after 120s"; }
        sleep 2
    done
}

# The identity snapshot of docs/NUZUR_AGENT.md §10.3, box half. Only uuids,
# non-secret file hashes, inode/size/mtime and object uids. The token is
# never hashed or read.
snapshot() {
    _conn=$(ids_get CONNECTION_UUID)
    printf 'ids AGENT_UUID=%s CONNECTION_UUID=%s MACHINE_ID_SHA256=%s\n' \
        "$(ids_get AGENT_UUID)" "${_conn}" "$(ids_get MACHINE_ID_SHA256)"
    printf 'pvc uid=%s\n' "$(kube get pvc data-nuzur-agent-0 -o 'jsonpath={.metadata.uid}')"
    podx env CONN="${_conn}" CFG="${CFG}" sh -c '
        printf "agent_uuid=%s\n" "$(cat "$CFG/local_agent_uuid.txt")"
        printf "sha256 uuid_file=%s\n" "$(sha256sum < "$CFG/local_agent_uuid.txt" | cut -d" " -f1)"
        printf "sha256 registry=%s\n" "$(sha256sum < "$CFG/local_agent_connections.json" | cut -d" " -f1)"
        printf "token inode/size/mtime=%s\n" "$(stat -c "%i %s %Y" "$CFG/local_agent_token.txt")"
        printf "keyring=%s\n" "$(ls "$CFG/keyring" | tr "\n" " ")"
        printf "dsn item inode/size/mtime=%s\n" "$(stat -c "%i %s %Y" "$CFG/keyring/dsn-$CONN")"
        printf "hostname=%s USER=%s machine-id sha256=%s\n" "$(hostname)" "$USER" "$(sha256sum < /etc/machine-id | cut -d" " -f1)"
        for f in local_agent_dsn.txt local_agent_driver.txt; do [ -e "$CFG/$f" ] && echo "FORBIDDEN $f present"; done
        [ -e "$XDG_CONFIG_HOME/nuzur/token.txt" ] && echo "FORBIDDEN token.txt present"
        true'
}

# ── steps ───────────────────────────────────────────────────────────────────

step_status() {
    step "nuzur-agent status (${METICHE_NAMESPACE})"
    log "release mode:    $(current_mode || true)"
    log "image tag:       $(current_tag || true)"
    log "pod:             $(kube get pod "${POD}" -o 'jsonpath={.status.phase} restarts={.status.containerStatuses[0].restartCount}' 2>/dev/null || echo absent)"
    log "AGENT_UUID:      $(ids_get AGENT_UUID)"
    log "CONNECTION_UUID: $(ids_get CONNECTION_UUID)"
    for _o in "secret nuzur-agent-db" "secret nuzur-agent-machine-id" "configmap nuzur-agent-ids" "pvc data-nuzur-agent-0"; do
        # shellcheck disable=SC2086
        exists ${_o} && log "present: ${_o}" || log "absent:  ${_o}"
    done
    for _f in "${RO_ENV}" "${MID_FILE}" "${IDS_FILE}"; do
        [ -e "${_f}" ] && log "present: ${_f}" || log "absent:  ${_f}"
    done
    if exists pod "${POD}"; then
        log "preflight last line: $(kube logs "${POD}" -c preflight --tail=1 2>/dev/null || echo '(none: setup mode)')"
    fi
}

step_preconditions() {
    step "preconditions (read-only)"
    need_root_paths preconditions
    need_cmd "${KUBECTL}" "Set KUBECTL='microk8s kubectl'."
    need_cmd "${HELM}" "Set HELM='microk8s helm3'."
    namespace_exists || die "namespace ${METICHE_NAMESPACE} does not exist"
    ok "namespace ${METICHE_NAMESPACE}"
    _p=$(mysql_pod)
    [ -n "${_p}" ] || die "no metiche-mysql pod"
    kube wait --for=condition=ready "pod/${_p}" --timeout=60s >/dev/null
    ok "${_p} Ready"
    "${GEN}" check
    for _m in setup run; do
        helmx template nuzur-agent "${CHART}" -n "${METICHE_NAMESPACE}" --set-string agent.mode="${_m}" --set-string image.tag=check >/dev/null
    done
    ok "chart renders in both modes"
    [ "$(uname -m)" = "${METICHE_TARGET_ARCH}" ] && ok "architecture $(uname -m)" || warn "architecture $(uname -m): the image step must run on the ${METICHE_TARGET_ARCH} box"
}

step_secrets() {
    step "secrets: nuzur_ro password, machine-id, ids"
    need_root_paths secrets
    namespace_exists || die "namespace ${METICHE_NAMESPACE} does not exist"
    mkdir -p "${METICHE_CRED_DIR}" "${NUZUR_AGENT_STATE_DIR}"

    # 1. The nuzur_ro password. The file is the source of truth; the Secret is
    #    refreshed from it every run so the two can never differ.
    if [ -s "${RO_ENV}" ]; then
        log "keeping existing ${RO_ENV}"
    else
        ( umask 077; { printf 'NUZUR_RO_PASSWORD='; LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32; printf '\n'; } > "${RO_ENV}" )
        log "generated ${RO_ENV} (root 0600, never printed)"
    fi
    grep -Eq '^NUZUR_RO_PASSWORD=[A-Za-z0-9]{32}$' "${RO_ENV}" || stop "${RO_ENV} is not one NUZUR_RO_PASSWORD=[A-Za-z0-9]{32} line"
    [ "$(wc -l < "${RO_ENV}" | tr -d ' ')" = "1" ] || stop "${RO_ENV} must hold exactly one line"
    secret_from_env_file nuzur-agent-db "${RO_ENV}"

    # 2. The machine-id: generated once, NEVER regenerated or retyped; its
    #    trailing newline is part of the keyring passphrase.
    if [ -f "${MID_FILE}" ]; then
        grep -Eq '^[0-9a-f]{32}$' "${MID_FILE}" && [ "$(wc -c < "${MID_FILE}" | tr -d ' ')" = "33" ] \
            || stop "${MID_FILE} is not 32 lowercase hex characters and a newline"
        if exists secret nuzur-agent-machine-id; then
            _have=$(kube get secret nuzur-agent-machine-id -o 'go-template={{index .data "machine-id"}}')
            _want=$(base64 < "${MID_FILE}" | tr -d '\n')
            [ "${_have}" = "${_want}" ] || stop "Secret nuzur-agent-machine-id differs from ${MID_FILE}. Never regenerate: decide which one the pairing was sealed with (docs/NUZUR_AGENT.md §4.D)."
            log "machine-id file and Secret agree"
        else
            secret_from_file nuzur-agent-machine-id machine-id "${MID_FILE}"
        fi
    else
        exists secret nuzur-agent-machine-id && stop "Secret nuzur-agent-machine-id exists but ${MID_FILE} does not. Restore the file from the Secret; never generate a new machine-id."
        ( umask 077; od -An -tx1 -N16 /dev/urandom | tr -d ' \n' > "${MID_FILE}"; printf '\n' >> "${MID_FILE}" )
        log "generated ${MID_FILE} (0600, never printed)"
        secret_from_file nuzur-agent-machine-id machine-id "${MID_FILE}"
    fi
    _mid_sha=$(sha256_file "${MID_FILE}")

    # 3. The identity ConfigMap. CONNECTION_UUID, once set, never changes.
    if exists configmap nuzur-agent-ids; then
        [ -n "$(ids_get CONNECTION_UUID)" ] || stop "ConfigMap nuzur-agent-ids has no CONNECTION_UUID"
        [ "$(ids_get EXPECT_HOSTNAME)" = "nuzur-agent-0" ] || stop "EXPECT_HOSTNAME in nuzur-agent-ids is not nuzur-agent-0"
        [ "$(ids_get EXPECT_USER)" = "nuzur" ] || stop "EXPECT_USER in nuzur-agent-ids is not nuzur"
        [ "$(ids_get MACHINE_ID_SHA256)" = "${_mid_sha}" ] || stop "MACHINE_ID_SHA256 in nuzur-agent-ids does not match ${MID_FILE}"
        log "keeping ConfigMap nuzur-agent-ids (CONNECTION_UUID $(ids_get CONNECTION_UUID))"
    else
        if [ -f "${IDS_FILE}" ] && [ -n "$(mirror_get CONNECTION_UUID)" ]; then
            [ "$(mirror_get MACHINE_ID_SHA256)" = "${_mid_sha}" ] || stop "${IDS_FILE} records a different machine-id hash"
            log "recreating ConfigMap nuzur-agent-ids from the mirror ${IDS_FILE}"
            cp "${IDS_FILE}" "${IDS_FILE}.src"
        else
            {
                printf 'AGENT_UUID=\n'
                printf 'CONNECTION_UUID=%s\n' "$(new_uuid)"
                printf 'EXPECT_HOSTNAME=nuzur-agent-0\n'
                printf 'EXPECT_USER=nuzur\n'
                printf 'MACHINE_ID_SHA256=%s\n' "${_mid_sha}"
            } > "${IDS_FILE}.src"
            log "generated CONNECTION_UUID $(sed -n 's/^CONNECTION_UUID=//p' "${IDS_FILE}.src")"
        fi
        # shellcheck disable=SC2086
        ${KUBECTL} create configmap nuzur-agent-ids --namespace "${METICHE_NAMESPACE}" \
            --from-env-file="${IDS_FILE}.src" --dry-run=client -o yaml \
            | ${KUBECTL} apply --namespace "${METICHE_NAMESPACE}" -f - >/dev/null
        rm -f "${IDS_FILE}.src"
    fi
    mirror_ids
    ok "Secret nuzur-agent-db, Secret nuzur-agent-machine-id, ConfigMap nuzur-agent-ids, mirror ${IDS_FILE}"
}

step_db() {
    step "database ${VIEW_DB} and accounts (grants.sql)"
    need_root_paths db
    exists secret nuzur-agent-db || die "Secret nuzur-agent-db is missing: run the secrets step"
    # The password goes Secret → pipe → gen-views.sh's stdin → the printf
    # builtin → mysql's stdin inside the MySQL pod. Never argv, never a file.
    kube get secret nuzur-agent-db -o 'go-template={{index .data "NUZUR_RO_PASSWORD" | base64decode}}' \
        | "${GEN}" grants
}

step_views() {
    step "views (${VIEW_DB}), verified by check-live"
    "${GEN}" apply
}

step_image() {
    step "image nuzur-agent"
    assert_native_arch
    need_cmd "${METICHE_BUILDER}" "Set METICHE_BUILDER."
    _ver=$(sed -n 's/^ARG NUZUR_CLI_VERSION=//p' "${DOCKERFILE}" | head -1)
    [ -n "${_ver}" ] || die "no NUZUR_CLI_VERSION in ${DOCKERFILE}"
    _sha=$(git -C "${METICHE_REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || date -u +%Y%m%d%H%M%S)
    if git -C "${METICHE_REPO_ROOT}" rev-parse HEAD >/dev/null 2>&1 && \
       ! git -C "${METICHE_REPO_ROOT}" diff --quiet HEAD -- deploy/docker 2>/dev/null; then
        _sha="${_sha}-dirty-$(date -u +%H%M%S)"
    fi
    _tag="${_ver}-${_sha}"
    # shellcheck disable=SC2086
    if ${CTR} images ls -q 2>/dev/null | grep -q "nuzur-agent:${_tag}\$"; then
        log "nuzur-agent:${_tag} already in containerd"
    else
        "${METICHE_BUILDER}" build -t "nuzur-agent:${_tag}" -f "${DOCKERFILE}" "${METICHE_DEPLOY_DIR}/docker" >&2
        "${METICHE_BUILDER}" run --rm "nuzur-agent:${_tag}" --version | grep -q "version ${_ver}\$" \
            || die "the built image does not report nuzur CLI version ${_ver}"
        _tmp=$(mktemp -d)
        "${METICHE_BUILDER}" save -o "${_tmp}/img.tar" "nuzur-agent:${_tag}" >&2
        # shellcheck disable=SC2086
        ${CTR} image import "${_tmp}/img.tar" >&2
        rm -rf "${_tmp}"
    fi
    ok "nuzur-agent:${_tag}"
    printf '%s\n' "${_tag}"
}

verify_setup_pod() {
    [ "$(podx hostname)" = "nuzur-agent-0" ] || stop "pod hostname is not nuzur-agent-0 (duplicate path 9 / passphrase input). Do not pair."
    [ "$(podx sh -c 'printf %s "$XDG_CONFIG_HOME"')" = "/var/lib/nuzur/config" ] || stop "XDG_CONFIG_HOME is not /var/lib/nuzur/config. Do not pair."
    [ "$(podx sh -c 'printf %s "$USER"')" = "nuzur" ] || stop "USER is not nuzur. Do not pair."
    [ "$(podx id -u)" = "10001" ] || stop "the agent container is not uid 10001"
    podx awk '$5 == "/var/lib/nuzur" { f = 1 } END { exit !f }' /proc/self/mountinfo || stop "/var/lib/nuzur is not the PVC mount. Do not pair."
    podx test -w /var/lib/nuzur || stop "/var/lib/nuzur is not writable by uid 10001 (storage class permissions)"
    [ "$(podx sh -c 'sha256sum < /etc/machine-id | cut -d" " -f1')" = "$(ids_get MACHINE_ID_SHA256)" ] || stop "/etc/machine-id in the pod does not match MACHINE_ID_SHA256"
    [ -z "$(podx sh -c 'env | grep "^NUZUR_" || true')" ] || stop "a NUZUR_* variable is set in the pod"
}

step_install_setup() {
    _tag=${1:-}
    step "helm install nuzur-agent in setup mode"
    [ -n "${_tag}" ] || die "usage: install-setup TAG   (the tag the image step printed)"
    exists secret nuzur-agent-machine-id || die "run the secrets step first"
    exists configmap nuzur-agent-ids || die "run the secrets step first"
    exists secret nuzur-agent-db || die "run the secrets step first"
    _mode=$(current_mode)
    if [ "${_mode}" = "run" ] && [ "${NUZUR_AGENT_RESEAL:-0}" != "1" ]; then
        stop "the agent is in run mode. Switching back to setup is only for re-seal or PVC loss (docs/NUZUR_AGENT.md §4.D, §8); set NUZUR_AGENT_RESEAL=1 if that is what this is."
    fi
    # shellcheck disable=SC2046
    helmx upgrade --install "${REL}" "${CHART}" --namespace "${METICHE_NAMESPACE}" \
        --set-string agent.mode=setup --set-string image.tag="${_tag}" $(storage_flags) \
        --wait --timeout 5m >&2
    wait_pod_running
    verify_setup_pod
    ok "setup mode: hostname nuzur-agent-0, USER nuzur, uid 10001, XDG_CONFIG_HOME on the PVC, machine-id matches, no NUZUR_* env"
    kube logs "${POD}" -c agent --tail=1 | sed 's/^/  /' >&2
}

step_pair_check() {
    step "pair-check: is it safe for the owner to pair now?"
    [ "$(current_mode)" = "setup" ] || stop "the release is not in setup mode"
    verify_setup_pod
    _rec=$(ids_get AGENT_UUID)
    if podx test -s "${CFG}/local_agent_uuid.txt"; then
        _pod_uuid=$(podx cat "${CFG}/local_agent_uuid.txt" | tr -d ' \r\n')
        if [ -n "${_rec}" ]; then
            [ "${_pod_uuid}" = "${_rec}" ] || stop "the PVC is paired as ${_pod_uuid} but AGENT_UUID is ${_rec}"
            ok "already paired and recorded (${_rec}); do NOT pair again. Next: connection"
        else
            stop "the PVC is paired as ${_pod_uuid} but nothing is recorded. Do NOT pair again: confirm it with the listLocalAgents diff, then: record-agent ${_pod_uuid}"
        fi
        return 0
    fi
    [ -z "${_rec}" ] || stop "AGENT_UUID ${_rec} is recorded but the PVC has no pairing: this is PVC loss (runbook 'PVC loss'). Never pair over it without that procedure."
    ok "not paired, nothing recorded, identity verified"
    cat >&2 <<EOF

  SAFE TO PAIR. In this order:
    1. (coordinator, laptop) listLocalAgents -> save the set of agent uuids   [snapshot A]
    2. (OWNER, browser)      app.nuzur.com/pair -> "Pair a server" -> copy the token. It lives 15 minutes.
    3. (OWNER, on the box)   sudo ${KUBECTL} -n ${METICHE_NAMESPACE} exec -it ${POD} -c agent -- nuzur-cli agent pair
                             Paste the token at the masked "Pairing token" prompt.
                             Never --provisioning-token (argv), never --force.
    4. (coordinator, laptop) listLocalAgents again [snapshot B]. B minus A must be exactly ONE uuid,
                             machine_name nuzur-agent-0, connections empty.
    5. (box)                 sudo deploy/scripts/nuzur-agent-setup.sh record-agent <that uuid>
EOF
}

step_record_agent() {
    _uuid=${1:-}
    step "record-agent ${_uuid}"
    need_root_paths record-agent
    printf '%s' "${_uuid}" | grep -q "${UUID_RE}" || die "usage: record-agent UUID (lowercase uuid from the listLocalAgents diff)"
    podx test -s "${CFG}/local_agent_uuid.txt" || stop "the pod is not paired"
    _pod_uuid=$(podx cat "${CFG}/local_agent_uuid.txt" | tr -d ' \r\n')
    [ "${_pod_uuid}" = "${_uuid}" ] || stop "the listLocalAgents diff says ${_uuid} but the pod is paired as ${_pod_uuid}. Investigate; record nothing."
    podx nuzur-cli agent status 2>/dev/null | grep -q "uuid:  ${_uuid}\$" || stop "nuzur-cli agent status does not show ${_uuid}"
    _rec=$(ids_get AGENT_UUID)
    if [ -n "${_rec}" ]; then
        [ "${_rec}" = "${_uuid}" ] || stop "AGENT_UUID is already recorded as ${_rec}"
        log "already recorded"
    else
        kube patch configmap nuzur-agent-ids --type merge -p "{\"data\":{\"AGENT_UUID\":\"${_uuid}\"}}" >/dev/null
    fi
    mirror_ids
    [ "$(mirror_get AGENT_UUID)" = "${_uuid}" ] || die "mirror ${IDS_FILE} did not take AGENT_UUID"
    ok "AGENT_UUID ${_uuid} in ConfigMap nuzur-agent-ids and ${IDS_FILE}"
}

step_connection() {
    _again=${1:-}
    step "connection ${CONN_NAME}"
    [ "$(current_mode)" = "setup" ] || stop "the release is not in setup mode (the password is mounted only there)"
    _agent=$(ids_get AGENT_UUID)
    _conn=$(ids_get CONNECTION_UUID)
    [ -n "${_agent}" ] || stop "AGENT_UUID is not recorded: pair-check / record-agent first"
    [ -n "${_conn}" ] || stop "CONNECTION_UUID is not recorded: secrets first"
    [ "$(podx cat "${CFG}/local_agent_uuid.txt" | tr -d ' \r\n')" = "${_agent}" ] || stop "the pod is not paired as the recorded AGENT_UUID"
    podx test -r /run/secrets/nuzur-ro/NUZUR_RO_PASSWORD || stop "the nuzur_ro password is not mounted"
    _entries=$(podx sh -c "grep -c '\"uuid\"' ${CFG}/local_agent_connections.json 2>/dev/null || echo 0")
    if [ "${_entries}" != "0" ]; then
        podx grep -q "\"uuid\": \"${_conn}\"" "${CFG}/local_agent_connections.json" && [ "${_entries}" = "1" ] \
            || stop "the registry holds ${_entries} entries that are not exactly ${CONN_NAME} (${_conn}). Remove nothing automatically; investigate."
        if [ "${_again}" != "--again" ]; then
            ok "already registered as ${_conn}; skipping. Use 'connection --again' only if the previous run said publishing failed."
            return 0
        fi
        log "re-running the same add with the same uuid (upsert in place, republish)"
    fi
    # The whole script is single-quoted: every $ expands INSIDE the pod. kubectl's
    # argv and the exec request carry the literal script, never the password,
    # which exists only in nuzur-cli's argv inside the pod for its lifetime (D7).
    kube exec "${POD}" -c agent -- env CONNECTION_UUID="${_conn}" DSN_HOST="${DSN_HOST}" sh -c '
        set -eu
        PW=$(cat /run/secrets/nuzur-ro/NUZUR_RO_PASSWORD)
        exec nuzur-cli agent connection add \
          --uuid "$CONNECTION_UUID" --driver mysql --schema metiche_nuzur --non-interactive \
          --dsn "nuzur_ro:${PW}@tcp(${DSN_HOST}:3306)/metiche_nuzur?parseTime=true&timeout=5s&readTimeout=60s&writeTimeout=60s" \
          metiche-prod' > "${NUZUR_AGENT_STATE_DIR}/connection-add.out" 2>&1 || true
    sed 's/^/  /' "${NUZUR_AGENT_STATE_DIR}/connection-add.out" >&2
    grep -q "Added connection \"${CONN_NAME}\" (uuid: ${_conn}, dsn: nuzur_ro:\*\*\*@" "${NUZUR_AGENT_STATE_DIR}/connection-add.out" \
        || die "the add did not report ${CONN_NAME} with ${_conn}"
    if grep -q 'publishing the connection to nuzur failed' "${NUZUR_AGENT_STATE_DIR}/connection-add.out"; then
        die "saved in the pod but NOT published. Run: connection --again (same uuid, never a new one)"
    fi
    grep -q '^Published' "${NUZUR_AGENT_STATE_DIR}/connection-add.out" || die "no 'Published' line"
    rm -f "${NUZUR_AGENT_STATE_DIR}/connection-add.out"
    ok "${CONN_NAME} (${_conn}) registered and published by agent ${_agent}. Laptop: listLocalAgents must show exactly that one connection on ${_agent}."
}

step_run() {
    step "switch to run mode"
    _agent=$(ids_get AGENT_UUID)
    _conn=$(ids_get CONNECTION_UUID)
    [ -n "${_agent}" ] && [ -n "${_conn}" ] || stop "AGENT_UUID and CONNECTION_UUID must both be recorded"
    _tag=$(current_tag)
    [ -n "${_tag}" ] || die "no nuzur-agent StatefulSet"
    if [ "$(current_mode)" = "setup" ]; then
        # Dry run of the exact preflight, inside the setup pod, before the switch.
        podx env AGENT_UUID="${_agent}" CONNECTION_UUID="${_conn}" EXPECT_HOSTNAME="$(ids_get EXPECT_HOSTNAME)" \
            EXPECT_USER="$(ids_get EXPECT_USER)" MACHINE_ID_SHA256="$(ids_get MACHINE_ID_SHA256)" \
            /usr/local/bin/nuzur-agent-preflight >&2 || stop "the preflight would refuse (above); fix that in setup mode first"
    fi
    # shellcheck disable=SC2046
    helmx upgrade --install "${REL}" "${CHART}" --namespace "${METICHE_NAMESPACE}" \
        --set-string agent.mode=run --set-string image.tag="${_tag}" $(storage_flags) \
        --wait --timeout 5m >&2
    wait_pod_running
    kube logs "${POD}" -c preflight --tail=20 | sed 's/^/  /' >&2
    kube logs "${POD}" -c preflight --tail=1 | grep -q 'preflight ok' || die "preflight did not end with 'preflight ok'"
    if [ "${NUZUR_AGENT_TEST_OFFLINE:-0}" = "1" ]; then
        warn "NUZUR_AGENT_TEST_OFFLINE=1 (local test cluster only): not waiting for 'paired and online'"
    else
        wait_online
        kube logs "${POD}" -c agent | grep 'paired and online' | tail -1 | sed 's/^/  /' >&2
    fi
    ok "run mode as agent ${_agent}. Laptop: listLocalAgents shows ${_agent} status 1 (ONLINE)."
}

step_netpol() {
    step "metiche-mysql NetworkPolicy (backend and nuzur agent only)"
    need_root_paths netpol
    _mysql=$(mysql_pod)
    [ -n "${_mysql}" ] || die "no metiche-mysql pod"
    _uid0=$(kube get pod "${_mysql}" -o 'jsonpath={.metadata.uid} {.status.containerStatuses[0].restartCount}')
    if [ ! -f "${NP_VALUES}" ]; then
        printf '# Keeps the metiche-mysql NetworkPolicy on: deploy/scripts/helm-deploy.sh adds\n# -f %s to every metiche-mysql upgrade while this file exists.\n# To remove the policy: enabled: false here, then\n#   METICHE_ALLOW_NETPOL_REMOVAL=1 deploy/scripts/helm-deploy.sh mysql\nnetworkPolicy:\n  enabled: true\n' "${NP_VALUES}" > "${NP_VALUES}"
        chmod 644 "${NP_VALUES}"
    fi
    # The routine path, with no METICHE_VALUES_MYSQL: helm-deploy.sh picks the
    # file up by itself, so this step deploys exactly as every later redeploy will.
    "${METICHE_SCRIPT_DIR}/helm-deploy.sh" mysql >&2
    exists networkpolicy metiche-mysql-ingress || die "NetworkPolicy metiche-mysql-ingress was not created"
    [ "$(kube get pod "${_mysql}" -o 'jsonpath={.metadata.uid} {.status.containerStatuses[0].restartCount}')" = "${_uid0}" ] \
        || warn "the MySQL pod was replaced or restarted during the upgrade; check it"
    ok "policy present; MySQL pod not restarted"
    _tag=$(current_tag)
    [ -n "${_tag}" ] || die "the probes use the nuzur-agent image; install the agent first"
    _probe() {  # $1 name suffix, $2 extra kubectl run flags; exit status of nc
        # shellcheck disable=SC2086
        kube run "np-probe-$1-$$" --rm -i --restart=Never --image="nuzur-agent:${_tag}" --image-pull-policy=Never \
            $2 --command -- nc -z -w 3 metiche-mysql 3306 >/dev/null 2>&1
    }
    if _probe stranger ""; then stop "a pod with no allowed labels reached metiche-mysql:3306 — the policy is not enforced"; fi
    ok "a pod without the allowed labels is refused"
    _probe agentlabels "--labels=app.kubernetes.io/name=nuzur-agent,app.kubernetes.io/instance=nuzur-agent" \
        || stop "a pod with the nuzur-agent labels could NOT reach metiche-mysql:3306"
    ok "a pod with the nuzur-agent labels connects"
    log "Now check the backend by hand (runbook): no DB errors in its log, the board loads and updates."
    log "Every later 'helm-deploy.sh mysql' (and deploy.sh) adds ${NP_VALUES} itself, and refuses to drop a live policy."
    log "Revert: set 'enabled: false' in ${NP_VALUES}, then METICHE_ALLOW_NETPOL_REMOVAL=1 deploy/scripts/helm-deploy.sh mysql"
}

step_p1() {
    step "P1: identity is stable across restarts, a delete and a crash (box half)"
    [ "$(current_mode)" = "run" ] || stop "P1 runs in run mode"
    _dir="${NUZUR_AGENT_STATE_DIR}/p1-$(date -u +%Y%m%dT%H%M%SZ)"
    mkdir -p "${_dir}"
    _online() { if [ "${NUZUR_AGENT_TEST_OFFLINE:-0}" = "1" ]; then return 0; fi; wait_online; }
    wait_pod_running; _online
    snapshot > "${_dir}/S0"
    sed 's/^/  S0 /' "${_dir}/S0" >&2
    grep -q FORBIDDEN "${_dir}/S0" && stop "forbidden state in S0 (above)"
    _fail=0
    _cmp() {
        wait_pod_running; _online
        snapshot > "${_dir}/$1"
        if cmp -s "${_dir}/S0" "${_dir}/$1"; then ok "$1 == S0"
        else warn "$1 differs from S0:"; diff "${_dir}/S0" "${_dir}/$1" >&2 || true; _fail=1; fi
        log "  laptop: listLocalAgents must equal the S0 listing (uuids, count, created_at, machine_name, connections)"
    }
    for _n in 1 2; do
        _uid=$(kube get pod "${POD}" -o 'jsonpath={.metadata.uid}')
        kube rollout restart statefulset/nuzur-agent >/dev/null
        kube rollout status statefulset/nuzur-agent --timeout=300s >&2
        _w=0; while [ "$(kube get pod "${POD}" -o 'jsonpath={.metadata.uid}' 2>/dev/null)" = "${_uid}" ]; do _w=$((_w+1)); [ "${_w}" -lt 90 ] || die "pod not replaced"; sleep 2; done
        _cmp "S-restart-${_n}"
    done
    _uid=$(kube get pod "${POD}" -o 'jsonpath={.metadata.uid}')
    kube delete pod "${POD}" --wait=true >/dev/null   # graceful; never --force
    _w=0; while [ "$(kube get pod "${POD}" -o 'jsonpath={.metadata.uid}' 2>/dev/null || true)" = "${_uid}" ] || ! exists pod "${POD}"; do _w=$((_w+1)); [ "${_w}" -lt 90 ] || die "pod not recreated"; sleep 2; done
    _cmp "S-delete"
    _rc=$(kube get pod "${POD}" -o 'jsonpath={.status.containerStatuses[?(@.name=="agent")].restartCount}')
    podx kill 1 || true
    _w=0; while [ "$(kube get pod "${POD}" -o 'jsonpath={.status.containerStatuses[?(@.name=="agent")].restartCount}')" = "${_rc}" ]; do _w=$((_w+1)); [ "${_w}" -lt 90 ] || die "container did not restart"; sleep 2; done
    _cmp "S-crash"
    log "snapshots kept in ${_dir}"
    [ "${_fail}" = "0" ] || stop "P1 FAILED: a snapshot differs from S0 (above)"
    ok "P1 box half passed: restart x2, delete, crash — identity unchanged"
    log "Still to do by hand (runbook): image upgrade, and the negative control (EXPECT_USER=wrong)."
}

step_drift_timer() {
    step "daily views drift check (systemd timer on the box)"
    need_root drift-timer
    need_cmd systemctl "This step is for the box."
    cat > /etc/systemd/system/nuzur-agent-views-drift.service <<EOF
[Unit]
Description=metiche: nuzur agent views drift check (gen-views.sh check-live)
Documentation=file://${METICHE_REPO_ROOT}/docs/NUZUR_AGENT.md

[Service]
Type=oneshot
Environment=PATH=/snap/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
Environment="KUBECTL=${KUBECTL}"
Environment=METICHE_NAMESPACE=${METICHE_NAMESPACE}
Environment=NUZUR_RO_HOST=${NUZUR_RO_HOST}
ExecStart=${GEN} check-live
EOF
    cat > /etc/systemd/system/nuzur-agent-views-drift.timer <<'EOF'
[Unit]
Description=metiche: daily nuzur agent views drift check

[Timer]
OnCalendar=daily
RandomizedDelaySec=1h
Persistent=true

[Install]
WantedBy=timers.target
EOF
    systemctl daemon-reload
    systemctl enable --now nuzur-agent-views-drift.timer >&2
    systemctl start nuzur-agent-views-drift.service
    systemctl --no-pager status nuzur-agent-views-drift.service 2>&1 | tail -3 | sed 's/^/  /' >&2
    ok "timer enabled; a failure shows in 'systemctl --failed' and 'journalctl -u nuzur-agent-views-drift'"
}

step_teardown_cut_access() {
    step "teardown 1/3: cut data access (DROP USER nuzur_ro, kill its sessions)"
    printf "DROP USER IF EXISTS 'nuzur_ro'@'%s';\n" "${NUZUR_RO_HOST}" | mysql_root >/dev/null
    printf "SELECT ID FROM information_schema.PROCESSLIST WHERE USER = 'nuzur_ro';\n" | mysql_root | while IFS= read -r _id; do
        case "${_id}" in *[!0-9]*|"") continue ;; esac
        printf 'KILL %s;\n' "${_id}" | mysql_root >/dev/null 2>&1 || true
        log "killed session ${_id}"
    done
    [ "$(printf "SELECT COUNT(*) FROM mysql.user WHERE User = 'nuzur_ro';\n" | mysql_root)" = "0" ] || die "nuzur_ro still exists"
    [ "$(printf "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE USER = 'nuzur_ro';\n" | mysql_root)" = "0" ] || die "nuzur_ro sessions remain"
    ok "nuzur_ro dropped, no sessions. Next (OWNER, laptop): revoke the recorded AGENT_UUID $(mirror_get AGENT_UUID) — by uuid only."
}

step_teardown_remove() {
    _uuid=${1:-}
    step "teardown 3/3: remove the agent, its state and the views"
    need_root_paths teardown-remove
    printf '%s' "${_uuid}" | grep -q "${UUID_RE}" || die "usage: teardown-remove AGENT_UUID"
    _rec=$(mirror_get AGENT_UUID)
    [ -n "${_rec}" ] || _rec=$(ids_get AGENT_UUID)
    [ -n "${_rec}" ] || stop "no recorded AGENT_UUID in ${IDS_FILE} or the ConfigMap"
    [ "${_uuid}" = "${_rec}" ] || stop "${_uuid} is not the recorded AGENT_UUID ${_rec}. Teardown acts on the recorded uuid only."
    if exists configmap nuzur-agent-ids && [ -n "$(ids_get AGENT_UUID)" ]; then
        [ "$(ids_get AGENT_UUID)" = "${_rec}" ] || stop "the ConfigMap and ${IDS_FILE} disagree on AGENT_UUID"
    fi
    confirm "Has the OWNER revoked agent ${_rec} in nuzur, and does listLocalAgents show it REVOKED (status 3) with every other agent unchanged?" \
        || stop "revoke first (runbook teardown step 2)"
    helmx status "${REL}" --namespace "${METICHE_NAMESPACE}" >/dev/null 2>&1 && helmx uninstall "${REL}" --namespace "${METICHE_NAMESPACE}" --wait >&2
    kube delete pvc data-nuzur-agent-0 --ignore-not-found --wait=true >&2
    kube delete secret nuzur-agent-db nuzur-agent-machine-id --ignore-not-found >&2
    kube delete configmap nuzur-agent-ids --ignore-not-found >&2
    printf "DROP DATABASE IF EXISTS \`%s\`;\nDROP USER IF EXISTS 'nuzur_views'@'localhost';\nDROP USER IF EXISTS 'nuzur_ro'@'%s';\n" \
        "${VIEW_DB}" "${NUZUR_RO_HOST}" | mysql_root >/dev/null
    if command -v systemctl >/dev/null 2>&1 && [ -f /etc/systemd/system/nuzur-agent-views-drift.timer ]; then
        systemctl disable --now nuzur-agent-views-drift.timer >/dev/null 2>&1 || true
        rm -f /etc/systemd/system/nuzur-agent-views-drift.timer /etc/systemd/system/nuzur-agent-views-drift.service
        systemctl daemon-reload || true
    fi
    rm -f "${RO_ENV}" "${MID_FILE}"
    # shellcheck disable=SC2086
    ${CTR} images ls -q 2>/dev/null | grep 'nuzur-agent:' | while IFS= read -r _img; do ${CTR} images rm "${_img}" >/dev/null 2>&1 || true; done
    ok "release, PVC, Secrets, ConfigMap, ${VIEW_DB}, nuzur_views, drift timer, credential files and images removed"
    log "Kept, deliberately: ${IDS_FILE} (the uuids, for the record) and ${NP_VALUES}."
    log "Next: set networkPolicy.allowFrom without the nuzur-agent entry in ${NP_VALUES} and re-run helm-deploy.sh mysql;"
    log "then (laptop) confirm listLocalAgents: ${_rec} REVOKED, every other agent unchanged. Remove ${IDS_FILE} after that."
}

case "${1:-}" in
    status)               step_status ;;
    preconditions)        step_preconditions ;;
    secrets)              step_secrets ;;
    db)                   step_db ;;
    views)                step_views ;;
    image)                step_image ;;
    install-setup)        step_install_setup "${2:-}" ;;
    pair-check)           step_pair_check ;;
    record-agent)         step_record_agent "${2:-}" ;;
    connection)           step_connection "${2:-}" ;;
    run)                  step_run ;;
    netpol)               step_netpol ;;
    p1)                   step_p1 ;;
    snapshot)             snapshot ;;
    drift-timer)          step_drift_timer ;;
    teardown-cut-access)  step_teardown_cut_access ;;
    teardown-remove)      step_teardown_remove "${2:-}" ;;
    -h|--help|"")         sed -n '2,45p' "$0"; exit 0 ;;
    *)                    die "unknown step: $1 (see --help)" ;;
esac
