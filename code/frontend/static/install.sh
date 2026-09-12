#!/bin/sh
# metiche installer — https://metiche.xyz
#
#   curl -fsSL https://metiche.xyz/install.sh | sh
#
# Points your coding assistants at a metiche team: joins the team once, then
# installs the metiche-teamwork skill where it can and registers the metiche
# MCP server where it can.
#
# A JOIN CODE IS NOT A BEARER TOKEN. The join code is an invite — an argument
# you pass to the join_team tool. The bearer is minted by the server when you
# redeem it. Configuring a client with the join code as its bearer produces a
# 401 on its very first request, before it can ever reach join_team. So this
# script does the join itself, once, and configures every client with the
# token that comes back.
#
# What it touches, and nothing else:
#
#   ~/.metiche/env                              your metiche TOKEN, mode 0600
#   ~/.metiche/src                              a shallow clone, for the plugin
#   ~/.cursor/mcp.json                          Cursor's global MCP config
#   ~/.codeium/windsurf/mcp_config.json         Windsurf's MCP config
#   ~/.codex/config.toml                        via the `codex mcp add` CLI
#   Claude Code's user-scope plugin config      via the `claude` CLI
#   your shell profile                          only with --write-profile
#
# What it sends over the network: exactly one join_team or create_team call,
# then one list_teams call to prove the token it got back actually works. It
# writes nothing at all until that proof succeeds — a half-configured machine
# is worse than an unconfigured one, because you believe it is done.
#
# It never writes anything inside the current directory, never writes a token
# or a join code into a file in a repository, never passes either through a
# command line (arguments are visible in `ps`), and never runs sudo. Run it
# with --dry-run to see every change it would make, and every call it would
# make, without making any of them.
#
# SPDX-License-Identifier: Apache-2.0

set -eu

# ---------------------------------------------------------------- defaults ---

METICHE_URL_DEFAULT="https://mcp.metiche.xyz/v1/mcp"
METICHE_REPO_DEFAULT="https://github.com/mklfarha/metiche.git"
PLUGIN_NAME="metiche"
MARKETPLACE_NAME="metiche"
SERVER_NAME="metiche"
# The environment variable Codex is told to read the bearer from. Must match
# what write_env_file exports, or Codex authenticates as nobody. It holds the
# server-minted TOKEN — never the join code, which is not a credential this
# endpoint accepts.
TOKEN_ENV="METICHE_TOKEN"
# MCP protocol revision this script speaks in its handshake.
MCP_PROTOCOL="2025-06-18"
# Seconds for any single HTTP call. Two round trips to join, two to verify.
HTTP_TIMEOUT=30

URL="${METICHE_URL:-$METICHE_URL_DEFAULT}"
REPO="${METICHE_REPO:-$METICHE_REPO_DEFAULT}"
MARKETPLACE="${METICHE_MARKETPLACE:-}"
DRY_RUN=0
WRITE_PROFILE=1
ONLY=""

ENV_DIR="$HOME/.metiche"
ENV_FILE="$ENV_DIR/env"
SRC_DIR="$ENV_DIR/src"

# ACTION is one of join, create, token: redeem an invite, make a new team, or
# use a token the caller already has. Chosen before anything touches the
# network, and never from a command-line argument.
ACTION=""
JOIN_CODE=""
CODE_SOURCE=""
TOKEN=""
TOKEN_SOURCE=""
TEAM_SLUG=""
TEAM_NAME="${METICHE_TEAM_NAME:-}"
MEMBER_NAME=""
AGENT_LABEL=""
CLIENT_KEY=""
CLIENT_KIND="installer"
NEW_JOIN_CODE=""
CREATED="false"
DID_SOMETHING=0

# Scratch for the HTTP calls. Created on demand, mode 0700, removed on exit.
MCP_TMP=""
MCP_SESSION=""
MCP_HTTP=""
MCP_ERROR=""

# ------------------------------------------------------------------ output ---

say()  { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# Says what is about to happen. Returns 1 under --dry-run so callers skip the
# write; that keeps "announce then act" in one place instead of in every branch.
plan() {
    if [ "$DRY_RUN" -eq 1 ]; then
        printf '    would %s\n' "$*"
        return 1
    fi
    printf '    %s\n' "$*"
    return 0
}

usage() {
    cat <<'EOF'
metiche installer

Usage:
  curl -fsSL https://metiche.xyz/install.sh | sh
  sh install.sh [options]

It joins your team once, over the wire, and configures every assistant on
this machine with the TOKEN the server mints. A join code is an invite, not
a credential: a client configured with a join code as its bearer gets a 401
on its first request and can never reach join_team to fix itself.

Options:
  --dry-run           Print every change and every call that would be made;
                      change nothing and call nothing.
  --url <url>         MCP endpoint (default: https://mcp.metiche.xyz/v1/mcp).
  --marketplace <src> Claude Code plugin marketplace source. By default the
                      script uses ./plugin if you are standing in a clone of
                      the metiche repository, and otherwise makes a shallow
                      clone at ~/.metiche/src and uses ~/.metiche/src/plugin.
  --only <list>       Comma-separated subset of: claude,cursor,windsurf,codex.
                      Default: every assistant detected on this machine.
  --no-write-profile  Do NOT append the line that loads ~/.metiche/env to your
                      shell profile. It is appended by default: the token is
                      read from the environment, so without that line every
                      client sends an empty bearer and gets a 401.
  --write-profile     Explicitly ask for the default.
  -h, --help          This.

Joining, or creating. With no join code the script asks which you want; it
never guesses. Non-interactively, pick with one environment variable:

  METICHE_JOIN_CODE=your-code sh install.sh    # join a team that exists
  METICHE_TEAM_NAME="Payments squad" sh install.sh
                                               # create a team, print its
                                               # join code for your teammates
  METICHE_TOKEN=your-token sh install.sh       # already joined elsewhere:
                                               # skip the join, just configure

Secrets are read from the environment or from a prompt and are deliberately
NOT accepted as command-line arguments: arguments are visible in `ps` to every
user on the machine and land in your shell history. For the same reason the
token and the join code are handed to curl through a config document on its
standard input, never as curl arguments.

Optional, all defaulted: METICHE_MEMBER_NAME (else `git config user.name`,
else $USER), METICHE_AGENT_LABEL and METICHE_CLIENT_KEY (else this machine's
hostname). client_key is what makes a re-join idempotent, so it must be the
same on every run — change it only if you really do want a second agent.
A token already in ~/.metiche/env is sent with the join for the same reason:
the account is minted for a join that carries no bearer, so without it a
re-run would add a second account, member and agent however stable
client_key is.

jq is required for the join step, and only for it. See the comment above
require_join_tools for why there is no regex fallback.

Cursor is configured globally (~/.cursor/mcp.json) and never per-project: the
only project-scoped location is .cursor/mcp.json inside your repository, and a
credential does not belong in a repository.

Codex is configured through its own `codex mcp add` CLI, so metiche never
parses or rewrites ~/.codex/config.toml. Codex is also the only client where
the token is NOT written to disk: it stores the NAME of an environment
variable (METICHE_TOKEN) and reads the value at connect time. That means
~/.metiche/env has to be loaded in the shell you launch codex from — see
--write-profile.

Zed is not configured. Its MCP config format was not verified when this script
was written, and guessing at a config file is worse than printing the endpoint
and letting you paste it. See the note it prints.
EOF
}

# ------------------------------------------------------------------- flags ---

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run)       DRY_RUN=1 ;;
        --write-profile)    WRITE_PROFILE=1 ;;
        --no-write-profile) WRITE_PROFILE=0 ;;
        --url)           [ $# -ge 2 ] || die "--url needs a value"; URL="$2"; shift ;;
        --url=*)         URL="${1#--url=}" ;;
        --marketplace)   [ $# -ge 2 ] || die "--marketplace needs a value"; MARKETPLACE="$2"; shift ;;
        --marketplace=*) MARKETPLACE="${1#--marketplace=}" ;;
        --only)          [ $# -ge 2 ] || die "--only needs a value"; ONLY="$2"; shift ;;
        --only=*)        ONLY="${1#--only=}" ;;
        -h|--help)       usage; exit 0 ;;
        *)
            # A bare argument is most likely someone passing their join code
            # or their token. Refuse loudly rather than accept a credential
            # through argv.
            say "error: unexpected argument: $1" >&2
            say "" >&2
            say "If that was your join code or your token: neither is ever" >&2
            say "accepted as an argument, because arguments are visible in the" >&2
            say "process list and in shell history. Use:" >&2
            say "" >&2
            say "  METICHE_JOIN_CODE=... sh install.sh" >&2
            say "  METICHE_TOKEN=... sh install.sh" >&2
            say "" >&2
            say "or run the script with no arguments and it will prompt you." >&2
            exit 2
            ;;
    esac
    shift
