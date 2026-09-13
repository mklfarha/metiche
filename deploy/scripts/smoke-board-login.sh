#!/bin/sh
# smoke-board-login.sh — docs/BOARD_LOGIN.md §8 "Verification — through the
# real artifacts", items 1-14, against throwaway local processes only:
#
#   a fresh MySQL container (smoke-db, 127.0.0.1:33340), a locally built
#   backend, a locally built board on :8787, the real install.sh in scratch
#   HOMEs, and a real headless Chrome.
#
# It never contacts production. It prints one PASS / FAIL / NOT RUN line per
# item and exits non-zero when any item FAILs.
#
# Requires: docker, go, jq, curl, lsof, script(1), node (>= 22, for its
# built-in WebSocket), Google Chrome, templ.
#
# Environment:
#   SMOKE_CDP_HELPER     REQUIRED unless SMOKE_SKIP_BROWSER=1: path to the
#                        headless-Chrome driver (smoke-cdp.mjs). It is a
#                        throwaway helper and is not kept in this repository.
#   SMOKE_SKIP_BROWSER=1 mark item 3 NOT RUN instead of requiring the helper.
#   SMOKE_CHROME         Chrome binary (default: the macOS app bundle path).
#   SMOKE_BACKEND_PORT   backend HTTP port (default 18480).
#   SMOKE_SKIP_SUITES=1  mark item 14 NOT RUN (the suites take minutes).
#
# Secrets: every token, link secret and session secret stays in shell
# variables or 0600 files inside a 0700 work directory removed on exit. curl
# gets credentials through -K config files, never argv. Output shows only
# prefixes and lengths.
#
# POSIX sh. set -e, deliberately not pipefail (see lib.sh).
set -eu

# shellcheck source=lib.sh
. "$(dirname -- "$0")/lib.sh"

REPO="$METICHE_REPO_ROOT"
BACKEND_DIR="$REPO/code/backend/metiche"
FRONTEND_DIR="$REPO/code/frontend"
DB_NAME_CONTAINER="smoke-db"
DB_PORT=33340
BACKEND_PORT="${SMOKE_BACKEND_PORT:-18480}"
BOARD_PORT=8787
BOARD_BASE="http://localhost:${BOARD_PORT}"
BOARD="http://127.0.0.1:${BOARD_PORT}"
BACKEND="http://127.0.0.1:${BACKEND_PORT}"
MCP_URL="${BACKEND}/v1/mcp"
CHROME="${SMOKE_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
NOSUCH="no-such-team-7q"
MCP_PROTOCOL="2025-06-18"
START_EPOCH=$(date +%s)

for c in docker go jq curl lsof script shasum sed awk; do
    need_cmd "$c" "smoke-board-login.sh needs it."
done
if [ "${SMOKE_SKIP_BROWSER:-0}" != "1" ]; then
    [ -n "${SMOKE_CDP_HELPER:-}" ] ||
        die "SMOKE_CDP_HELPER is not set. Point it at the headless-Chrome driver (smoke-cdp.mjs), or set SMOKE_SKIP_BROWSER=1 to mark item 3 NOT RUN."
    [ -f "$SMOKE_CDP_HELPER" ] || die "SMOKE_CDP_HELPER=$SMOKE_CDP_HELPER is not a file."
    need_cmd node "item 3 drives Chrome with node."
    [ -x "$CHROME" ] || die "no Chrome at $CHROME; set SMOKE_CHROME."
fi

# ── work dir, cleanup ────────────────────────────────────────────────────────
W=$(mktemp -d "${TMPDIR:-/tmp}/metiche-smoke.XXXXXX")
chmod 700 "$W"
mkdir -p "$W/bin" "$W/c" "$W/shim" "$W/open" "$W/r"
umask 077
: > "$W/secrets.list"
: > "$W/results"
: > "$W/pids"
: > "$W/c/none"
DB_CREATED=0

cleanup() {
    _rc=$?
    trap - EXIT INT TERM
    # Dying before the results table is a failure, whatever $? says.
    if [ "${FINISHED:-0}" != "1" ] && [ "$_rc" -eq 0 ]; then
        warn "the run stopped before the results table"
        _rc=1
    fi
    if [ -s "$W/pids" ]; then
        while read -r _p; do
            [ -n "$_p" ] && kill "$_p" 2>/dev/null || true
        done < "$W/pids"
        sleep 1
        while read -r _p; do
            [ -n "$_p" ] && kill -9 "$_p" 2>/dev/null || true
        done < "$W/pids"
    fi
    if [ "$DB_CREATED" = "1" ]; then
        docker unpause "$DB_NAME_CONTAINER" >/dev/null 2>&1 || true
        docker rm -f "$DB_NAME_CONTAINER" >/dev/null 2>&1 || true
    fi
    if [ "${SMOKE_KEEP_WORKDIR:-0}" = "1" ]; then
        warn "SMOKE_KEEP_WORKDIR=1: leaving $W (it holds secrets; delete it)"
    else
        rm -rf "$W"
    fi
    exit "$_rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

track_pid() { printf '%s\n' "$1" >> "$W/pids"; }

# ── results ──────────────────────────────────────────────────────────────────
ITEM=""
ITEM_FAILS=0
ITEM_NOTES=""
begin_item() { ITEM="$1"; ITEM_FAILS=0; ITEM_NOTES=""; step "item $1: $2"; }
ok()   { log "ok: $*"; }
bad()  { ITEM_FAILS=$((ITEM_FAILS + 1)); warn "FAIL: $*"; ITEM_NOTES="${ITEM_NOTES:+$ITEM_NOTES; }FAIL $*"; }
note() { log "note: $*"; ITEM_NOTES="${ITEM_NOTES:+$ITEM_NOTES; }$*"; }
check() {
    # check "<description>" <command...>
    _d="$1"; shift
    if "$@"; then ok "$_d"; else bad "$_d"; fi
}
end_item() {
    if [ "$ITEM_FAILS" -gt 0 ]; then _s=FAIL; else _s=PASS; fi
    printf '%s|%s|%s\n' "$ITEM" "$_s" "$ITEM_NOTES" >> "$W/results"
}
not_run() { printf '%s|NOT RUN|%s\n' "$1" "$2" >> "$W/results"; step "item $1: NOT RUN ($2)"; }

# ── small helpers ────────────────────────────────────────────────────────────
remember_secret() { printf '%s\n' "$1" >> "$W/secrets.list"; }
# show_redacted <file> [lines]: the tail of a file with every credential masked.
show_redacted() {
    tail -n "${2:-25}" "$1" | tr -d '\r' |
        sed -E 's/(mtk|mbl|mbs)_[A-Za-z0-9_-]+/\1_<redacted>/g' |
        if [ -n "${DB_PASS:-}" ]; then sed "s/$DB_PASS/<redacted>/g"; else cat; fi |
        sed 's/^/      | /' >&2
}
shape() {
    # A secret shown as its prefix and length only.
    printf '%s…(%s chars)' "$(printf '%s' "$1" | cut -c1-4)" "$(printf '%s' "$1" | wc -c | tr -d ' ')"
}
sha256_of() { printf '%s' "$1" | shasum -a 256 | awk '{print $1}'; }
eq() { [ "$1" = "$2" ]; }
contains() { case "$1" in *"$2"*) return 0 ;; *) return 1 ;; esac; }
port_free() { ! lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }
wait_listen() {
    # $1 port, $2 pid, $3 seconds
    _i=0
    while [ "$_i" -lt "$(( $3 * 10 ))" ]; do
        if ! kill -0 "$2" 2>/dev/null; then return 1; fi
        if lsof -nP -a -p "$2" -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; then return 0; fi
        sleep 0.1; _i=$((_i + 1))
    done
    return 1
}

