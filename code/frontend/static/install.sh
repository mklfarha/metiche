#!/bin/sh
# metiche installer — https://metiche.xyz
#
#   curl -fsSL https://metiche.xyz/install.sh | sh
#   sh install.sh --uninstall
#
# Points your coding assistants at a metiche team: ONE JOIN PER CLIENT. Every
# assistant it finds gets its own agent on the board and its own token, and
# the token is the whole identity — nothing else rides along, because the
# Authorization header is the one thing every MCP client reliably forwards.
# It also installs the metiche-teamwork skill where it can, and gives Codex a
# standing instruction in AGENTS.md.
#
# A JOIN CODE IS NOT A BEARER TOKEN. The join code is an invite — an argument
# you pass to the join_team tool. The bearer is minted by the server when you
# redeem it. Configuring a client with the join code as its bearer produces a
# 401 on its very first request, before it can ever reach join_team. So this
# script does the joins itself and configures each client with the token that
# came back FOR THAT CLIENT.
#
# What it touches — and inside each file, ONLY what metiche owns:
#
#   ~/.metiche/env                        the METICHE_* lines (the anchor token)
#   ~/.metiche/src                        a shallow clone, for the plugin
#   ~/.cursor/mcp.json                    mcpServers.metiche
#   ~/.codeium/windsurf/mcp_config.json   mcpServers.metiche
#   ~/.codex/config.toml                  the [mcp_servers.metiche] table
#   ~/.codex/AGENTS.md                    the block between <!-- metiche:start -->
#                                         and <!-- metiche:end -->
#   ~/.claude.json (user scope)           servers metiche and the legacy
#                                         metiche-direct, via the `claude` CLI
#   ~/.claude/skills/metiche-teamwork     removed only if byte-identical to a
#                                         shipped copy (the plugin ships it)
#   Claude Code's user-scope plugins      via the `claude` CLI (skill only)
#   your shell profile                    one loader line
#   ~/.metiche/bin/metiche                the metiche CLI, unless --no-cli
#   ~/.local/bin/metiche                  a symlink to it, only when ~/.local/bin
#                                         is on your PATH and nothing else is there
#
# An existing file is never replaced wholesale. Before the first change to any
# file that already exists, a copy is made next to it as
# <file>.metiche-backup-<timestamp>, and the script says where. A file it does
# not change gets no backup. A machine with old metiche state is converged:
# stale entries are rewritten, legacy ones removed, and everything else in
# those files is left byte for byte as it was.
#
# What it sends over the network: one list_teams to check a token you already
# have (and, if the server rejects it, one anonymous health call to be sure the
# server itself is fine before believing it), then for each client one
# join_team (or, for the first client of a new team, create_team) followed by
# one list_teams on a fresh connection that proves the token it got back
# works. Nothing for a client is written until that client's proof succeeds.
# Unless --no-cli: one request to the GitHub API for the latest metiche CLI
# release, and the download of its archive and checksums file from GitHub
# releases. The archive is installed only if its sha256 matches; any failure
# there is a warning and never fails the install.
# A saved token the server REJECTS starts a new identity, and says so; a
# network error, a 5xx or a timeout refuses and writes nothing — a flaky
# connection must never turn you into somebody new. A $METICHE_TOKEN that is
# byte for byte the token in ~/.metiche/env (the profile line loads it) is the
# saved token; one that matches a BACKUP of that file is a stale shell from
# before the last install, and the current saved token is used instead; any
# other one was passed on purpose, and a rejection of it refuses.
#
# Last, only in a terminal, never under CI, and only when you say yes (the
# default answer): one open_board call, which returns a single-use sign-in
# link to your team's board. The link reaches the browser through a 0600 file
# in a private temporary directory; the file is what `open` or `xdg-open` is
# given, never the link, and it is removed before the script exits. Over SSH
# the link is printed for you to open on your own machine instead.
#
# --uninstall removes exactly what the list above says metiche owns, with the
# same backups, and does not contact the server.
#
# It never writes anything inside the current directory, never writes a token
# or a join code into a file in a repository, and never runs sudo. It never
# passes a credential on a command line (arguments are visible in `ps`), with
# ONE exception: `claude mcp add --header "Authorization: Bearer ..."`. The
# claude CLI is the only safe writer of ~/.claude.json while Claude Code is
# running — it rewrites that file constantly, and a jq merge racing it loses —
# so the token rides on that one command line for the moment it runs, and
# only when the entry is missing or differs; a re-run that changes nothing
# runs no such command. Run with --dry-run to see every change and every call
# it would make, without making any of them.
#
# SPDX-License-Identifier: Apache-2.0

set -eu

# ---------------------------------------------------------------- defaults ---

METICHE_URL_DEFAULT="https://mcp.metiche.xyz/v1/mcp"
METICHE_REPO_DEFAULT="https://github.com/mklfarha/metiche.git"
PLUGIN_NAME="metiche"
MARKETPLACE_NAME="metiche"
SERVER_NAME="metiche"
# A server name that was added by hand, with a custom header, before the
# installer registered Claude Code itself. Owned by metiche; removed on sight.
LEGACY_CLAUDE_SERVER="metiche-direct"
# The variable ~/.metiche/env exports the ANCHOR under. Clients do not read it
# — each has its own token written literally into its own config — but later
# runs of this script do, and so does the .mcp.json at the root of the metiche
# repository, which expands ${METICHE_TOKEN}. It is always a server-minted
# token, never a join code.
TOKEN_ENV="METICHE_TOKEN"
# MCP protocol revision this script speaks in its handshake.
MCP_PROTOCOL="2025-06-18"
# Seconds for any single HTTP call. Two round trips to join, two to verify.
HTTP_TIMEOUT=30
# The clients this script knows, in the order it configures them. The order
# is load-bearing: on a new team the FIRST one creates it and the rest attach
# by slug, and Claude Code first means its token is the one that becomes the
# anchor.
KNOWN_CLIENTS="claude cursor windsurf codex"

# Codex's project-doc budget (project_doc_max_bytes): past 32 KiB it cuts the
# file off, so a block appended at the end would be the part it never reads.
AGENTS_MAX_BYTES=32768
AGENTS_START="<!-- metiche:start -->"
AGENTS_END="<!-- metiche:end -->"
ENV_START="# >>> metiche >>>"
ENV_END="# <<< metiche <<<"
# The first line of every ~/.metiche/env an earlier installer wrote.
ENV_LEGACY_HEADER="# metiche — created by install.sh. Keep this file private."
# sha256 of every SKILL.md metiche has shipped, oldest first. A standalone copy
# of the skill is removed only when it matches one of these, or a copy on this
# machine — an older shipped version is ours to replace, anything else is the
# person's own edit and stays. So every entry stays forever: people still have
# those versions installed. Whitespace separates them; `for _k in` splits it.
# Ship a new SKILL.md, append its sha256 here — TestInstallerKnowsTheCurrentSkill
# fails the build if you forget.
#   0.5.0  902d2254...
#   0.6.0  168388be...
#   0.6.1  9e05ef0a...
#   0.7.0  cc539632...
KNOWN_SKILL_SHA256="902d22540c064631ccdfef1d0da288cb46af00d9b34f55c02718f088f794faba
168388bed24d41c7581acd21b24a456c8b1bfde49595d8f8f47a2b6db61d9cbc
9e05ef0a5e6bc1058e0cb94cebf3afa4c9615db6a2e1a696b31ae286ad1d5ee7
cc539632123d8008c93b4e8f295811c2ed90523e9105bdfcd040e07a27b2b962"

URL="${METICHE_URL:-$METICHE_URL_DEFAULT}"
# The metiche CLI (docs/CLI.md §6.3), installed from a GitHub release. The two
# URL overrides exist to test the step against a local mirror of a release:
# METICHE_CLI_RELEASE_BASE replaces https://github.com/mklfarha/metiche/releases/download
# (the archive is <base>/v<version>/metiche_<OS>_<ARCH>.tar.gz) and
# METICHE_CLI_LATEST_URL replaces the GitHub API's releases/latest address.
CLI_RELEASE_BASE="${METICHE_CLI_RELEASE_BASE:-https://github.com/mklfarha/metiche/releases/download}"
CLI_LATEST_URL="${METICHE_CLI_LATEST_URL:-https://api.github.com/repos/mklfarha/metiche/releases/latest}"
CLI_VERSION="${METICHE_CLI_VERSION:-}"
INSTALL_CLI=1
REPO="${METICHE_REPO:-$METICHE_REPO_DEFAULT}"
MARKETPLACE="${METICHE_MARKETPLACE:-}"
DRY_RUN=0
UNINSTALL=0
WRITE_PROFILE=1
ONLY=""
# ask, yes or no: whether the last step opens the board signed in. See
# board_step. --open and --no-open override the environment.
OPEN_BOARD="${METICHE_OPEN_BOARD:-ask}"
# How long the sign-in file stays on disk after the opener returns: the opener
# hands the path to the browser and exits, and the browser reads the file a
# moment later.
BOARD_FILE_SECONDS=10

ENV_DIR="$HOME/.metiche"
ENV_FILE="$ENV_DIR/env"
SRC_DIR="$ENV_DIR/src"
CODEX_DIR="${CODEX_HOME:-$HOME/.codex}"
CLAUDE_DIR="${CLAUDE_CONFIG_DIR:-$HOME/.claude}"

# One timestamp per run, so every backup this run makes shares a suffix.
TS=$(date +%Y%m%d-%H%M%S)
BACKED_UP=""
TMP_SEQ=0

# ACTION is one of join, create, token: redeem an invite, make a new team, or
# attach to a team with a token you already have. Chosen before anything
# touches the network, and never from a command-line argument.
ACTION=""
JOIN_CODE=""
CODE_SOURCE=""
TEAM_SLUG=""
TEAM_NAME="${METICHE_TEAM_NAME:-}"
MEMBER_NAME=""
NEW_JOIN_CODE=""
CREATED="false"
DID_SOMETHING=0

# The anchor: a token of YOURS, carried on a join so the server attaches a new
# client to the same person instead of minting a new one. See anchor_token.
ANCHOR=""
ANCHOR_SOURCE=""
ANCHOR_TEAMS=""
# 1 only when $METICHE_TOKEN is a token somebody PASSED: set, and not simply
# the token in ~/.metiche/env that the profile line loaded. See anchor_token.
TOKEN_EXPLICIT=0
# 1 when $METICHE_TOKEN is set but is byte for byte the saved token.
TOKEN_FROM_PROFILE=0
# 1 when $METICHE_TOKEN is not the saved token but WAS: it matches a backup of
# ~/.metiche/env, left in this shell from before the last install rewrote it.
TOKEN_STALE_SHELL=0

# The clients found on this machine, in KNOWN_CLIENTS order; the one being
# configured and its generated identity; and what is done so far.
CLIENTS=""
FIRST_CLIENT=""
CLIENT=""
CLIENT_KEY=""
AGENT_LABEL=""
CLIENT_KIND=""
CONFIGURED=""
ENV_DONE=0
DRY_JOINED=0

# Per-client tokens, one variable per known client. See tok_set.
TOK_CLAUDE=""
TOK_CURSOR=""
TOK_WINDSURF=""
TOK_CODEX=""

# Scratch for the HTTP calls and for files being assembled before they are
# committed. Created on demand, mode 0700, outside $HOME, removed on exit.
MCP_TMP=""
MCP_SESSION=""
MCP_HTTP=""
MCP_ERROR=""
LAST_TEAMS=""

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
  sh install.sh --uninstall [--dry-run]

One join per client. Every assistant it finds on this machine becomes its own
agent on your team, with its own TOKEN written into its own config — so Claude
Code and Codex on one laptop are two agents, and neither has to be told which
one it is. A join code is an invite, not a credential: a client configured
with a join code as its bearer gets a 401 on its first request.

Options:
  --dry-run           Print every change and every call that would be made —
                      which file, which entry, what it is now and what it
                      would become — and change nothing, call nothing.
  --uninstall         Remove what metiche added: its server entries, its
                      AGENTS.md block, its profile line and ~/.metiche.
                      Backs up every file it changes. Contacts no server.
  --url <url>         MCP endpoint (default: https://mcp.metiche.xyz/v1/mcp).
  --marketplace <src> Claude Code plugin marketplace source. By default the
                      script uses ./plugin if you are standing in a clone of
                      the metiche repository, and otherwise makes a shallow
                      clone at ~/.metiche/src and uses ~/.metiche/src/plugin.
  --only <list>       Comma-separated subset of: claude,cursor,windsurf,codex.
                      Default: every assistant detected on this machine.
  --no-write-profile  Do NOT append the line that loads ~/.metiche/env to your
                      shell profile. Your assistants do not need it — each has
                      its own token in its own config — but the .mcp.json in
                      the metiche repository expands ${METICHE_TOKEN}.
  --write-profile     Explicitly ask for the default.
  --open              At the end, open your team's board in a browser, signed
                      in, without asking. Only in a terminal and never under
                      CI: without a terminal no sign-in link is created.
  --no-open           Never create a sign-in link. By default a terminal run
                      asks, and the default answer is yes.
                      METICHE_OPEN_BOARD=ask|yes|no says the same.
  --no-cli            Do not install the metiche command-line tool. By default
                      the latest release is downloaded from GitHub, verified
                      against its sha256 checksums file and installed as
                      ~/.metiche/bin/metiche. METICHE_CLI_VERSION=vX.Y.Z pins a
                      version. A failed download never fails the install.
  -h, --help          This.

Joining, or creating. With no join code and no token of yours on this machine
the script asks which you want; it never guesses. Non-interactively, pick with
one environment variable:

  METICHE_JOIN_CODE=your-code sh install.sh    # join a team that exists
  METICHE_TEAM_NAME="Payments squad" sh install.sh
                                               # create a team, print its
                                               # join code for your teammates
  METICHE_TOKEN=your-token sh install.sh       # already joined elsewhere:
                                               # attach this machine's clients

A re-run needs none of them: the token in ~/.metiche/env is used, and if you
are on several teams, METICHE_TEAM_SLUG says which one. If the server REJECTS
that saved token (it was reset, or the token revoked), the script says so and
starts a new identity; a network error or a 5xx refuses instead.

$METICHE_TOKEN holding the SAME token as ~/.metiche/env (the profile line
loads it into every shell) counts as that saved token: a rejection starts a
new identity, and METICHE_JOIN_CODE / METICHE_TEAM_NAME still apply. A token
that matches a backup of ~/.metiche/env is what a shell opened before the last
install still holds: the current saved token is used, and you are told to open
a new terminal. Only a token that is neither is one you passed; it outranks
both, and a rejection of it refuses.

Signing in to the board. The last step offers to open your team's board in
your browser, signed in. open_board returns a sign-in link that works once,
for 10 minutes, and signs one browser in as you. It is handed to the browser
through a private 0600 file, never on a command line, and the file is removed
before the script exits. Over SSH the link is printed instead, to open on your
own machine. Nothing is created without a terminal, or when CI is set.

Every file it changes that already existed is backed up first, next to
itself, as <file>.metiche-backup-<timestamp>.

Secrets are read from the environment or from a prompt and are deliberately
NOT accepted as command-line arguments: arguments are visible in `ps` to every
user on the machine and land in your shell history. For the same reason the
token and the join code are handed to curl through a config document on its
standard input, never as curl arguments. The one exception is noted under
Claude Code below.

Identity is generated, never typed. Each client's client_key is
<machine>-<client> (e.g. laptop-claude, laptop-codex) and its label is
"<client> on <machine>"; <machine> is this machine's short hostname. Both are
stable across re-runs, which is what makes a re-run keep the same agents and
the same tokens instead of adding new ones. Optional overrides:

  METICHE_MEMBER_NAME   your name (else `git config user.name`, else $USER)
  METICHE_MACHINE_ID    the <machine> part (else the hostname)
  METICHE_CLIENT_KEY    honoured only with --only <one client>
  METICHE_AGENT_LABEL   honoured only with --only <one client>

jq and curl are required for the joins. See the comment above
require_join_tools for why there is no regex fallback.

Claude Code: the plugin installs the metiche-teamwork skill only. The server
is registered with `claude mcp add --scope user`, carrying Claude Code's own
token in an Authorization header. That command line is the one place a token
is briefly visible in `ps`, and it runs only when ~/.claude.json does not
already carry the same URL and token.

Cursor is configured globally (~/.cursor/mcp.json) and never per-project: the
only project-scoped location is .cursor/mcp.json inside your repository, and a
credential does not belong in a repository.

Codex gets a [mcp_servers.metiche] table in ~/.codex/config.toml (or
$CODEX_HOME) with its token in http_headers, written literally: a
GUI-launched Codex does not read your shell profile, so a token it would have
to find in the environment never reaches it. Its standing instruction goes in
a delimited block in AGENTS.md next to that config; nothing else in that file
is touched, and if AGENTS.override.md exists (which Codex reads instead) the
script warns rather than writing a block Codex would ignore.

Zed is not configured. Its MCP config format was not verified when this script
was written, and guessing at a config file is worse than printing the endpoint
and letting you paste it. See the note it prints.
EOF
}