done

case "$URL" in
    https://*) ;;
    http://localhost*|http://127.0.0.1*) warn "using a plaintext local endpoint: $URL" ;;
    *) die "--url must be https (or a local http endpoint), got: $URL" ;;
esac

want() {
    [ -z "$ONLY" ] && return 0
    case ",$ONLY," in
        *",$1,"*) return 0 ;;
        *) return 1 ;;
    esac
}

# -------------------------------------------------------------- identity ---
#
# Three ways to end up with a token, and the script never guesses between
# them:
#
#   join    you were given a join code. Redeem it; the server mints a token.
#   create  nobody has made the team yet. Make it; the same call mints a
#           token AND the team's first join code, which you hand to teammates.
#   token   you already joined on another machine. The token is the PERSON,
#           not the machine and not the team, so re-using it is correct and
#           joining again would only add a duplicate agent to the board.

# [ -r /dev/tty ] is not the question. On macOS the node exists and is
# readable for a process with no controlling terminal, and the open then fails
# with "Device not configured" — a raw shell error instead of this script's own
# advice. Actually opening it is the only honest test.
have_tty() {
    { : < /dev/tty; } 2>/dev/null
}

read_join_code() {
    if [ -n "${METICHE_JOIN_CODE:-}" ]; then
        JOIN_CODE="$METICHE_JOIN_CODE"
        CODE_SOURCE="METICHE_JOIN_CODE"
        return 0
    fi

    if ! have_tty; then
        die "no join code. Set METICHE_JOIN_CODE, or run this in a terminal so it can prompt."
    fi

    printf 'metiche team join code (not echoed): ' >/dev/tty
    if stty_saved=$(stty -g </dev/tty 2>/dev/null); then
        stty -echo </dev/tty
        IFS= read -r JOIN_CODE </dev/tty || true
        stty "$stty_saved" </dev/tty
        printf '\n' >/dev/tty
    else
        # No terminal control available; read it, visibly, rather than fail.
        warn "cannot disable echo; your join code will be visible as you type"
        IFS= read -r JOIN_CODE </dev/tty || true
    fi
    CODE_SOURCE="prompt"
}

validate_join_code() {
    [ -n "$JOIN_CODE" ] || die "empty join code"
    case "$JOIN_CODE" in
        *[!A-Za-z0-9._-]*) die "join code contains unexpected characters (expected letters, digits, . _ -)" ;;
    esac
    if [ "${#JOIN_CODE}" -lt 6 ]; then
        die "join code looks too short — check you pasted the whole thing"
    fi
}

validate_token() {
    [ -n "$TOKEN" ] || die "empty token"
    case "$TOKEN" in
        *[!A-Za-z0-9._-]*) die "token contains unexpected characters (expected letters, digits, . _ -)" ;;
    esac
    if [ "${#TOKEN}" -lt 16 ]; then
        die "that does not look like a metiche token — it is longer than a join code"
    fi
}

# Names travel inside a JSON body, so rather than write a JSON escaper for
# three fields, the allowed set is narrowed to what cannot need escaping.
# A refusal a person can read beats a quoting bug they cannot see.
validate_name() {
    case "$2" in
        "") die "$1 must not be empty" ;;
        *[!A-Za-z0-9\ ._@-]*)
            die "$1 contains unexpected characters: \"$2\"
Use letters, digits, spaces and . _ - @ only." ;;
    esac
    if [ "${#2}" -gt 80 ]; then
        die "$1 is too long (80 characters maximum)"
    fi
}

read_team_name() {
    have_tty || die "no team name. Set METICHE_TEAM_NAME, or run this in a terminal."
    printf 'name for the new team (e.g. Payments squad): ' >/dev/tty
    IFS= read -r TEAM_NAME </dev/tty || true
}