# sql: run SQL (stdin or $1) in the smoke database, tab-separated, no headers.
sql() {
    if [ $# -gt 0 ]; then
        printf '%s\n' "$1" | docker exec -i "$DB_NAME_CONTAINER" mysql -h127.0.0.1 -N -B metiche
    else
        docker exec -i "$DB_NAME_CONTAINER" mysql -h127.0.0.1 -N -B metiche
    fi
}

# A curl -K file carrying one header. $1 file name, $2 header line.
header_cfg() { printf 'header = "%s"\n' "$(printf '%s' "$2" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')" > "$W/c/$1"; }
cookie_cfg() { header_cfg "$1" "Cookie: metiche_session=$2"; }
session_cfg() { header_cfg "$1" "X-Metiche-Browser-Session: $2"; }

# req: one HTTP request. $1 out prefix under $W/r, then curl args.
# Writes <prefix>.h (headers), <prefix>.b (body); prints the status code.
req() {
    _o="$W/r/$1"; shift
    curl -sS --max-time "${REQ_TIMEOUT:-20}" -D "$_o.h" -o "$_o.b" -w '%{http_code}' "$@" 2>"$_o.e" || {
        printf '000'
        return 0
    }
}
hdr() {
    # hdr <prefix> <Header-Name>: the first value, CR stripped.
    tr -d '\r' < "$W/r/$1.h" | awk -v n="$2" 'BEGIN{IGNORECASE=1} { split($0, a, ":"); if (tolower(a[1]) == tolower(n)) { sub(/^[^:]*:[ \t]*/, ""); print; exit } }'
}
set_cookie_line() { tr -d '\r' < "$W/r/$1.h" | sed -n 's/^[Ss][Ee][Tt]-[Cc][Oo][Oo][Kk][Ii][Ee]: *//p' | sed -n '1p'; }
cookie_secret() { set_cookie_line "$1" | sed -n 's/^metiche_session=\([^;]*\).*/\1/p'; }
cleared_cookie() {
    # true when the response deletes the session cookie
    _l=$(set_cookie_line "$1")
    case "$_l" in
        metiche_session=\;*Max-Age=0*|metiche_session=\;*[Ee]xpires=*1970*) return 0 ;;
        *) return 1 ;;
    esac
}
has_set_cookie() { [ -n "$(set_cookie_line "$1")" ]; }

# ── MCP client (same wire behaviour as install.sh: bearer via -K on stdin) ───
MCP_SID=""
MCP_HTTP=""
mcp_post() {
    # $1 bearer, $2 JSON body. Body in $W/r/mcp.b, status in MCP_HTTP.
    _cfg=$(
        printf 'header = "Content-Type: application/json"\n'
        printf 'header = "Accept: application/json, text/event-stream"\n'
        [ -n "$MCP_SID" ] && printf 'header = "Mcp-Session-Id: %s"\n' "$MCP_SID"
        [ -n "$1" ] && printf 'header = "Authorization: Bearer %s"\n' "$1"
        printf 'data = "%s"\n' "$(printf '%s' "$2" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
    )
    MCP_HTTP=$(printf '%s\n' "$_cfg" |
        curl -sS -X POST -K - --max-time 20 -D "$W/r/mcp.h" -o "$W/r/mcp.b" -w '%{http_code}' "$MCP_URL" 2>"$W/r/mcp.e") ||
        MCP_HTTP=000
}
mcp_connect() {
    MCP_SID=""
    mcp_post "$1" "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"$MCP_PROTOCOL\",\"capabilities\":{},\"clientInfo\":{\"name\":\"metiche-smoke\",\"version\":\"1\"}}}"
    case "$MCP_HTTP" in 2*) ;; *) return 1 ;; esac
    MCP_SID=$(tr -d '\r' < "$W/r/mcp.h" | sed -n 's/^[Mm][Cc][Pp]-[Ss][Ee][Ss][Ss][Ii][Oo][Nn]-[Ii][Dd]: *//p' | sed -n '1p')
    mcp_post "$1" '{"jsonrpc":"2.0","method":"notifications/initialized"}'
    case "$MCP_HTTP" in 2*) return 0 ;; *) return 1 ;; esac
}
# mcp_tool <bearer> <tool> <args-json>: the tool's JSON text goes to
# $W/r/mcp.out; returns 1 on any error (MCP_HTTP keeps the status, a redacted
# message goes to $W/r/mcp.err). Not for use in $(...).
mcp_tool() {
    : > "$W/r/mcp.out"
    printf 'transport or HTTP error' > "$W/r/mcp.err"
    mcp_connect "$1" || return 1
    mcp_post "$1" "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"$2\",\"arguments\":$3}}"
    case "$MCP_HTTP" in 2*) ;; *) return 1 ;; esac
    if grep -q '^data:' "$W/r/mcp.b"; then
        _pl=$(sed -n 's/^data: \{0,1\}//p' "$W/r/mcp.b" | sed -n '1p')
    else
        _pl=$(cat "$W/r/mcp.b")
    fi
    if [ "$(printf '%s' "$_pl" | jq -r '(has("error")) or (.result.isError // false)' 2>/dev/null)" != "false" ]; then
        printf '%s' "$_pl" | jq -r '.error.message // .result.content[0].text // "error"' 2>/dev/null |
            sed -E 's/(mtk|mbl|mbs)_[A-Za-z0-9_-]+/\1_<redacted>/g' > "$W/r/mcp.err"
        return 1
    fi
    printf '%s' "$_pl" | jq -r '.result.content[0].text // empty' > "$W/r/mcp.out"
    _pl=""
}
cursor_token() {
    # The bearer the installer wrote for Cursor in scratch HOME $1. Never printed.
    jq -r '.mcpServers.metiche.headers.Authorization // empty' "$1/.cursor/mcp.json" 2>/dev/null | sed -n 's/^Bearer //p'
}
# mint <bearer> <slug>: sets LINK and SECRET (never printed).
mint() {
    LINK=""; SECRET=""
    mcp_tool "$1" open_board "{\"team_slug\":\"$2\",\"requested_via\":\"cli\"}" || return 1
    LINK=$(jq -r '.login_url // empty' "$W/r/mcp.out")
    : > "$W/r/mcp.out"
    SECRET=${LINK#*#}
    case "$SECRET" in mbl_?*) remember_secret "$SECRET"; return 0 ;; *) return 1 ;; esac
}
# redeem <secret> <prefix>: POST /signin. Sets REDEEM_STATUS and SESSION
# (empty when no cookie was set). Not for use in $(...): it sets variables.
redeem() {
    printf 'header = "Origin: %s"\nheader = "Content-Type: application/x-www-form-urlencoded"\ndata = "link=%s"\n' \
        "$BOARD_BASE" "$1" > "$W/c/redeem.cfg"
    REDEEM_STATUS=$(req "$2" -X POST -K "$W/c/redeem.cfg" "$BOARD/signin")
    : > "$W/c/redeem.cfg"
    SESSION=$(cookie_secret "$2")
    if [ -n "$SESSION" ]; then remember_secret "$SESSION"; fi
}
random_b64url() { LC_ALL=C tr -dc 'A-Za-z0-9_-' < /dev/urandom | head -c "${1:-43}"; }
# redirect_file <link> <path>: the installer's own redirect page, 0600.
redirect_file() {
    printf '<!doctype html>\n<meta charset="utf-8">\n<meta name="referrer" content="no-referrer">\n<meta http-equiv="refresh" content="0; url=%s">\n<title>metiche: signing in</title>\n<script>location.replace("%s")</script>\n' "$1" "$1" > "$2"
    chmod 600 "$2"
}
same_body() { cmp -s "$W/r/$1.b" "$W/r/$2.b"; }
# normalize <prefix> <slug>: body with the slug text replaced, in <prefix>.n
normalize() { sed "s/$2/SLUG/g" "$W/r/$1.b" > "$W/r/$1.n"; }
same_norm() { cmp -s "$W/r/$1.n" "$W/r/$2.n"; }