# ------------------------------------------------------------------- flags ---

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run)       DRY_RUN=1 ;;
        --uninstall)     UNINSTALL=1 ;;
        --write-profile)    WRITE_PROFILE=1 ;;
        --no-write-profile) WRITE_PROFILE=0 ;;
        --url)           [ $# -ge 2 ] || die "--url needs a value"; URL="$2"; shift ;;
        --url=*)         URL="${1#--url=}" ;;
        --marketplace)   [ $# -ge 2 ] || die "--marketplace needs a value"; MARKETPLACE="$2"; shift ;;
        --marketplace=*) MARKETPLACE="${1#--marketplace=}" ;;
        --only)          [ $# -ge 2 ] || die "--only needs a value"; ONLY="$2"; shift ;;
        --only=*)        ONLY="${1#--only=}" ;;
        --open)          OPEN_BOARD="yes" ;;
        --no-open)       OPEN_BOARD="no" ;;
        --no-cli)        INSTALL_CLI=0 ;;
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
    http://localhost*|http://127.0.0.1*) [ "$UNINSTALL" -eq 1 ] || warn "using a plaintext local endpoint: $URL" ;;
    *) die "--url must be https (or a local http endpoint), got: $URL" ;;
esac

for _cli_url in "$CLI_RELEASE_BASE" "$CLI_LATEST_URL"; do
    case "$_cli_url" in
        https://*|http://localhost*|http://127.0.0.1*) ;;
        *) die "METICHE_CLI_RELEASE_BASE and METICHE_CLI_LATEST_URL must be https (or a local http address), got: $_cli_url" ;;
    esac
done
case "$CLI_VERSION" in
    *[!A-Za-z0-9.+-]*) die "METICHE_CLI_VERSION must look like v1.2.3, got: $CLI_VERSION" ;;
esac

case "$OPEN_BOARD" in
    ask|yes|no) ;;
    *) die "METICHE_OPEN_BOARD must be ask, yes or no, got: $OPEN_BOARD" ;;
esac

# A typo in --only would otherwise configure nothing and say so only as "no
# supported assistant detected", which reads like a detection bug.
if [ -n "$ONLY" ]; then
    _rest="$ONLY,"
    while [ -n "$_rest" ]; do
        _item=${_rest%%,*}
        _rest=${_rest#*,}
        case "$_item" in
            claude|cursor|windsurf|codex) ;;
            *) die "--only: unknown assistant \"$_item\" (expected a comma-separated subset of: claude,cursor,windsurf,codex)" ;;
        esac
    done
fi

want() {
    [ -z "$ONLY" ] && return 0
    case ",$ONLY," in
        *",$1,"*) return 0 ;;
        *) return 1 ;;
    esac
}

# True when --only names exactly one client: the only case in which a
# hand-typed client_key or agent_label can mean one thing.
only_one_client() {
    case "$ONLY" in
        ""|*,*) return 1 ;;
        *) return 0 ;;
    esac
}

# ------------------------------------------------------------------- files ---
#
# Every change to a file goes through commit_file, and every file that already
# exists is backed up by backup_file before its first change. There is no
# other write path for an existing file in this script: that is what makes
# "overwrote a file it did not create" a mistake it cannot make.

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

# A fresh scratch file name inside the 0700 temp directory.
new_tmp() {
    TMP_SEQ=$((TMP_SEQ + 1))
    printf '%s/f%s' "$MCP_TMP" "$TMP_SEQ"
}

file_ends_with_newline() {
    [ -s "$1" ] && [ "$(tail -c 1 "$1" | od -An -c | tr -d ' ')" = '\n' ]
}

sha256_of() {
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | cut -d' ' -f1
    elif command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    fi
}

# Copies $1 next to itself before this run's first change to it. A backup that
# cannot be made stops the run before the change it was meant to protect.
backup_file() {
    [ -e "$1" ] || return 0
    case "$BACKED_UP" in
        *"|$1|"*) return 0 ;;
    esac
    _bk="$1.metiche-backup-$TS"
    [ -e "$_bk" ] && _bk="$_bk-$$"
    cp -p "$1" "$_bk" || die "could not back up $1 to $_bk; nothing in it was changed"
    chmod go-rwx "$_bk" 2>/dev/null || true
    info "backup: $_bk"
    BACKED_UP="$BACKED_UP|$1|"
}

# The dry-run half of backup_file.
note_backup() {
    if [ -e "$1" ]; then
        info "would back up $1 to $1.metiche-backup-$TS first"
    fi
    return 0
}

# Puts the bytes of $2 into $1. An existing file is backed up, then written IN
# PLACE, which keeps its inode, owner, mode and any symlink pointing at it. A
# new one is created mode 0600.
commit_file() {
    if [ -e "$1" ] || [ -L "$1" ]; then
        backup_file "$1"
        cat "$2" > "$1"
    else
        mkdir -p "$(dirname "$1")"
        (umask 077; cat "$2" > "$1")
    fi
    DID_SOMETHING=1
}

# -------------------------------------------------------------- identity ---
#
# Three ways to reach a team, and the script never guesses between them:
#
#   join    you were given a join code. The first client redeems it; the
#           others attach to the team by slug.
#   create  nobody has made the team yet. The first client creates it; the
#           same call mints the team's first join code, which you hand to
#           teammates, and the others attach by slug.
#   token   you already have a token (METICHE_TOKEN, or ~/.metiche/env from an
#           earlier run). Every client attaches by slug.
#
# In all three, every client ends up with its OWN token. A token names one
# agent, so two clients sharing one would be one agent on the board.

# [ -r /dev/tty ] is not the question. On macOS the node exists and is
# readable for a process with no controlling terminal, and the open then fails
# with "Device not configured" — a raw shell error instead of this script's own
# advice. Actually opening it is the only honest test.
#
# In a SUBSHELL: `:` is a special built-in, and a failed redirection on a
# special built-in makes a non-interactive POSIX shell (dash) exit on the spot,
# silently, instead of returning false.
have_tty() {
    (: < /dev/tty) 2>/dev/null
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

# A token goes, literally, into JSON and TOML documents and one header, so its
# shape is checked before it is used anywhere — a supplied one, one read back
# from a config, and one the server just returned alike.
token_shape_ok() {
    case "$1" in
        ""|*[!A-Za-z0-9._-]*) return 1 ;;
    esac
    [ "${#1}" -ge 16 ]
}

validate_token() {
    [ -n "$1" ] || die "empty token"
    case "$1" in
        *[!A-Za-z0-9._-]*) die "token contains unexpected characters (expected letters, digits, . _ -)" ;;
    esac
    if [ "${#1}" -lt 16 ]; then
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

validate_slug() {
    case "$1" in
        ""|*[!A-Za-z0-9._-]*) die "unexpected team slug: \"$1\"" ;;
    esac
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

# The <machine> half of every client_key. Stable across re-runs, needs no state
# on disk, and already means something to a human reading the board.
machine_id() {
    _host="${METICHE_MACHINE_ID:-}"
    if [ -z "$_host" ] && command -v hostname >/dev/null 2>&1; then
        _host=$(hostname 2>/dev/null || true)
    fi
    [ -n "$_host" ] || _host="${HOSTNAME:-}"
    [ -n "$_host" ] || _host="unknown-host"
    _host=${_host%%.*}
    printf '%s' "$_host" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9-' '-'
}

# agent_label and client_key are per (MACHINE, CLIENT), and generated.
#
# client_key is what makes a re-join idempotent: the server keys the agent row
# — and now its token — on (account, client_key). Per machine was the old
# default, and it made Claude Code and Codex on one laptop the SAME agent. Per
# run would put a new agent on the board every time anyone re-ran this script.
# <machine>-<client> is neither, and no human has to type it or remember it.
client_identity() {
    CLIENT="$1"
    _mid=$(machine_id)
    CLIENT_KEY="$_mid-$1"
    AGENT_LABEL="$1 on $_mid"
    CLIENT_KIND="$1"
    if only_one_client; then
        if [ -n "${METICHE_CLIENT_KEY:-}" ]; then CLIENT_KEY="$METICHE_CLIENT_KEY"; fi
        if [ -n "${METICHE_AGENT_LABEL:-}" ]; then AGENT_LABEL="$METICHE_AGENT_LABEL"; fi
    fi
    validate_name "agent label" "$AGENT_LABEL"
    validate_name "client key" "$CLIENT_KEY"
}

client_title() {
    case "$1" in
        claude)   printf 'Claude Code' ;;
        cursor)   printf 'Cursor' ;;
        windsurf) printf 'Windsurf' ;;
        codex)    printf 'Codex' ;;
    esac
}

# Where each client keeps the entry this script writes.
client_config() {
    case "$1" in
        claude)   printf '%s' "${CLAUDE_CONFIG_DIR:-$HOME}/.claude.json" ;;
        cursor)   printf '%s' "$HOME/.cursor/mcp.json" ;;
        windsurf) printf '%s' "$HOME/.codeium/windsurf/mcp_config.json" ;;
        codex)    printf '%s' "$CODEX_DIR/config.toml" ;;
    esac
}

# Per-client token storage without arrays and without eval: one variable per
# known client, selected by a case.
tok_set() {
    case "$1" in
        claude)   TOK_CLAUDE="$2" ;;
        cursor)   TOK_CURSOR="$2" ;;
        windsurf) TOK_WINDSURF="$2" ;;
        codex)    TOK_CODEX="$2" ;;
    esac
}

tok_get() {
    case "$1" in
        claude)   printf '%s' "$TOK_CLAUDE" ;;
        cursor)   printf '%s' "$TOK_CURSOR" ;;
        windsurf) printf '%s' "$TOK_WINDSURF" ;;
        codex)    printf '%s' "$TOK_CODEX" ;;
    esac
}

# create_team wants an idempotency_key of at least 8 characters and derives the
# team's primary key from it, so the same key means "the same team" rather than
# "a second team with the same name". The seed is the team name and the
# MACHINE — not a client_key, which is now one of several on this machine.
idempotency_key() {
    _seed="metiche-install|$1|$(machine_id)"
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

# The METICHE_TOKEN= value in an env file ($1), if it has one of token shape.
# Read with sed: the file is never sourced or eval'd. Never printed.
env_file_token() {
    [ -f "$1" ] || return 0
    _prior=$(sed -n "s/^$TOKEN_ENV=//p" "$1" | sed -n '1p')
    case "$_prior" in
        ""|*[!A-Za-z0-9._-]*) return 0 ;;
    esac
    printf '%s' "$_prior"
}

# A token already sitting in ~/.metiche/env. Read only; never printed.
existing_token() {
    env_file_token "$ENV_FILE"
}

# True when $1 is the token in a backup of ~/.metiche/env: one backup_file made
# (~/.metiche/env.metiche-backup-<TS>) or one --uninstall made
# (~/.metiche.metiche-backup-<TS>/env). Compared here, never printed.
token_in_env_backups() {
    for _bk in "$ENV_FILE".metiche-backup-* "$ENV_DIR".metiche-backup-*/env; do
        [ -f "$_bk" ] || continue
        if [ "$(env_file_token "$_bk")" = "$1" ]; then
            return 0
        fi
    done
    return 1
}