# The name of the PERSON, which is how metiche tells "two of my own agents
# collided" apart from "I collided with a colleague". Not a secret, so it can
# be echoed.
resolve_member_name() {
    MEMBER_NAME="${METICHE_MEMBER_NAME:-}"
    if [ -z "$MEMBER_NAME" ] && command -v git >/dev/null 2>&1; then
        MEMBER_NAME=$(git config --get user.name 2>/dev/null || true)
    fi
    [ -n "$MEMBER_NAME" ] || MEMBER_NAME="${USER:-}"
    [ -n "$MEMBER_NAME" ] || MEMBER_NAME="${LOGNAME:-}"
    if [ -z "$MEMBER_NAME" ]; then
        have_tty || die "cannot work out your name. Set METICHE_MEMBER_NAME."
        printf 'your name, as your teammates would write it: ' >/dev/tty
        IFS= read -r MEMBER_NAME </dev/tty || true
    fi
    validate_name "member name" "$MEMBER_NAME"
}

# agent_label and client_key are per-MACHINE, not per-run.
#
# client_key is what makes a re-join idempotent: the server keys the agent row
# on (account, client_key), so a key that changed between runs would put a new
# agent on the board every single time anyone re-ran this script, and the ghost
# of the previous run would sit there next to the live one. The hostname is
# stable across re-runs, needs no state on disk, and already means something to
# a human reading the board.
machine_id() {
    _host=""
    if command -v hostname >/dev/null 2>&1; then
        _host=$(hostname 2>/dev/null || true)
    fi
    [ -n "$_host" ] || _host="${HOSTNAME:-}"
    [ -n "$_host" ] || _host="unknown-host"
    _host=${_host%%.*}
    printf '%s' "$_host" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9-' '-'
}

resolve_agent_identity() {
    AGENT_LABEL="${METICHE_AGENT_LABEL:-$(machine_id)}"
    CLIENT_KEY="${METICHE_CLIENT_KEY:-metiche-install-$(machine_id)}"
    validate_name "agent label" "$AGENT_LABEL"
    validate_name "client key" "$CLIENT_KEY"
}

# create_team wants an idempotency_key of at least 8 characters and derives the
# team's primary key from it, so the same key means "the same team" rather than
# "a second team with the same name". Deriving it from the team name and the
# client key makes re-running this script a no-op instead of a duplicate.
idempotency_key() {
    _seed="metiche-install|$1|$CLIENT_KEY"
    if command -v shasum >/dev/null 2>&1; then
        printf '%s' "$_seed" | shasum -a 256 | cut -c1-32
    elif command -v sha256sum >/dev/null 2>&1; then
        printf '%s' "$_seed" | sha256sum | cut -c1-32
    else
        # No hasher on this machine. The seed itself is still stable across
        # re-runs, which is the only property the key has to have.
        printf '%s' "$_seed" | tr -c 'A-Za-z0-9' '-' | cut -c1-64
    fi
}

choose_identity() {
    if [ -n "${METICHE_TOKEN:-}" ]; then
        ACTION="token"
        TOKEN="$METICHE_TOKEN"
        TOKEN_SOURCE="METICHE_TOKEN"
        validate_token
        return 0
    fi
    if [ -n "${METICHE_JOIN_CODE:-}" ]; then
        ACTION="join"
        read_join_code
        validate_join_code
        return 0
    fi
    if [ -n "$TEAM_NAME" ]; then
        ACTION="create"
        validate_name "team name" "$TEAM_NAME"
        return 0
    fi

    if ! have_tty; then
        die "nothing to identify you with, and no terminal to ask. Set one of:
  METICHE_JOIN_CODE=<code>       join a team somebody already created
  METICHE_TEAM_NAME=<name>       create a new team
  METICHE_TOKEN=<token>          you already have a metiche token"
    fi

    say ""
    info "A metiche team is one you join, or one you create."
    info "  [j] join — you have a join code from a teammate"
    info "  [c] create — nobody has set the team up yet"
    printf '    j or c: ' >/dev/tty
    IFS= read -r _answer </dev/tty || _answer=""
    case "$_answer" in
        j|J|join|Join)
            ACTION="join"
            read_join_code
            validate_join_code
            ;;
        c|C|create|Create)
            ACTION="create"
            read_team_name
            validate_name "team name" "$TEAM_NAME"
            ;;
        *)
            die "expected j or c, got: $_answer"
            ;;
    esac
}

# ------------------------------------------------------------ mcp over http ---
#
# Enough of a streamable-HTTP MCP client to make two calls. Three POSTs to the
# same URL: initialize (whose RESPONSE HEADER carries the session id), the
# initialized notification, then tools/call. Replies come back either as one
# JSON object or as an SSE frame whose payload sits on a "data: " line, and
# both are handled because which one you get depends on the server's content
# negotiation, not on anything this script controls.

# jq is REQUIRED for the join, and there is deliberately no fallback.
#
# The answer is a JSON document nested inside a JSON string inside a JSON-RPC
# envelope, and it holds a bearer token next to a join code and an account
# key. A regex that picks the wrong one of those three does not fail: it
# writes the wrong secret into every client config on this machine, and you
# find out at the first 401 — or you do not find out at all, because you just
# published your team's invite as a bearer. That confusion IS the bug this
# script exists to fix, so guessing here would be reintroducing it with extra
# steps. A refusal with two ways out is better than a parser that is quietly
# wrong.
#
# Nothing else in the script needs jq. A machine without it can still be
# configured: join on a machine that has jq, then run this one with
# METICHE_TOKEN set, and the join step is skipped entirely.
require_join_tools() {
    command -v curl >/dev/null 2>&1 ||
        die "curl is required to reach $URL. Install curl, or set METICHE_TOKEN
to a token you minted elsewhere and this script will skip the join."
    command -v jq >/dev/null 2>&1 ||
        die "jq is required for the join step, and only for it.
Install jq, or join on a machine that has it and re-run here with
METICHE_TOKEN=<that token>, which skips the join and just configures clients."
}

mcp_tmpdir() {
    [ -n "$MCP_TMP" ] && return 0
    MCP_TMP=$(mktemp -d "${TMPDIR:-/tmp}/metiche-install.XXXXXX") ||
        die "cannot create a temporary directory"
    chmod 700 "$MCP_TMP" 2>/dev/null || true
    trap 'rm -rf "$MCP_TMP"' EXIT
    trap 'rm -rf "$MCP_TMP"; exit 130' INT
    trap 'rm -rf "$MCP_TMP"; exit 143' TERM
    return 0
}

# Why the failure reason goes in a FILE and not just a variable: mcp_call has
# to run inside a command substitution to capture what the tool returned, and
# a subshell cannot write its parent's variables. Parking it on disk (inside
# the 0700 scratch directory, and never the credential itself) is what lets
# the caller print a real reason instead of an empty string.
mcp_fail() {
    MCP_ERROR="$1"
    if [ -n "$MCP_TMP" ]; then
        printf '%s' "$1" > "$MCP_TMP/error"
    fi
    return 1
}