# ── installer runs ───────────────────────────────────────────────────────────
make_home() {
    mkdir -p "$1/.cursor" "$1/.codex" "$1/tmp"
    : > "$1/.zshrc"
}
# run_installer <home> <out-file> <tokfile-or-empty> <env assignments...> -- <installer args...>
run_installer() {
    _h="$1"; _out="$2"; _tokfile="$3"; shift 3
    _envs=""
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do
        # KEY='value': values here are team and member names, never quotes.
        _envs="$_envs
${1%%=*}='${1#*=}'"
        shift
    done
    [ "${1:-}" = "--" ] && shift
    printf '%s\n' "$_envs" | sed '/^$/d' > "$W/c/installer.env"
    env -i \
        HOME="$_h" CODEX_HOME="$_h/.codex" ZDOTDIR="$_h" SHELL=/bin/zsh TERM=dumb LANG=C \
        TMPDIR="$_h/tmp" PATH="$W/shim:/usr/bin:/bin:/usr/sbin:/sbin" \
        SMOKE_OPEN_LOG_DIR="$W/open" SMOKE_ENVFILE="$W/c/installer.env" SMOKE_TOKFILE="$_tokfile" \
        /bin/sh -c 'set -a; . "$SMOKE_ENVFILE"; if [ -n "$SMOKE_TOKFILE" ]; then . "$SMOKE_TOKFILE"; fi; set +a
                    unset SMOKE_ENVFILE SMOKE_TOKFILE
                    exec script -q /dev/null /bin/sh "$@"' smoke "$REPO/install.sh" --url "$MCP_URL" "$@" \
        </dev/null > "$_out" 2>&1
}

# ═════════════════════════════════════════════════════════════════════════════
step "setup"

port_free "$BACKEND_PORT" || die "port $BACKEND_PORT is already in use; set SMOKE_BACKEND_PORT to a free one."
port_free "$BOARD_PORT"   || die "port $BOARD_PORT is already in use (the board must run on it: links are minted for $BOARD_BASE)."
port_free "$DB_PORT"      || die "port $DB_PORT is already in use."
if docker inspect "$DB_NAME_CONTAINER" >/dev/null 2>&1; then
    die "a container named $DB_NAME_CONTAINER already exists; it is not this run's, so it is left alone. Remove it yourself."
fi

log "building the backend and the board"
(cd "$BACKEND_DIR" && go build -o "$W/bin/metiche-backend" .) || die "backend build failed"
(cd "$FRONTEND_DIR" && go build -o "$W/bin/metiche-web" ./cmd/metiche-web) || die "board build failed"

log "starting MySQL ($DB_NAME_CONTAINER on 127.0.0.1:$DB_PORT)"
DB_PASS=$(LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 32)
printf 'MYSQL_ROOT_PASSWORD=%s\n' "$DB_PASS" > "$W/c/db.env"
printf '[client]\nuser=root\npassword=%s\n' "$DB_PASS" > "$W/c/my.cnf"
docker run -d --name "$DB_NAME_CONTAINER" --env-file "$W/c/db.env" \
    -p "127.0.0.1:${DB_PORT}:3306" mysql:8.4 >/dev/null
DB_CREATED=1
: > "$W/c/db.env"
_i=0
until docker exec -i "$DB_NAME_CONTAINER" sh -c 'cat > /root/.my.cnf && chmod 600 /root/.my.cnf' < "$W/c/my.cnf" 2>/dev/null; do
    _i=$((_i + 1)); [ "$_i" -lt 60 ] || die "could not reach the container"; sleep 1
done
_i=0
until docker exec "$DB_NAME_CONTAINER" mysql -h127.0.0.1 -N -B -e 'SELECT 1' >/dev/null 2>&1; do
    _i=$((_i + 1)); [ "$_i" -lt 120 ] || die "MySQL did not come up"; sleep 1
done
docker exec "$DB_NAME_CONTAINER" mysql -h127.0.0.1 -e 'CREATE DATABASE metiche CHARACTER SET utf8mb4; CREATE DATABASE metiche_suite CHARACTER SET utf8mb4;'
sql < "$BACKEND_DIR/core/repository/sql/schema/create.sql"
sql < "$REPO/deploy/sql/seed-default-plan.sql"
docker exec -i "$DB_NAME_CONTAINER" mysql -h127.0.0.1 metiche_suite < "$BACKEND_DIR/core/repository/sql/schema/create.sql"
log "schema applied: $(sql 'SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE()') tables"

cat > "$W/c/backend.yaml" <<EOF
ports:
  http: "$BACKEND_PORT"
db:
  - name: metiche
    host: 127.0.0.1
    port: "$DB_PORT"
    user: root
    pswd: "$DB_PASS"
    params: "parseTime=true&interpolateParams=true&charset=utf8mb4&loc=UTC"
    driver: "mysql"
monitoring:
  enabled: false
EOF

log "starting the backend on $BACKEND"
env -i PATH=/usr/bin:/bin HOME="$W" \
    CONFIG="$BACKEND_DIR/config,$W/c/backend.yaml" METICHE_ROLE=all \
    METICHE_BOARD_BASE_URL="$BOARD_BASE" \
    "$W/bin/metiche-backend" > "$W/backend.log" 2>&1 &
BACKEND_PID=$!
track_pid "$BACKEND_PID"
wait_listen "$BACKEND_PORT" "$BACKEND_PID" 60 || { tail -n 20 "$W/backend.log" >&2; die "the backend did not start"; }

log "starting the board on $BOARD"
env -i PATH=/usr/bin:/bin HOME="$W" \
    "$W/bin/metiche-web" -addr "127.0.0.1:$BOARD_PORT" -backend "$BACKEND" -dev-insecure-cookie \
    -base-url "$BOARD_BASE" -viewer-reauth 2s > "$W/board.log" 2>&1 &
BOARD_PID=$!
track_pid "$BOARD_PID"
wait_listen "$BOARD_PORT" "$BOARD_PID" 30 || { tail -n 20 "$W/board.log" >&2; die "the board did not start"; }

# The open shim: records argv, the mode of the file it was given, and a copy.
cat > "$W/shim/open" <<'EOF'
#!/bin/sh
d="${SMOKE_OPEN_LOG_DIR:?}"
n="$(date +%s).$$"
for a in "$@"; do printf '%s\n' "$a" >> "$d/argv.$n"; done
for a in "$@"; do
    if [ -f "$a" ]; then
        stat -f '%Lp' "$a" > "$d/mode.$n"
        cp "$a" "$d/file.$n"
    fi
done
exit 0
EOF
chmod 755 "$W/shim/open"

# ═════════════════════════════════════════════════════════════════════════════
# Item 1: the installer mints and opens.
H1="$W/h1"; make_home "$H1"
T1_NAME="Smoke Alpha"
begin_item 1 "installer mint (--only cursor --open)"
_rc=0
run_installer "$H1" "$W/install1.out" "" "METICHE_TEAM_NAME=$T1_NAME" "METICHE_MEMBER_NAME=Smoke One" -- \
    --only cursor --open || _rc=$?
check "installer exit 0 (got $_rc)" eq "$_rc" 0
[ "$_rc" -eq 0 ] || show_redacted "$W/install1.out" 30
T1=$(sql "SELECT slug FROM team WHERE name = '$T1_NAME' LIMIT 1")
check "team created (slug ${T1:-<none>})" test -n "$T1"
TOK1=$(cursor_token "$H1")
check "cursor token written to ~/.cursor/mcp.json ($(shape "$TOK1"))" test -n "$TOK1"
_argvs=$(ls "$W/open"/argv.* 2>/dev/null | wc -l | tr -d ' ')
check "open shim called once (got $_argvs)" eq "$_argvs" 1
LINK1=""
if [ "$_argvs" -ge 1 ]; then
    _argv=$(ls "$W/open"/argv.* | sed -n 1p)
    _n=${_argv##*/argv.}
    check "shim got exactly one argument" eq "$(wc -l < "$_argv" | tr -d ' ')" 1
    _arg=$(sed -n 1p "$_argv")
    case "$_arg" in /*.html) ok "the argument is a file path (…/${_arg##*/})" ;; *) bad "the argument is not an .html file path" ;; esac
    if grep -q 'mbl_' "$W/open"/argv.*; then bad "an argv contains mbl_"; else ok "no argv contains mbl_"; fi
    check "the file was mode 600 when opened (got $(cat "$W/open/mode.$_n" 2>/dev/null))" eq "$(cat "$W/open/mode.$_n" 2>/dev/null)" 600
    LINK1=$(sed -n 's/.*location\.replace("\([^"]*\)").*/\1/p' "$W/open/file.$_n" 2>/dev/null | sed -n 1p)
