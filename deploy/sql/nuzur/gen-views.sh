#!/bin/sh
# gen-views.sh — the column policy behind the nuzur agent's read-only views.
#
#   deploy/sql/nuzur/gen-views.sh check      [create.sql]   no database: every column classified (CI)
#   deploy/sql/nuzur/gen-views.sh sql        [create.sql]   no database: check, then print the view SQL
#   deploy/sql/nuzur/gen-views.sh apply                     live: check the LIVE schema, create/replace
#                                                           the views, drop stale ones, then check-live
#   deploy/sql/nuzur/gen-views.sh check-live                live: daily drift check (see below)
#   deploy/sql/nuzur/gen-views.sh grants  < password        live: apply grants.sql; password on STDIN
#   deploy/sql/nuzur/gen-views.sh columns|redacted [create.sql]    no database: helpers for tests
#
# docs/NUZUR_AGENT.md §5 is the design. In short: nuzur reads database
# metiche_nuzur, which holds one view per metiche table with the same name and
# every column, and a redacted column comes back as a typed NULL under its own
# name. The login account nuzur_ro can SELECT those views and nothing else.
#
# ALLOWLIST WITH TOTAL CLASSIFICATION. Every column of every table must have a
# line in columns.policy saying expose, expose! or redact. An unclassified
# column, a stale line, a risky-looking name marked plain `expose`, or a
# malformed or duplicate line stops everything BEFORE any DDL, and each
# offender is printed. The views already in the database stay as they were:
# stale, but never wider.
#
# ─────────────────────────────────────────────────────────────────────────────
# LIVE MODES run SQL as MySQL root through $NUZUR_MYSQL_ROOT, a command that
# reads SQL on stdin and prints tab-separated rows without headers. By default
# that is the same in-pod pattern as deploy/scripts/apply-schema.sh: the root
# password is taken from the MySQL pod's own environment, inside the pod, so it
# is never in this script, in argv or on this machine.
#
# check-live never selects a row value. It reads information_schema, SHOW
# GRANTS, and for each redacted column one COUNT(*) of non-NULL values through
# the view, which must be 0. It verifies:
#   * the live metiche schema is totally classified by the policy;
#   * metiche_nuzur holds exactly one view per table and nothing else, each
#     DEFINER nuzur_views@localhost, SQL SECURITY DEFINER, and with exactly the
#     base table's column names in order;
#   * every redacted column is NULL on every row through its view;
#   * nuzur_ro and nuzur_views have exactly the expected grants and limits.
# ─────────────────────────────────────────────────────────────────────────────
#
# Environment:
#   NUZUR_POLICY       default: columns.policy next to this script
#   NUZUR_MYSQL_ROOT   default: kubectl exec into the metiche-mysql pod (see mysql_root)
#   NUZUR_RO_HOST      default: 10.1.0.0/255.255.0.0 (the pod network, §5)
#   KUBECTL, METICHE_NAMESPACE   as in deploy/scripts/lib.sh
#
# POSIX sh and POSIX awk only: the box runs dash and mawk, CI and laptops run
# bash and BSD awk, and all four are exercised.

set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
REPO_ROOT=$(CDPATH='' cd -- "${HERE}/../../.." && pwd)
: "${NUZUR_POLICY:=${HERE}/columns.policy}"
: "${NUZUR_RO_HOST:=10.1.0.0/255.255.0.0}"
: "${METICHE_NAMESPACE:=metiche}"
: "${KUBECTL:=kubectl}"
DEFAULT_SCHEMA="${REPO_ROOT}/code/backend/metiche/core/repository/sql/schema/create.sql"
SRC_DB=metiche
VIEW_DB=metiche_nuzur
DEFINER='`nuzur_views`@`localhost`'
TAB=$(printf '\t')

# Names that look like they could hold a credential or personal data. Such a
# column may be `redact` or an acknowledged `expose!`, never plain `expose`.
# docs/NUZUR_AGENT.md §5.2 pattern, plus ip, user_agent and email.
RISKY='(token|secret|code|key|pass|pwd|url|uri|snapshot|hash|salt|credential|cookie|signature|private|webhook|dsn|auth|email|user_agent|(^|_)ip(_|$))'

