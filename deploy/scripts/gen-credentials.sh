#!/bin/sh
# gen-credentials.sh — generate the database passwords ON THE BOX, render the
# runtime config from deploy/prod.yaml.example, and load both into Kubernetes
# Secrets in the metiche namespace.
#
#   sudo deploy/scripts/gen-credentials.sh              generate if absent, then apply
#   sudo deploy/scripts/gen-credentials.sh --secret-only  re-apply from existing files
#   sudo deploy/scripts/gen-credentials.sh --rotate-app   new app password (see below)
#
# ─────────────────────────────────────────────────────────────────────────────
# THE RULES THIS SCRIPT EXISTS TO ENFORCE
#
#  * A password is generated here, on the machine that will use it, and is
#    written straight into a root-owned 0600 file. It is never typed, never
#    chosen by a human, and never travels.
#
#  * It is NEVER echoed. Not to a terminal, not to a log, not in a --dry-run.
#    If you want to see it: sudo cat the file, on the box, knowing what you
#    are doing. This script will not print it for you.
#
#  * It is NEVER a command-line argument. Every process on the box can read
#    every other process's argv out of /proc, shared box or not, and argv also
#    lands in shell history and in the audit log. So: no
#    `kubectl create secret --from-literal`, no `mysql -p<pass>`, no
#    `sed s/x/$PASS/`. Values move through files and through stdin only.
#
#  * The repository is public and nothing this script writes may reach it.
#    Both output files are already covered by the repository .gitignore
#    (prod.yaml, credentials.env) AND they are written outside the checkout.
# ─────────────────────────────────────────────────────────────────────────────

set -eu
. "$(dirname -- "$0")/lib.sh"

MODE=all
case "${1:-}" in
    "")             MODE=all ;;
    --secret-only)  MODE=secret ;;
    --rotate-app)   MODE=rotate ;;
    -h|--help)      sed -n '2,40p' "$0"; exit 0 ;;
    *)              die "unknown argument: $1 (try --help)" ;;
esac

need_root "gen-credentials.sh"
need_cmd "${KUBECTL}" "Install kubectl, or set KUBECTL='microk8s kubectl'."

TEMPLATE="${METICHE_DEPLOY_DIR}/prod.yaml.example"
[ -f "${TEMPLATE}" ] || die "missing template: ${TEMPLATE}"

# Every file this script creates is 0600 root:root, including anything a
# failure leaves behind.
umask 077

# ── Password generation ──────────────────────────────────────────────────────
# 32 characters from [A-Za-z0-9]: ~190 bits, which is far past the point where
# anything but the storage matters.
#
# Alphanumeric ONLY, on purpose. The password travels through a YAML scalar, a
# base64 Secret, a MySQL DSN and a shell, and every one of those has an escape
# rule of its own. A password containing a quote, a backslash, an @ or a # has
# broken at least one deployment for everybody who has ever done this. The
# entropy given up by dropping punctuation is about 20 bits at this length and
# is not worth one minute of debugging.
#
# No pipefail (see lib.sh), so `tr` being killed by SIGPIPE when head has had
# enough is not an error.
gen_password() {
    LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32
}

read_cred() {
    # Read one KEY=value out of the credentials file without running it and
    # without echoing anything. `.`-sourcing the file would execute whatever
    # is in it, which is a bad habit for a file this sensitive.
    _key=$1
    while IFS= read -r _line; do
        case "$_line" in
            "${_key}="*) printf '%s' "${_line#*=}"; return 0 ;;
        esac
    done < "${METICHE_CRED_FILE}"
    return 1
}

# ── 1. The credentials file ──────────────────────────────────────────────────
step "Database credentials"

mkdir -p "${METICHE_CRED_DIR}"
chmod 700 "${METICHE_CRED_DIR}"
chown root:root "${METICHE_CRED_DIR}" 2>/dev/null || true

if [ -f "${METICHE_CRED_FILE}" ] && [ "${MODE}" != "rotate" ]; then
    log "${METICHE_CRED_FILE} already exists — keeping it."
    log "Regenerating would mint a password the EXISTING MySQL volume does not"
    log "know: MYSQL_ROOT_PASSWORD is honoured only on an empty datadir, so the"
    log "pod would come up and reject every login. Use --rotate-app to change"
    log "the application password properly (it runs ALTER USER first)."
elif [ "${MODE}" = "secret" ]; then
    die "--secret-only, but ${METICHE_CRED_FILE} does not exist. Run without arguments first."