fi
SECRET1=${LINK1#*#}
case "$LINK1" in
    "$BOARD_BASE/signin#mbl_"?*) ok "link extracted from the copied file ($BOARD_BASE/signin#$(shape "$SECRET1"))"; remember_secret "$SECRET1" ;;
    *) bad "no link of the expected shape in the copied file"; SECRET1="" ;;
esac
if [ -n "$SECRET1" ]; then
    _cnt=$(grep -o -F "$SECRET1" "$W/install1.out" | wc -l | tr -d ' ')
    check "the link appears in stdout at most once (got $_cnt)" test "$_cnt" -le 1
    [ "$_cnt" = 1 ] || note "link printed $_cnt times"
fi
_mtk=$(grep -c 'mtk_' "$W/install1.out" || true)
check "no mtk_ in installer output (got $_mtk lines)" eq "$_mtk" 0
end_item

# ═════════════════════════════════════════════════════════════════════════════
# Item 2: curl redeems.
begin_item 2 "curl exchange"
_s=$(req i2get "$BOARD/signin")
check "GET /signin 200 (got $_s)" eq "$_s" 200
check "GET /signin Cache-Control no-store" contains "$(hdr i2get Cache-Control)" no-store
check "GET /signin Referrer-Policy no-referrer" eq "$(hdr i2get Referrer-Policy)" no-referrer
check "GET /signin has a Content-Security-Policy with script-src 'self'" contains "$(hdr i2get Content-Security-Policy)" "script-src 'self'"
COOKIE1=""
if [ -n "$SECRET1" ]; then
    redeem "$SECRET1" i2post
    _s=$REDEEM_STATUS
    COOKIE1="$SESSION"
    check "POST /signin 200 (got $_s)" eq "$_s" 200
    check "POST /signin body redirects to /t/$T1" eq "$(jq -r '.redirect // empty' "$W/r/i2post.b" 2>/dev/null)" "/t/$T1"
    _sc=$(set_cookie_line i2post)
    check "Set-Cookie present ($(shape "$COOKIE1"))" test -n "$COOKIE1"
    case "$COOKIE1" in mbs_?*) ok "session secret has the mbs_ prefix" ;; *) bad "session secret lacks the mbs_ prefix" ;; esac
    _attrs=$(printf '%s' "$_sc" | sed 's/^[^;]*;//' | tr ';' '\n' | sed 's/^ *//')
    check "cookie name metiche_session" contains "$_sc" "metiche_session="
    check "HttpOnly" sh -c 'printf "%s\n" "$1" | grep -qx HttpOnly' _ "$_attrs"
    check "SameSite=Lax" sh -c 'printf "%s\n" "$1" | grep -qx "SameSite=Lax"' _ "$_attrs"
    check "Path=/" sh -c 'printf "%s\n" "$1" | grep -qx "Path=/"' _ "$_attrs"
    check "no Domain" sh -c '! printf "%s\n" "$1" | grep -qi "^Domain="' _ "$_attrs"
    check "no Secure (dev mode)" sh -c '! printf "%s\n" "$1" | grep -qix "Secure"' _ "$_attrs"
    _age=$(printf '%s\n' "$_attrs" | sed -n 's/^Max-Age=//p')
    check "Max-Age ≈ 30 d (got ${_age:-none})" test "${_age:-0}" -ge 2588400 -a "${_age:-0}" -le 2592000
    check "POST /signin Cache-Control no-store" contains "$(hdr i2post Cache-Control)" no-store
    cookie_cfg c1 "$COOKIE1"
    _s=$(req i2board -K "$W/c/c1" "$BOARD/t/$T1")
    check "GET /t/$T1 with the cookie 200 (got $_s)" eq "$_s" 200
    check "the team name is in the body" grep -q "$T1_NAME" "$W/r/i2board.b"
    check "Cache-Control: private, no-store (got $(hdr i2board Cache-Control))" eq "$(hdr i2board Cache-Control)" "private, no-store"
else
    bad "no link from item 1 to redeem"
fi
end_item

# ═════════════════════════════════════════════════════════════════════════════
# Item 3: a real browser.
if [ "${SMOKE_SKIP_BROWSER:-0}" = "1" ]; then
    not_run 3 "SMOKE_SKIP_BROWSER=1"
else
    begin_item 3 "real headless Chrome"
    if [ -z "$TOK1" ] || [ -z "$T1" ]; then
        bad "no token or team from item 1"
    elif ! mint "$TOK1" "$T1"; then
        bad "open_board via MCP failed (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
    else
        ok "a new link minted via MCP with the installer-written cursor token ($(shape "$SECRET"))"
        mkdir -p "$W/cdp"
        redirect_file "$LINK" "$W/c/signin3.html"
        LINK=""
        node "$SMOKE_CDP_HELPER" "$CHROME" "$W/c/signin3.html" "$W/cdp" "$BOARD_BASE" "$T1" > "$W/cdp.log" 2>&1 &
        CDP_PID=$!
        track_pid "$CDP_PID"
        _i=0
        while [ ! -f "$W/cdp/ready.json" ] && kill -0 "$CDP_PID" 2>/dev/null && [ "$_i" -lt 600 ]; do
            sleep 0.1; _i=$((_i + 1))
        done
        if [ ! -f "$W/cdp/ready.json" ]; then
            bad "the browser never became ready: $(jq -r '.error // "no result"' "$W/cdp/result.json" 2>/dev/null)"
            show_redacted "$W/cdp.log" 10
        else
            check "landed on /t/$T1 (href $(jq -r .href "$W/cdp/ready.json"))" eq "$(jq -r .landed "$W/cdp/ready.json")" true
            check "location.href has no '#'" eq "$(jq -r .hrefHasHash "$W/cdp/ready.json")" false
            check "signed in (sign-out form / csrf meta present)" eq "$(jq -r .signedIn "$W/cdp/ready.json")" true
            check "board SSE connected (status $(jq -r .streamStatus "$W/cdp/ready.json"))" eq "$(jq -r .streamOpened "$W/cdp/ready.json")" true
            MARKER="smoke marker $(date +%s)"
            if mcp_tool "$TOK1" start_session "$(jq -nc --arg s "$T1" --arg m "$MARKER" \
                '{team_slug:$s, project_key:"smoke-repo", goal:$m, status_line:$m, confirm_new_project:"person"}')"; then
                ok "start_session with the agent token"
            else
                bad "start_session failed (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
            fi
            printf '%s\n' "$MARKER" > "$W/cdp/marker.txt"
        fi
        wait "$CDP_PID" 2>/dev/null || true
        if [ -f "$W/cdp/result.json" ]; then
            check "the start_session appears on the board" eq "$(jq -r .markerSeen "$W/cdp/result.json")" true
            check "request recorder: no URL contains mbl_ (of $(jq -r .requests "$W/cdp/result.json") requests)" \
                eq "$(jq -r '.requestsWithMbl | length' "$W/cdp/result.json")" 0
            check "request recorder: no Referer contains mbl_" eq "$(jq -r .referersWithMbl "$W/cdp/result.json")" 0
            check "SSE delivered messages ($(jq -r .sseMessages "$W/cdp/result.json"))" test "$(jq -r .sseMessages "$W/cdp/result.json")" -gt 0
            _err=$(jq -r '.error // empty' "$W/cdp/result.json")
            [ -z "$_err" ] || bad "browser helper error: $_err"
        else
            bad "no result from the browser helper"
        fi
    fi
    end_item
fi

# ═════════════════════════════════════════════════════════════════════════════
# Item 4: privacy. A second identity; 404s compared byte for byte.
begin_item 4 "privacy (second identity, 404 comparisons)"
H2="$W/h2"; make_home "$H2"
T2_NAME="Smoke Bravo"
T2_GUESS="smoke-bravo"
# The same slug BEFORE the team exists: anonymous and as a signed-in
# non-member. After it exists (private) the bytes must not change.
PRE_TS=$(date +%s)
_pa=$(req i4pre_anon "$BOARD/t/$T2_GUESS")
_pc=""
[ -n "$COOKIE1" ] && _pc=$(req i4pre_c1 -K "$W/c/c1" "$BOARD/t/$T2_GUESS")
_rc=0
run_installer "$H2" "$W/install2.out" "" "METICHE_TEAM_NAME=$T2_NAME" "METICHE_MEMBER_NAME=Smoke Two" -- \
    --only cursor --no-open || _rc=$?
