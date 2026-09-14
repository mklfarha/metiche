#!/bin/sh
# nuzur-agent-test-pod.sh — LOCAL ONLY. Exercise the nuzur-agent image and its
# run-mode preflight in plain Docker, shaped like the StatefulSet pod, with NO
# NETWORK AT ALL (`--network none` on every container). Nothing here can reach
# nuzur, pair, publish or create anything outside this machine.
#
#   deploy/scripts/nuzur-agent-test-pod.sh [IMAGE]      default IMAGE: nuzur-agent:1.9.2-local-arm64
#
# The pod shape it reproduces (deploy/.helm/nuzur-agent/templates/statefulset.yaml):
#   hostname nuzur-agent-0, uid/gid 10001, read-only root filesystem, a tmpfs
#   /tmp, the "PVC" as a named volume at /var/lib/nuzur (read-only for the
#   preflight), /etc/machine-id mounted read-only, HOME/XDG_CONFIG_HOME/USER/
#   LOGNAME exactly as the chart sets them, and ids as the ConfigMap provides.
#
# A REAL pairing cannot be made locally, so "setup" writes a FAKE agent uuid
# and token file in the layout `nuzur-cli agent pair` writes, and registers the
# connection with the real `nuzur-cli agent connection add … --no-publish` (a
# throwaway DSN with a random fake password). The run-mode preflight and its
# real decryption check (`nuzur-cli agent connection list`) then run against it.

set -eu

IMAGE="${1:-nuzur-agent:1.9.2-local-arm64}"
VOL="nuzur-agent-test-pvc-$$"
HOST=nuzur-agent-0

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
docker image inspect "${IMAGE}" >/dev/null 2>&1 || { echo "image ${IMAGE} not found; build deploy/docker/nuzur-agent.Dockerfile first" >&2; exit 1; }

umask 077
W=$(mktemp -d "${TMPDIR:-/tmp}/nuzur-agent-pod.XXXXXX")
cleanup() { docker volume rm -f "${VOL}" >/dev/null 2>&1 || true; rm -rf "${W}"; }
trap cleanup EXIT INT TERM

# Results go to a file, so a check may run in a subshell. Every override below
# runs in one: in POSIX sh `VAR=x some_function` leaves VAR set afterwards.
: > "${W}/results"
pass() { printf 'PASS  %s\n' "$*"; echo PASS >> "${W}/results"; }
bad()  { printf 'FAIL  %s\n' "$*"; echo FAIL >> "${W}/results"; }
section() { printf '\n== %s\n' "$*"; }

uuidgen_lc() { od -An -tx1 -N16 /dev/urandom | tr -d ' \n' | sed 's/^\(.\{8\}\)\(.\{4\}\)\(.\{4\}\)\(.\{4\}\)\(.\{12\}\)$/\1-\2-\3-\4-\5/'; }
machine_id() { od -An -tx1 -N16 /dev/urandom | tr -d ' \n'; printf '\n'; }
sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

AGENT_UUID=$(uuidgen_lc)
CONNECTION_UUID=$(uuidgen_lc)
machine_id > "${W}/machine-id"
chmod 644 "${W}/machine-id"
write_ids() {  # $1 file; overrides via env: X_AGENT X_CONN X_HOST X_USER X_MID
    {
        printf 'AGENT_UUID=%s\n' "${X_AGENT:-${AGENT_UUID}}"
        printf 'CONNECTION_UUID=%s\n' "${X_CONN:-${CONNECTION_UUID}}"
        printf 'EXPECT_HOSTNAME=%s\n' "${X_HOST:-${HOST}}"
        printf 'EXPECT_USER=%s\n' "${X_USER:-nuzur}"
        printf 'MACHINE_ID_SHA256=%s\n' "${X_MID:-$(sha "${W}/machine-id")}"
    } > "$1"
    chmod 644 "$1"
}
write_ids "${W}/ids.env"

# The pod, as the chart renders it. $1 = mount mode for the PVC (rw|ro); the
# remaining args are `docker run` options, then the image and command.
pod() {
    _mode=$1; shift
    docker run --rm --network none --read-only --tmpfs /tmp:rw,size=16m,uid=10001,gid=10001 \
        --user 10001:10001 --cap-drop ALL --security-opt no-new-privileges \
        --hostname "${POD_HOST:-${HOST}}" \
        -e HOME=/var/lib/nuzur -e XDG_CONFIG_HOME=/var/lib/nuzur/config \
        -e "USER=${POD_USER:-nuzur}" -e LOGNAME=nuzur \
        -v "${VOL}:/var/lib/nuzur:${_mode}" \
        -v "${POD_MID:-${W}/machine-id}:/etc/machine-id:ro" \
        "$@"
}
preflight() { pod ro --env-file "${POD_IDS:-${W}/ids.env}" --entrypoint /usr/local/bin/nuzur-agent-preflight "$@" "${IMAGE}"; }