mcp_error() {
    if [ -n "$MCP_TMP" ] && [ -s "$MCP_TMP/error" ]; then
        cat "$MCP_TMP/error"
    else
        printf '%s' "${MCP_ERROR:-no reason given}"
    fi
}

# curl reads a quoted value with \ and " as escapes; everything else is
# literal. Only these two need rewriting.
curl_quote() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'
}

# One POST. $1 is the bearer (may be empty), $2 the JSON-RPC body.
#
# Both go to curl through a --config document on STDIN, never through argv:
# the whole point of refusing a credential as a command-line argument is that
# `ps` shows it to every user on the machine, and handing the same string to
# curl on ITS command line would give that away again one line later.
mcp_post() {
    _bearer="$1"
    _body="$2"
    mcp_tmpdir
    : > "$MCP_TMP/error"
    _config=$(
        printf 'header = "Content-Type: application/json"\n'
        printf 'header = "Accept: application/json, text/event-stream"\n'
        if [ -n "$MCP_SESSION" ]; then
            printf 'header = "Mcp-Session-Id: %s"\n' "$MCP_SESSION"
        fi
        if [ -n "$_bearer" ]; then
            printf 'header = "Authorization: Bearer %s"\n' "$(curl_quote "$_bearer")"
        fi
        printf 'data = "%s"\n' "$(curl_quote "$_body")"
    )
    MCP_HTTP=$(
        printf '%s\n' "$_config" |
        (umask 077; curl -sS -X POST -K - --max-time "$HTTP_TIMEOUT" \
            -D "$MCP_TMP/head" -o "$MCP_TMP/body" -w '%{http_code}' \
            "$URL" 2>"$MCP_TMP/err")
    ) || {
        _why=$(tr -d '\r' < "$MCP_TMP/err" 2>/dev/null | tr '\n' ' ')
        [ -n "$_why" ] || _why="curl could not reach $URL"
        mcp_fail "$_why"
        return 1
    }
    case "$MCP_HTTP" in
        2*) return 0 ;;
        401|403)
            mcp_fail "the endpoint refused the credential (HTTP $MCP_HTTP)" ;;
        *)
            mcp_fail "the endpoint answered HTTP $MCP_HTTP" ;;
    esac
}

# The session id arrives as a RESPONSE header on initialize and has to ride on
# every later POST of the same connection.
read_session_id() {
    MCP_SESSION=$(tr -d '\r' < "$MCP_TMP/head" |
        sed -n 's/^[Mm][Cc][Pp]-[Ss][Ee][Ss][Ss][Ii][Oo][Nn]-[Ii][Dd]: *//p' |
        sed -n '1p')
}

# The JSON-RPC document from the last reply, whether it came back bare or
# wrapped in an SSE frame. One request gets one response here, so the first
# data: line is the whole of it.
mcp_payload() {
    if grep -q '^data:' "$MCP_TMP/body" 2>/dev/null; then
        sed -n 's/^data: \{0,1\}//p' "$MCP_TMP/body" | sed -n '1p'
    else
        cat "$MCP_TMP/body"
    fi
}

# A fresh connection, with the bearer set from the very first byte when there
# is one. See verify_token for why "from the first byte" is the whole point.
mcp_connect() {
    MCP_SESSION=""
    mcp_post "$1" "$(printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"%s","capabilities":{},"clientInfo":{"name":"metiche-install","version":"1"}}}' "$MCP_PROTOCOL")" ||
        return 1
    read_session_id
    mcp_post "$1" '{"jsonrpc":"2.0","method":"notifications/initialized"}' || return 1
    return 0
}

# Calls one tool and prints the JSON document it returned. Anything that went
# wrong — transport, protocol, or the tool itself saying no — lands in
# MCP_ERROR and returns 1, so callers have one thing to check.
mcp_call() {
    _bearer="$1"
    _tool="$2"
    _args="$3"
    mcp_post "$_bearer" "$(printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"%s","arguments":%s}}' "$_tool" "$_args")" ||
        return 1

    _payload=$(mcp_payload)
    if [ -z "$_payload" ]; then
        mcp_fail "empty response from $URL"
        return 1
    fi
    if ! printf '%s' "$_payload" | jq -e . >/dev/null 2>&1; then
        mcp_fail "the endpoint did not answer with JSON (is $URL an MCP endpoint?)"
        return 1
    fi
    if printf '%s' "$_payload" | jq -e 'has("error")' >/dev/null 2>&1; then
        mcp_fail "$(printf '%s' "$_payload" | jq -r '.error.message // "protocol error"')"
        return 1
    fi
    if [ "$(printf '%s' "$_payload" | jq -r '.result.isError // false')" = "true" ]; then
        mcp_fail "$(printf '%s' "$_payload" | jq -r '.result.content[0].text // "the tool reported an error"')"
        return 1
    fi
    printf '%s' "$_payload" | jq -r '.result.content[0].text // empty'
}

# --------------------------------------------------------------- the join ---

retry_advice() {
    say ""
    info "Nothing was written: no $ENV_FILE, no client config, no change at all."
    info "Check the endpoint and try again:"
    say ""
    say "      sh install.sh --url $URL --dry-run    # see what it would do"
    say ""
}

# Proves the token works BEFORE a single byte of it is written anywhere.
#
# Two things make this worth the extra round trip. The connection is FRESH and
# carries the bearer from the first byte, which is the state a configured
# assistant starts in — the connection the token was minted on was anonymous
# at initialize, so succeeding there proves nothing about succeeding here. And
# the tool is list_teams, not health: health answers without a token at all,
# so it would return ok for an empty bearer and tell us nothing. list_teams is
# account-scoped, and checking the expected slug is in its answer confirms the
# token is on the team we just joined rather than merely valid.
verify_token() {
    _tok="$1"
    _want="$2"
    mcp_connect "$_tok" || return 1
    _teams=$(mcp_call "$_tok" "list_teams" '{}') || return 1
    if [ -n "$_want" ]; then
        printf '%s' "$_teams" |
            jq -e --arg s "$_want" 'any(.teams[]?; .slug == $s)' >/dev/null 2>&1 || {
                mcp_fail "the token authenticates, but \"$_want\" is not in the teams it can see"
                return 1
            }
    fi
    return 0
}