check "second installer exit 0 (got $_rc)" eq "$_rc" 0
[ "$_rc" -eq 0 ] || show_redacted "$W/install2.out" 30
T2=$(sql "SELECT slug FROM team WHERE name = '$T2_NAME' LIMIT 1")
TOK2=$(cursor_token "$H2")
check "second team $T2 and token ($(shape "$TOK2"))" test -n "$T2" -a -n "$TOK2"
COOKIE2=""
if [ -n "$TOK2" ] && mint "$TOK2" "$T2"; then
    redeem "$SECRET" i4redeem
    COOKIE2="$SESSION"
    check "second identity redeems its own link (status $REDEEM_STATUS, $(shape "$COOKIE2"))" test -n "$COOKIE2"
else
    bad "second identity could not mint (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
fi
if [ -n "$COOKIE2" ] && [ -n "$T1" ]; then
    cookie_cfg c2 "$COOKIE2"
    # Board, signed-in non-member.
    _a=$(req i4c2_t1 -K "$W/c/c2" "$BOARD/t/$T1")
    _b=$(req i4c2_ns -K "$W/c/c2" "$BOARD/t/$NOSUCH")
    check "non-member GET /t/$T1 404 (got $_a; unknown slug $_b)" test "$_a" = 404 -a "$_b" = 404
    normalize i4c2_t1 "$T1"; normalize i4c2_ns "$NOSUCH"
    check "non-member: /t/$T1 and /t/$NOSUCH identical with the slug normalised" same_norm i4c2_t1 i4c2_ns
    REQ_TIMEOUT=10
    _a=$(req i4c2_t1s -K "$W/c/c2" "$BOARD/t/$T1/stream")
    _b=$(req i4c2_nss -K "$W/c/c2" "$BOARD/t/$NOSUCH/stream")
    check "non-member /t/$T1/stream 404 (got $_a; unknown $_b)" test "$_a" = 404 -a "$_b" = 404
    check "non-member stream 404 byte-identical to the unknown slug's" same_body i4c2_t1s i4c2_nss
    # Board, anonymous.
    _a=$(req i4an_t1 "$BOARD/t/$T1")
    _b=$(req i4an_ns "$BOARD/t/$NOSUCH")
    check "anonymous GET /t/$T1 404 (got $_a; unknown $_b)" test "$_a" = 404 -a "$_b" = 404
    normalize i4an_t1 "$T1"; normalize i4an_ns "$NOSUCH"
    check "anonymous: /t/$T1 and /t/$NOSUCH identical with the slug normalised" same_norm i4an_t1 i4an_ns
    _a=$(req i4an_t1s "$BOARD/t/$T1/stream")
    _b=$(req i4an_nss "$BOARD/t/$NOSUCH/stream")
    check "anonymous /t/$T1/stream 404 byte-identical to unknown (got $_a/$_b)" \
        sh -c '[ "$1" = 404 ] && [ "$2" = 404 ] && cmp -s "$3" "$4"' _ "$_a" "$_b" "$W/r/i4an_t1s.b" "$W/r/i4an_nss.b"
    REQ_TIMEOUT=20
    # Same slug across team states: before Smoke Bravo existed, and now.
    if [ "$T2" = "$T2_GUESS" ]; then
        _wait=$(( PRE_TS + 31 - $(date +%s) ))
        if [ "$_wait" -gt 0 ]; then log "waiting ${_wait}s for the anonymous negative cache (30s) to expire"; sleep "$_wait"; fi
        _a=$(req i4post_anon "$BOARD/t/$T2")
        check "anonymous /t/$T2 before creation ($_pa) and now private ($_a): identical bytes" \
            sh -c '[ "$1" = 404 ] && [ "$2" = 404 ] && cmp -s "$3" "$4"' _ "$_pa" "$_a" "$W/r/i4pre_anon.b" "$W/r/i4post_anon.b"
        ANON_T2_TS=$(date +%s)
        if [ -n "$_pc" ]; then
            _a=$(req i4post_c1 -K "$W/c/c1" "$BOARD/t/$T2")
            check "signed-in non-member /t/$T2 before creation ($_pc) and now ($_a): identical bytes" \
                sh -c '[ "$1" = 404 ] && [ "$2" = 404 ] && cmp -s "$3" "$4"' _ "$_pc" "$_a" "$W/r/i4pre_c1.b" "$W/r/i4post_c1.b"
        fi
    else
        note "slug $T2 differs from the guessed $T2_GUESS: the before/after-creation comparison was not run"
        ANON_T2_TS=$(date +%s)
    fi
    if cmp -s "$W/r/i4an_t1.b" "$W/r/i4c2_t1.b"; then
        note "anonymous and signed-in 404 for /t/$T1 are identical"
    else
        log "info: anonymous and signed-in 404s differ by $(diff "$W/r/i4an_t1.b" "$W/r/i4c2_t1.b" | grep -c '^[<>]') lines (the signed-in indicator, §4.3)"
    fi
    # Backend directly: no header, non-member session, garbage session.
    GARBAGE="mbs_$(random_b64url 43)"
    session_cfg s2 "$COOKIE2"
    session_cfg sg "$GARBAGE"
    : > "$W/c/none"
    _first=""; _allsame=1; _codes=""
    for _p in "/v1/teams/$T1" "/v1/teams/$T1/access" "/v1/teams/$NOSUCH" "/v1/teams/$NOSUCH/access"; do
        for _cr in none s2 sg; do
            _k="i4be_$(printf '%s' "$_p$_cr" | tr -c 'A-Za-z0-9' '_')"
            _code=$(req "$_k" -K "$W/c/$_cr" "$BACKEND$_p")
            _codes="$_codes $_code"
            [ "$_code" = 404 ] || _allsame=0
            if [ -z "$_first" ]; then _first=$_k; elif ! same_body "$_first" "$_k"; then _allsame=0; fi
        done
    done
    check "backend /v1/teams/{slug}[/access] x {no header, non-member, garbage} x {$T1, $NOSUCH}: 12 identical 404s (codes:$_codes)" eq "$_allsame" 1
    if [ -n "$COOKIE1" ]; then
        session_cfg s1 "$COOKIE1"
        _code=$(req i4be_member -K "$W/c/s1" "$BACKEND/v1/teams/$T1/access")
        check "sanity: the member's session gets /access 200 (got $_code, $(tr -d '\n' < "$W/r/i4be_member.b"))" eq "$_code" 200
    fi
else
    bad "no second session to compare with"
fi
end_item

# ═════════════════════════════════════════════════════════════════════════════
# Item 5: reuse refused.
begin_item 5 "reuse refused"
UNKNOWN="mbl_$(random_b64url 43)"
redeem "$UNKNOWN" i5unknown
_ustatus=$REDEEM_STATUS
check "an unknown link: 200 {\"error\":\"link\"} (status $_ustatus, body $(tr -d '\n' < "$W/r/i5unknown.b"))" \
    eq "$(jq -c . "$W/r/i5unknown.b" 2>/dev/null)" '{"error":"link"}'
if [ -n "$SECRET1" ]; then
    sleep 1.2
    redeem "$SECRET1" i5reuse
    check "reusing item 1's link: status $REDEEM_STATUS, the one failure body" same_body i5reuse i5unknown
    check "reuse sets no cookie" sh -c '! grep -qi "^set-cookie:" "$1"' _ "$W/r/i5reuse.h"
    check "sanity: status equals the unknown link's ($_ustatus)" eq "$REDEEM_STATUS" "$_ustatus"
else
    bad "no link from item 1"
fi
end_item

# ═════════════════════════════════════════════════════════════════════════════
# Item 6: expiry refused.
begin_item 6 "expiry refused"
if [ -n "$TOK1" ] && mint "$TOK1" "$T1"; then
    _h=$(sha256_of "$SECRET")
    _rows=$(sql "UPDATE board_login_link SET expires_at = NOW() - INTERVAL 1 SECOND WHERE secret_hash = '$_h'; SELECT ROW_COUNT();")
    check "expired the link in the DB (rows $_rows)" eq "$_rows" 1
    redeem "$SECRET" i6
    check "expired link: the one failure body (status $REDEEM_STATUS)" same_body i6 i5unknown
    check "expired link sets no cookie" sh -c '! grep -qi "^set-cookie:" "$1"' _ "$W/r/i6.h"
    check "the expired link stays unconsumed" eq "$(sql "SELECT consumed_at IS NULL FROM board_login_link WHERE secret_hash = '$_h'")" 1