# The ANCHOR is a token of yours that a join carries so the server attaches a
# client to the SAME person. Without one, the server mints a new account for a
# join with no bearer — so a re-run, or a second client, would add a second
# person to the board however stable client_key is.
#
# $METICHE_TOKEN first, else ~/.metiche/env. Sets ANCHOR, ANCHOR_SOURCE,
# TOKEN_EXPLICIT and TOKEN_FROM_PROFILE; touches no network. A client's own
# token, when it has one, is carried in preference to the anchor — see
# join_client.
#
# $METICHE_TOKEN is not always something you passed. The profile line this
# script writes loads ~/.metiche/env into every new shell, so on a re-run the
# variable usually holds the SAVED token. When it is byte for byte the token in
# that file (read the way existing_token reads it: never sourced, never
# eval'd), it is the saved token and is treated exactly as the file would be —
# which is what lets a saved token the server has since rejected start a new
# identity instead of refusing forever.
#
# Nor is a token that WAS the saved token. An install that starts a new
# identity rewrites ~/.metiche/env and keeps the old file as a backup, but the
# shell that ran it still exports the old value until a new terminal opens. A
# $METICHE_TOKEN that matches a backup is that stale shell: with a saved token
# the saved token is the anchor, and without one (after --uninstall) the stale
# value is an anchor candidate exactly as the saved token would be.
#
# A token that matches neither was passed on purpose, and a rejection of it
# still refuses.
anchor_token() {
    ANCHOR=""
    ANCHOR_SOURCE=""
    TOKEN_EXPLICIT=0
    TOKEN_FROM_PROFILE=0
    TOKEN_STALE_SHELL=0
    _found=$(existing_token)
    if [ -n "${METICHE_TOKEN:-}" ]; then
        if [ -n "$_found" ] && [ "$METICHE_TOKEN" = "$_found" ]; then
            TOKEN_FROM_PROFILE=1
        elif token_in_env_backups "$METICHE_TOKEN"; then
            TOKEN_STALE_SHELL=1
            if [ -z "$_found" ]; then
                ANCHOR="$METICHE_TOKEN"
                ANCHOR_SOURCE="your shell's \$METICHE_TOKEN (it matches a backup of $ENV_FILE)"
                return 0
            fi
        else
            validate_token "$METICHE_TOKEN"
            TOKEN_EXPLICIT=1
            ANCHOR="$METICHE_TOKEN"
            ANCHOR_SOURCE="\$METICHE_TOKEN"
            return 0
        fi
    fi
    if [ -n "$_found" ]; then
        ANCHOR="$_found"
        ANCHOR_SOURCE="$ENV_FILE"
    fi
    return 0
}

prompt_join_or_create() {
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

choose_identity() {
    anchor_token

    # Only a token you PASSED outranks a join code or a team name. The saved
    # token, loaded into $METICHE_TOKEN by the profile line, does not: it is
    # carried as the anchor like any saved token, and the join or create you
    # asked for still happens.
    if [ "$TOKEN_EXPLICIT" -eq 1 ]; then
        ACTION="token"
        if [ -n "${METICHE_JOIN_CODE:-}" ] || [ -n "$TEAM_NAME" ]; then
            warn "\$METICHE_TOKEN is set, so METICHE_JOIN_CODE / METICHE_TEAM_NAME are ignored"
        fi
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
    # A re-run. The token from the last run is enough to find the team again,
    # so nothing needs typing — which is what makes "re-run the installer"
    # an instruction anyone can follow.
    if [ -n "$ANCHOR" ]; then
        ACTION="token"
        return 0
    fi
    if [ -n "${METICHE_TEAM_SLUG:-}" ]; then
        die "METICHE_TEAM_SLUG names a team, but there is no token to attach with.
Set METICHE_TOKEN, or use METICHE_JOIN_CODE."
    fi
    prompt_join_or_create
}

# ------------------------------------------------------------ mcp over http ---
#
# Enough of a streamable-HTTP MCP client to make a few calls. Three POSTs to
# the same URL: initialize (whose RESPONSE HEADER carries the session id), the
# initialized notification, then tools/call. Replies come back either as one
# JSON object or as an SSE frame whose payload sits on a "data: " line, and
# both are handled because which one you get depends on the server's content
# negotiation, not on anything this script controls.

# jq is REQUIRED for the joins, and there is deliberately no fallback.
#
# The answer is a JSON document nested inside a JSON string inside a JSON-RPC
# envelope, and it holds a bearer token next to a join code and an account
# key. A regex that picks the wrong one of those three does not fail: it
# writes the wrong secret into a client config, and you find out at the first
# 401 — or you do not find out at all, because you just published your team's
# invite as a bearer. A refusal with a way out is better than a parser that is
# quietly wrong.
require_join_tools() {
    command -v curl >/dev/null 2>&1 ||
        die "curl is required to reach $URL. Install curl and re-run."
    command -v jq >/dev/null 2>&1 ||
        die "jq is required for the joins. Install jq and re-run."
}

# Why the failure reason goes in a FILE and not just a variable: mcp_call has
# to run inside a command substitution to capture what the tool returned, and
# a subshell cannot write its parent's variables. Parking it on disk (inside
# the 0700 scratch directory, and never the credential itself) is what lets
# the caller print a real reason instead of an empty string. The HTTP status
# of the last POST is parked the same way, for token_is_dead.
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

mcp_last_http() {
    if [ -n "$MCP_TMP" ] && [ -s "$MCP_TMP/http" ]; then
        cat "$MCP_TMP/http"
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
    printf '000' > "$MCP_TMP/http"
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
    printf '%s' "$MCP_HTTP" > "$MCP_TMP/http"
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

# Proves a token works BEFORE a single byte of it is written anywhere.
#
# Two things make this worth the extra round trip. The connection is FRESH and
# carries the bearer from the first byte, which is the state a configured
# assistant starts in — the connection the token was minted on was
# authenticated differently (or not at all) at initialize, so succeeding there
# proves nothing about succeeding here. And the tool is list_teams, not
# health: health answers without a token at all, so it would return ok for an
# empty bearer and tell us nothing. list_teams is account-scoped, and checking
# the expected slug is in its answer confirms the token is on the team rather
# than merely valid. The answer is kept in LAST_TEAMS.
verify_token() {
    _tok="$1"
    _want="$2"
    LAST_TEAMS=""
    mcp_connect "$_tok" || return 1
    LAST_TEAMS=$(mcp_call "$_tok" "list_teams" '{}') || return 1
    if [ -n "$_want" ]; then
        printf '%s' "$LAST_TEAMS" |
            jq -e --arg s "$_want" 'any(.teams[]?; .slug == $s)' >/dev/null 2>&1 || {
                mcp_fail "the token authenticates, but \"$_want\" is not in the teams it can see"
                return 1
            }
    fi
    return 0
}

# Call right after a request carrying a token failed. Succeeds only when the
# SERVER has said this token is no good — and nothing else could have.
#
# A 401 alone is not enough. The server answers 401 for a token it does not
# know, and ALSO when its database is down, deliberately indistinguishable
# (one of the two is a probe). So a 401 is believed only when an anonymous
# health call, on a fresh connection, reports the server up and its database
# reachable. Anything else — no answer, a 5xx, a timeout, a sick database —
# is a failure to check, and a failure to check must never be what turns
# someone into a new identity.
token_is_dead() {
    [ "$(mcp_last_http)" = "401" ] || return 1
    mcp_connect "" || return 1
    _health=$(mcp_call "" "health" '{}') || return 1
    [ "$(printf '%s' "$_health" | jq -r '(.ok == true) and (.database == "reachable")' 2>/dev/null)" = "true" ]
}

dead_token_note() {
    say "    your saved metiche token is no longer valid — the server may have been reset or the token revoked; starting a new identity (token from $1)"
}

# ------------------------------------------------- what a client has already ---
#
# Every reader below is READ ONLY and prints a token only into the command
# substitution that captures it, never to the terminal.

# The url of an entry in an mcpServers document. $1 file, $2 server name.
json_server_url() {
    { [ -f "$1" ] && command -v jq >/dev/null 2>&1; } || return 0
    jq -r --arg n "$2" \
        '.mcpServers[$n].url? // empty | select(type == "string")' "$1" 2>/dev/null || true
}

# The header NAMES on an entry, comma-joined; values never leave jq.
json_server_header_names() {
    { [ -f "$1" ] && command -v jq >/dev/null 2>&1; } || return 0
    jq -r --arg n "$2" \
        '.mcpServers[$n].headers? // {} | keys | join(",")' "$1" 2>/dev/null || true
}

json_server_bearer() {
    { [ -f "$1" ] && command -v jq >/dev/null 2>&1; } || return 0
    jq -r --arg n "$2" \
        '.mcpServers[$n].headers.Authorization? // empty | select(type == "string") | ltrimstr("Bearer ")' \
        "$1" 2>/dev/null || true
}

json_has_server() {
    { [ -f "$1" ] && command -v jq >/dev/null 2>&1; } || return 1
    jq -e --arg n "$2" '(.mcpServers? // {}) | type == "object" and has($n)' "$1" >/dev/null 2>&1
}

# A one-line description of an entry for --dry-run and the log: url and the
# header NAMES. A header value — a token — is never shown.
json_entry_state() {
    if [ ! -f "$1" ]; then printf 'absent (no such file)'; return 0; fi
    if ! command -v jq >/dev/null 2>&1; then printf 'unknown (jq is not installed)'; return 0; fi
    if ! jq -e . "$1" >/dev/null 2>&1; then printf 'unreadable (not valid JSON)'; return 0; fi
    if ! json_has_server "$1" "$2"; then printf 'absent'; return 0; fi
    jq -r --arg n "$2" '.mcpServers[$n] |
        "url=\(.url // "(none)"), headers: \((.headers // {}) | keys | if length == 0 then "none" else join(", ") end)" +
        (if (.headers.Authorization? // "" | test("\\$\\{")) then " (Authorization expands an environment variable)" else "" end) +
        " (values not shown)"' "$1" 2>/dev/null || printf 'present'
}

# TOML is read line by line, and only headers are interpreted: a line that is
# exactly `[name]` or `[[name]]`, optionally followed by a comment. A header's
# name is normalised by dropping quotes and the spaces around dots, so
# [mcp_servers."metiche"] is recognised too. A table runs from its header to
# the next header.
TOML_AWK_LIB='
function tname(l,   t) {
    t = l
    sub(/^[ \t]*\[+[ \t]*/, "", t)
    sub(/[ \t]*\]+[ \t]*(#.*)?$/, "", t)
    gsub(/"/, "", t)
    gsub(/[ \t]*\.[ \t]*/, ".", t)
    return t
}
function is_header(l) { return l ~ /^[ \t]*\[\[?[^][]+\]\]?[ \t]*(#.*)?$/ }
'

# The non-blank lines of our [mcp_servers.metiche] table, trimmed. Read with
# awk rather than `codex mcp get`: the codex CLI writes into CODEX_HOME even
# to read, and --dry-run promises to write nothing.
codex_table() {
    _cfg=$(client_config codex)
    [ -f "$_cfg" ] || return 0
    awk -v want="mcp_servers.$SERVER_NAME" "$TOML_AWK_LIB"'
        is_header($0) { inside = (tname($0) == want); next }
        inside { line = $0; sub(/^[ \t]+/, "", line); sub(/[ \t]+$/, "", line); if (line != "") print line }
    ' "$_cfg" 2>/dev/null || true
}

# Our table as it is now, for the log, with every bearer value redacted.
codex_table_state() {
    _state=$(codex_table | sed 's/Bearer [^"]*/Bearer <redacted>/g' | awk '{ printf "%s%s", (NR > 1 ? " | " : ""), $0 }')
    if [ -n "$_state" ]; then printf '%s' "$_state"; else printf 'absent'; fi
}

# Prints the Codex config with every [mcp_servers.metiche] table — and its
# dotted sub-tables — taken out, and every other byte as it was.
#
# NOT `codex mcp remove`. Verified against codex-cli 0.154.0: it re-serialises
# the tables it did not remove (a multi-line args array came back on one
# line), and those are the user's settings, not ours.
#
# Blank lines directly above a removed table are dropped only when nothing
# follows it, so removing a table this script appended gives back the file as
# it was before. A missing final newline stays missing.
codex_strip() {
    _nl=0
    if file_ends_with_newline "$1"; then _nl=1; fi
    awk -v want="mcp_servers.$SERVER_NAME" -v nl="$_nl" "$TOML_AWK_LIB"'
        function emit(s) { if (n++) printf "\n"; printf "%s", s }
        {
            if (is_header($0)) {
                t = tname($0)
                skip = (t == want || index(t, want ".") == 1)
                if (skip) cut = 1
            }
            if (skip) next
            if ($0 ~ /^[ \t]*$/) { buf[nb++] = $0; next }
            for (i = 0; i < nb; i++) emit(buf[i])
            nb = 0
            cut = 0
            emit($0)
        }
        END {
            if (!cut) for (i = 0; i < nb; i++) emit(buf[i])
            if (n && nl) printf "\n"
        }
    ' "$1"
}

# Exactly what install_codex writes into the table, minus its header.
codex_desired_table() {
    printf 'url = "%s"\n' "$URL"
    printf 'http_headers = { Authorization = "Bearer %s" }\n' "$1"
}

# The literal bearer this client is configured with right now, if it has one
# and it looks like a token. An ${ENV} placeholder, a join code, or garbage all
# read as "none".
client_current_token() {
    case "$1" in
        claude|cursor|windsurf)
            _cur=$(json_server_bearer "$(client_config "$1")" "$SERVER_NAME")
            ;;
        codex)
            _cur=$(codex_table |
                sed -n 's/^http_headers[[:space:]]*=.*"\{0,1\}Authorization"\{0,1\}[[:space:]]*=[[:space:]]*"Bearer \([^"]*\)".*/\1/p' |
                sed -n '1p')
            ;;
        *) _cur="" ;;
    esac
    if token_shape_ok "$_cur"; then printf '%s' "$_cur"; fi
    return 0
}

# ------------------------------------------------------------- detection ---

client_present() {
    case "$1" in
        claude)   command -v claude >/dev/null 2>&1 ;;
        cursor)   [ -d "$HOME/.cursor" ] || command -v cursor >/dev/null 2>&1 ;;
        windsurf) [ -d "$HOME/.codeium" ] || command -v windsurf >/dev/null 2>&1 ;;
        # The codex CLI is what reads the entry back, so Codex without it is
        # a client this script cannot prove it configured.
        codex)    command -v codex >/dev/null 2>&1 ;;
        *)        return 1 ;;
    esac
}

# Every client that is wanted AND present, in KNOWN_CLIENTS order. Detected
# before any network call, because a join is per client: with no client there
# is nothing to join as.
detect_clients() {
    step "Assistants"
    CLIENTS=""
    for _dc in $KNOWN_CLIENTS; do
        want "$_dc" || continue
        if client_present "$_dc"; then
            CLIENTS="${CLIENTS:+$CLIENTS }$_dc"
            info "found: $(client_title "$_dc")"
        elif [ "$_dc" = "codex" ] && [ -d "$CODEX_DIR" ]; then
            info "skipping Codex: $CODEX_DIR exists but codex is not on PATH,"
            info "and the codex CLI is what proves the entry was written. Install it and re-run."
        fi
    done
    if [ -z "$CLIENTS" ]; then
        say ""
        warn "no supported assistant detected on this machine (claude, cursor, windsurf, codex)"
        die "nothing to join as: metiche makes one agent per assistant. Nothing was called or written."
    fi
    FIRST_CLIENT=${CLIENTS%% *}

    if { [ -n "${METICHE_CLIENT_KEY:-}" ] || [ -n "${METICHE_AGENT_LABEL:-}" ]; } && ! only_one_client; then
        warn "METICHE_CLIENT_KEY / METICHE_AGENT_LABEL are honoured only with --only <one client>;"
        warn "ignoring them — one value for several clients would make them one agent"
    fi
}

# --------------------------------------------------------------- the team ---

retry_advice() {
    say ""
    if [ -n "$CONFIGURED" ]; then
        info "Configured on this run, each with its own verified token:$CONFIGURED"
        info "Nothing was written for the client that failed. Re-running is safe: every"
        info "configured client carries its own token, and the server keeps it."
    else
        info "Nothing was written: no $ENV_FILE, no client config, no change at all."
    fi
    info "Check the endpoint and try again:"
    say ""
    say "      sh install.sh --url $URL --dry-run    # see what it would do"
    say ""
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

# Decides WHICH team, before any client joins: checks the anchor, and on the
# token path picks the slug; on create, refuses a duplicate team name. The
# join code itself is redeemed by the first client, in join_client.
resolve_team() {
    step "Team"
    info "one join per client, in this order: $CLIENTS"
    _rest_clients=${CLIENTS#"$FIRST_CLIENT"}
    _rest_clients=${_rest_clients# }
    if [ -n "$ANCHOR" ]; then
        info "anchor: the token from $ANCHOR_SOURCE (never printed)"
        if [ "$TOKEN_FROM_PROFILE" -eq 1 ]; then
            info "  \$METICHE_TOKEN is set to that same saved token (your shell profile loads $ENV_FILE), so it counts as the saved token, not one you passed"
        fi
        if [ "$TOKEN_STALE_SHELL" -eq 1 ] && [ "$ANCHOR_SOURCE" = "$ENV_FILE" ]; then
            warn "your shell still has the METICHE_TOKEN from before this machine's last install"
            warn "(it matches a backup of $ENV_FILE); using the current saved token."
            warn "Open a new terminal, or run: . $ENV_FILE"
        elif [ "$TOKEN_STALE_SHELL" -eq 1 ]; then
            info "  it is not a token you passed: there is no saved token now, so if the server rejects it a new identity is started"
        fi
    else
        info "anchor: none yet — the first client's token becomes it"
    fi

    if [ "$DRY_RUN" -eq 1 ]; then
        if [ -n "$ANCHOR" ]; then
            info "would check the anchor with list_teams on a fresh connection:"
            info "  rejected (401, with the server's health ok) -> a new identity is started, and said so"
            info "  no answer, a 5xx or a timeout              -> refuse, and write nothing"
        fi
        case "$ACTION" in
            create)
                info "would check list_teams and refuse if you already have a team named \"$TEAM_NAME\""
                info "would create_team once as $FIRST_CLIENT${_rest_clients:+, then join by slug for $_rest_clients}"
                ;;
            join)
                info "would redeem the join code once as $FIRST_CLIENT${_rest_clients:+, then join by slug for $_rest_clients}"
                ;;
            token)
                if [ -n "${METICHE_TEAM_SLUG:-}" ]; then
                    info "would attach every client to team \"$METICHE_TEAM_SLUG\" by slug, if list_teams shows it"
                else
                    info "would pick the team from list_teams (your only team; with several, METICHE_TEAM_SLUG)"
                fi
                info "would join by slug for $CLIENTS"
                ;;
        esac
        return 0
    fi

    require_join_tools

    if [ -n "$ANCHOR" ]; then
        info "checking the anchor with list_teams on a fresh connection"
        if verify_token "$ANCHOR" ""; then
            ANCHOR_TEAMS="$LAST_TEAMS"
            info "verified: the anchor authenticates"
        else
            _why=$(mcp_error)
            if [ "$ANCHOR_SOURCE" != "\$METICHE_TOKEN" ] && token_is_dead; then
                dead_token_note "$ANCHOR_SOURCE"
                ANCHOR=""
                ANCHOR_SOURCE=""
                if [ "$ACTION" = "token" ]; then
                    # The saved token was the only thing to go on. A new
                    # identity needs a team to join or create.
                    if have_tty; then
                        prompt_join_or_create
                    else
                        die "nothing was written. To start the new identity, re-run with one of:
  METICHE_JOIN_CODE=<code>       join a team somebody already created
  METICHE_TEAM_NAME=<name>       create a new team"
                    fi
                fi
            else
                say ""
                if [ "$ANCHOR_SOURCE" = "\$METICHE_TOKEN" ] && [ "$(mcp_last_http)" = "401" ]; then
                    warn "the token in \$METICHE_TOKEN was rejected by $URL: $_why"
                    retry_advice
                    die "refusing to configure anything with a token that does not authenticate"
                fi
                warn "could not check the token from $ANCHOR_SOURCE against $URL: $_why"
                warn "a failure to check is not a rejected token: refusing rather than starting a new identity"
                retry_advice
                die "the server could not be checked; nothing was written"
            fi
        fi
    fi

    case "$ACTION" in
        token)
            _count=$(printf '%s' "$ANCHOR_TEAMS" | jq '[.teams[]?] | length' 2>/dev/null || true)
            [ -n "$_count" ] || _count=0
            if [ -n "${METICHE_TEAM_SLUG:-}" ]; then
                validate_slug "$METICHE_TEAM_SLUG"
                printf '%s' "$ANCHOR_TEAMS" |
                    jq -e --arg s "$METICHE_TEAM_SLUG" 'any(.teams[]?; .slug == $s)' >/dev/null 2>&1 ||
                    die "METICHE_TEAM_SLUG=$METICHE_TEAM_SLUG is not among the teams your token can see.
Nothing was written. To join a team you are not on yet, use METICHE_JOIN_CODE."
                TEAM_SLUG="$METICHE_TEAM_SLUG"
            elif [ "$_count" = "1" ]; then
                TEAM_SLUG=$(printf '%s' "$ANCHOR_TEAMS" | jq -r '.teams[0].slug // empty')
                validate_slug "$TEAM_SLUG"
            elif [ "$_count" = "0" ]; then
                die "your token is not on any team yet. Nothing was written.
Join one with METICHE_JOIN_CODE, or create one with METICHE_TEAM_NAME."
            else
                _slugs=$(printf '%s' "$ANCHOR_TEAMS" | jq -r '[.teams[]?.slug] | join(", ")')
                die "your token is on $_count teams ($_slugs), and this script will not guess.
Nothing was written. Re-run with METICHE_TEAM_SLUG=<one of them>."
            fi
            info "attaching every client to team \"$TEAM_SLUG\" by slug"
            ;;
        create)
            # The check runs as the person the create will carry: the anchor,
            # else the first client's own token. With neither, the create mints
            # a new person, who by definition has no teams.
            _teams="$ANCHOR_TEAMS"
            if [ -z "$ANCHOR" ]; then
                _own=$(client_current_token "$FIRST_CLIENT")
                if [ -n "$_own" ]; then
                    if verify_token "$_own" ""; then
                        _teams="$LAST_TEAMS"
                    else
                        _why=$(mcp_error)
                        if ! token_is_dead; then
                            say ""
                            warn "could not check the token in $(client_config "$FIRST_CLIENT") against $URL: $_why"
                            retry_advice
                            die "the server could not be checked; nothing was written"
                        fi
                    fi
                fi
            fi
            if [ -n "$_teams" ]; then
                _dup=$(printf '%s' "$_teams" | jq -r --arg n "$TEAM_NAME" \
                    '[.teams[]? | select((.name // "" | ascii_downcase) == ($n | ascii_downcase)) | .slug] | .[0] // empty' 2>/dev/null || true)
                if [ -n "$_dup" ]; then
                    die "you already have a team named \"$TEAM_NAME\" (slug: $_dup); refusing to create a second.