announce_join_code() {
    say ""
    say "    ┌──────────────────────────────────────────────────────────"
    if [ "$CREATED" = "true" ]; then
        say "    │  Team created: $TEAM_NAME  (slug: $TEAM_SLUG)"
    else
        say "    │  Team already existed: $TEAM_NAME  (slug: $TEAM_SLUG)"
    fi
    say "    │"
    say "    │  Join code:  $NEW_JOIN_CODE"
    say "    │"
    say "    │  This is the team's SHARED SECRET. Anyone holding it can"
    say "    │  join the team and see the board. Send it to your team"
    say "    │  the way you would send a password — never commit it,"
    say "    │  never paste it in a public channel."
    say "    │"
    say "    │  They run:  METICHE_JOIN_CODE=$NEW_JOIN_CODE sh install.sh"
    say "    └──────────────────────────────────────────────────────────"
    say ""
}

# A token already sitting in ~/.metiche/env is SENT on the join, and that is
# not an optimisation — it is what makes a second run of this script a no-op.
#
# The server mints the ACCOUNT on a join that arrives with no bearer, and the
# agent row is keyed on (account, client_key). So re-running without the token
# you already have mints a second account, and with it a second member and a
# second agent on the board, however stable client_key is. A stable client_key
# is necessary and not sufficient. Carrying the token lands all three on the
# rows that already exist, which is exactly what the server's own instructions
# say: joining again with your token adds a membership, not an identity.
existing_token() {
    [ -f "$ENV_FILE" ] || return 0
    _prior=$(sed -n "s/^$TOKEN_ENV=//p" "$ENV_FILE" | sed -n '1p')
    case "$_prior" in
        ""|*[!A-Za-z0-9._-]*) return 0 ;;
    esac
    printf '%s' "$_prior"
}

# The one mutating network call this script makes, and the one thing every
# step below it depends on. If it fails, nothing is written at all.
do_join() {
    step "Team"

    if [ "$ACTION" = "token" ]; then
        info "a token was supplied in \$METICHE_TOKEN — no join needed"
        info "(the token is the PERSON, not the machine: re-using it here is"
        info "correct, and joining again would only add a duplicate agent)"
        if [ "$DRY_RUN" -eq 1 ]; then
            info "would verify it by calling list_teams at $URL"
            return 0
        fi
        require_join_tools
        info "verifying the token against $URL"
        verify_token "$TOKEN" "" || {
            say ""
            warn "that token did not work against $URL: $(mcp_error)"
            retry_advice
            die "refusing to configure anything with a token that does not authenticate"
        }
        info "verified: the token authenticates and list_teams answered"
        return 0
    fi

    resolve_member_name
    resolve_agent_identity

    if [ "$ACTION" = "create" ]; then
        _tool="create_team"
        _args=$(printf '{"team_name":"%s","member_name":"%s","agent_label":"%s","client_key":"%s","client_kind":"%s","idempotency_key":"%s"}' \
            "$TEAM_NAME" "$MEMBER_NAME" "$AGENT_LABEL" "$CLIENT_KEY" "$CLIENT_KIND" \
            "$(idempotency_key "$TEAM_NAME")")
        _what="create the team \"$TEAM_NAME\""
    else
        _tool="join_team"
        _args=$(printf '{"join_code":"%s","member_name":"%s","agent_label":"%s","client_key":"%s","client_kind":"%s"}' \
            "$JOIN_CODE" "$MEMBER_NAME" "$AGENT_LABEL" "$CLIENT_KEY" "$CLIENT_KIND")
        _what="redeem your join code (read from $CODE_SOURCE, never printed)"
    fi

    info "as: member_name=$MEMBER_NAME agent_label=$AGENT_LABEL client_key=$CLIENT_KEY"

    # plan() prints and returns 1 under --dry-run, which is what keeps the one
    # mutating call in this script behind the same gate as every write.
    plan "call $_tool at $URL to $_what" || {
        info "then verify the minted token with list_teams on a fresh connection"
        info "dry run: no call was made, so there is no token — everything"
        info "below would be skipped too."
        return 0
    }

    require_join_tools

    _prior=$(existing_token)
    if [ -n "$_prior" ]; then
        info "an existing token is in $ENV_FILE; sending it with this call so"
        info "the re-run lands on the same account, member and agent"
    fi

    mcp_connect "$_prior" || {
        # A stale token — a re-pointed endpoint, a reset instance — is a 401
        # at initialize. Drop it and join anonymously rather than dying: the
        # user asked to be on a team, not to defend an old credential.
        if [ -n "$_prior" ]; then
            warn "the token in $ENV_FILE was refused by $URL; ignoring it and"
            warn "joining as a new identity"
            _prior=""
            mcp_connect "" || {
                say ""
                warn "could not reach the metiche endpoint at $URL"
                warn "$(mcp_error)"
                retry_advice
                die "the join did not happen"
            }
        else
            say ""
            warn "could not reach the metiche endpoint at $URL"
            warn "$(mcp_error)"
            retry_advice
            die "the join did not happen"
        fi
    }

    _result=$(mcp_call "$_prior" "$_tool" "$_args") || {
        say ""
        warn "$_tool was refused by $URL"
        warn "$(mcp_error)"
        retry_advice
        die "the join did not happen"
    }

    TOKEN=$(printf '%s' "$_result" | jq -r '.token // empty')
    TEAM_SLUG=$(printf '%s' "$_result" | jq -r '.team_slug // empty')
    TOKEN_SOURCE="$_tool"
    if [ -z "$TOKEN" ] && [ -n "$_prior" ]; then
        # Expected, not an error: one token per PERSON, so a call that carried
        # one gets no second one back. Keep the one we sent.
        TOKEN="$_prior"
        TOKEN_SOURCE="$ENV_FILE, re-used (one token per person; no second was minted)"
    fi
    if [ -z "$TOKEN" ]; then
        say ""
        warn "$_tool succeeded but returned no token, and this call carried none."
        retry_advice
        die "no token to configure anything with"
    fi
    info "$_tool succeeded: you are on team \"$TEAM_SLUG\" (the token is never printed)"

    if [ "$ACTION" = "create" ]; then
        NEW_JOIN_CODE=$(printf '%s' "$_result" | jq -r '.join_code // empty')
        # create_team is retry-safe: the same idempotency_key lands on the
        # team that exists rather than making a second one. Say which
        # happened, so a re-run does not read as "I just made another team".
        CREATED=$(printf '%s' "$_result" | jq -r '.created // false')
    fi

    info "verifying the token on a fresh connection that carries it from the first byte"
    verify_token "$TOKEN" "$TEAM_SLUG" || {
        say ""
        warn "the join SUCCEEDED at $URL, but the token it returned did not work:"
        warn "$(mcp_error)"
        say ""
        info "You are on the team; only this machine is unconfigured."
        retry_advice
        die "refusing to write a token that does not authenticate"
    }
    info "verified: list_teams answered and \"$TEAM_SLUG\" is in it"

    # Printed only now, and only on creation: a join code the user cannot see
    # is a team nobody else can be invited to.
    [ -n "$NEW_JOIN_CODE" ] && announce_join_code
    return 0
}

