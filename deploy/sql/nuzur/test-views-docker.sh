#!/bin/sh
# test-views-docker.sh — prove the nuzur views, grants and drift check against
# a THROWAWAY MySQL in Docker. Never touches any other database.
#
#   deploy/sql/nuzur/test-views-docker.sh
#
# Environment:
#   NUZUR_TEST_MYSQL_IMAGE   default mysql:8.4 (production runs the metiche-mysql chart's mysql:8.0)
#   NUZUR_TEST_KEEP=1        leave the container running (its passwords are still deleted)
#
# What it does, all through the real deploy/sql/nuzur/gen-views.sh:
#   1. Starts container nuzur-db on a Docker network with subnet 10.1.0.0/16 —
#      the production pod network — so nuzur_ro@'10.1.0.0/255.255.0.0' is
#      exercised as written, plus a second network 10.99.0.0/16 that is not.
#      Root and nuzur_ro passwords are random, in a 0700 mktemp dir, never
#      printed, never in argv, deleted on exit with the container.
#   2. Loads create.sql, then one fixture row per table. Every REDACTED column
#      holds a canary (NZCANARY…), and one exposed text column per table holds
#      a visible marker (NZVISIBLE…) as the positive control.
#   3. gen-views.sh grants, then gen-views.sh apply (which runs check-live).
#   4. As nuzur_ro, from inside 10.1.0.0/16: SELECT * on every view works,
#      every view's columns equal the base table's, no canary appears, every
#      visible marker does, every redacted column is NULL. INSERT, UPDATE,
#      DELETE, DDL, GRANT and SHOW CREATE VIEW are denied; the metiche base
#      tables and mysql.* are denied; SHOW DATABASES shows no metiche.
#   5. From 10.99.0.0/16 nuzur_ro is refused outright.
#   6. Mutations: a redacted column marked plain `expose` is refused before
#      DDL; marked `expose!` it applies and the canary check FAILS; the real
#      policy restores it and the canary check passes.
#   7. Drift: an unclassified new column, a view widened by hand, an extra
#      grant and a stale view are each caught by check-live, and each recovers.

set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "${HERE}/../../.." && pwd)
GEN="${HERE}/gen-views.sh"
SCHEMA="${REPO_ROOT}/code/backend/metiche/core/repository/sql/schema/create.sql"
IMAGE="${NUZUR_TEST_MYSQL_IMAGE:-mysql:8.4}"
DB=nuzur-db
NET=nuzur-test-net
OTHER=nuzur-other-net
RO_HOST='10.1.0.0/255.255.0.0'