Nothing was written. To attach this machine's clients to it, re-run with:

      METICHE_TEAM_SLUG=$_dup sh install.sh"
                fi
            fi
            info "no team named \"$TEAM_NAME\" on this account: create_team runs once, as $FIRST_CLIENT"
            ;;
        join)
            info "the join code is redeemed once, as $FIRST_CLIENT${_rest_clients:+; then $_rest_clients attach by slug}"
            ;;
    esac
    return 0
}

# ------------------------------------------------------------- per client ---

client_fail() {
    say ""
    warn "$(client_title "$CLIENT"): $1"
    if [ "${2:-}" = "mcp" ]; then
        warn "$(mcp_error)"
    fi
    retry_advice
    die "$(client_title "$CLIENT") is not configured"
}

# One client's join: carry its own token if it has one that still works (the
# server keeps it), else the anchor (the server mints one for THIS client_key
# only), else nothing (only the very first join of a fresh machine: the server
# mints a new person). The join code is sent at most once per run, by the
# first client; every later call names the team by slug.
join_client() {
    _jc="$1"
    _jcfg=$(client_config "$_jc")
    _jown=$(client_current_token "$_jc")
    _jbearer=""

    if [ -n "$_jown" ]; then
        if mcp_connect "$_jown"; then
            _jbearer="$_jown"
            info "carrying its own token from $_jcfg (the server keeps it)"
        else
            _jwhy=$(mcp_error)
            if token_is_dead; then
                dead_token_note "$_jcfg"
            else
                client_fail "could not check the token in $_jcfg against $URL: $_jwhy — a failure to check is not a rejected token, so no new identity was started"
            fi
        fi
    fi
    if [ -z "$_jbearer" ] && [ -n "$ANCHOR" ]; then
        mcp_connect "$ANCHOR" || client_fail "could not reach $URL with the anchor" mcp
        _jbearer="$ANCHOR"
        info "carrying the anchor from $ANCHOR_SOURCE (the server mints a token for this client)"
    fi
    if [ -z "$_jbearer" ]; then
        [ -z "$TEAM_SLUG" ] || client_fail "no token to carry on a join by slug (this is a bug in install.sh)"
        mcp_connect "" || client_fail "could not reach the metiche endpoint at $URL" mcp
        info "carrying no token (the server mints a new identity for you)"
    fi

    if [ -z "$TEAM_SLUG" ] && [ "$ACTION" = "create" ]; then
        _jtool="create_team"
        _jargs=$(printf '{"team_name":"%s","member_name":"%s","agent_label":"%s","client_key":"%s","client_kind":"%s","idempotency_key":"%s"}' \
            "$TEAM_NAME" "$MEMBER_NAME" "$AGENT_LABEL" "$CLIENT_KEY" "$CLIENT_KIND" \
            "$(idempotency_key "$TEAM_NAME")")
        _jwhat="create the team \"$TEAM_NAME\""
    elif [ -z "$TEAM_SLUG" ]; then
        _jtool="join_team"
        _jargs=$(printf '{"join_code":"%s","member_name":"%s","agent_label":"%s","client_key":"%s","client_kind":"%s"}' \
            "$JOIN_CODE" "$MEMBER_NAME" "$AGENT_LABEL" "$CLIENT_KEY" "$CLIENT_KIND")
        _jwhat="redeem your join code (read from $CODE_SOURCE, never printed)"
    else
        _jtool="join_team"
        _jargs=$(printf '{"team_slug":"%s","member_name":"%s","agent_label":"%s","client_key":"%s","client_kind":"%s"}' \
            "$TEAM_SLUG" "$MEMBER_NAME" "$AGENT_LABEL" "$CLIENT_KEY" "$CLIENT_KIND")
        _jwhat="attach to team \"$TEAM_SLUG\" by slug"
    fi

    plan "call $_jtool at $URL to $_jwhat" || return 0
    _jresult=$(mcp_call "$_jbearer" "$_jtool" "$_jargs") ||
        client_fail "$_jtool was refused by $URL" mcp

    _jtok=$(printf '%s' "$_jresult" | jq -r '.token // empty')
    _jkept=$(printf '%s' "$_jresult" | jq -r '.token_kept // false')
    _jslug=$(printf '%s' "$_jresult" | jq -r '.team_slug // empty')
    _jscope=$(printf '%s' "$_jresult" | jq -r '.token_scope // empty')
    if [ -z "$_jtok" ]; then
        # A keep is something the server SAYS, not something an empty field
        # implies: an empty token without token_kept is a server we do not
        # understand, and configuring a client with a guess is how two
        # clients end up one agent.
        [ "$_jkept" = "true" ] ||
            client_fail "$_jtool returned no token and did not say token_kept; refusing to guess which token this client should use"
        [ -n "$_jbearer" ] ||
            client_fail "$_jtool says it kept a token, but this call carried none"
        _jtok="$_jbearer"
        info "the server kept the token this client carried (token_kept); none was minted"
    else
        info "the server minted a token for this client (never printed)"
    fi
    token_shape_ok "$_jtok" ||
        client_fail "the server returned a token of an unexpected shape; refusing to write it"
    if [ "$_jscope" != "agent" ]; then
        warn "$URL did not answer token_scope \"agent\": it predates per-agent tokens, so"
        warn "this client may share an identity with your other clients"
    fi

    if [ -z "$TEAM_SLUG" ]; then
        [ -n "$_jslug" ] || client_fail "$_jtool succeeded but named no team"
        validate_slug "$_jslug"
        TEAM_SLUG="$_jslug"
    elif [ -n "$_jslug" ] && [ "$_jslug" != "$TEAM_SLUG" ]; then
        client_fail "$_jtool answered for team \"$_jslug\", not \"$TEAM_SLUG\"; refusing to write it"
    fi
    info "$_jtool succeeded: on team \"$TEAM_SLUG\" as $CLIENT_KEY"

    if [ "$_jtool" = "create_team" ]; then
        NEW_JOIN_CODE=$(printf '%s' "$_jresult" | jq -r '.join_code // empty')
        # create_team is retry-safe: the same idempotency_key lands on the
        # team that exists rather than making a second one. Say which
        # happened, so a re-run does not read as "I just made another team".
        CREATED=$(printf '%s' "$_jresult" | jq -r '.created // false')
    fi

    info "verifying this client's token on a fresh connection that carries it from the first byte"
    verify_token "$_jtok" "$TEAM_SLUG" ||
        client_fail "the join SUCCEEDED, but the token it returned did not work; refusing to write it" mcp
    info "verified: list_teams answered and \"$TEAM_SLUG\" is in it"

    tok_set "$_jc" "$_jtok"
    if [ -z "$ANCHOR" ]; then
        ANCHOR="$_jtok"
        ANCHOR_SOURCE="$(client_title "$_jc")'s agent token"
    fi

    # Printed only now, and only on creation: a join code the user cannot see
    # is a team nobody else can be invited to.
    if [ -n "$NEW_JOIN_CODE" ]; then
        announce_join_code
        NEW_JOIN_CODE=""
    fi
    return 0
}