# ------------------------------------------------------------- json output ---

# The MCP server entry, shared by every client that speaks the common shape.
# The bearer is the server-minted TOKEN, verified working a few lines above —
# never the join code, which this endpoint answers with a 401. Written
# literally, to the user's own home-directory config, mode 0600.
server_json() {
    cat <<EOF
{
  "type": "http",
  "url": "$URL",
  "headers": {
    "Authorization": "Bearer $TOKEN"
  }
}
EOF
}

# Merge our server into an existing mcpServers document, or create one.
# jq is required only for the merge; a fresh file needs no tools at all.
write_mcp_json() {
    target="$1"
    label="$2"
    dir=$(dirname "$target")
    tmp="$target.metiche.tmp.$$"

    if [ -f "$target" ]; then
        if ! command -v jq >/dev/null 2>&1; then
            warn "$label: $target already exists and jq is not installed, so it cannot be"
            warn "merged safely. Add this entry to its \"mcpServers\" object by hand:"
            say ""
            say "      \"$SERVER_NAME\": { \"type\": \"http\", \"url\": \"$URL\","
            say "        \"headers\": { \"Authorization\": \"Bearer <the token in ~/.metiche/env>\" } }"
            say ""
            return 0
        fi
        if ! jq -e . "$target" >/dev/null 2>&1; then
            warn "$label: $target is not valid JSON; leaving it alone."
            return 0
        fi
        new=$(jq --argjson s "$(server_json)" --arg n "$SERVER_NAME" \
                 '.mcpServers = ((.mcpServers // {}) | .[$n] = $s)' "$target")
    else
        if command -v jq >/dev/null 2>&1; then
            new=$(jq -n --argjson s "$(server_json)" --arg n "$SERVER_NAME" \
                     '{mcpServers: {($n): $s}}')
        else
            new=$(printf '{\n  "mcpServers": {\n    "%s": %s\n  }\n}\n' \
                     "$SERVER_NAME" "$(server_json)")
        fi
    fi

    if [ -f "$target" ] && [ "$new" = "$(cat "$target")" ]; then
        info "$label: $target already correct, nothing to do"
        return 0
    fi

    if [ -f "$target" ]; then
        plan "update $target (a backup is kept alongside it)" || return 0
    else
        plan "create $target" || return 0
    fi

    mkdir -p "$dir"
    [ -f "$target" ] && cp "$target" "$target.metiche-backup"
    (umask 077; printf '%s\n' "$new" > "$tmp")
    mv "$tmp" "$target"
    chmod 600 "$target" 2>/dev/null || true
    DID_SOMETHING=1
}

# -------------------------------------------------------------- the env file ---

PROFILE_LINE='[ -f "$HOME/.metiche/env" ] && . "$HOME/.metiche/env"'

# METICHE_JOIN_CODE is deliberately NOT exported any more. Nothing reads it
# at runtime: it is an argument to one tool call, consumed during this script's
# join, and keeping it in the environment only invited the mistake this whole
# change is fixing — a client picking it up and sending it as a bearer.
write_env_file() {
    step "Token"
    if [ -z "$TOKEN" ]; then
        # Only reachable under --dry-run: with no join there is no token.
        info "would write $ENV_FILE (mode 0600), exporting $TOKEN_ENV=<the minted token>"
        return 0
    fi
    desired="# metiche — created by install.sh. Keep this file private.
#
# This is the TOKEN the metiche server minted for you. It identifies the
# person, not one agent and not one team, and it is what every client sends
# as its bearer. It is NOT the team's join code: a join code is an invite you
# hand to a teammate, and sending one as a bearer gets a 401.
$TOKEN_ENV=$TOKEN
export $TOKEN_ENV
"
    # Both sides through a command substitution: it strips trailing newlines,
    # and $desired has one that the file also has, so comparing the raw string
    # to $(cat ...) would never match and every run would rewrite the file.
    if [ -f "$ENV_FILE" ] && [ "$(printf '%s' "$desired")" = "$(cat "$ENV_FILE")" ]; then
        info "$ENV_FILE already correct, nothing to do"
    else
        plan "write $ENV_FILE (mode 0600)" && {
            mkdir -p "$ENV_DIR"
            chmod 700 "$ENV_DIR" 2>/dev/null || true
            (umask 077; printf '%s' "$desired" > "$ENV_FILE")
            chmod 600 "$ENV_FILE" 2>/dev/null || true
            DID_SOMETHING=1
        }
    fi
    info "$TOKEN_ENV, from: $TOKEN_SOURCE (the token itself is never printed)"
}

detect_profile() {
    case "${SHELL:-}" in
        */zsh)  printf '%s\n' "${ZDOTDIR:-$HOME}/.zshrc" ;;
        */bash) if [ -f "$HOME/.bash_profile" ]; then printf '%s\n' "$HOME/.bash_profile";
                else printf '%s\n' "$HOME/.bashrc"; fi ;;
        */fish) printf '%s\n' "$HOME/.config/fish/config.fish" ;;
        *)      printf '%s\n' "$HOME/.profile" ;;
    esac
}