die() { printf 'test-views: %s\n' "$*" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || die "docker is required"
docker inspect "${DB}" >/dev/null 2>&1 && die "a container named ${DB} already exists; refusing to touch it"

umask 077
W=$(mktemp -d "${TMPDIR:-/tmp}/nuzur-views-test.XXXXXX")
cleanup() {
    if [ "${NUZUR_TEST_KEEP:-0}" != "1" ]; then
        docker rm -f "${DB}" >/dev/null 2>&1 || true
        docker network rm "${NET}" "${OTHER}" >/dev/null 2>&1 || true
    fi
    rm -rf "${W}"
}
trap cleanup EXIT INT TERM

NPASS=0
NFAIL=0
pass() { printf 'PASS  %s\n' "$*"; NPASS=$((NPASS + 1)); }
bad()  { printf 'FAIL  %s\n' "$*"; NFAIL=$((NFAIL + 1)); }
check() { if [ "$2" = "$3" ]; then pass "$1"; else bad "$1 (want '$3', got '$2')"; fi; }
section() { printf '\n== %s\n' "$*"; }

# ── 1. Throwaway MySQL ──────────────────────────────────────────────────────
section "throwaway ${IMAGE} on ${NET} (10.1.0.0/16) and ${OTHER} (10.99.0.0/16)"
{ printf 'MYSQL_ROOT_PASSWORD='; LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32; printf '\n'; } > "${W}/root.env"
LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32 > "${W}/ro.pw"
{ printf '[client]\nuser=nuzur_ro\npassword='; cat "${W}/ro.pw"; printf '\n'; } > "${W}/ro.cnf"

docker network create --subnet 10.1.0.0/16 "${NET}" >/dev/null
docker network create --subnet 10.99.0.0/16 "${OTHER}" >/dev/null
docker run -d --name "${DB}" --network "${NET}" --ip 10.1.0.10 \
    --env-file "${W}/root.env" -p 127.0.0.1:33500:3306 \
    "${IMAGE}" --skip-name-resolve >/dev/null
docker network connect --ip 10.99.0.10 "${OTHER}" "${DB}"

NUZUR_MYSQL_ROOT="docker exec -i ${DB} sh -c 'export MYSQL_PWD=\"\$MYSQL_ROOT_PASSWORD\"; exec mysql --user=root --batch --skip-column-names --default-character-set=utf8mb4'"
NUZUR_RO_HOST="${RO_HOST}"
export NUZUR_MYSQL_ROOT NUZUR_RO_HOST
root() { sh -c "${NUZUR_MYSQL_ROOT}"; }

_i=0
until docker exec "${DB}" sh -c 'MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -h127.0.0.1 -uroot -N -e "SELECT 1"' >/dev/null 2>&1; do
    _i=$((_i + 1)); [ "${_i}" -lt 90 ] || die "MySQL did not come up"; sleep 2
done
printf 'server %s\n' "$(printf 'SELECT VERSION();\n' | root)"

# ── 2. Schema and fixtures ──────────────────────────────────────────────────
section "schema and fixtures"
{ printf 'CREATE DATABASE metiche CHARACTER SET utf8mb4;\nUSE metiche;\n'; cat "${SCHEMA}"; } | root
"${GEN}" columns "${SCHEMA}" > "${W}/cols"
"${GEN}" redacted "${SCHEMA}" > "${W}/redacted"
awk -F'\t' '
    FNR == NR { red[$1 "." $2] = 1; next }
    function q(s) { return "\047" s "\047" }
    function fit(s, len) { return (len != "" && length(s) > len + 0) ? substr(s, 1, len + 0) : s }
    function flush() { if (t != "") printf "INSERT INTO `metiche`.`%s` (%s) VALUES (%s);\n", t, cols, vals }
    $1 != t { flush(); t = $1; cols = ""; vals = ""; marked = 0 }
    {
        c = $3; ty = $4; len = $5
        if ((t "." c) in red) {
            s = fit("NZCANARY_" t "_" c, len)
            v = (ty == "json") ? "JSON_QUOTE(" q(s) ")" : q(s)
        } else if (ty == "char") {
            v = (len == 36) ? "UUID()" : q("x")
        } else if (ty == "varchar") {
            if (!marked && len + 0 >= 20) { v = q(fit("NZVISIBLE_" t, len)); marked = 1 } else v = q("fx")
        } else if (ty ~ /int$/) v = "1"
        else if (ty == "decimal") v = "1.50"
        else if (ty ~ /^(datetime|timestamp|date)$/) v = "NOW()"
        else if (ty == "json") v = "JSON_OBJECT(\047fixture\047, 1)"
        else if (ty ~ /text$/) v = q("fx")
        else v = "NULL"
        cols = cols (cols == "" ? "" : ", ") "`" c "`"
        vals = vals (vals == "" ? "" : ", ") v
    }
    END { flush() }' "${W}/redacted" "${W}/cols" > "${W}/fixtures.sql"
{ printf 'SET FOREIGN_KEY_CHECKS=0;\n'; cat "${W}/fixtures.sql"; } | root
NTABLES=$(cut -f1 "${W}/cols" | sort -u | wc -l | tr -d ' ')
NREDACT=$(wc -l < "${W}/redacted" | tr -d ' ')
cut -f1 "${W}/cols" | sort -u > "${W}/tables"
awk '{ printf "SELECT * FROM `%s`;\n", $1 }' "${W}/tables" > "${W}/select_all_base.sql"
{ printf 'USE metiche;\n'; cat "${W}/select_all_base.sql"; } | root > "${W}/root_dump"
check "fixtures: one row in each of the ${NTABLES} tables" \
    "$(awk '{ printf "SELECT COUNT(*) FROM `metiche`.`%s`;\n", $1 }' "${W}/tables" | root | grep -c '^1$')" "${NTABLES}"
check "fixtures: a canary stored in each of the ${NREDACT} redacted columns (as root, base tables)" \
    "$(grep -o 'NZCANARY_' "${W}/root_dump" | wc -l | tr -d ' ')" "${NREDACT}"
NVISIBLE=$(grep -o 'NZVISIBLE_' "${W}/root_dump" | wc -l | tr -d ' ')
printf 'redacted columns under test:\n'; sed 's/\t/./; s/^/  /' "${W}/redacted"

# ── 3. Grants and views through the real scripts ───────────────────────────
section "gen-views.sh grants, then apply (runs check-live)"
"${GEN}" grants < "${W}/ro.pw"
if "${GEN}" apply; then pass "apply + check-live on a fresh database"; else bad "apply"; fi
"${GEN}" grants < "${W}/ro.pw" && "${GEN}" apply >/dev/null 2>&1 && pass "grants and apply are idempotent (second run clean)" || bad "second run"

# ── 4. As nuzur_ro, from inside the pod network ─────────────────────────────
ro() {  # $1 network, $2 server ip; SQL on stdin; extra mysql flags after
    _net=$1; _ip=$2; shift 2
    docker run --rm -i --network "${_net}" -v "${W}/ro.cnf:/ro.cnf:ro" --entrypoint mysql "${IMAGE}" \
        --defaults-extra-file=/ro.cnf -h "${_ip}" --default-character-set=utf8mb4 --batch "$@"
}

canary_check() {  # 0 = no canary visible to nuzur_ro and every marker visible
    { printf 'USE metiche_nuzur;\n'; cat "${W}/select_all_base.sql"; } | ro "${NET}" 10.1.0.10 > "${W}/ro_dump" 2> "${W}/ro_dump.err" || return 2
    _c=$(grep -o 'NZCANARY_[A-Za-z_]*' "${W}/ro_dump" | sort -u | tr '\n' ' ')
    _v=$(grep -o 'NZVISIBLE_' "${W}/ro_dump" | wc -l | tr -d ' ')
    if [ -n "${_c}" ]; then printf '  canaries visible to nuzur_ro: %s\n' "${_c}"; return 1; fi
    [ "${_v}" = "${NVISIBLE}" ] || { printf '  visible markers: want %s, got %s\n' "${NVISIBLE}" "${_v}"; return 1; }
    return 0
}

section "as nuzur_ro from 10.1.0.0/16"
if canary_check; then
    pass "SELECT * on all ${NTABLES} views: no error, 0 of ${NREDACT} canaries visible, ${NVISIBLE}/${NVISIBLE} visible markers present"
else
    bad "canary check on the real policy"; cat "${W}/ro_dump.err" >&2
fi
check "SELECT * returned a header and one row from every view" \
    "$(wc -l < "${W}/ro_dump" | tr -d ' ')" "$((NTABLES * 2))"

printf "SELECT TABLE_NAME, ORDINAL_POSITION, COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='metiche_nuzur';\n" \
    | ro "${NET}" 10.1.0.10 --skip-column-names | LC_ALL=C sort -t "$(printf '\t')" -k1,1 -k2,2n > "${W}/ro_view_cols"
cut -f1-3 "${W}/cols" > "${W}/want_cols"
if cmp -s "${W}/want_cols" "${W}/ro_view_cols"; then
    pass "information_schema as nuzur_ro: all $(wc -l < "${W}/want_cols" | tr -d ' ') columns, same names and order as the base tables"
else bad "view columns seen by nuzur_ro differ from the base tables"; diff "${W}/want_cols" "${W}/ro_view_cols" | head >&2; fi

awk -F'\t' '{ printf "SELECT CONCAT(\047%s.%s\047, \047 rows=\047, COUNT(*), \047 null=\047, SUM(`%s` IS NULL)) FROM `metiche_nuzur`.`%s`;\n", $1, $2, $2, $1 }' \
    "${W}/redacted" | ro "${NET}" 10.1.0.10 --skip-column-names > "${W}/ro_nulls"
sed 's/^/  /' "${W}/ro_nulls"
check "every redacted column is NULL through its view" "$(grep -c 'rows=1 null=1$' "${W}/ro_nulls")" "${NREDACT}"

printf "SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA='metiche_nuzur' AND CONCAT(TABLE_NAME,'.',COLUMN_NAME) IN (%s) ORDER BY 1,2;\n" \
    "$(awk -F'\t' '{ printf "%s\047%s.%s\047", (NR > 1 ? "," : ""), $1, $2 }' "${W}/redacted")" \
    | ro "${NET}" 10.1.0.10 --skip-column-names | sed 's/^/  typed NULL: /'

printf 'SELECT id, `key`, client_key, token_hash FROM agent;\nSELECT id, code, label FROM invite;\nSELECT `key`, secret_hash, user_agent, ip_hint, auth_method FROM browser_session;\n' \
    | ro "${NET}" 10.1.0.10 --database=metiche_nuzur --table | sed 's/^/  /'

printf 'SHOW DATABASES;\n' | ro "${NET}" 10.1.0.10 --skip-column-names | tr '\n' ' ' > "${W}/dbs"
check "SHOW DATABASES as nuzur_ro lists no metiche" "$(cat "${W}/dbs")" "information_schema metiche_nuzur performance_schema "

expect_denied() {  # $1 label; statements on stdin, one per line; each must fail
    cat > "${W}/stmts"
    _n=$(wc -l < "${W}/stmts" | tr -d ' ')
    ro "${NET}" 10.1.0.10 --force --skip-column-names < "${W}/stmts" > /dev/null 2> "${W}/denied.err" || true
    _e=$(grep -c '^ERROR' "${W}/denied.err" || true)
    sed 's/^/  /' "${W}/denied.err"
    check "$1: ${_n} statements, ${_n} refused" "${_e}" "${_n}"
}
expect_denied "writes and DDL through the views" <<'SQL'
INSERT INTO metiche_nuzur.plan (id, `key`, name, status) VALUES (UUID(), 'x', 'x', 1);
UPDATE metiche_nuzur.account SET display_name = display_name;
DELETE FROM metiche_nuzur.team_event WHERE 1=0;
REPLACE INTO metiche_nuzur.plan (id, `key`, name, status) VALUES (UUID(), 'y', 'y', 1);
CREATE TABLE metiche_nuzur.t (i INT);
DROP VIEW metiche_nuzur.plan;
CREATE OR REPLACE VIEW metiche_nuzur.plan AS SELECT 1 AS id;
GRANT SELECT ON metiche_nuzur.* TO 'nuzur_ro'@'10.1.0.0/255.255.0.0';
SHOW CREATE VIEW metiche_nuzur.account;
SQL
expect_denied "the metiche base tables and mysql.*" <<'SQL'
SELECT * FROM metiche.account;
SELECT token_hash FROM metiche.agent;
SELECT code FROM metiche.invite;
INSERT INTO metiche.plan (id, `key`, name, status) VALUES (UUID(), 'z', 'z', 1);
UPDATE metiche.account SET display_name = display_name;
USE metiche;
SELECT user, host FROM mysql.user;
SQL

# ── 5. Host part ────────────────────────────────────────────────────────────
section "nuzur_ro from outside 10.1.0.0/16"
if printf 'SELECT 1;\n' | ro "${OTHER}" 10.99.0.10 > /dev/null 2> "${W}/other.err"; then
    bad "nuzur_ro connected from 10.99.0.0/16"
else
    sed 's/^/  /' "${W}/other.err"
    check "refused from 10.99.0.0/16" "$(grep -c 'ERROR 1045' "${W}/other.err")" "1"
fi

# ── 6. Policy mutations ─────────────────────────────────────────────────────
section "mutation: invite.code marked plain expose"
awk '$1 == "invite.code" { print "invite.code  expose"; next } { print }' "${HERE}/columns.policy" > "${W}/policy.plain"
if NUZUR_POLICY="${W}/policy.plain" "${GEN}" apply 2> "${W}/mut.err"; then bad "plain expose of a risky name was applied"; else
    sed 's/^/  /' "${W}/mut.err"; pass "refused before any DDL"; fi
canary_check >/dev/null && pass "views unchanged: canary check still passes" || bad "views changed after a refused apply"

section "mutation: browser_session.ip_hint marked plain expose (extended risky pattern)"
awk '$1 == "browser_session.ip_hint" { print "browser_session.ip_hint  expose"; next } { print }' "${HERE}/columns.policy" > "${W}/policy.ip"
if NUZUR_POLICY="${W}/policy.ip" "${GEN}" apply 2> "${W}/mut.err"; then bad "plain expose of ip_hint applied"; else
    sed 's/^/  /' "${W}/mut.err"; pass "refused before any DDL"; fi

section "mutation: invite.code marked expose! (acknowledged) — the canary check must FAIL"
awk '$1 == "invite.code" { print "invite.code  expose!  MUTATION TEST"; next } { print }' "${HERE}/columns.policy" > "${W}/policy.mut"
if NUZUR_POLICY="${W}/policy.mut" "${GEN}" apply 2>/dev/null; then pass "mutated policy applied (it is well-formed)"; else bad "mutated policy did not apply"; fi
if canary_check; then bad "canary check passed with invite.code exposed"; else pass "canary check FAILED as it must: the exposed canary is visible"; fi
section "restore the real policy"
"${GEN}" apply 2>/dev/null && pass "real policy re-applied" || bad "restore apply"
canary_check && pass "canary check passes again" || bad "canary check after restore"

# ── 7. Drift ────────────────────────────────────────────────────────────────
section "drift: a migration adds an unclassified column"
printf 'ALTER TABLE metiche.agent ADD COLUMN recovery_secret VARCHAR(64) NULL;\n' | root
if "${GEN}" check-live 2> "${W}/drift.err"; then bad "check-live passed"; else sed 's/^/  /' "${W}/drift.err"; pass "check-live fails naming it"; fi
if "${GEN}" apply 2>/dev/null; then bad "apply went ahead"; else pass "apply refuses before DDL"; fi
printf 'ALTER TABLE metiche.agent DROP COLUMN recovery_secret;\n' | root
"${GEN}" check-live 2>/dev/null && pass "recovers when the column is gone (or classified)" || bad "recovery"

section "drift: a view widened by hand"
printf 'CREATE OR REPLACE ALGORITHM=MERGE DEFINER=`nuzur_views`@`localhost` SQL SECURITY DEFINER VIEW metiche_nuzur.agent AS SELECT * FROM metiche.agent;\n' | root
if "${GEN}" check-live 2> "${W}/drift.err"; then bad "check-live passed"; else sed 's/^/  /' "${W}/drift.err"; pass "check-live fails naming the column"; fi
"${GEN}" apply 2>/dev/null && pass "apply restores it" || bad "apply after widening"

section "drift: an extra grant for nuzur_ro"
printf "GRANT SELECT ON metiche.* TO 'nuzur_ro'@'%s';\n" "${RO_HOST}" | root
if "${GEN}" check-live 2> "${W}/drift.err"; then bad "check-live passed"; else sed 's/^/  /' "${W}/drift.err"; pass "check-live fails listing the grants"; fi
printf "REVOKE SELECT ON metiche.* FROM 'nuzur_ro'@'%s';\n" "${RO_HOST}" | root
"${GEN}" check-live 2>/dev/null && pass "recovers after a deliberate REVOKE (never automatic)" || bad "recovery"

section "drift: a stale view"
printf 'CREATE VIEW metiche_nuzur.retired_table AS SELECT 1 AS x;\n' | root
if "${GEN}" check-live 2> "${W}/drift.err"; then bad "check-live passed"; else sed 's/^/  /' "${W}/drift.err"; pass "check-live fails naming it"; fi
"${GEN}" apply 2>/dev/null && pass "apply drops it" || bad "apply after stale view"

section "final state"
"${GEN}" check-live && pass "check-live clean" || bad "final check-live"
canary_check && pass "canary check clean" || bad "final canary check"

printf '\n%s passed, %s failed (%s)\n' "${NPASS}" "${NFAIL}" "$(printf 'SELECT VERSION();\n' | root)"
[ "${NFAIL}" -eq 0 ]