die() { printf 'gen-views: %s\n' "$*" >&2; exit 1; }
say() { printf 'gen-views: %s\n' "$*" >&2; }

usage() { sed -n '2,10p' "$0" >&2; exit 2; }

# grants.sql with its two placeholders filled, written to stdout (which the
# caller pipes into mysql, never to a file). The password arrives on stdin and
# lives only in a shell variable: `read` and `printf` are builtins, so it is
# never the argument of any process. [A-Za-z0-9]{32} only, which is what
# nuzur-agent-setup.sh generates and what needs no SQL quoting.
fill_grants() {
    IFS= read -r _pw || true
    case "${_pw}" in
        *[!A-Za-z0-9]*|"") die "the nuzur_ro password on stdin must be [A-Za-z0-9]{32}; refusing" ;;
    esac
    [ "${#_pw}" -eq 32 ] || die "the nuzur_ro password on stdin must be exactly 32 characters; refusing"
    case "${NUZUR_RO_HOST}" in
        *[!0-9./]*|"") die "NUZUR_RO_HOST must look like 10.1.0.0/255.255.0.0" ;;
    esac
    while IFS= read -r _line || [ -n "${_line}" ]; do
        case "${_line}" in --*) continue ;; esac
        while :; do
            case "${_line}" in
                *__NUZUR_RO_PASSWORD__*) _line="${_line%%__NUZUR_RO_PASSWORD__*}${_pw}${_line#*__NUZUR_RO_PASSWORD__}" ;;
                *__NUZUR_RO_HOST__*) _line="${_line%%__NUZUR_RO_HOST__*}${NUZUR_RO_HOST}${_line#*__NUZUR_RO_HOST__}" ;;
                *) break ;;
            esac
        done
        printf '%s\n' "${_line}"
    done < "${HERE}/grants.sql"
    _pw=
}