# The dry-run half of join_client: the same decisions, announced, with no
# network at all.
dry_run_join() {
    _dc="$1"
    _dcfg=$(client_config "$_dc")
    _down=$(client_current_token "$_dc")
    if [ -n "$_down" ]; then
        info "would carry: its own token, read from $_dcfg (kept if the server still accepts it;"
        info "  if the server rejects it, a new token — and says so; if the server cannot be checked, refuse)"
    elif [ -n "$ANCHOR" ]; then
        info "would carry: the anchor from $ANCHOR_SOURCE (the server mints a token for this client only)"
    elif [ "$DRY_JOINED" -eq 1 ]; then
        info "would carry: the anchor, i.e. the first client's token (the server mints a token for this client only)"
    else
        info "would carry: no token (the server mints a new identity; this client's token becomes the anchor)"
    fi
    if [ "$DRY_JOINED" -eq 0 ] && [ "$ACTION" = "create" ]; then
        plan "call create_team at $URL to create \"$TEAM_NAME\", as this client" || true
    elif [ "$DRY_JOINED" -eq 0 ] && [ "$ACTION" = "join" ]; then
        plan "call join_team at $URL to redeem your join code (read from $CODE_SOURCE, never printed)" || true
    else
        plan "call join_team at $URL with team_slug=${METICHE_TEAM_SLUG:-<the team>} and no join code" || true
    fi
    info "would verify this client's token on a fresh connection: list_teams must contain the team"
    DRY_JOINED=1
}

configure_client() {
    _cc="$1"
    step "$(client_title "$_cc")"
    client_identity "$_cc"
    info "identity: client_key=$CLIENT_KEY  agent_label=\"$AGENT_LABEL\"  client_kind=$CLIENT_KIND"

    if [ "$DRY_RUN" -eq 1 ]; then
        dry_run_join "$_cc"
    else
        join_client "$_cc"
    fi

    # The anchor is written as soon as the first token is proved, before the
    # first client config: a run that dies on a later client must still leave
    # behind the token that lets a re-run attach to the same person.
    if [ "$ENV_DONE" -eq 0 ]; then
        write_env_file
        ENV_DONE=1
    fi

    _ctok=$(tok_get "$_cc")
    case "$_cc" in
        claude)   install_claude "$_ctok" ;;
        cursor)   install_cursor "$_ctok" ;;
        windsurf) install_windsurf "$_ctok" ;;
        codex)    install_codex "$_ctok" ;;
    esac
    if [ "$DRY_RUN" -eq 0 ]; then
        CONFIGURED="$CONFIGURED $(client_title "$_cc") ($CLIENT_KEY)"
    fi
}

# ------------------------------------------------------------- json output ---

# The MCP server entry, shared by every client that speaks the common shape.
# $1 is THIS client's token, verified working a few lines above — never the
# join code, which this endpoint answers with a 401, and never another
# client's token, which would make the two clients one agent. Written
# literally, to the user's own home-directory config, mode 0600.
#
# One header, deliberately. The token names the agent, so there is nothing
# else to say. A second, custom header was tried for "which agent am I" and
# Claude Code dropped it on the floor; Authorization is the one header every
# MCP client forwards.
server_json() {
    cat <<EOF
{
  "type": "http",
  "url": "$URL",
  "headers": {
    "Authorization": "Bearer $1"
  }
}
EOF
}

# Sets mcpServers.metiche in a JSON config, and nothing else in it. $3 is the
# client's token. The entry reaches jq on STANDARD INPUT, not as an argument,
# for the same reason curl gets its bearer that way.
write_mcp_json() {
    target="$1"
    label="$2"

    info "$label: mcpServers.$SERVER_NAME in $target"
    info "  now:    $(json_entry_state "$target" "$SERVER_NAME")"

    if [ -f "$target" ] && ! command -v jq >/dev/null 2>&1; then
        warn "$label: $target already exists and jq is not installed, so it cannot be"
        warn "merged safely. Add this entry to its \"mcpServers\" object by hand:"
        say ""
        say "      \"$SERVER_NAME\": { \"type\": \"http\", \"url\": \"$URL\","
        say "        \"headers\": { \"Authorization\": \"Bearer <$label's own token>\" } }"
        say ""
        return 0
    fi
    if [ -f "$target" ] && ! jq -e . "$target" >/dev/null 2>&1; then
        warn "$label: $target is not valid JSON; leaving it alone."
        return 0
    fi

    mcp_tmpdir
    _jnew=$(new_tmp)
    if [ -f "$target" ]; then
        server_json "$3" | jq --arg n "$SERVER_NAME" --slurpfile cur "$target" \
            '. as $s | $cur[0] | .mcpServers = ((.mcpServers // {}) | .[$n] = $s)' > "$_jnew"
    elif command -v jq >/dev/null 2>&1; then
        server_json "$3" | jq --arg n "$SERVER_NAME" '{mcpServers: {($n): .}}' > "$_jnew"
    else
        printf '{\n  "mcpServers": {\n    "%s": %s\n  }\n}\n' "$SERVER_NAME" "$(server_json "$3")" > "$_jnew"
    fi

    if [ -f "$target" ] && cmp -s "$_jnew" "$target"; then
        info "  already correct, nothing to do"
        return 0
    fi
    info "  becomes: url=$URL, headers: Authorization ($label's own token); every other key unchanged"

    if [ -f "$target" ]; then
        plan "update $target" || { note_backup "$target"; return 0; }
    else
        plan "create $target" || return 0
    fi
    commit_file "$target" "$_jnew"
    chmod 600 "$target" 2>/dev/null || true
}

# Removes one server from a JSON config, and nothing else in it.
remove_json_server() {
    _rf="$1"; _rl="$2"; _rn="$3"
    info "$_rl: mcpServers.$_rn in $_rf"
    info "  now:    $(json_entry_state "$_rf" "$_rn")"
    json_has_server "$_rf" "$_rn" || { info "  nothing to remove"; return 0; }
    mcp_tmpdir
    _rnew=$(new_tmp)
    jq --arg n "$_rn" 'del(.mcpServers[$n])' "$_rf" > "$_rnew" ||
        { warn "could not edit $_rf; leaving it alone"; return 0; }
    info "  becomes: absent; every other key unchanged"
    plan "remove mcpServers.$_rn from $_rf" || { note_backup "$_rf"; return 0; }
    commit_file "$_rf" "$_rnew"
}

# -------------------------------------------------------------- the env file ---

PROFILE_LINE='[ -f "$HOME/.metiche/env" ] && . "$HOME/.metiche/env"'

env_block() {
    printf '%s\n' "$ENV_START managed by install.sh; this block is rewritten on every run"
    printf '%s\n' "# Keep this file private. The anchor: sent on later runs so new clients attach"
    printf '%s\n' "# to the same person; each client's own token is in its own config. A token"
    printf '%s\n' "# the metiche server minted — never the team's join code, which is an invite"
    printf '%s\n' "# and gets a 401 as a bearer."
    printf '%s=%s\n' "$TOKEN_ENV" "$1"
    printf 'export %s\n' "$TOKEN_ENV"
    printf '%s\n' "$ENV_END"
}

# True when every line of ~/.metiche/env is metiche's own: the file starts as
# an installer wrote it (the legacy header, or our block), and holds nothing
# but comments, blank lines and METICHE_* assignments and exports. Then the
# whole file is ours to rewrite. Otherwise only the METICHE_* lines and our
# block are, and everything else in it stays.
env_is_all_metiche() {
    _first=$(sed -n '1p' "$ENV_FILE")
    case "$_first" in
        "$ENV_LEGACY_HEADER"|"$ENV_START"*) ;;
        *) return 1 ;;
    esac
    ! grep -v -E '^[[:space:]]*(#.*)?$|^METICHE_[A-Z0-9_]*=|^export METICHE_[A-Z0-9_]*[[:space:]]*$' "$ENV_FILE" >/dev/null 2>&1
}

# METICHE_JOIN_CODE is deliberately NOT exported: it is an argument to one tool
# call, and keeping it in the environment only invited a client to send it as
# a bearer. METICHE_CLIENT_KEY is not exported any more either — the token
# names the agent now — and converging an old file drops it.
write_env_file() {
    info "anchor: METICHE_* lines in $ENV_FILE"
    if [ -f "$ENV_FILE" ]; then
        _vars=$(sed -n 's/^\(export \)\{0,1\}\(METICHE_[A-Z0-9_]*\).*/\2/p' "$ENV_FILE" | sort -u | tr '\n' ' ')
        info "  now:    ${_vars:-no METICHE_* lines}(values not shown)"
    else
        info "  now:    absent"
    fi
    if [ "$DRY_RUN" -eq 1 ]; then
        if [ -n "$ANCHOR" ]; then
            info "  becomes: $TOKEN_ENV=<the anchor from $ANCHOR_SOURCE, or a new token if the server rejects it>"
        else
            info "  becomes: $TOKEN_ENV=<this client's token: the anchor>"
        fi
        plan "write the metiche block into $ENV_FILE (mode 0600); other lines kept" || note_backup "$ENV_FILE"
        return 0
    fi
    [ -n "$ANCHOR" ] || return 0

    mcp_tmpdir
    _enew=$(new_tmp)
    if [ -f "$ENV_FILE" ] && ! env_is_all_metiche; then
        awk -v s="$ENV_START" -v e="$ENV_END" '
            index($0, s) == 1 { inblock = 1; next }
            inblock { if (index($0, e) == 1) inblock = 0; next }
            /^METICHE_[A-Z0-9_]*=/ { next }
            /^export METICHE_[A-Z0-9_]*[ \t]*$/ { next }
            { print }
        ' "$ENV_FILE" > "$_enew"
        if [ -s "$_enew" ] && [ -n "$(tail -n 1 "$_enew")" ]; then
            printf '\n' >> "$_enew"
        fi
        env_block "$ANCHOR" >> "$_enew"
    else
        env_block "$ANCHOR" > "$_enew"
    fi

    if [ -f "$ENV_FILE" ] && cmp -s "$_enew" "$ENV_FILE"; then
        info "  already correct, nothing to do"
    else
        info "  becomes: $TOKEN_ENV=<the anchor>, and no other METICHE_* line"
        plan "write the metiche block into $ENV_FILE (mode 0600)" && {
            mkdir -p "$ENV_DIR"
            chmod 700 "$ENV_DIR" 2>/dev/null || true
            commit_file "$ENV_FILE" "$_enew"
            chmod 600 "$ENV_FILE" 2>/dev/null || true
        }
    fi
    info "  $TOKEN_ENV is the anchor, from: $ANCHOR_SOURCE (never printed)"
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
    step "Shell profile"
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

    if [ -f "$profile" ] && grep -qF "$PROFILE_LINE" "$profile" 2>/dev/null; then
        info "$profile already loads ~/.metiche/env"
        return 0
    fi

    # Your assistants no longer need this line: each has its own token written
    # into its own config. What still reads $METICHE_TOKEN from the environment
    # is the .mcp.json at the root of the metiche repository (and anything you
    # script against metiche yourself), so the line stays on by default and
    # --no-write-profile is there for anyone who manages their dotfiles.
    if [ "$WRITE_PROFILE" -eq 1 ]; then
        if [ ! -f "$profile" ]; then
            info "$profile does not exist yet; creating it"
        fi
        plan "append the loader line to $profile" || { note_backup "$profile"; return 0; }
        backup_file "$profile"
        printf '\n# metiche - load the anchor token (install.sh re-runs, metiche repo .mcp.json)\n%s\n' \
            "$PROFILE_LINE" >> "$profile"
        DID_SOMETHING=1
    else
        info "--no-write-profile: your assistants do not need it. To expand \${$TOKEN_ENV}"
        info "in the metiche repository's .mcp.json, add this to $profile yourself:"
        say ""
        say "      $PROFILE_LINE"
        say ""
    fi
}

# ------------------------------------------------------------- Claude Code ---