else
    bad "could not mint (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
fi
end_item

# ═════════════════════════════════════════════════════════════════════════════
# Items run in the order their identities allow (9, 11, 12, 7, 8, 13, 10, 14);
# the table is printed by item number.

# bg_stream <prefix> <cfg> <url>: an SSE request in the background; sets BG_PID.
bg_stream() {
    curl -sS -N -K "$W/c/$2" -D "$W/r/$1.h" -o "$W/r/$1.b" "$3" 2>"$W/r/$1.e" &
    BG_PID=$!
    track_pid "$BG_PID"
}
# wait_exit <pid> <seconds>: 0 when the process ended in time (reaped), else kills it and returns 1.
wait_exit() {
    _i=0
    while kill -0 "$1" 2>/dev/null; do
        _i=$((_i + 1))
        if [ "$_i" -gt "$(( $2 * 10 ))" ]; then kill "$1" 2>/dev/null || true; wait "$1" 2>/dev/null || true; return 1; fi
        sleep 0.1
    done
    wait "$1" 2>/dev/null || true
    return 0
}
status_of() { tr -d '\r' < "$W/r/$1.h" 2>/dev/null | sed -n 's/^HTTP\/[0-9.]* \([0-9]*\).*/\1/p' | sed -n 1p; }
# signin_as <bearer> <slug> <prefix>: mint and redeem; sets SESSION.
signin_as() {
    SESSION=""
    mint "$1" "$2" || return 1
    redeem "$SECRET" "$3"
    [ -n "$SESSION" ]
}

# Item 9: sign out.
begin_item 9 "sign out"
if signin_as "$TOK1" "$T1" i9a && C9A=$SESSION && signin_as "$TOK1" "$T1" i9b && C9B=$SESSION; then
    cookie_cfg c9a "$C9A"; cookie_cfg c9b "$C9B"
    _s=$(req i9page -K "$W/c/c9a" "$BOARD/t/$T1")
    check "browser A signed in: /t/$T1 200 (got $_s)" eq "$_s" 200
    CSRF=$(sed -n 's/.*<meta name="csrf-token" content="\([^"]*\)".*/\1/p' "$W/r/i9page.b" | sed -n 1p)
    _field=$(sed -n 's/.*name="csrf" value="\([^"]*\)".*/\1/p' "$W/r/i9page.b" | sed -n 1p)
    check "CSRF token in the page meta ($(printf '%s' "$CSRF" | wc -c | tr -d ' ') chars)" test -n "$CSRF"
    check "the sign-out form's hidden csrf field matches the meta" eq "$_field" "$CSRF"
    printf 'header = "Cookie: metiche_session=%s"\nheader = "Origin: %s"\nheader = "Content-Type: application/x-www-form-urlencoded"\ndata = ""\n' \
        "$C9A" "$BOARD_BASE" > "$W/c/c9a_nocsrf"
    _s=$(req i9nocsrf -X POST -K "$W/c/c9a_nocsrf" "$BOARD/signout")
    check "POST /signout without CSRF: 403 (got $_s)" eq "$_s" 403
    check "the refused sign-out does not clear the cookie" sh -c '! grep -qi "^set-cookie:" "$1"' _ "$W/r/i9nocsrf.h"
    _s=$(req i9still -K "$W/c/c9a" "$BOARD/t/$T1")
    check "still signed in after the refused sign-out (got $_s)" eq "$_s" 200
    printf 'header = "Cookie: metiche_session=%s"\nheader = "Origin: %s"\nheader = "Content-Type: application/x-www-form-urlencoded"\ndata = "csrf=%s"\n' \
        "$C9A" "$BOARD_BASE" "$(printf '%s' "$CSRF" | sed 's/=/%3D/g')" > "$W/c/c9a_csrf"
    _s=$(req i9signout -X POST -K "$W/c/c9a_csrf" "$BOARD/signout")
    check "POST /signout with CSRF: 303 (got $_s) to $(hdr i9signout Location)" eq "$_s" 303
    check "the sign-out response clears the cookie ($(set_cookie_line i9signout | sed 's/=[^;]*;/=…;/'))" cleared_cookie i9signout
    _s=$(req i9replay -K "$W/c/c9a" "$BOARD/t/$T1")
    check "replaying the old cookie: 404 on the private board (got $_s)" eq "$_s" 404
    check "the session row says SIGNED_OUT (revoked, end_reason set)" \
        eq "$(sql "SELECT revoked_at IS NOT NULL AND end_reason IS NOT NULL FROM browser_session WHERE secret_hash = '$(sha256_of "$C9A")'")" 1
    : > "$W/c/c9a_csrf"; CSRF=""
    _s=$(req i9b_pre -K "$W/c/c9b" "$BOARD/t/$T1")
    check "browser B signed in (got $_s)" eq "$_s" 200
    if mcp_tool "$TOK1" sign_out_browsers '{"all":true}'; then
        check "sign_out_browsers all=true revoked sessions (revoked $(jq -r .revoked "$W/r/mcp.out"))" \
            test "$(jq -r '.revoked // 0' "$W/r/mcp.out")" -ge 1
    else
        bad "sign_out_browsers failed (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
    fi
    _s=$(req i9b_after -K "$W/c/c9b" "$BOARD/t/$T1")
    check "browser B after sign_out_browsers: 404 (got $_s)" eq "$_s" 404
    check "browser B's row is revoked" eq "$(sql "SELECT revoked_at IS NOT NULL FROM browser_session WHERE secret_hash = '$(sha256_of "$C9B")'")" 1
else
    bad "could not sign two browsers in (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
fi
end_item

# Item 11: DB outage.
begin_item 11 "DB outage (docker pause)"
DEMO=$(sed -n 's/.*msg="demo team registered" slug=\([^ ]*\).*/\1/p' "$W/board.log" | sed -n 1p)
if signin_as "$TOK1" "$T1" i11redeem; then
    C11=$SESSION; cookie_cfg c11 "$C11"
    _t_signin=$(date +%s)
    _s=$(req i11pre -K "$W/c/c11" "$BOARD/t/$T1")
    check "signed in before the outage (got $_s)" eq "$_s" 200
    if [ -n "$DEMO" ]; then
        _s=$(req i11demo_pre "$BOARD/t/$DEMO"); check "demo board /t/$DEMO 200 before (got $_s)" eq "$_s" 200
    fi
    docker pause "$DB_NAME_CONTAINER" >/dev/null
    log "paused $DB_NAME_CONTAINER"
    REQ_TIMEOUT=30
    _t0=$(date +%s)
    _s=$(req i11during -K "$W/c/c11" "$BOARD/t/$T1")
    _el=$(( $(date +%s) - _t0 ))
    check "private board during the outage (viewer cached): 503 (got $_s after ${_el}s)" eq "$_s" 503
    check "no cookie deletion in that response" sh -c '! grep -qi "^set-cookie:" "$1"' _ "$W/r/i11during.h"
    _wait=$(( _t_signin + 17 - $(date +%s) ))
    [ "$_wait" -gt 0 ] && sleep "$_wait"
    _t0=$(date +%s)
    _s=$(req i11during2 -K "$W/c/c11" "$BOARD/t/$T1")
    _el=$(( $(date +%s) - _t0 ))
    check "private board during the outage (session cache expired): 503 (got $_s after ${_el}s)" eq "$_s" 503
    check "no cookie deletion in that response either" sh -c '! grep -qi "^set-cookie:" "$1"' _ "$W/r/i11during2.h"
    if [ -n "$DEMO" ]; then
        _s=$(req i11demo "$BOARD/t/$DEMO")
        check "demo board /t/$DEMO still 200 during the outage (got $_s)" eq "$_s" 200
    else
        note "demo sub-step NOT RUN: no demo team in the board log"
    fi
    REQ_TIMEOUT=20
    docker unpause "$DB_NAME_CONTAINER" >/dev/null
    log "unpaused $DB_NAME_CONTAINER"
    _t0=$(date +%s); _s=000
    while [ "$(( $(date +%s) - _t0 ))" -lt 90 ]; do
        _s=$(req i11post -K "$W/c/c11" "$BOARD/t/$T1")
        [ "$_s" = 200 ] && break
        sleep 1
    done
    check "back with the same cookie after unpause: 200 (got $_s after $(( $(date +%s) - _t0 ))s)" eq "$_s" 200