mysql_root() {
    if [ -n "${NUZUR_MYSQL_ROOT:-}" ]; then
        # shellcheck disable=SC2086
        sh -c "${NUZUR_MYSQL_ROOT}"
        return
    fi
    # shellcheck disable=SC2086
    _pod=$(${KUBECTL} -n "${METICHE_NAMESPACE}" get pod -l app.kubernetes.io/name=metiche-mysql \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    [ -n "${_pod}" ] || die "no metiche-mysql pod in namespace ${METICHE_NAMESPACE}"
    # shellcheck disable=SC2086
    ${KUBECTL} -n "${METICHE_NAMESPACE}" exec -i "${_pod}" -c mysql -- sh -c '
        export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
        exec mysql --user=root --batch --skip-column-names --default-character-set=utf8mb4'
}

# ── Column sources ──────────────────────────────────────────────────────────
# Both emit the same shape, sorted, so one classifier and one SQL writer serve
# the CI path and the live path:
#   table <TAB> ordinal <TAB> column <TAB> data_type(lowercase) <TAB> char_length(char/varchar only)

columns_from_create_sql() {
    [ -f "$1" ] || die "schema not found: $1"
    awk '
    /^CREATE TABLE/ {
        t = $0; sub(/^[^`]*`/, "", t); sub(/`.*$/, "", t); ord = 0; intable = 1; next
    }
    intable && /^\)/ { intable = 0; next }
    intable && /^[ \t]+`/ {
        line = $0; sub(/^[ \t]+`/, "", line)
        col = substr(line, 1, index(line, "`") - 1)
        rest = substr(line, index(line, "`") + 1); sub(/^[ \t]+/, "", rest)
        type = rest; sub(/[ \t].*$/, "", type); sub(/,$/, "", type)
        dt = tolower(type); len = ""
        if (index(dt, "(") > 0) { len = dt; sub(/^[^(]*\(/, "", len); sub(/[,)].*$/, "", len); sub(/\(.*$/, "", dt) }
        if (dt != "char" && dt != "varchar") len = ""
        printf "%s\t%d\t%s\t%s\t%s\n", t, ++ord, col, dt, len
    }' "$1" | LC_ALL=C sort -t "${TAB}" -k1,1 -k2,2n
}

columns_from_live() {
    printf '%s\n' "SELECT c.TABLE_NAME, c.ORDINAL_POSITION, c.COLUMN_NAME, LOWER(c.DATA_TYPE),
  IF(c.DATA_TYPE IN ('char','varchar'), c.CHARACTER_MAXIMUM_LENGTH, '')
FROM information_schema.COLUMNS c
JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
WHERE c.TABLE_SCHEMA = '${SRC_DB}' AND t.TABLE_TYPE = 'BASE TABLE';" \
    | mysql_root | LC_ALL=C sort -t "${TAB}" -k1,1 -k2,2n
}

# ── Classifier and writers ──────────────────────────────────────────────────
# $1 = what to print: "check" (nothing on success), "sql" (view DDL),
# "redacted" (table<TAB>column per redacted column), "expected" (table<TAB>
# ordinal<TAB>column), "tables". Reads the columns file $2. Exits 1 on any
# classification error, printing every offender first.
classify() {
    awk -v what="$1" -v risky="${RISKY}" -v srcdb="${SRC_DB}" -v viewdb="${VIEW_DB}" -v definer="${DEFINER}" -v policyname="${NUZUR_POLICY}" '
    function err(msg) { print "  " msg > "/dev/stderr"; nerr++ }
    FILENAME == ARGV[1] {
        raw = $0
        line = $0; sub(/#.*$/, "", line); gsub(/^[ \t]+|[ \t]+$/, "", line)
        if (line == "") next
        n = split(line, f, /[ \t]+/)
        if (n < 2 || f[1] !~ /^[a-z0-9_]+\.[a-z0-9_]+$/ || (f[2] != "expose" && f[2] != "expose!" && f[2] != "redact")) {
            err("malformed policy line " FNR ": " raw); next
        }
        if ((f[2] == "expose!" || f[2] == "redact") && n < 3) {
            err("policy line " FNR ": " f[1] " " f[2] " needs a reason after it"); next
        }
        if (f[1] in decision) { err("duplicate policy line " FNR ": " f[1] " (first on line " seen[f[1]] ")"); next }
        decision[f[1]] = f[2]; seen[f[1]] = FNR
        next
    }
    {
        split($0, c, "\t")
        t = c[1]; key = t "." c[3]
        if (!(t in tseen)) { tseen[t] = 1; tables[++nt] = t }
        ncol[t]++; cname[t, ncol[t]] = c[3]; ctype[t, ncol[t]] = c[4]; clen[t, ncol[t]] = c[5]
        live[key] = 1
        if (!(key in decision)) { unclassified[t]++; err("unclassified column: " key "  (add a line to " policyname ": expose, expose! or redact)"); next }
        if (decision[key] == "expose" && tolower(c[3]) ~ risky) {
            err("risky name marked plain expose: " key "  (write expose! with a reason, or redact)")
        }
    }
    END {
        for (k in decision) if (!(k in live)) err("stale policy line " seen[k] ": " k " (no such column in the schema)")
        for (i = 1; i <= nt; i++) {
            t = tables[i]; covered = 0
            for (j = 1; j <= ncol[t]; j++) if ((t "." cname[t, j]) in decision) covered++
            if (covered == 0) err("table with no policy lines: " t)
        }
        if (nt == 0) err("no tables found in the column source")
        if (nerr > 0) { printf "gen-views: %d policy error(s); no SQL was generated and nothing was applied\n", nerr > "/dev/stderr"; exit 1 }
        for (i = 1; i <= nt; i++) {
            t = tables[i]
            if (what == "tables") print t
            if (what == "sql") {
                printf "CREATE OR REPLACE ALGORITHM=MERGE DEFINER=%s SQL SECURITY DEFINER VIEW `%s`.`%s` AS SELECT\n", definer, viewdb, t
            }
            for (j = 1; j <= ncol[t]; j++) {
                col = cname[t, j]; d = decision[t "." col]
                if (what == "expected") printf "%s\t%d\t%s\n", t, j, col
                if (what == "redacted" && d == "redact") printf "%s\t%s\n", t, col
                if (what != "sql") continue
                if (d == "redact") {
                    ty = ctype[t, j]
                    if ((ty == "char" || ty == "varchar") && clen[t, j] != "") expr = "CAST(NULL AS CHAR(" clen[t, j] "))"
                    else if (ty ~ /text$/) expr = "CAST(NULL AS CHAR)"
                    else if (ty == "json") expr = "CAST(NULL AS JSON)"
                    else expr = "NULL"
                } else {
                    expr = "`" col "`"
                }
                printf "  %s AS `%s`%s\n", expr, col, (j < ncol[t] ? "," : "")
            }
            if (what == "sql") printf "FROM `%s`.`%s`;\n", srcdb, t
        }
    }' "${NUZUR_POLICY}" "$2"
}

# ── Live checks ─────────────────────────────────────────────────────────────

check_live() {
    _cols=$1
    _fail=0
    fail() { printf '  %s\n' "$*" >&2; _fail=1; }

    classify tables "${_cols}" > "${WORK}/tables"
    classify expected "${_cols}" > "${WORK}/expected"
    classify redacted "${_cols}" > "${WORK}/redacted"

    # 1. metiche_nuzur holds views only, one per table, with the right definer.
    printf '%s\n' "SELECT TABLE_NAME, TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = '${VIEW_DB}';" \
        | mysql_root | LC_ALL=C sort > "${WORK}/live_objects"
    awk -F'\t' '$2 != "VIEW" { print "  not a view in '"${VIEW_DB}"': " $1 }' "${WORK}/live_objects" > "${WORK}/msg"
    [ -s "${WORK}/msg" ] && { cat "${WORK}/msg" >&2; _fail=1; }
    awk -F'\t' '$2 == "VIEW" { print $1 }' "${WORK}/live_objects" > "${WORK}/live_views"
    LC_ALL=C comm -23 "${WORK}/tables" "${WORK}/live_views" | while IFS= read -r _t; do
        printf '  missing view: %s.%s  (run: gen-views.sh apply)\n' "${VIEW_DB}" "${_t}"
    done > "${WORK}/msg"
    [ -s "${WORK}/msg" ] && { cat "${WORK}/msg" >&2; _fail=1; }
    LC_ALL=C comm -13 "${WORK}/tables" "${WORK}/live_views" | while IFS= read -r _t; do
        printf '  unexpected view: %s.%s  (no such table in %s; run: gen-views.sh apply)\n' "${VIEW_DB}" "${_t}" "${SRC_DB}"
    done > "${WORK}/msg"
    [ -s "${WORK}/msg" ] && { cat "${WORK}/msg" >&2; _fail=1; }

    printf '%s\n' "SELECT TABLE_NAME, DEFINER, SECURITY_TYPE FROM information_schema.VIEWS WHERE TABLE_SCHEMA = '${VIEW_DB}';" \
        | mysql_root | awk -F'\t' '$2 != "nuzur_views@localhost" || $3 != "DEFINER" { print "  wrong definer or security type on view " $1 ": " $2 " " $3 }' > "${WORK}/msg"
    [ -s "${WORK}/msg" ] && { cat "${WORK}/msg" >&2; _fail=1; }

    # 2. Each view's column names equal the base table's, in order.
    printf '%s\n' "SELECT TABLE_NAME, ORDINAL_POSITION, COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = '${VIEW_DB}';" \
        | mysql_root | LC_ALL=C sort -t "${TAB}" -k1,1 -k2,2n > "${WORK}/live_view_cols"
    if ! cmp -s "${WORK}/expected" "${WORK}/live_view_cols"; then
        fail "view columns differ from the base tables (a migration ran without gen-views.sh apply):"
        diff "${WORK}/expected" "${WORK}/live_view_cols" | sed -n 's/^[<>]/    &/p' | head -40 >&2 || true
    fi

    # 3. Every redacted column is NULL on every row, through the view. A count,
    #    never a value. With the view in place the optimizer sees
    #    NULL IS NOT NULL and answers without reading a row.
    if [ -s "${WORK}/redacted" ]; then
        awk -F'\t' -v db="${VIEW_DB}" '
            { printf "%sSELECT %c%s.%s%c, COUNT(*) FROM `%s`.`%s` WHERE `%s` IS NOT NULL\n", (NR > 1 ? "UNION ALL " : ""), 39, $1, $2, 39, db, $1, $2 }
            END { print ";" }' "${WORK}/redacted" \
            | mysql_root > "${WORK}/redacted_counts" 2>"${WORK}/redacted_err" || {
                fail "could not count redacted columns: $(head -1 "${WORK}/redacted_err")"; }
        awk -F'\t' '$2 != "0" { print "  REDACTED COLUMN NOT NULL through the view: " $1 " (" $2 " rows)  — run gen-views.sh apply NOW" }' \
            "${WORK}/redacted_counts" > "${WORK}/msg"
        [ -s "${WORK}/msg" ] && { cat "${WORK}/msg" >&2; _fail=1; }
        _want=$(wc -l < "${WORK}/redacted" | tr -d ' ')
        _got=$(wc -l < "${WORK}/redacted_counts" | tr -d ' ')
        [ "${_want}" = "${_got}" ] || fail "expected ${_want} redacted-column counts, got ${_got}"
    fi

    # 4. Accounts and grants, exactly.
    _ro="\`nuzur_ro\`@\`${NUZUR_RO_HOST}\`"
    printf '%s\n' "SHOW GRANTS FOR 'nuzur_ro'@'${NUZUR_RO_HOST}';" | mysql_root 2>/dev/null | LC_ALL=C sort > "${WORK}/grants_ro" || true
    printf '%s\n%s\n' "GRANT SELECT ON \`${VIEW_DB}\`.* TO ${_ro}" "GRANT USAGE ON *.* TO ${_ro}" | LC_ALL=C sort > "${WORK}/grants_ro_want"
    cmp -s "${WORK}/grants_ro" "${WORK}/grants_ro_want" || {
        fail "nuzur_ro@${NUZUR_RO_HOST} grants are not exactly the expected set (never revoked automatically). Have:"
        sed 's/^/    /' "${WORK}/grants_ro" >&2; }
    printf '%s\n' "SHOW GRANTS FOR 'nuzur_views'@'localhost';" | mysql_root 2>/dev/null | LC_ALL=C sort > "${WORK}/grants_v" || true
    printf '%s\n%s\n' "GRANT SELECT ON \`${SRC_DB}\`.* TO ${DEFINER}" "GRANT USAGE ON *.* TO ${DEFINER}" | LC_ALL=C sort > "${WORK}/grants_v_want"
    cmp -s "${WORK}/grants_v" "${WORK}/grants_v_want" || {
        fail "nuzur_views@localhost grants are not exactly the expected set. Have:"
        sed 's/^/    /' "${WORK}/grants_v" >&2; }
    printf '%s\n' "SELECT CONCAT(User, '@', Host), account_locked, max_user_connections FROM mysql.user WHERE User IN ('nuzur_ro', 'nuzur_views') ORDER BY User, Host;" \
        | mysql_root > "${WORK}/accounts"
    printf 'nuzur_ro@%s\tN\t5\nnuzur_views@localhost\tY\t0\n' "${NUZUR_RO_HOST}" > "${WORK}/accounts_want"
    cmp -s "${WORK}/accounts" "${WORK}/accounts_want" || {
        fail "accounts differ from: nuzur_ro@${NUZUR_RO_HOST} unlocked with MAX_USER_CONNECTIONS 5, and nuzur_views@localhost locked. Have (user, locked, max_user_connections):"
        sed 's/^/    /' "${WORK}/accounts" >&2; }

    return "${_fail}"
}

# ── Main ────────────────────────────────────────────────────────────────────

MODE="${1:-}"
[ -f "${NUZUR_POLICY}" ] || die "policy not found: ${NUZUR_POLICY}"
WORK=$(mktemp -d)
trap 'rm -rf "${WORK}"' EXIT INT TERM

case "${MODE}" in
    check)
        columns_from_create_sql "${2:-${DEFAULT_SCHEMA}}" > "${WORK}/cols"
        classify check "${WORK}/cols"
        say "ok: $(classify tables "${WORK}/cols" | wc -l | tr -d ' ') tables, $(wc -l < "${WORK}/cols" | tr -d ' ') columns, $(classify redacted "${WORK}/cols" | wc -l | tr -d ' ') redacted — every column classified"
        ;;
    sql)
        columns_from_create_sql "${2:-${DEFAULT_SCHEMA}}" > "${WORK}/cols"
        classify sql "${WORK}/cols"
        ;;
    apply)
        columns_from_live > "${WORK}/cols"
        [ -s "${WORK}/cols" ] || die "read no columns for database ${SRC_DB}; is the schema applied?"
        # Classify first: any error exits here, before any DDL.
        classify sql "${WORK}/cols" > "${WORK}/views.sql"
        classify tables "${WORK}/cols" > "${WORK}/tables"
        printf '%s\n' "SELECT TABLE_NAME FROM information_schema.VIEWS WHERE TABLE_SCHEMA = '${VIEW_DB}';" \
            | mysql_root | LC_ALL=C sort > "${WORK}/live_views"
        {
            printf 'CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4;\n' "${VIEW_DB}"
            cat "${WORK}/views.sql"
            LC_ALL=C comm -13 "${WORK}/tables" "${WORK}/live_views" | while IFS= read -r _t; do
                printf 'DROP VIEW IF EXISTS `%s`.`%s`;\n' "${VIEW_DB}" "${_t}"
            done
        } > "${WORK}/apply.sql"
        say "applying $(wc -l < "${WORK}/tables" | tr -d ' ') views ($(LC_ALL=C comm -13 "${WORK}/tables" "${WORK}/live_views" | wc -l | tr -d ' ') stale dropped)"
        mysql_root < "${WORK}/apply.sql" > /dev/null
        check_live "${WORK}/cols" || die "applied, but check-live FAILED (above)"
        say "ok: views applied and verified"
        ;;
    grants)
        [ ! -t 0 ] || die "grants reads the nuzur_ro password on stdin; pipe it in (see deploy/scripts/nuzur-agent-setup.sh db)"
        fill_grants > "${WORK}/grants.fifo.sql"
        # The filled SQL touched only this private mktemp dir (0700), for the
        # time it takes to pipe it once; the EXIT trap removes it.
        chmod 600 "${WORK}/grants.fifo.sql"
        if grep -q '__NUZUR_' "${WORK}/grants.fifo.sql"; then die "a placeholder survived in grants.sql; refusing"; fi
        # A MySQL syntax error quotes the SQL "near" the fault, which here
        # could be the password. So the server's message is never shown: only
        # the error code and line.
        if ! mysql_root < "${WORK}/grants.fifo.sql" > /dev/null 2> "${WORK}/grants.err"; then
            : > "${WORK}/grants.fifo.sql"
            _code=$(sed -n 's/^\(ERROR [0-9]* ([0-9A-Z]*)\( at line [0-9]*\)\{0,1\}\).*/\1/p' "${WORK}/grants.err" | head -1)
            die "applying grants.sql failed: ${_code:-mysql exited non-zero} (message suppressed: it could quote the password)"
        fi
        : > "${WORK}/grants.fifo.sql"
        say "ok: database ${VIEW_DB}, nuzur_views@localhost (locked) and nuzur_ro@${NUZUR_RO_HOST} applied"
        ;;
    columns)
        columns_from_create_sql "${2:-${DEFAULT_SCHEMA}}"
        ;;
    redacted)
        columns_from_create_sql "${2:-${DEFAULT_SCHEMA}}" > "${WORK}/cols"
        classify redacted "${WORK}/cols"
        ;;
    check-live)
        columns_from_live > "${WORK}/cols"
        [ -s "${WORK}/cols" ] || die "read no columns for database ${SRC_DB}"
        classify check "${WORK}/cols"
        check_live "${WORK}/cols" || die "DRIFT: check-live failed (above)"
        say "ok: live schema classified, views current, redacted columns NULL, grants exact"
        ;;
    *) usage ;;
esac