expect_refusal() {  # $1 label, $2 expected text; extra docker options after
    _label=$1; _want=$2; shift 2
    if preflight "$@" > "${W}/pf.out" 2>&1; then
        bad "${_label}: preflight PASSED"; sed 's/^/    /' "${W}/pf.out"; return 0
    fi
    if grep -q "${_want}" "${W}/pf.out"; then
        pass "${_label}"
        grep 'preflight FAILED' "${W}/pf.out" | sed 's/^/        /'
    else
        bad "${_label}: refused, but not with '${_want}'"; sed 's/^/    /' "${W}/pf.out"
    fi
}

# ── "PVC" ───────────────────────────────────────────────────────────────────
section "pod-shaped container: ${IMAGE}, --network none, read-only rootfs, uid 10001"
docker volume create "${VOL}" >/dev/null
# microk8s' hostpath provisioner creates PVC directories 0777 root:root (§4.C).
docker run --rm --network none -v "${VOL}:/v" --user 0:0 --entrypoint sh "${IMAGE}" -c 'chmod 0777 /v'

# ── setup mode ──────────────────────────────────────────────────────────────
section "setup mode"
pod rw --entrypoint sh "${IMAGE}" -c 'timeout 3 /usr/local/bin/nuzur-agent-hold || true' 2>&1 | head -2 | sed 's/^/  /' || true

if pod rw --entrypoint sh -e "AGENT_UUID=${AGENT_UUID}" -e "CONNECTION_UUID=${CONNECTION_UUID}" \
    -e "FAKEPW=$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32)" "${IMAGE}" -c '
    set -eu
    CFG="$XDG_CONFIG_HOME/nuzur/agent"
    # FAKE pairing, in the layout `agent pair` writes (0600 files in a 0700 dir).
    mkdir -p "$CFG"; chmod 700 "$XDG_CONFIG_HOME/nuzur" "$CFG"
    printf %s "$AGENT_UUID" > "$CFG/local_agent_uuid.txt"
    printf %s "fake-local-test-token-not-a-real-credential" > "$CFG/local_agent_token.txt"
    chmod 600 "$CFG/local_agent_uuid.txt" "$CFG/local_agent_token.txt"
    # REAL connection add, scripted, never published, no network.
    nuzur-cli agent connection add --uuid "$CONNECTION_UUID" --driver mysql --schema metiche_nuzur \
      --non-interactive --no-publish \
      --dsn "nuzur_ro:${FAKEPW}@tcp(metiche-mysql.metiche.svc.cluster.local:3306)/metiche_nuzur?parseTime=true&timeout=5s&readTimeout=60s&writeTimeout=60s" \
      metiche-prod | sed -n 1p
    ls -ln "$CFG" "$CFG/keyring" | sed "s/^/  /"
' > "${W}/setup.out" 2>&1; then
    sed 's/^/  /' "${W}/setup.out"
    grep -q "Added connection \"metiche-prod\" (uuid: ${CONNECTION_UUID}, dsn: nuzur_ro:\*\*\*@" "${W}/setup.out" \
        && pass "connection add stored the entry with the recorded uuid; the CLI masks the password" \
        || bad "connection add output unexpected"
    grep -q 'dsn-' "${W}/setup.out" && pass "the DSN is in the encrypted file keyring (no dbus in the image)" || bad "no keyring item"
else
    sed 's/^/  /' "${W}/setup.out"; bad "setup step failed"
fi
if pod ro --entrypoint sh "${IMAGE}" -c 'grep -rl nuzur_ro: /var/lib/nuzur 2>/dev/null' > "${W}/plain.out" 2>&1; then
    bad "the DSN appears in plaintext on the PVC"; cat "${W}/plain.out"
else
    pass "no plaintext DSN anywhere on the PVC"
fi