else
    bad "could not sign in (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
fi
end_item

# Item 12: visibility flip.
begin_item 12 "visibility flip (F2)"
if [ -n "$TOK2" ] && signin_as "$TOK2" "$T2" i12redeem; then
    C12=$SESSION; cookie_cfg c12 "$C12"
    _s=$(req i12m0 -K "$W/c/c12" "$BOARD/t/$T2")
    check "member sees private $T2 (got $_s)" eq "$_s" 200
    _wait=$(( ${ANON_T2_TS:-0} + 31 - $(date +%s) ))
    if [ "$_wait" -gt 0 ]; then log "waiting ${_wait}s for the anonymous negative cache to expire"; sleep "$_wait"; fi
    sql "UPDATE team SET visibility = 2 WHERE slug = '$T2'"
    _s=$(req i12pub -K "$W/c/none" "$BOARD/t/$T2")
    check "public: anonymous /t/$T2 200 (got $_s)" eq "$_s" 200
    check "public: the team name is on the anonymous page" grep -q "$T2_NAME" "$W/r/i12pub.b"
    _s=$(req i12mpub -K "$W/c/c12" "$BOARD/t/$T2")
    check "public: the member still 200 (got $_s)" eq "$_s" 200
    sql "UPDATE team SET visibility = 1 WHERE slug = '$T2'"
    _t0=$(date +%s); _s=200
    while [ "$(( $(date +%s) - _t0 ))" -lt 100 ]; do
        _s=$(req i12anon "$BOARD/t/$T2")
        [ "$_s" = 404 ] && break
        sleep 1
    done
    _el=$(( $(date +%s) - _t0 ))
    check "private again: anonymous 404 (got $_s after ${_el}s; bound: backend re-auth 60s + reconnect)" eq "$_s" 404
    note "anonymous 404 ${_el}s after going private"
    if [ "$_s" = 404 ]; then
        normalize i12anon "$T2"
        check "that 404 matches the unknown slug's (slug normalised)" same_norm i12anon i4an_ns
    fi
    _s=$(req i12mpriv -K "$W/c/c12" "$BOARD/t/$T2")
    check "private again: the signed-in member still sees it (got $_s)" eq "$_s" 200
else
    bad "could not sign the second identity in (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
fi
end_item

# Item 7: retired agent (the second identity's cursor agent is X).
begin_item 7 "retired agent"
if [ -n "$TOK2" ] && mint "$TOK2" "$T2"; then
    SA=$SECRET
    if signin_as "$TOK2" "$T2" i7redeem; then
        C7=$SESSION; cookie_cfg c7 "$C7"
        _s=$(req i7pre -K "$W/c/c7" "$BOARD/t/$T2")
        check "signed in with a session minted by X (got $_s)" eq "$_s" 200
        _rows=$(sql "UPDATE agent SET status = 2 WHERE token_hash = '$(sha256_of "$TOK2")'; SELECT ROW_COUNT();")
        check "X retired in the DB (rows $_rows)" eq "$_rows" 1
        redeem "$SA" i7link
        check "a link minted by X before retirement: refused, the one failure body (status $REDEEM_STATUS)" same_body i7link i5unknown
        check "  and no cookie" sh -c '! grep -qi "^set-cookie:" "$1"' _ "$W/r/i7link.h"
        _s=$(req i7after -K "$W/c/c7" "$BOARD/t/$T2")
        check "the next page is 404 (got $_s)" eq "$_s" 404
        if cleared_cookie i7after; then
            ok "that response clears the cookie"
        else
            bad "that response does not clear the cookie (Set-Cookie: $(set_cookie_line i7after | sed 's/=[^;]*;/=…;/'))"
            sleep 16
            _s=$(req i7after2 -K "$W/c/c7" "$BOARD/t/$T2")
            if cleared_cookie i7after2; then
                note "the page 16s later (after the 15s session cache) got $_s and did clear the cookie"
            else
                note "the page 16s later got $_s and still did not clear the cookie"
            fi
        fi
    else
        bad "could not redeem (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
    fi
    SA=""
    if mcp_tool "$TOK2" open_board "{\"team_slug\":\"$T2\"}"; then
        bad "open_board with X's token succeeded"
    else
        check "open_board with X's token: HTTP 401 (got $MCP_HTTP)" eq "$MCP_HTTP" 401
    fi
else
    bad "could not mint with X (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
fi
end_item

# Item 8: removed member.
begin_item 8 "removed member"
if signin_as "$TOK1" "$T1" i8redeem; then
    C8=$SESSION; cookie_cfg c8 "$C8"; session_cfg s8 "$C8"
    _s=$(req i8pre -K "$W/c/c8" "$BOARD/t/$T1")
    check "member signed in (got $_s)" eq "$_s" 200
    bg_stream i8board c8 "$BOARD/t/$T1/stream"; P_BOARD=$BG_PID
    bg_stream i8backend s8 "$BACKEND/v1/teams/$T1/stream"; P_BACKEND=$BG_PID
    sleep 2
    check "board stream open (status $(status_of i8board))" sh -c 'kill -0 "$1" && [ "$2" = 200 ]' _ "$P_BOARD" "$(status_of i8board)"
    check "backend stream open with the session header (status $(status_of i8backend))" sh -c 'kill -0 "$1" && [ "$2" = 200 ]' _ "$P_BACKEND" "$(status_of i8backend)"
    _rows=$(sql "UPDATE member SET revoked_at = NOW(), updated_at = NOW() WHERE team_uuid = (SELECT id FROM team WHERE slug = '$T1'); SELECT ROW_COUNT();")
    _t0=$(date +%s)
    check "member.revoked_at set (rows $_rows; status left ACTIVE, so revoked_at alone is tested)" eq "$_rows" 1
    _s=$(req i8after -K "$W/c/c8" "$BOARD/t/$T1")
    check "the next page is 404 (got $_s)" eq "$_s" 404
    if wait_exit "$P_BOARD" 10; then
        _el=$(( $(date +%s) - _t0 ))
        check "the open board stream ended within the 2s re-auth window (+2s slack): ${_el}s" test "$_el" -le 4
    else
        bad "the open board stream was still open 10s after the revocation"
    fi
    log "waiting up to 75s for the backend stream (re-auth tick 60s)"
    if wait_exit "$P_BACKEND" 75; then
        _el=$(( $(date +%s) - _t0 ))
        check "the open backend stream ended within 60s (+5s slack): ${_el}s" test "$_el" -le 65
        note "backend stream ended after ${_el}s"
    else
        bad "the open backend stream was still open 75s after the revocation"
    fi
else
    bad "could not sign in (HTTP $MCP_HTTP: $(cat "$W/r/mcp.err"))"
fi
end_item

# Item 13: §5.2, the installer re-run in a shell with a stale METICHE_TOKEN.
begin_item 13 "§5.2 installer re-run with a stale METICHE_TOKEN"
H3="$W/h3"; make_home "$H3"
_rc=0
run_installer "$H3" "$W/install3a.out" "" "METICHE_TEAM_NAME=Smoke Charlie" "METICHE_MEMBER_NAME=Smoke Three" -- \
    --only cursor --no-open || _rc=$?