handle_profile() {
    profile=$(detect_profile)
    case "$profile" in
        *config.fish)
            say ""
            info "fish detected: ~/.metiche/env is POSIX syntax and fish cannot source it."
            info "Add this to $profile instead:"
            say ""
            say "      set -gx $TOKEN_ENV (string match -r '^$TOKEN_ENV=(.*)' < ~/.metiche/env)[2]"
            say ""
            return 0
            ;;
    esac

    if [ -f "$profile" ] && grep -qF '.metiche/env' "$profile" 2>/dev/null; then
        info "$profile already loads ~/.metiche/env"
        return 0
    fi

    # WRITING THIS BY DEFAULT IS THE POINT, not a convenience.
    #
    # The token is only ever read from the ENVIRONMENT. Codex stores the NAME
    # of the variable and resolves it at connect time; Claude Code expands
    # ${METICHE_TOKEN} in .mcp.json the same way. So a machine missing this
    # line is a machine where every client is configured, the installer
    # reports success, and every call then sends an EMPTY bearer -- which the
    # server answers with a 401 that reads like a bad token rather than an
    # absent one. That is an expensive way to learn about a shell profile.
    #
    # This was opt-in, out of caution about touching someone's shell config.
    # The caution bought nothing and cost an install that looks finished and
    # does not work. One guarded line in a profile is a smaller intrusion than
    # a broken client left behind; --no-write-profile is there for anyone who
    # manages their dotfiles themselves.
    if [ "$WRITE_PROFILE" -eq 1 ]; then
        if [ ! -f "$profile" ]; then
            info "$profile does not exist yet; creating it"
        fi
        plan "append the loader line to $profile" && {
            printf '\n# metiche - load the token your assistants authenticate with\n%s\n' \
                "$PROFILE_LINE" >> "$profile"
            DID_SOMETHING=1
        }
        # Say this whether or not we just wrote it: an assistant already
        # running has read its environment and will not read it again.
        info "open a new terminal (or run: . \"\$HOME/.metiche/env\")"
        info "then RESTART your assistant — the MCP connection is made at startup"
    else
        say ""
        info "--no-write-profile: add this line to $profile yourself, or your"
        info "assistants will send an empty bearer and every call will 401:"
        say ""
        say "      $PROFILE_LINE"
        say ""
    fi
}

# ------------------------------------------------------------- Claude Code ---

manual_mcp_note() {
    warn "You can register just the MCP server (no skill) by hand with:"
    say ""
    say "      claude mcp add --transport http --scope user $SERVER_NAME $URL \\"
    say "        --header \"Authorization: Bearer \$$TOKEN_ENV\""
    say ""
    warn "That command puts the expanded token in your process list while it runs,"
    warn "which is why this script does not run it for you."
}

# The marketplace manifest lives at plugin/.claude-plugin/marketplace.json in
# the metiche repository, so the source is a directory: the local one if you
# are standing in a clone, otherwise a shallow clone under ~/.metiche.
resolve_marketplace() {
    [ -n "$MARKETPLACE" ] && return 0

    if [ -f "./plugin/.claude-plugin/marketplace.json" ]; then
        MARKETPLACE="./plugin"
        info "using the marketplace in this checkout: $MARKETPLACE"
        return 0
    fi

    if ! command -v git >/dev/null 2>&1; then
        warn "git is not installed, so the plugin cannot be fetched."
        return 1
    fi

    if [ -d "$SRC_DIR/.git" ]; then
        plan "update the shallow clone at $SRC_DIR" && {
            git -C "$SRC_DIR" fetch --depth 1 origin HEAD >/dev/null 2>&1 &&
            git -C "$SRC_DIR" reset --hard FETCH_HEAD >/dev/null 2>&1 ||
                warn "could not update $SRC_DIR — using what is already there"
        }
    else
        plan "shallow-clone $REPO into $SRC_DIR" || return 1
        mkdir -p "$ENV_DIR"
        git clone --depth 1 "$REPO" "$SRC_DIR" >/dev/null 2>&1 ||
            { warn "clone failed: $REPO"; return 1; }
        DID_SOMETHING=1
    fi

    MARKETPLACE="$SRC_DIR/plugin"
    [ -f "$MARKETPLACE/.claude-plugin/marketplace.json" ] || {
        warn "no marketplace manifest at $MARKETPLACE/.claude-plugin/"
        return 1
    }
    return 0
}

install_claude() {
    step "Claude Code"

    if ! command -v claude >/dev/null 2>&1; then
        info "not found on PATH — skipping"
        return 0
    fi
    info "found: $(command -v claude)"

    # The plugin bundles the skill and the MCP server definition together, and
    # that definition expands its bearer from the environment at runtime, so no
    # credential passes through argv or gets written into the plugin. The
    # variable it expands must be $TOKEN_ENV: a plugin still naming
    # METICHE_JOIN_CODE would send an invite as a bearer and get a 401 on its
    # first call, which is the bug this script exists to stop making.
    if ! resolve_marketplace; then
        warn "skipping the plugin."
        manual_mcp_note
        return 0
    fi

    if [ "$DRY_RUN" -eq 1 ]; then
        info "would run: claude plugin marketplace add $MARKETPLACE"
        info "would run: claude plugin install $PLUGIN_NAME@$MARKETPLACE_NAME --scope user -y"
        return 0
    fi

    info "running: claude plugin marketplace add $MARKETPLACE"
    claude plugin marketplace add "$MARKETPLACE" 2>&1 | sed 's/^/      /' ||
        warn "marketplace add reported a problem (already added?) — continuing"

    info "running: claude plugin install $PLUGIN_NAME@$MARKETPLACE_NAME --scope user -y"
    if claude plugin install "$PLUGIN_NAME@$MARKETPLACE_NAME" --scope user -y 2>&1 | sed 's/^/      /'; then
        info "installed: the metiche-teamwork skill and the metiche MCP server."
        info "Restart Claude Code to load them."
        DID_SOMETHING=1
    else
        warn "plugin install failed."
        manual_mcp_note
    fi
}

# ------------------------------------------------------------------ Cursor ---

install_cursor() {
    step "Cursor"
    target="$HOME/.cursor/mcp.json"
    if [ ! -d "$HOME/.cursor" ] && ! command -v cursor >/dev/null 2>&1; then
        info "not found (no ~/.cursor, no cursor on PATH) — skipping"
        return 0
    fi
    info "configuring globally at $target"
    info "(project-scoped .cursor/mcp.json is skipped on purpose — it lives in your repo)"
    write_mcp_json "$target" "Cursor"
    info "Cursor has no plugin format for the skill: point it at"
    info "skill/metiche-teamwork/SKILL.md, or paste it into your project rules."
}

# ---------------------------------------------------------------- Windsurf ---

install_windsurf() {
    step "Windsurf"
    target="$HOME/.codeium/windsurf/mcp_config.json"
    if [ ! -d "$HOME/.codeium" ] && ! command -v windsurf >/dev/null 2>&1; then
        info "not found (no ~/.codeium, no windsurf on PATH) — skipping"
        return 0
    fi
    info "configuring at $target"
    write_mcp_json "$target" "Windsurf"
    info "Windsurf has no plugin format for the skill: point it at"
    info "skill/metiche-teamwork/SKILL.md, or paste it into your workspace rules."
}