else
    if [ "${MODE}" = "rotate" ]; then
        [ -f "${METICHE_CRED_FILE}" ] || die "--rotate-app, but there is nothing to rotate yet."
        warn "Rotating the APPLICATION password only. The root password is left alone:"
        warn "the mysql image sets it once, on an empty datadir, and nothing can change"
        warn "it afterwards except an ALTER USER you run yourself."
        confirm "Rotate the application password? The backend will fail auth until it is redeployed AND the ALTER USER below has run." \
            || die "aborted."
        _root=$(read_cred METICHE_DB_ROOT_PASSWORD) || die "no root password in ${METICHE_CRED_FILE}"
        _app=$(gen_password)
    else
        log "generating new passwords"
        _root=$(gen_password)
        _app=$(gen_password)
    fi

    # Written by redirection, in one shot, into a file created by this umask.
    # Nothing reaches stdout.
    {
        printf '# metiche database credentials. Generated on this machine by\n'
        printf '# deploy/scripts/gen-credentials.sh. root:root 0600.\n'
        printf '#\n'
        printf '# DO NOT COPY THIS FILE ANYWHERE. Not to a laptop, not into a chat,\n'
        printf '# not into the repository (which is public and ignores this name).\n'
        printf '# The Kubernetes Secrets built from it live in the metiche namespace.\n'
        printf 'METICHE_DB_ROOT_PASSWORD=%s\n' "${_root}"
        printf 'METICHE_DB_PASSWORD=%s\n' "${_app}"
    } > "${METICHE_CRED_FILE}"
    chmod 600 "${METICHE_CRED_FILE}"
    chown root:root "${METICHE_CRED_FILE}" 2>/dev/null || true
    unset _root
    log "wrote ${METICHE_CRED_FILE} (mode 600, contents not shown)"

    if [ "${MODE}" = "rotate" ]; then
        warn "Now apply it to the running database, then redeploy the backend:"
        warn "  kubectl -n ${METICHE_NAMESPACE} exec -i \$(POD) -- sh -c 'export MYSQL_PWD=\"\$MYSQL_ROOT_PASSWORD\"; exec mysql -u root' <<SQL"
        warn "  ALTER USER 'metiche'@'%' IDENTIFIED BY '<the new password, read from the file>';"
        warn "  FLUSH PRIVILEGES;"
        warn "  SQL"
    fi
    unset _app
fi

# ── 2. The runtime config overlay ────────────────────────────────────────────
step "Runtime config overlay"

if [ -f "${METICHE_CONFIG_FILE}" ] && [ "${MODE}" = "secret" ]; then
    log "${METICHE_CONFIG_FILE} exists — using it as is."
else
    _pw=$(read_cred METICHE_DB_PASSWORD) || die "no METICHE_DB_PASSWORD in ${METICHE_CRED_FILE}"
    [ -n "${_pw}" ] || die "METICHE_DB_PASSWORD in ${METICHE_CRED_FILE} is empty"

    # Substitution in pure shell. NOT sed/awk/envsubst: every one of those
    # would put the password in a process's argv (sed -e, awk -v) or in the
    # environment of a process whose /proc/<pid>/environ is world-readable to
    # the same uid. Parameter expansion happens inside this shell only.
    _tmp="${METICHE_CONFIG_FILE}.tmp.$$"
    : > "${_tmp}"
    chmod 600 "${_tmp}"
    while IFS= read -r _line || [ -n "${_line}" ]; do
        case "${_line}" in
            *__DB_PASSWORD__*)
                printf '%s%s%s\n' "${_line%%__DB_PASSWORD__*}" "${_pw}" "${_line#*__DB_PASSWORD__}"
                ;;
            *)
                printf '%s\n' "${_line}"
                ;;
        esac
    done < "${TEMPLATE}" > "${_tmp}"
    unset _pw

    # An `&&` here would be a `set -e` trap for the next reader; be explicit.
    if grep -q '__DB_PASSWORD__' "${_tmp}"; then
        rm -f "${_tmp}"
        die "the placeholder is still present after substitution — prod.yaml.example changed shape. Nothing was installed."
    fi

    mv -f "${_tmp}" "${METICHE_CONFIG_FILE}"
    chmod 600 "${METICHE_CONFIG_FILE}"
    chown root:root "${METICHE_CONFIG_FILE}" 2>/dev/null || true
    log "wrote ${METICHE_CONFIG_FILE} (mode 600, contents not shown)"
fi

# ── 3. The Kubernetes Secrets ────────────────────────────────────────────────
step "Kubernetes Secrets in namespace ${METICHE_NAMESPACE}"
ensure_namespace

# metiche-db: consumed by the metiche-mysql chart (env + the probe's file
# mount). Keys are the variable names from the credentials file, so the two
# cannot drift apart silently.
secret_from_env_file "${METICHE_DB_SECRET}" "${METICHE_CRED_FILE}"

# metiche-config: the whole prod.yaml, mounted as a file by the metiche chart.
# This is the "config from outside the image" half of the contract — the image
# keeps its committed placeholders and this overrides them at runtime.
secret_from_file "${METICHE_CONFIG_SECRET}" "prod.yaml" "${METICHE_CONFIG_FILE}"

step "Done"
log "Nothing above printed a password, and nothing wrote one into the checkout."
log "If the backend is already running, it will not pick up a changed config"
log "until its pods restart:"
log "  kubectl -n ${METICHE_NAMESPACE} rollout restart deploy -l app.kubernetes.io/part-of=metiche"