check "identity A installed (exit $_rc)" eq "$_rc" 0
[ "$_rc" -eq 0 ] || show_redacted "$W/install3a.out" 30
TOKA=$(sed -n 's/^METICHE_TOKEN=//p' "$H3/.metiche/env" 2>/dev/null | sed -n 1p)
T3=$(sql "SELECT slug FROM team WHERE name = 'Smoke Charlie' LIMIT 1")
if [ -n "$TOKA" ] && [ -n "$T3" ]; then
    remember_secret "$TOKA"
    printf "METICHE_TOKEN='%s'\n" "$TOKA" > "$W/c/tokA.env"
    _acc=$(sql "SELECT account_uuid FROM member m JOIN team t ON t.id = m.team_uuid WHERE t.slug = '$T3' LIMIT 1")
    _k=$(sql "UPDATE account SET token_hash = SHA2(UUID(), 256) WHERE id = '$_acc'; SELECT ROW_COUNT(); UPDATE agent SET token_hash = SHA2(UUID(), 256) WHERE account_uuid = '$_acc'; SELECT ROW_COUNT();" | tr '\n' ' ')
    ok "A's tokens killed in the DB (rows: $_k)"
    _rc=0
    run_installer "$H3" "$W/install3b.out" "$W/c/tokA.env" "METICHE_TEAM_NAME=Smoke Delta" "METICHE_MEMBER_NAME=Smoke Three" -- \
        --only cursor --no-open || _rc=$?
    check "re-run with A's dead token exported: a new identity (exit $_rc)" eq "$_rc" 0
    [ "$_rc" -eq 0 ] || show_redacted "$W/install3b.out" 30
    TOKB=$(sed -n 's/^METICHE_TOKEN=//p' "$H3/.metiche/env" 2>/dev/null | sed -n 1p)
    [ -n "$TOKB" ] && remember_secret "$TOKB"
    check "~/.metiche/env now holds a different token" sh -c '[ -n "$1" ] && [ "$1" != "$2" ]' _ "$TOKB" "$TOKA"
    _match=0
    for _bk in "$H3"/.metiche/env.metiche-backup-*; do
        [ -f "$_bk" ] || continue
        [ "$(sed -n 's/^METICHE_TOKEN=//p' "$_bk" | sed -n 1p)" = "$TOKA" ] && _match=1
    done
    check "a backup of ~/.metiche/env holds A's token" eq "$_match" 1
    check "that run's end-of-run note names the stale terminal" grep -q "this terminal still has the previous METICHE_TOKEN" "$W/install3b.out"
    _rc=0
    run_installer "$H3" "$W/install3c.out" "$W/c/tokA.env" "METICHE_MEMBER_NAME=Smoke Three" -- \
        --only cursor --no-open || _rc=$?
    check "same shell again (METICHE_TOKEN = the backup's): exit 0, no refusal (exit $_rc)" eq "$_rc" 0
    [ "$_rc" -eq 0 ] || show_redacted "$W/install3c.out" 30
    check "the stale-shell warning is printed" grep -q "your shell still has the METICHE_TOKEN from before this machine's last install" "$W/install3c.out"
    check "no 'refusing' in the output" sh -c '! grep -qi refusing "$1"' _ "$W/install3c.out"
    check "the saved token is unchanged" eq "$(sed -n 's/^METICHE_TOKEN=//p' "$H3/.metiche/env" | sed -n 1p)" "$TOKB"
    _m=$(cat "$W/install3a.out" "$W/install3b.out" "$W/install3c.out" | grep -c 'mtk_' || true)
    check "no mtk_ in any of the three outputs ($_m lines)" eq "$_m" 0
    : > "$W/c/tokA.env"; TOKA=""; TOKB=""
else
    bad "no token or team from the first run"
fi
end_item

# Item 10: no secret in logs.
begin_item 10 "no secret in logs"
for _v in "$TOK1" "$TOK2"; do [ -n "$_v" ] && remember_secret "$_v"; done
sort -u "$W/secrets.list" | sed '/^$/d' > "$W/secrets.u"
_nl=$(grep -c '^mbl_' "$W/secrets.u" || true)
_ns=$(grep -c '^mbs_' "$W/secrets.u" || true)
_nt=$(grep -c '^mtk_' "$W/secrets.u" || true)
log "checking $_nl mbl_, $_ns mbs_ and $_nt mtk_ values"
check "at least the links and sessions of this run are in the list" test "$_nl" -ge 5 -a "$_ns" -ge 5
for _f in backend.log board.log; do
    _hits=$(grep -c -F -f "$W/secrets.u" "$W/$_f" || true)
    check "$_f: zero hits for every value used ($_hits)" eq "$_hits" 0
    _pref=$(grep -c -E '(mbl|mbs|mtk)_[A-Za-z0-9_-]{8,}' "$W/$_f" || true)
    check "$_f: no credential-shaped string at all ($_pref)" eq "$_pref" 0
done
note "ingress-controller log NOT RUN (no cluster)"
end_item

# Item 14: suites, LAST (the backend suite truncates tables; it runs against metiche_suite).
if [ "${SMOKE_SKIP_SUITES:-0}" = "1" ]; then
    not_run 14 "SMOKE_SKIP_SUITES=1"
else
    begin_item 14 "suites"
    _t0=$(date +%s)
    if ( METICHE_TEST_MYSQL_DSN="root:${DB_PASS}@tcp(127.0.0.1:${DB_PORT})/metiche_suite?parseTime=true&interpolateParams=true"
         export METICHE_TEST_MYSQL_DSN
         cd "$BACKEND_DIR" && go test -count=1 -p 1 ./app/... ) > "$W/suite-backend.log" 2>&1; then
        ok "backend go test -p 1 ./app/... ($(grep -c '^ok' "$W/suite-backend.log") packages ok, $(( $(date +%s) - _t0 ))s)"
    else
        bad "backend go test -p 1 ./app/..."
        grep -E '^(--- FAIL|FAIL|panic)' "$W/suite-backend.log" | head -n 20 > "$W/suite-backend.fail" || true
        show_redacted "$W/suite-backend.fail" 20
    fi
    _sk=$(grep -c -- '--- SKIP' "$W/suite-backend.log" || true)
    [ "$_sk" -eq 0 ] || note "backend: $_sk skipped tests"
    if ( cd "$BACKEND_DIR" && go vet ./... ) > "$W/vet.log" 2>&1; then ok "backend go vet ./..."; else bad "backend go vet"; show_redacted "$W/vet.log" 15; fi
    _fmt=$(cd "$BACKEND_DIR" && gofmt -l . 2>&1)
    check "backend gofmt -l empty (${_fmt:-empty})" test -z "$_fmt"
    if ( cd "$FRONTEND_DIR" && go test -count=1 ./... ) > "$W/suite-frontend.log" 2>&1; then
        ok "frontend go test ./... ($(grep -c '^ok' "$W/suite-frontend.log") packages ok)"
    else
        bad "frontend go test ./..."
        grep -E '^(--- FAIL|FAIL|panic)' "$W/suite-frontend.log" | head -n 20 > "$W/suite-frontend.fail" || true
        show_redacted "$W/suite-frontend.fail" 20
    fi
    # templ generate in a copy of the whole module, then compare: the check must
    # not rewrite the repo. It runs at the module root, as a developer would:
    # generated code embeds each .templ path relative to where templ ran, so
    # generating inside a copied internal/view alone differs in every file.
    need_cmd templ "item 14 checks templ generate."
    cp -R "$FRONTEND_DIR" "$W/fe"
    if (cd "$W/fe" && templ generate) > "$W/templ.log" 2>&1; then
        _d=$(diff -r -q "$FRONTEND_DIR" "$W/fe" 2>&1 | sed "s|$W/fe|<copy>|g" | head -n 5 || true)
        check "templ generate clean ($(templ version), no file differs from the committed output${_d:+: $_d})" test -z "$_d"
    else
        bad "templ generate failed"; show_redacted "$W/templ.log" 10
    fi
    end_item
fi

# ═════════════════════════════════════════════════════════════════════════════
step "results"
FINISHED=1
_fail=0
printf '\n%-5s %-8s %s\n' item result notes
sort -t'|' -k1,1n "$W/results" | while IFS='|' read -r _n _st _nt; do
    printf '%-5s %-8s %s\n' "$_n" "$_st" "$_nt"
done
grep -q '|FAIL|' "$W/results" && _fail=1
printf '\nelapsed: %ss\n' "$(( $(date +%s) - START_EPOCH ))"
[ "$_fail" -eq 0 ]