# ------------------------------------------------------------------- Codex ---

# Codex is the one client where the token does NOT go in the config file.
#
# `codex mcp add --bearer-token-env-var NAME` stores the NAME of an environment
# variable; Codex reads the value from its own environment at connect time. So
# the config file contains no secret and is safe to read over someone's
# shoulder — but it also means ~/.metiche/env MUST be loaded in the shell that
# launches codex, or the bearer is empty and every call is unauthorized. For
# the other clients the profile line is a convenience. Here it is required,
# which is why this function says so out loud.
#
# We shell out to `codex mcp add` rather than editing config.toml ourselves,
# deliberately. That file is mode 0600 and full of the user's real settings
# (model, plugins, marketplaces), there is no toml equivalent of jq to lean on,
# and a hand-rolled TOML merge that gets it wrong breaks their whole assistant.
# The CLI owns its own format. Verified against codex-cli 0.151.0: re-running
# is idempotent, and unrelated keys survive untouched.
install_codex() {
    step "Codex"

    if ! command -v codex >/dev/null 2>&1; then
        if [ -d "$HOME/.codex" ]; then
            info "found ~/.codex but no codex on PATH — cannot use \`codex mcp add\`."
            codex_manual_note
        else
            info "not found (no codex on PATH, no ~/.codex) — skipping"
        fi
        return 0
    fi

    # Already pointing at the same endpoint? Say nothing and change nothing.
    if command -v grep >/dev/null 2>&1 &&
       codex mcp get "$SERVER_NAME" --json 2>/dev/null | grep -q "\"$URL\""; then
        info "already registered at $URL, nothing to do"
        codex_env_warning
        return 0
    fi

    plan "run: codex mcp add $SERVER_NAME --url $URL --bearer-token-env-var $TOKEN_ENV" || {
        codex_env_warning
        return 0
    }

    if codex mcp add "$SERVER_NAME" \
            --url "$URL" \
            --bearer-token-env-var "$TOKEN_ENV" >/dev/null 2>&1; then
        info "registered in ~/.codex/config.toml (no token written to disk)"
        DID_SOMETHING=1
    else
        warn "\`codex mcp add\` failed. Your Codex may predate streamable-HTTP support"
        warn "(verified working on codex-cli 0.151.0). Add it by hand:"
        codex_manual_note
        return 0
    fi

    codex_env_warning
    info "Codex reads AGENTS.md for standing instructions: ~/.codex/AGENTS.md globally,"
    info "or AGENTS.md in a repository root. Append skill/metiche-teamwork/SKILL.md to"
    info "one of those to teach it the cadence. This script will not edit an existing"
    info "AGENTS.md for you — it is your file and merging it is your call."
}

# The env var is load-bearing for Codex specifically, so warn every time
# rather than only on a fresh install.
codex_env_warning() {
    # Read the variable TOKEN_ENV names, without eval. Only one name is ever
    # correct here, so testing it directly is clearer than indirection.
    if [ -z "${METICHE_TOKEN:-}" ]; then
        warn "$TOKEN_ENV is not set in this shell, so Codex will send an empty bearer."
        warn "Load it before starting codex:  . \"\$HOME/.metiche/env\""
        warn "Make it permanent with --write-profile, or add that line to your profile."
    else
        info "$TOKEN_ENV is set in this shell; Codex inherits it when launched from here."
    fi
}

# Printed only when we cannot run the CLI. Unlike the note for Zed, this format
# IS verified (codex-cli 0.151.0), so it is safe to hand someone.
codex_manual_note() {
    say ""
    say "      Add to ~/.codex/config.toml:"
    say ""
    say "          [mcp_servers.$SERVER_NAME]"
    say "          url = \"$URL\""
    say "          bearer_token_env_var = \"$TOKEN_ENV\""
    say ""
    say "      Then load ~/.metiche/env in the shell you start codex from."
    say ""
}

# ------------------------------------------------------------------- other ---

note_unverified() {
    step "Other assistants"
    cat <<EOF
    Zed is not configured automatically: its MCP config format was not
    verified, and this script will not invent one. To wire it up by hand, any
    MCP client needs exactly this:

        transport  streamable http
        url        $URL
        header     Authorization: Bearer <the token in ~/.metiche/env>

    That token is minted by join_team, which this script already called for
    you. It is not your join code: a join code sent as a bearer is a 401.

    And the skill is one markdown file — skill/metiche-teamwork/SKILL.md in the
    metiche repository. Any assistant that takes a rules or instructions file
    can use it as-is.
EOF
}

# -------------------------------------------------------------------- main ---

main() {
    say "metiche installer"
    say "  endpoint: $URL"
    if [ "$DRY_RUN" -eq 1 ]; then
        say "  DRY RUN — nothing will be written and nothing will be called"
    else
        say ""
        say "  This joins your team over the network before it configures"
        say "  anything. If the endpoint above does not answer, nothing on"
        say "  this machine is changed at all."
    fi

    # Order matters, and it is the whole design: decide, join, verify, and
    # only then write. Every step below do_join needs $TOKEN, and $TOKEN does
    # not exist until a fresh authenticated connection has proved it works.
    choose_identity
    do_join
    write_env_file

    found=0
    if want claude   && command -v claude >/dev/null 2>&1; then found=1; fi
    if want cursor   && { [ -d "$HOME/.cursor" ] || command -v cursor >/dev/null 2>&1; }; then found=1; fi
    if want windsurf && { [ -d "$HOME/.codeium" ] || command -v windsurf >/dev/null 2>&1; }; then found=1; fi
    if want codex    && { [ -d "$HOME/.codex" ]   || command -v codex >/dev/null 2>&1; }; then found=1; fi

    want claude   && install_claude
    want cursor   && install_cursor
    want windsurf && install_windsurf
    want codex    && install_codex

    if [ "$found" -eq 0 ]; then
        warn "no supported assistant detected on this machine"
    fi

    handle_profile
    note_unverified

    step "Done"
    if [ "$DRY_RUN" -eq 1 ]; then
        info "dry run — nothing was called and nothing was changed"
    elif [ "$DID_SOMETHING" -eq 1 ]; then
        if [ -n "$TEAM_SLUG" ]; then
            info "you are on team \"$TEAM_SLUG\" and this machine is configured."
        else
            info "this machine is configured with the token you supplied."
        fi
        info "Restart your assistant — it is already authenticated, so it can"
        info "call start_session straight away. It does not need to join."
    else
        info "everything was already configured."
    fi
}

main