# ── run mode: the preflight ─────────────────────────────────────────────────
section "run mode: preflight against the recorded identity"
snap() { pod ro --entrypoint sh "${IMAGE}" -c '
    CFG="$XDG_CONFIG_HOME/nuzur/agent"
    sha256sum "$CFG/local_agent_uuid.txt" "$CFG/local_agent_connections.json" | cut -d" " -f1
    stat -c "%i %s %Y" "$CFG/local_agent_token.txt" "$CFG/keyring/dsn-'"${CONNECTION_UUID}"'"
    ls "$CFG/keyring"' 2>/dev/null; }
snap > "${W}/s0" || bad "could not take the identity snapshot (setup failed above?)"
if preflight > "${W}/pf.out" 2>&1; then
    sed 's/^/  /' "${W}/pf.out"; pass "preflight ok on the recorded identity (PVC read-only)"
else
    sed 's/^/  /' "${W}/pf.out"; bad "preflight refused the recorded identity"
fi
for _i in 1 2; do preflight > /dev/null 2>&1 || bad "preflight restart ${_i}"; done
snap > "${W}/s1"
cmp -s "${W}/s0" "${W}/s1" && pass "three fresh containers later: uuid, registry, token inode and keyring item unchanged" || { bad "state changed across restarts"; diff "${W}/s0" "${W}/s1"; }

section "negative controls (each must be refused, with its reason)"
( POD_HOST=nuzur-agent-1; expect_refusal "hostname changed" "hostname is 'nuzur-agent-1'" )
( X_USER=wrong; write_ids "${W}/ids.user"; POD_IDS="${W}/ids.user"
  expect_refusal "EXPECT_USER wrong in the ConfigMap" "USER is 'nuzur', but the recorded EXPECT_USER is 'wrong'" )
( X_USER=other; write_ids "${W}/ids.other"; POD_USER=other; POD_IDS="${W}/ids.other"
  expect_refusal "USER changed AND recorded to match: the real decryption catches it" "does not decrypt with this pod's identity" )
machine_id > "${W}/machine-id.new"; chmod 644 "${W}/machine-id.new"
( POD_MID="${W}/machine-id.new"; expect_refusal "machine-id replaced" "sha256 of /etc/machine-id does not match" )
( X_MID=$(sha "${W}/machine-id.new"); write_ids "${W}/ids.mid"; POD_MID="${W}/machine-id.new"; POD_IDS="${W}/ids.mid"
  expect_refusal "machine-id replaced AND its hash recorded: decryption catches it" "does not decrypt with this pod's identity" )
( X_AGENT=$(uuidgen_lc); write_ids "${W}/ids.agent"; POD_IDS="${W}/ids.agent"
  expect_refusal "a different AGENT_UUID recorded" "is not the recorded AGENT_UUID" )
( X_CONN=$(uuidgen_lc); write_ids "${W}/ids.conn"; POD_IDS="${W}/ids.conn"
  expect_refusal "a different CONNECTION_UUID recorded" "is not the recorded CONNECTION_UUID" )
expect_refusal "NUZUR_AGENT_DSN set" "forbidden environment variable(s) set: NUZUR_AGENT_DSN" -e NUZUR_AGENT_DSN=x
expect_refusal "NUZUR_PROVISIONING_TOKEN set" "forbidden environment variable(s) set: NUZUR_PROVISIONING_TOKEN" -e NUZUR_PROVISIONING_TOKEN=x
expect_refusal "XDG_CONFIG_HOME off the PVC" "XDG_CONFIG_HOME is '/tmp/cfg'" -e XDG_CONFIG_HOME=/tmp/cfg
if docker run --rm --network none --read-only --tmpfs /tmp --user 10001:10001 --hostname "${HOST}" \
    -e HOME=/var/lib/nuzur -e XDG_CONFIG_HOME=/var/lib/nuzur/config -e USER=nuzur \
    -v "${W}/machine-id:/etc/machine-id:ro" --env-file "${W}/ids.env" \
    --entrypoint /usr/local/bin/nuzur-agent-preflight "${IMAGE}" > "${W}/pf.out" 2>&1; then
    bad "no PVC: preflight passed"
else
    grep -q "is not a mount" "${W}/pf.out" && { pass "no PVC mounted"; sed 's/^/        /' "${W}/pf.out"; } || { bad "no PVC: wrong reason"; cat "${W}/pf.out"; }
fi

# State the preflight must refuse, planted and removed one at a time (rw).
plant() { pod rw --entrypoint sh "${IMAGE}" -c "$1" >/dev/null 2>&1; }
plant 'touch "$XDG_CONFIG_HOME/nuzur/token.txt"'
expect_refusal "a user login token on the PVC" "token.txt exists"
plant 'rm -f "$XDG_CONFIG_HOME/nuzur/token.txt"; printf x > "$XDG_CONFIG_HOME/nuzur/agent/local_agent_dsn.txt"'
expect_refusal "a saved fallback DSN on the PVC" "local_agent_dsn.txt exists"
plant 'rm -f "$XDG_CONFIG_HOME/nuzur/agent/local_agent_dsn.txt"; : > "$XDG_CONFIG_HOME/nuzur/agent/local_agent_uuid.txt"'
expect_refusal "pairing gone (PVC loss)" "agent not paired on this PVC; this pod never pairs itself"
plant 'printf %s "'"${AGENT_UUID}"'" > "$XDG_CONFIG_HOME/nuzur/agent/local_agent_uuid.txt"'
if pod rw --entrypoint sh -e "FAKEPW=$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32)" "${IMAGE}" -c \
    'nuzur-cli agent connection add --driver mysql --non-interactive --no-publish --dsn "u:${FAKEPW}@tcp(127.0.0.1:1)/x" second >/dev/null' >/dev/null 2>&1; then
    expect_refusal "a second registry entry" "the registry has 2 entries"
    plant 'nuzur-cli agent connection remove --no-publish second 2>/dev/null || nuzur-cli agent connection remove second'
else
    bad "could not plant a second entry"
fi

section "back to the recorded state"
if preflight > "${W}/pf.out" 2>&1; then pass "preflight ok again"; else bad "preflight after cleanup"; sed 's/^/  /' "${W}/pf.out"; fi
snap > "${W}/s2"
cmp -s "${W}/s0" "${W}/s2" && pass "identity snapshot equals the first one" || { bad "identity snapshot changed"; diff "${W}/s0" "${W}/s2"; }

NPASS=$(grep -c PASS "${W}/results" || true)
NFAIL=$(grep -c FAIL "${W}/results" || true)
printf '\n%s passed, %s failed\n' "${NPASS}" "${NFAIL}"
[ "${NFAIL}" -eq 0 ]