manual_mcp_note() {
    warn "You can register the MCP server by hand with:"
    say ""
    say "      claude mcp add --transport http --scope user $SERVER_NAME $URL \\"
    say "        --header \"Authorization: Bearer <Claude Code's own token>\""
    say ""
    warn "Re-running this script with --only claude mints or keeps that token."
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

# Runs one claude CLI command with its output indented. The exit status is
# claude's, not sed's.
run_claude_cli() {
    info "running: claude $*"
    _out=$(claude "$@" 2>&1) && _rc=0 || _rc=$?
    if [ -n "$_out" ]; then printf '%s\n' "$_out" | sed 's/^/      /'; fi
    return "$_rc"
}

# The plugin is the SKILL only. It used to carry the MCP server too, in a
# .mcp.json that expanded ${METICHE_TOKEN} — one token for every Claude Code
# window, and a custom header Claude Code dropped. Version 0.2.0 ships no
# .mcp.json; `plugin update` is what takes an existing install there, because
# `plugin install` reports an older installed version as already installed.
install_claude_plugin() {
    if ! resolve_marketplace; then
        if [ "$DRY_RUN" -eq 1 ]; then
            # resolve_marketplace declined only because it would have cloned.
            MARKETPLACE="$SRC_DIR/plugin"
        else
            warn "skipping the plugin: the metiche-teamwork skill is not installed."
            warn "(the MCP server is registered regardless, below)"
            return 0
        fi
    fi

    if [ "$DRY_RUN" -eq 1 ]; then
        info "would run: claude plugin marketplace add $MARKETPLACE"
        info "would run: claude plugin marketplace update $MARKETPLACE_NAME"
        info "would run: claude plugin install $PLUGIN_NAME@$MARKETPLACE_NAME --scope user -y"
        info "would run: claude plugin update $PLUGIN_NAME@$MARKETPLACE_NAME --scope user -y"
        return 0
    fi

    run_claude_cli plugin marketplace add "$MARKETPLACE" ||
        warn "marketplace add reported a problem (already added?) — continuing"
    run_claude_cli plugin marketplace update "$MARKETPLACE_NAME" ||
        warn "marketplace update reported a problem — continuing"
    if run_claude_cli plugin install "$PLUGIN_NAME@$MARKETPLACE_NAME" --scope user -y; then
        run_claude_cli plugin update "$PLUGIN_NAME@$MARKETPLACE_NAME" --scope user -y ||
            warn "plugin update reported a problem — continuing"
        info "installed: the metiche-teamwork skill."
        DID_SOMETHING=1
    else
        warn "plugin install failed: the metiche-teamwork skill is not installed."
    fi
}

# True when ~/.claude.json already carries our user-scope entry at $URL with
# exactly one header, Authorization, bearing $1.
claude_entry_matches() {
    _cf=$(client_config claude)
    [ "$(json_server_url "$_cf" "$SERVER_NAME")" = "$URL" ] || return 1
    [ "$(json_server_header_names "$_cf" "$SERVER_NAME")" = "Authorization" ] || return 1
    [ "$(client_current_token claude)" = "$1" ]
}

# Removes one user-scope server through the claude CLI, backing the file up
# first. Only for names metiche owns.
claude_remove_server() {
    _crf=$(client_config claude)
    plan "remove the user-scope server $1 from $_crf (claude mcp remove $1 --scope user)" ||
        { note_backup "$_crf"; return 0; }
    backup_file "$_crf"
    claude mcp remove "$1" --scope user >/dev/null 2>&1 || true
    DID_SOMETHING=1
    if json_has_server "$_crf" "$1"; then
        warn "claude mcp remove $1 ran, but $_crf still lists it; remove it by hand"
    else
        info "removed: $1"
    fi
}

install_claude() {
    _cltok="$1"
    _clcfg=$(client_config claude)
    install_claude_plugin

    info "Claude Code: user-scope servers in $_clcfg"
    info "  now:    $SERVER_NAME — $(json_entry_state "$_clcfg" "$SERVER_NAME")"
    info "          $LEGACY_CLAUDE_SERVER — $(json_entry_state "$_clcfg" "$LEGACY_CLAUDE_SERVER")"

    if json_has_server "$_clcfg" "$LEGACY_CLAUDE_SERVER"; then
        info "  $LEGACY_CLAUDE_SERVER is a legacy hand-added entry with its own token: it becomes absent"
        claude_remove_server "$LEGACY_CLAUDE_SERVER"
    fi

    if [ -n "$_cltok" ] && claude_entry_matches "$_cltok"; then
        info "  $SERVER_NAME already correct, nothing to do"
        return 0
    fi
    info "  becomes: $SERVER_NAME url=$URL, headers: Authorization (Claude Code's own token); every other key unchanged"

    plan "register the metiche server at user scope in $_clcfg, with Claude Code's own token" || {
        note_backup "$_clcfg"
        info "would run: claude mcp remove $SERVER_NAME --scope user"
        info "would run: claude mcp add --transport http --scope user $SERVER_NAME $URL --header \"Authorization: Bearer <Claude Code's token>\""
        info "(neither runs when $_clcfg already carries $URL and that token)"
        return 0
    }

    # The one command line in this script that carries a token. See the note
    # at the top of the file for why; the output is swallowed because it may
    # echo the header back.
    backup_file "$_clcfg"
    claude mcp remove "$SERVER_NAME" --scope user >/dev/null 2>&1 || true
    if ! claude mcp add --transport http --scope user "$SERVER_NAME" "$URL" \
            --header "Authorization: Bearer $_cltok" >/dev/null 2>&1; then
        warn "claude mcp add failed (its output is not shown: it can echo the token)."
        manual_mcp_note
        return 0
    fi
    DID_SOMETHING=1

    if claude_entry_matches "$_cltok"; then
        info "registered, and read back from $_clcfg: $URL with Claude Code's own token"
    else
        warn "claude mcp add succeeded, but $_clcfg does not show the entry this script expected."
        warn "Check it with: claude mcp get $SERVER_NAME"
    fi
    info "Restart Claude Code to load it."
}

# A standalone copy of the skill, left from before the plugin shipped it. Two
# copies of one skill is one too many — but a copy someone edited is theirs.
# So it goes only if its SKILL.md is byte-identical to a SKILL.md metiche
# shipped, and the directory holds nothing else.
skill_is_known() {
    if [ -f "./plugin/.claude-plugin/marketplace.json" ]; then
        for _k in ./skill/metiche-teamwork/SKILL.md ./plugin/skills/metiche-teamwork/SKILL.md; do
            if [ -f "$_k" ] && cmp -s "$1" "$_k"; then return 0; fi
        done
    fi
    for _k in "$SRC_DIR/plugin/skills/metiche-teamwork/SKILL.md" "$SRC_DIR/skill/metiche-teamwork/SKILL.md"; do
        if [ -f "$_k" ] && cmp -s "$1" "$_k"; then return 0; fi
    done
    _sum=$(sha256_of "$1")
    for _k in $KNOWN_SKILL_SHA256; do
        if [ -n "$_sum" ] && [ "$_sum" = "$_k" ]; then return 0; fi
    done
    return 1
}

converge_skill_copy() {
    _sd="$CLAUDE_DIR/skills/metiche-teamwork"
    [ -e "$_sd" ] || return 0
    step "Standalone skill copy"
    info "now: $_sd exists; the plugin ships this skill, so a standalone copy is a duplicate"
    if [ ! -f "$_sd/SKILL.md" ] || [ "$(ls -A "$_sd")" != "SKILL.md" ]; then
        warn "$_sd holds something other than exactly one SKILL.md; leaving it alone"
        return 0
    fi
    if ! skill_is_known "$_sd/SKILL.md"; then
        warn "$_sd/SKILL.md differs from every SKILL.md metiche shipped, so it may carry"
        warn "your edits; leaving it alone. Delete it yourself if it is a stale copy."
        return 0
    fi
    # The backup goes one level up: a directory left under skills/ would be
    # loaded by Claude Code as a second copy of the very skill being removed.
    _skbk="$CLAUDE_DIR/metiche-teamwork-SKILL.md.metiche-backup-$TS"
    plan "remove $_sd: its SKILL.md is byte-identical to a shipped metiche SKILL.md" ||
        { info "would back up $_sd/SKILL.md to $_skbk first"; return 0; }
    cp -p "$_sd/SKILL.md" "$_skbk" || die "could not back up $_sd/SKILL.md; nothing was removed"
    info "backup: $_skbk"
    rm "$_sd/SKILL.md"
    rmdir "$_sd"
    DID_SOMETHING=1
}

# ------------------------------------------------------------------ Cursor ---

install_cursor() {
    target=$(client_config cursor)
    info "configuring globally (project-scoped .cursor/mcp.json is skipped on purpose — it lives in your repo)"
    write_mcp_json "$target" "Cursor" "$1"
    info "Cursor has no plugin format for the skill: point it at"
    info "skill/metiche-teamwork/SKILL.md, or paste it into your project rules."
}

# ---------------------------------------------------------------- Windsurf ---

install_windsurf() {
    target=$(client_config windsurf)
    write_mcp_json "$target" "Windsurf" "$1"
    info "Windsurf has no plugin format for the skill: point it at"
    info "skill/metiche-teamwork/SKILL.md, or paste it into your workspace rules."
}

# ------------------------------------------------------------------- Codex ---

# Codex gets its token written literally, in http_headers.
#
# WHY NOT --bearer-token-env-var, WHICH KEEPS THE TOKEN OFF DISK.
#
# Because it does not work for a GUI-launched Codex, and fails in a way
# nobody can see. Codex.app does not read your shell profile, so the
# variable is simply absent, and Codex then REFUSES TO START THE SERVER:
#
#   MCP server startup failed server_name="metiche"
#   error=Environment variable METICHE_TOKEN ... is not set
#
# What the user sees is `metiche: failed (0 tools)` and nothing else; the
# real reason is in a SQLite log in ~/.codex. A token that is theoretically
# safer but never reaches the client is worth nothing. config.toml is mode
# 0600 in the home directory, which is the same protection ~/.metiche/env
# has, so the honest difference is small.
#
# Written by taking our table out (codex_strip — see there for why not
# `codex mcp remove`) and appending a fresh one. TOML tables may appear in any
# order, so appending is safe once the old table is gone. Codex itself then
# reads the entry back.
install_codex() {
    _cxtok="$1"
    # CODEX_HOME relocates Codex's whole config directory, and the tests and
    # the two-agent setup both rely on that, so never assume ~/.codex.
    codex_cfg=$(client_config codex)

    info "Codex: the [mcp_servers.$SERVER_NAME] table in $codex_cfg"
    info "  now:    $(codex_table_state)"

    # Already exactly this table? The comparison is the whole table, so an
    # entry left by an older install — a second header, a
    # bearer_token_env_var — is rewritten rather than trusted.
    if [ -n "$_cxtok" ] && [ "$(codex_table)" = "$(codex_desired_table "$_cxtok")" ]; then
        info "  already correct, nothing to do"
    else
        info "  becomes: url = \"$URL\" | http_headers = { Authorization = \"Bearer <Codex's own token>\" }; every other table byte-identical"
        mcp_tmpdir
        _cxnew=$(new_tmp)
        : > "$_cxnew"
        if [ -f "$codex_cfg" ]; then
            codex_strip "$codex_cfg" > "$_cxnew"
            if [ -s "$_cxnew" ]; then
                file_ends_with_newline "$_cxnew" || printf '\n' >> "$_cxnew"
                if [ -n "$(tail -n 1 "$_cxnew")" ]; then printf '\n' >> "$_cxnew"; fi
            fi
        fi
        printf '[mcp_servers.%s]\n' "$SERVER_NAME" >> "$_cxnew"
        codex_desired_table "$_cxtok" >> "$_cxnew"

        if plan "write the [mcp_servers.$SERVER_NAME] table into $codex_cfg"; then
            # Codex does not create its own config directory, and appending
            # into a missing one fails in a way that reads like a permissions
            # problem.
            mkdir -p "$(dirname "$codex_cfg")"
            commit_file "$codex_cfg" "$_cxnew"
            chmod 600 "$codex_cfg" 2>/dev/null || true
            # Read back by Codex ITSELF: a table this script can parse but
            # Codex cannot is a Codex that shows "failed (0 tools)".
            _cxurl=$(codex mcp get "$SERVER_NAME" --json 2>/dev/null | jq -r '.transport.url // empty' 2>/dev/null || true)
            if [ "$_cxurl" = "$URL" ]; then
                info "registered, and read back by codex: $URL"
            else
                warn "codex could not read the entry back from $codex_cfg."
                warn "Check it by hand; the previous file is in its .metiche-backup-$TS."
                codex_manual_note
            fi
        else
            note_backup "$codex_cfg"
        fi
    fi

    install_agents_md
}

# Printed only when codex cannot read back what was written.
codex_manual_note() {
    say ""
    say "      It should read, in $codex_cfg:"
    say ""
    say "          [mcp_servers.$SERVER_NAME]"
    say "          url = \"$URL\""
    say "          http_headers = { Authorization = \"Bearer <Codex's own token>\" }"
    say ""
}

# ------------------------------------------------------- Codex AGENTS.md ---
#
# AGENTS.md is a SHARED file: other tools keep their own blocks in it, and the
# rest is the user's. So metiche owns one delimited block and nothing else,
# and every byte outside that block is left exactly as it was. The file is
# never written whole, never truncated, and never guessed at: a file with two
# metiche blocks, or a start marker with no end, is refused untouched,
# because there is no way to know which bytes are ours.

agents_block() {
    cat <<EOF
$AGENTS_START
## metiche: coordinate with the other agents on this repository

This block is managed by the metiche installer. Edits between these markers
are replaced when it runs again; everything outside them is left alone.

When the \`metiche\` MCP server is connected:

1. Call \`start_session\` before you touch any file. Your token already says
   which agent you are: never pass, invent or guess a \`client_key\`.
2. Call \`declare_intent\` with the paths you are about to change, before you
   change them, and act on any collision it reports.
3. Call \`heartbeat\` while you work. When a response says instructions are
   pending, call \`get_instructions\` and follow them.
4. Call \`end_session\` when the work is done.

If metiche is not connected, work normally and do not pretend to make these
calls.
$AGENTS_END
EOF
}

# Prints "<starts> <ends> <first start line> <first end line>" for a file.
# Markers count only as whole lines. Reads a file that exists; the caller has
# already established that with -f, never by inferring it from a match count.
agents_markers() {
    awk -v s="$AGENTS_START" -v e="$AGENTS_END" '
        $0 == s { ns++; if (!fs) fs = NR }
        $0 == e { ne++; if (!fe) fe = NR }
        END { printf "%d %d %d %d\n", ns, ne, fs, fe }
    ' "$1"
}

# Explains why a file's markers cannot be trusted. Returns 0 if they can.
agents_markers_ok() {
    if [ "$1" -eq 0 ] && [ "$2" -eq 0 ]; then return 0; fi
    if [ "$1" -eq 1 ] && [ "$2" -eq 1 ] && [ "$3" -lt "$4" ]; then return 0; fi
    if [ "$1" -gt 1 ]; then
        AGENTS_WHY="it has $1 \"$AGENTS_START\" lines: more than one metiche block"
    elif [ "$1" -eq 1 ] && [ "$2" -eq 0 ]; then
        AGENTS_WHY="\"$AGENTS_START\" at line $3 has no \"$AGENTS_END\" after it: an unterminated block"
    elif [ "$1" -eq 0 ]; then
        AGENTS_WHY="it has \"$AGENTS_END\" with no \"$AGENTS_START\""
    elif [ "$2" -gt 1 ]; then
        AGENTS_WHY="it has $2 \"$AGENTS_END\" lines"
    else
        AGENTS_WHY="\"$AGENTS_END\" (line $4) comes before \"$AGENTS_START\" (line $3)"
    fi
    return 1
}

install_agents_md() {
    _af="$CODEX_DIR/AGENTS.md"
    _ao="$CODEX_DIR/AGENTS.override.md"
    info "Codex standing instructions: the metiche block in $_af"

    if [ -e "$_ao" ]; then
        warn "$_ao exists, and Codex reads it INSTEAD of AGENTS.md in that directory."
        warn "Not writing a block Codex would ignore. To use it, copy the block between"
        warn "$AGENTS_START and $AGENTS_END into $_ao yourself."
        return 0
    fi
    if [ -e "$_af" ] && [ ! -f "$_af" ]; then
        warn "$_af exists but is not a regular file; leaving it alone"
        return 0
    fi

    mcp_tmpdir
    _anew=$(new_tmp)
    if [ ! -f "$_af" ]; then
        info "  now:    absent"
        agents_block > "$_anew"
        _averb="create $_af containing only the metiche block"
    else
        # shellcheck disable=SC2046
        set -- $(agents_markers "$_af")
        if ! agents_markers_ok "$1" "$2" "$3" "$4"; then
            warn "refusing to touch $_af: $AGENTS_WHY."
            warn "There is no way to tell which bytes are metiche's. Fix the markers by hand"
            warn "(or delete the metiche block entirely) and re-run; the file was not changed."
            return 0
        fi
        if [ "$1" -eq 0 ]; then
            info "  now:    $(wc -c < "$_af" | tr -d ' ') bytes, no metiche block"
            cat "$_af" > "$_anew"   # keep every existing byte
            if [ -s "$_af" ]; then
                file_ends_with_newline "$_af" || printf '\n' >> "$_anew"
                printf '\n' >> "$_anew"
            fi
            agents_block >> "$_anew"
            _averb="append the metiche block to $_af; every existing byte stays as it is"
        else
            info "  now:    a metiche block at lines $3-$4"
            if [ "$3" -gt 1 ]; then head -n $(($3 - 1)) "$_af" > "$_anew"; else : > "$_anew"; fi
            agents_block >> "$_anew"
            tail -n +$(($4 + 1)) "$_af" >> "$_anew"
            _averb="replace the metiche block in $_af (lines $3-$4); every byte outside it stays as it is"
        fi
    fi

    if [ -f "$_af" ] && cmp -s "$_anew" "$_af"; then
        info "  already carries the current metiche block, nothing to do"
        return 0
    fi
    _asize=$(wc -c < "$_anew" | tr -d ' ')
    if [ "$_asize" -gt "$AGENTS_MAX_BYTES" ]; then
        warn "refusing to write $_af: with the metiche block it would be $_asize bytes,"
        warn "over Codex's 32 KiB project-doc budget (project_doc_max_bytes), and Codex would"
        warn "cut it off. The file was not changed; trim it, or raise that setting and re-run."
        return 0
    fi
    plan "$_averb" || { note_backup "$_af"; return 0; }
    commit_file "$_af" "$_anew"
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
        header     Authorization: Bearer <a token>

    Every client should have a token of its own — a token names one agent,
    and two clients sharing one are one agent on the board. The anchor in
    ~/.metiche/env works, but that client then shares an identity with
    whichever client the anchor belongs to. It is never your join code: a
    join code sent as a bearer is a 401.

    And the skill is one markdown file — skill/metiche-teamwork/SKILL.md in the
    metiche repository. Any assistant that takes a rules or instructions file
    can use it as-is.
EOF
}

# --------------------------------------------------------------- the board ---
#
# The last step: open the team's board in a browser, signed in.
#
# open_board mints a sign-in link, <board>/signin#mbl_<secret>. The secret is a
# credential of its own: it works once, for ten minutes, signs ONE browser in
# as this account, and cannot call a tool. That is why it may be shown to the
# person running this, once, and a token may not.
#
# It still never goes on a command line, because `ps` shows every user's argv.
# The browser gets it through a small redirect page written with printf (a
# shell built-in, so no argv) into a 0600 file inside this run's 0700 scratch
# directory. The opener is given that file's path, and the file is removed
# before the script exits (by the exit trap too, on Ctrl-C).
#
# Nothing is minted under CI or without a terminal. Nobody is there to use the
# link, and a CI log is the worst place a secret can land.

# The opener for a local graphical session, or nothing.
board_opener() {
    case "$(uname -s 2>/dev/null || true)" in
        Darwin)
            if command -v open >/dev/null 2>&1; then printf 'open'; fi ;;
        Linux)
            if command -v xdg-open >/dev/null 2>&1 &&
               { [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ]; }; then
                printf 'xdg-open'
            fi ;;
    esac
    return 0
}

over_ssh() {
    [ -n "${SSH_CONNECTION:-}" ] || [ -n "${SSH_TTY:-}" ]
}

# The link goes, literally, into an HTML attribute, a script string and the
# terminal. So only the shape open_board makes is accepted, over a character
# set that cannot close a quote or start a tag.
board_link_ok() {
    case "$1" in
        https://*/signin#mbl_?*|http://localhost*/signin#mbl_?*|http://127.0.0.1*/signin#mbl_?*) ;;
        *) return 1 ;;
    esac
    case "$1" in
        *[!A-Za-z0-9._~:/#-]*|*"#"*"#"*) return 1 ;;
    esac
    return 0
}

board_url_ok() {
    case "$1" in
        https://?*|http://localhost*|http://127.0.0.1*) ;;
        *) return 1 ;;
    esac
    case "$1" in
        *[!A-Za-z0-9._~:/-]*) return 1 ;;
    esac
    return 0
}

# The page the browser opens: it replaces itself with the link, sending no
# referrer. $1 has passed board_link_ok.
board_redirect_html() {
    printf '<!doctype html>\n<meta charset="utf-8">\n<meta name="referrer" content="no-referrer">\n<meta http-equiv="refresh" content="0; url=%s">\n<title>metiche: signing in</title>\n<script>location.replace("%s")</script>\n' "$1" "$1"
}

# "18:12 (10 minutes)" in local time, from open_board's RFC 3339 UTC stamp.
board_expiry_text() {
    case "$1" in
        [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z) ;;
        *) printf 'a few minutes from now'; return 0 ;;
    esac
    _estamp=$(printf '%s' "$1" | cut -c1-19)
    _eepoch=$(TZ=UTC date -j -f '%Y-%m-%dT%H:%M:%S' "$_estamp" +%s 2>/dev/null) ||
        _eepoch=$(date -u -d "$_estamp" +%s 2>/dev/null) || _eepoch=""
    case "$_eepoch" in
        ""|*[!0-9]*)
            printf '%s UTC' "$(printf '%s' "$_estamp" | cut -c12-16)"
            return 0 ;;
    esac
    _emin=$(( (_eepoch - $(date +%s) + 30) / 60 ))
    _elocal=$(date -r "$_eepoch" +%H:%M 2>/dev/null) ||
        _elocal=$(date -d "@$_eepoch" +%H:%M 2>/dev/null) || _elocal=""
    if [ "$_emin" -eq 1 ]; then _eunit="minute"; else _eunit="minutes"; fi
    printf '%s (%s %s)' "${_elocal:-$(printf '%s' "$_estamp" | cut -c12-16) UTC}" "$_emin" "$_eunit"
}

board_later() {
    info "Later: ask your assistant to \"open the metiche board\", or re-run this installer in a terminal."
}

board_step() {
    step "Your board"
    _btok=$(tok_get "$FIRST_CLIENT")
    _btitle=$(client_title "$FIRST_CLIENT")
    if [ -z "$_btok" ]; then
        _btok="$ANCHOR"
        _btitle="the anchor"
    fi

    if [ "$DRY_RUN" -eq 1 ]; then
        if [ "$OPEN_BOARD" = "no" ]; then
            info "--no-open: would create no sign-in link"
            return 0
        fi
        if [ "$OPEN_BOARD" = "yes" ]; then
            info "would create a sign-in link with open_board as $FIRST_CLIENT for team \"${METICHE_TEAM_SLUG:-<the team>}\" (requested_via=installer), without asking"
        else
            info "would ask \"Open your board in a browser now, signed in? [Y/n]\" and, on yes, create a sign-in link with"
            info "  open_board as $FIRST_CLIENT for team \"${METICHE_TEAM_SLUG:-<the team>}\" (requested_via=installer)"
        fi
        info "  only in a terminal, and never when CI is set: otherwise no link is created"
        info "  on this machine's desktop: write it into a 0600 file in a private temporary directory, open that"
        info "  file (never the link itself), and remove it; over SSH: print the link to open on your own machine"
        return 0
    fi

    if [ "$OPEN_BOARD" = "no" ]; then
        info "not opening it (--no-open or METICHE_OPEN_BOARD=no): no sign-in link was created."
        board_later
        return 0
    fi
    if [ -n "${CI:-}" ]; then
        info "CI is set: no sign-in link was created (a secret has no business in a CI log)."
        board_later
        return 0
    fi
    if ! have_tty; then
        info "no terminal: no sign-in link was created."
        board_later
        return 0
    fi
    if [ -z "$_btok" ] || [ -z "$TEAM_SLUG" ]; then
        warn "no verified token or team to open the board with; skipping"
        return 0
    fi
    if [ "$OPEN_BOARD" = "ask" ]; then
        printf '    Open your board in a browser now, signed in? [Y/n] ' >/dev/tty
        IFS= read -r _bans </dev/tty || _bans=""
        case "$_bans" in
            n|N|no|No|NO)
                info "not opened: no sign-in link was created."
                board_later
                return 0 ;;
        esac
    fi

    info "creating a sign-in link with open_board, as $_btitle"
    _bres=""
    if ! mcp_connect "$_btok" ||
       ! _bres=$(mcp_call "$_btok" "open_board" "$(printf '{"team_slug":"%s","requested_via":"installer"}' "$TEAM_SLUG")"); then
        _bwhy=$(mcp_error)
        case "$_bwhy" in
            *"unknown tool"*)
                info "$URL has no open_board yet (an older metiche server), so the board cannot be opened signed in from here." ;;
            *)
                warn "could not create a sign-in link: $_bwhy"
                board_later ;;
        esac
        return 0
    fi
    _blink=$(printf '%s' "$_bres" | jq -r '.login_url // empty' 2>/dev/null) || _blink=""
    _bexp=$(printf '%s' "$_bres" | jq -r '.login_expires_at // empty' 2>/dev/null) || _bexp=""
    _bboard=$(printf '%s' "$_bres" | jq -r '.board_url // empty' 2>/dev/null) || _bboard=""
    _bvis=$(printf '%s' "$_bres" | jq -r '.team.visibility // empty' 2>/dev/null) || _bvis=""
    _bres=""
    : > "$MCP_TMP/body"
    if ! board_link_ok "$_blink"; then
        warn "open_board answered, but not with a sign-in link of the expected shape; not using it"
        board_later
        return 0
    fi
    board_url_ok "$_bboard" || _bboard=""
    case "$_bvis" in
        private) info "\"$TEAM_SLUG\" is private: its board is for members, after signing in." ;;
        public)  info "\"$TEAM_SLUG\" is public: anyone can open ${_bboard:-its board}; signing in adds your view." ;;
    esac
    _bwhen=$(board_expiry_text "$_bexp")

    _bopener=$(board_opener)
    if [ -n "$_bopener" ] && ! over_ssh; then
        mcp_tmpdir
        _bdir="$MCP_TMP/signin"
        _bfile="$_bdir/metiche-signin.html"
        if (umask 077; mkdir -p "$_bdir" && board_redirect_html "$_blink" > "$_bfile") &&
           chmod 600 "$_bfile" 2>/dev/null; then
            if "$_bopener" "$_bfile" >/dev/null 2>&1; then
                info "opened ${_bboard:-your board} in your browser."
            else
                warn "$_bopener could not open the sign-in page"
            fi
        else
            warn "could not write the sign-in page into $_bdir"
        fi
        say ""
        info "If nothing opened, this sign-in link does the same in any browser. It works ONCE and"
        info "only until $_bwhen, and it signs that browser in as you, so do not share it:"
        say ""
        say "      $_blink"
        say ""
        # The browser reads the file after the opener has returned.
        sleep "$BOARD_FILE_SECONDS"
        rm -rf "$_bdir"
        info "the sign-in page file is removed."
    else
        if over_ssh; then
            info "this is an SSH session, so nothing is opened here."
        else
            info "no desktop browser to open here."
        fi
        info "Open this sign-in link in a browser on your own machine. It works ONCE and only until"
        info "$_bwhen, and it signs that browser in as you, so do not share it:"
        say ""
        say "      $_blink"
        say ""
    fi
    _blink=""
    board_later
    return 0
}

# The shell that ran this still exports whatever METICHE_TOKEN it had when it
# started. If this run wrote a different anchor, a re-run or the metiche
# repository's .mcp.json in this shell would use the old one.
stale_shell_note() {
    [ "$DRY_RUN" -eq 0 ] || return 0
    [ -n "${METICHE_TOKEN:-}" ] || return 0
    _saved=$(existing_token)
    if [ -n "$_saved" ] && [ "$METICHE_TOKEN" != "$_saved" ]; then
        say ""
        warn "this terminal still has the previous METICHE_TOKEN. Open a new terminal (or run"
        warn ". $ENV_FILE) before re-running the installer or using the metiche repository's .mcp.json."
    fi
    return 0
}

# --------------------------------------------------------------- uninstall ---
#
# Removes exactly what metiche owns — the same list the header gives — and
# nothing else, backing up every file before changing it. No network.

uninstall_agents_md() {
    _uf="$CODEX_DIR/AGENTS.md"
    info "Codex: the metiche block in $_uf"
    if [ ! -f "$_uf" ]; then
        if [ -e "$_uf" ]; then warn "$_uf is not a regular file; leaving it alone"; else info "  now:    absent, nothing to remove"; fi
        return 0
    fi
    # shellcheck disable=SC2046
    set -- $(agents_markers "$_uf")
    if ! agents_markers_ok "$1" "$2" "$3" "$4"; then
        warn "refusing to touch $_uf: $AGENTS_WHY. The file was not changed."
        return 0
    fi
    if [ "$1" -eq 0 ]; then
        info "  now:    no metiche block, nothing to remove"
        return 0
    fi
    info "  now:    a metiche block at lines $3-$4"
    mcp_tmpdir
    _unew=$(new_tmp)
    _ukeep=$(($3 - 1))
    # The blank line the installer put above an appended block goes with it,
    # when the block is the end of the file.
    if [ "$_ukeep" -ge 1 ] && [ -z "$(sed -n "${_ukeep}p" "$_uf")" ] &&
       [ "$(tail -n +$(($4 + 1)) "$_uf" | wc -c | tr -d ' ')" = "0" ]; then
        _ukeep=$((_ukeep - 1))
    fi
    if [ "$_ukeep" -ge 1 ]; then head -n "$_ukeep" "$_uf" > "$_unew"; else : > "$_unew"; fi
    tail -n +$(($4 + 1)) "$_uf" >> "$_unew"
    if [ ! -s "$_unew" ]; then
        info "  becomes: deleted — the metiche block was its entire content"
        plan "delete $_uf" || { note_backup "$_uf"; return 0; }
        backup_file "$_uf"
        rm "$_uf"
        DID_SOMETHING=1
    else
        info "  becomes: $(wc -c < "$_unew" | tr -d ' ') bytes; every byte outside the block unchanged"
        plan "remove the metiche block from $_uf" || { note_backup "$_uf"; return 0; }
        commit_file "$_uf" "$_unew"
    fi
}

uninstall_codex_table() {
    _ucf=$(client_config codex)
    info "Codex: the [mcp_servers.$SERVER_NAME] table in $_ucf"
    info "  now:    $(codex_table_state)"
    if [ ! -f "$_ucf" ] || [ -z "$(codex_table)" ]; then
        info "  nothing to remove"
        return 0
    fi
    mcp_tmpdir
    _ucnew=$(new_tmp)
    codex_strip "$_ucf" > "$_ucnew"
    info "  becomes: absent; every other table byte-identical"
    plan "remove [mcp_servers.$SERVER_NAME] from $_ucf" || { note_backup "$_ucf"; return 0; }
    commit_file "$_ucf" "$_ucnew"
}

uninstall_claude() {
    _ucl=$(client_config claude)
    info "Claude Code: user-scope servers in $_ucl"
    for _un in "$SERVER_NAME" "$LEGACY_CLAUDE_SERVER"; do
        info "  now:    $_un — $(json_entry_state "$_ucl" "$_un")"
    done
    for _un in "$SERVER_NAME" "$LEGACY_CLAUDE_SERVER"; do
        json_has_server "$_ucl" "$_un" || continue
        if ! command -v claude >/dev/null 2>&1; then
            warn "$_un is in $_ucl, but claude is not on PATH, and the claude CLI is the"
            warn "only safe writer of that file. Remove it with: claude mcp remove $_un --scope user"
            continue
        fi
        claude_remove_server "$_un"
    done
    info "The plugin (the skill) stays installed; remove it with:"
    info "  claude plugin uninstall $PLUGIN_NAME@$MARKETPLACE_NAME --scope user"
}

uninstall_profiles() {
    step "Shell profile"
    _found_any=0
    _seen_profiles=""
    for _pf in "$(detect_profile)" "${ZDOTDIR:-$HOME}/.zshrc" "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.profile"; do
        [ -f "$_pf" ] || continue
        case "$_seen_profiles" in *"|$_pf|"*) continue ;; esac
        _seen_profiles="$_seen_profiles|$_pf|"
        grep -qxF "$PROFILE_LINE" "$_pf" 2>/dev/null || continue
        _found_any=1
        info "$_pf: the metiche loader line (and the comment metiche wrote above it)"
        mcp_tmpdir
        _pnew=$(new_tmp)
        _pnl=0
        if file_ends_with_newline "$_pf"; then _pnl=1; fi
        PL="$PROFILE_LINE" awk -v nl="$_pnl" '
            BEGIN { pl = ENVIRON["PL"] }
            { line[NR] = $0 }
            END {
                for (i = 1; i <= NR; i++) if (line[i] == pl) {
                    del[i] = 1
                    if (i > 1 && line[i-1] ~ /^# metiche - load/) {
                        del[i-1] = 1
                        if (i > 2 && line[i-2] == "") del[i-2] = 1
                    }
                }
                for (i = 1; i <= NR; i++) if (!del[i]) { if (n++) printf "\n"; printf "%s", line[i] }
                if (n && nl) printf "\n"
            }
        ' "$_pf" > "$_pnew"
        plan "remove it from $_pf" || { note_backup "$_pf"; continue; }
        commit_file "$_pf" "$_pnew"
    done
    if [ "$_found_any" -eq 0 ]; then info "no profile loads ~/.metiche/env, nothing to remove"; fi
}

# ------------------------------------------------------------ metiche CLI ---
#
# docs/CLI.md §6.3. The CLI is optional: the joins are the product, so every
# failure in this step is a warning and the install goes on. What is never
# skipped is the checksum: an archive whose sha256 does not match the release's
# checksums file is not installed, and nothing is written.

cli_os() {
    case "$(uname -s 2>/dev/null || true)" in
        Darwin) printf 'Darwin' ;;
        Linux)  printf 'Linux' ;;
    esac
    return 0
}

cli_arch() {
    case "$(uname -m 2>/dev/null || true)" in
        x86_64|amd64)  printf 'x86_64' ;;
        aarch64|arm64) printf 'arm64' ;;
    esac
    return 0
}

# Sets CLI_LATEST (without the v) and CLI_LATEST_STATUS: ok, not_published or
# error. Not for use in $(...): it sets variables.
cli_latest() {
    CLI_LATEST=""
    CLI_LATEST_STATUS="error"
    mcp_tmpdir
    _lj="$MCP_TMP/cli-latest.json"
    _lcode=$(curl -sS --max-time "$HTTP_TIMEOUT" -H 'Accept: application/vnd.github+json' \
        -o "$_lj" -w '%{http_code}' "$CLI_LATEST_URL" 2>/dev/null) || _lcode="000"
    case "$_lcode" in
        404) CLI_LATEST_STATUS="not_published"; return 0 ;;
        200) ;;
        *) return 0 ;;
    esac
    _ltag=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$_lj" | sed -n '1p')
    _ltag=${_ltag#v}
    case "$_ltag" in
        ""|*[!A-Za-z0-9.+-]*) return 0 ;;
    esac
    CLI_LATEST="$_ltag"
    CLI_LATEST_STATUS="ok"
}

# The ~/.local/bin symlink, only when that directory is on PATH and nothing of
# somebody else's is already there. $1 is "dry" under --dry-run.
cli_link() {
    _lbin="$HOME/.local/bin"
    _llink="$_lbin/metiche"
    _lbinary="$ENV_DIR/bin/metiche"
    case ":${PATH:-}:" in
        *":$_lbin:"*)
            if [ -L "$_llink" ]; then
                case "$(readlink "$_llink" 2>/dev/null || true)" in
                    "$ENV_DIR/bin/"*) ;;
                    *) warn "$_llink is a symlink to somewhere else; leaving it"; return 0 ;;
                esac
            elif [ -e "$_llink" ]; then
                warn "~/.local/bin/metiche exists and is not ours; leaving it"
                return 0
            fi
            if [ "$(readlink "$_llink" 2>/dev/null || true)" = "$_lbinary" ]; then
                info "$_llink already links to it"
            elif [ "${1:-}" = "dry" ]; then
                info "would link $_llink -> $_lbinary"
            else
                mkdir -p "$_lbin" && ln -sf "$_lbinary" "$_llink" && info "linked $_llink -> $_lbinary"
            fi
            ;;
        *)
            info "~/.local/bin is not on your PATH. To run it as \`metiche\`, add to your shell profile:"
            say '      export PATH="$HOME/.metiche/bin:$PATH"'
            ;;
    esac
    _lfirst=$(command -v metiche 2>/dev/null || true)
    case "$_lfirst" in
        ""|"$_llink"|"$_lbinary") ;;
        *) warn "another metiche comes first on your PATH: $_lfirst" ;;
    esac
    return 0
}

install_cli() {
    step "metiche CLI"
    if [ "$INSTALL_CLI" -eq 0 ]; then
        info "--no-cli: not installing the metiche command-line tool"
        return 0
    fi
    _cos=$(cli_os)
    _carch=$(cli_arch)
    if [ -z "$_cos" ]; then
        info "the metiche CLI has no build for $(uname -s 2>/dev/null || printf 'this system'); skipping"
        return 0
    fi
    if [ -z "$_carch" ]; then
        info "the metiche CLI has no build for $(uname -m 2>/dev/null || printf 'this machine'); skipping"
        return 0
    fi
    _casset="metiche_${_cos}_${_carch}.tar.gz"
    _cbin="$ENV_DIR/bin/metiche"
    _cver=${CLI_VERSION#v}

    if [ -z "$_cver" ]; then
        if [ "$DRY_RUN" -eq 1 ]; then
            info "would ask $CLI_LATEST_URL for the latest release"
            info "would download $CLI_RELEASE_BASE/v<latest>/$_casset"
            info "would verify it against $CLI_RELEASE_BASE/v<latest>/metiche_<latest>_checksums.txt (sha256)"
            info "would install $_cbin (mode 0755)"
            cli_link dry
            return 0
        fi
        cli_latest
        case "$CLI_LATEST_STATUS" in
            not_published) info "metiche CLI not published yet"; return 0 ;;
            error)
                warn "could not resolve the latest metiche CLI (GitHub API: $CLI_LATEST_URL); set METICHE_CLI_VERSION=vX.Y.Z to pin one"
                return 0 ;;
        esac
        _cver="$CLI_LATEST"
    fi
    _curl="$CLI_RELEASE_BASE/v$_cver/$_casset"
    _csums="$CLI_RELEASE_BASE/v$_cver/metiche_${_cver}_checksums.txt"
    if [ "$DRY_RUN" -eq 1 ]; then
        info "would download $_curl"
        info "would verify it against $_csums (sha256)"
        info "would install $_cbin (mode 0755)"
        cli_link dry
        return 0
    fi
    if [ -x "$_cbin" ] && [ "$("$_cbin" version --short 2>/dev/null || true)" = "$_cver" ]; then
        info "metiche CLI $_cver already installed"
        cli_link
        return 0
    fi
    if ! command -v shasum >/dev/null 2>&1 && ! command -v sha256sum >/dev/null 2>&1; then
        warn "cannot verify a metiche CLI download (no shasum or sha256sum); not installing it"
        return 0
    fi

    mcp_tmpdir
    _cdir="$MCP_TMP/cli"
    rm -rf "$_cdir"
    mkdir -p "$_cdir"
    if ! curl -fsSL --retry 3 --max-time 120 -o "$_cdir/$_casset" "$_curl" 2>/dev/null; then
        warn "could not download the metiche CLI $_cver from $_curl; skipping it"
        return 0
    fi
    if ! curl -fsSL --retry 3 --max-time "$HTTP_TIMEOUT" -o "$_cdir/checksums.txt" "$_csums" 2>/dev/null; then
        warn "could not download $_csums, so the CLI cannot be verified; not installing it"
        return 0
    fi
    _cwant=$(grep " $_casset\$" "$_cdir/checksums.txt" 2>/dev/null | cut -d' ' -f1 | sed -n '1p' || true)
    _cgot=$(sha256_of "$_cdir/$_casset")
    if [ -z "$_cwant" ] || [ "$_cwant" != "$_cgot" ]; then
        warn "the metiche CLI archive does not match its published sha256; not installing it. Nothing was written."
        warn "  archive:   $_curl"
        warn "  checksums: $_csums"
        return 0
    fi
    info "sha256 verified: $_casset ($_cver)"
    if ! tar -xzf "$_cdir/$_casset" -C "$_cdir" metiche 2>/dev/null || [ ! -f "$_cdir/metiche" ]; then
        warn "the archive $_curl holds no metiche binary; not installing it"
        return 0
    fi
    mkdir -p "$ENV_DIR/bin"
    chmod 700 "$ENV_DIR" 2>/dev/null || true
    if cp "$_cdir/metiche" "$ENV_DIR/bin/.metiche.new" && chmod 755 "$ENV_DIR/bin/.metiche.new" &&
        mv -f "$ENV_DIR/bin/.metiche.new" "$_cbin"; then
        info "installed $_cbin ($_cver)"
        DID_SOMETHING=1
    else
        rm -f "$ENV_DIR/bin/.metiche.new"
        warn "could not install $_cbin"
        return 0
    fi
    cli_link
    return 0
}

# --uninstall: ~/.metiche (the binary with it) goes in uninstall_env_dir. The
# ~/.local/bin symlink goes here, and only when it points into ~/.metiche/bin.
uninstall_cli_link() {
    _ulink="$HOME/.local/bin/metiche"
    [ -L "$_ulink" ] || return 0
    case "$(readlink "$_ulink" 2>/dev/null || true)" in
        "$ENV_DIR/bin/"*)
            if plan "remove the symlink $_ulink (the metiche CLI)"; then
                rm -f "$_ulink"
                DID_SOMETHING=1
            fi
            ;;
        *) info "$_ulink links somewhere other than ~/.metiche/bin; leaving it" ;;
    esac
    return 0
}

uninstall_env_dir() {
    step "~/.metiche"
    if [ -z "$HOME" ] || [ "$ENV_DIR" != "$HOME/.metiche" ]; then
        die "refusing to remove $ENV_DIR: it is not \$HOME/.metiche"
    fi
    if [ ! -e "$ENV_DIR" ]; then
        info "$ENV_DIR: absent, nothing to remove"
        return 0
    fi
    _edbk="$ENV_DIR.metiche-backup-$TS"
    info "$ENV_DIR: the anchor token, the plugin clone and the metiche CLI (bin/metiche)"
    plan "remove $ENV_DIR" || { info "would back up $ENV_DIR to $_edbk first"; return 0; }
    cp -R -p "$ENV_DIR" "$_edbk" || die "could not back up $ENV_DIR; nothing was removed"
    chmod go-rwx "$_edbk" 2>/dev/null || true
    info "backup: $_edbk (it holds your token: delete it once you are sure)"
    rm -rf "$ENV_DIR"
    DID_SOMETHING=1
}

main_uninstall() {
    say "metiche uninstaller"
    if [ "$DRY_RUN" -eq 1 ]; then
        say "  DRY RUN — nothing will be changed"
    fi
    say "  removes only what metiche added, backs up every file it changes, contacts no server"

    if want claude; then
        step "Claude Code"
        uninstall_claude
    fi
    if want cursor; then
        step "Cursor"
        remove_json_server "$(client_config cursor)" "Cursor" "$SERVER_NAME"
    fi
    if want windsurf; then
        step "Windsurf"
        remove_json_server "$(client_config windsurf)" "Windsurf" "$SERVER_NAME"
    fi
    if want codex; then
        step "Codex"
        uninstall_codex_table
        uninstall_agents_md
    fi
    if want claude; then
        converge_skill_copy
    fi
    if [ -z "$ONLY" ]; then
        uninstall_profiles
        step "metiche CLI"
        uninstall_cli_link
        uninstall_env_dir
    else
        info "(--only: the shell profile line and ~/.metiche are kept)"
    fi

    step "Done"
    if [ "$DRY_RUN" -eq 1 ]; then
        info "dry run — nothing was changed"
    elif [ "$DID_SOMETHING" -eq 1 ]; then
        info "metiche is removed from this machine; the backups are listed above."
        info "Your agents' tokens still exist on the server until the team retires them."
    else
        info "nothing of metiche's was found."
    fi
}

# -------------------------------------------------------------------- main ---

main() {
    if [ "$UNINSTALL" -eq 1 ]; then
        main_uninstall
        return 0
    fi

    say "metiche installer"
    say "  endpoint: $URL"
    if [ "$DRY_RUN" -eq 1 ]; then
        say "  DRY RUN — nothing will be written and nothing will be called"
    else
        say ""
        say "  This joins your team over the network — once per assistant — before"
        say "  it configures that assistant. If the endpoint above does not answer,"
        say "  nothing on this machine is changed at all."
    fi

    # Order matters, and it is the whole design: decide, detect clients, pick
    # the team, then for each client: identity, join, verify, write. No client
    # config is written until that client's own token has proved itself on a
    # fresh authenticated connection.
    choose_identity
    resolve_member_name
    detect_clients
    resolve_team

    for client in $CLIENTS; do
        configure_client "$client"
    done

    if want claude; then
        converge_skill_copy
    fi
    install_cli
    handle_profile
    note_unverified

    step "Done"
    if [ "$DRY_RUN" -eq 1 ]; then
        info "dry run — nothing was called and nothing was changed"
    elif [ "$DID_SOMETHING" -eq 1 ]; then
        info "on team \"$TEAM_SLUG\", each with its own token:$CONFIGURED"
        info "Restart your assistants. Each is already authenticated as its own agent,"
        info "so it can call start_session straight away — no join, no client_key."
    else
        info "everything was already configured:$CONFIGURED"
    fi

    board_step
    stale_shell_note
}

main
