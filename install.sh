#!/bin/sh
# metiche installer — https://metiche.xyz
#
#   curl -fsSL https://metiche.xyz/install.sh | sh
#
# Points your coding assistants at a metiche team: installs the metiche-teamwork
# skill where it can, and registers the metiche MCP server where it can.
#
# What it touches, and nothing else:
#
#   ~/.metiche/env                              your join code, mode 0600
#   ~/.metiche/src                              a shallow clone, for the plugin
#   ~/.cursor/mcp.json                          Cursor's global MCP config
#   ~/.codeium/windsurf/mcp_config.json         Windsurf's MCP config
#   ~/.codex/config.toml                        via the `codex mcp add` CLI
#   Claude Code's user-scope plugin config      via the `claude` CLI
#   your shell profile                          only with --write-profile
#
# It never writes anything inside the current directory, never writes a join
# code into a file in a repository, and never runs sudo. Run it with --dry-run
# to see every change it would make without making any of them.
#
# NOTE: the metiche backend is not deployed yet. This script configures clients
# to talk to an endpoint that does not answer yet. That is deliberate — the
# client side is ready first — but do not expect a working connection today.
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
# what write_env_file exports, or Codex authenticates as nobody.
TOKEN_ENV="METICHE_JOIN_CODE"

URL="${METICHE_URL:-$METICHE_URL_DEFAULT}"
REPO="${METICHE_REPO:-$METICHE_REPO_DEFAULT}"
MARKETPLACE="${METICHE_MARKETPLACE:-}"
DRY_RUN=0
WRITE_PROFILE=0
ONLY=""

ENV_DIR="$HOME/.metiche"
ENV_FILE="$ENV_DIR/env"
SRC_DIR="$ENV_DIR/src"

JOIN_CODE=""
CODE_SOURCE=""
DID_SOMETHING=0

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

Options:
  --dry-run           Print every change that would be made; change nothing.
  --url <url>         MCP endpoint (default: https://mcp.metiche.xyz/v1/mcp).
  --marketplace <src> Claude Code plugin marketplace source. By default the
                      script uses ./plugin if you are standing in a clone of
                      the metiche repository, and otherwise makes a shallow
                      clone at ~/.metiche/src and uses ~/.metiche/src/plugin.
  --only <list>       Comma-separated subset of: claude,cursor,windsurf,codex.
                      Default: every assistant detected on this machine.
  --write-profile     Append the line that loads ~/.metiche/env to your shell
                      profile. Without this the line is printed, not written.
  -h, --help          This.

The join code is read from the METICHE_JOIN_CODE environment variable, or
prompted for interactively. It is deliberately NOT accepted as a command-line
argument: arguments are visible in `ps` to every user on the machine and land
in your shell history.

  METICHE_JOIN_CODE=your-code sh install.sh        # non-interactive

Cursor is configured globally (~/.cursor/mcp.json) and never per-project: the
only project-scoped location is .cursor/mcp.json inside your repository, and a
join code does not belong in a repository.

Codex is configured through its own `codex mcp add` CLI, so metiche never
parses or rewrites ~/.codex/config.toml. Codex is also the only client where
the token is NOT written to disk: it stores the NAME of an environment
variable and reads the value at connect time. That means ~/.metiche/env has
to be loaded in the shell you launch codex from — see --write-profile.

Zed is not configured. Its MCP config format was not verified when this script
was written, and guessing at a config file is worse than printing the endpoint
and letting you paste it. See the note it prints.
EOF
}

# ------------------------------------------------------------------- flags ---

while [ $# -gt 0 ]; do
    case "$1" in
        --dry-run)       DRY_RUN=1 ;;
        --write-profile) WRITE_PROFILE=1 ;;
        --url)           [ $# -ge 2 ] || die "--url needs a value"; URL="$2"; shift ;;
        --url=*)         URL="${1#--url=}" ;;
        --marketplace)   [ $# -ge 2 ] || die "--marketplace needs a value"; MARKETPLACE="$2"; shift ;;
        --marketplace=*) MARKETPLACE="${1#--marketplace=}" ;;
        --only)          [ $# -ge 2 ] || die "--only needs a value"; ONLY="$2"; shift ;;
        --only=*)        ONLY="${1#--only=}" ;;
        -h|--help)       usage; exit 0 ;;
        *)
            # A bare argument is most likely someone passing their join code.
            # Refuse loudly rather than accept a credential through argv.
            say "error: unexpected argument: $1" >&2
            say "" >&2
            say "If that was your join code: join codes are never accepted as" >&2
            say "arguments, because arguments are visible in the process list" >&2
            say "and in shell history. Use:" >&2
            say "" >&2
            say "  METICHE_JOIN_CODE=... sh install.sh" >&2
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

# ------------------------------------------------------------- join code ---

read_join_code() {
    if [ -n "${METICHE_JOIN_CODE:-}" ]; then
        JOIN_CODE="$METICHE_JOIN_CODE"
        CODE_SOURCE="METICHE_JOIN_CODE"
        return 0
    fi

    if [ ! -r /dev/tty ]; then
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

# ------------------------------------------------------------- json output ---

# The MCP server entry, shared by every client that speaks the common shape.
# Written with the literal code, to the user's own home-directory config.
server_json() {
    cat <<EOF
{
  "type": "http",
  "url": "$URL",
  "headers": {
    "Authorization": "Bearer $JOIN_CODE"
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
            say "        \"headers\": { \"Authorization\": \"Bearer <your join code>\" } }"
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

write_env_file() {
    step "Join code"
    desired="# metiche — created by install.sh. Keep this file private.
METICHE_JOIN_CODE=$JOIN_CODE
export METICHE_JOIN_CODE
"
    if [ -f "$ENV_FILE" ] && [ "$desired" = "$(cat "$ENV_FILE")" ]; then
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
    info "read from: $CODE_SOURCE (the code itself is never printed)"
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
            say "      set -gx METICHE_JOIN_CODE (string split -f2 = < ~/.metiche/env)[1]"
            say ""
            return 0
            ;;
    esac

    if [ -f "$profile" ] && grep -qF '.metiche/env' "$profile" 2>/dev/null; then
        info "$profile already loads ~/.metiche/env"
        return 0
    fi

    if [ "$WRITE_PROFILE" -eq 1 ]; then
        plan "append the loader line to $profile" && {
            printf '\n# metiche\n%s\n' "$PROFILE_LINE" >> "$profile"
            DID_SOMETHING=1
        }
    else
        say ""
        info "Add this line to $profile so your assistants see the code"
        info "(or re-run with --write-profile and this script will append it):"
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
    say "        --header \"Authorization: Bearer \$METICHE_JOIN_CODE\""
    say ""
    warn "That command puts the expanded code in your process list while it runs,"
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
    # that definition reads the join code from $METICHE_JOIN_CODE at runtime,
    # so no credential passes through argv or gets written into the plugin.
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
    if [ -z "${METICHE_JOIN_CODE:-}" ]; then
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
        header     Authorization: Bearer <your join code>

    And the skill is one markdown file — skill/metiche-teamwork/SKILL.md in the
    metiche repository. Any assistant that takes a rules or instructions file
    can use it as-is.
EOF
}

# -------------------------------------------------------------------- main ---

main() {
    say "metiche installer"
    say "  endpoint: $URL"
    [ "$DRY_RUN" -eq 1 ] && say "  DRY RUN — nothing will be written"
    say ""
    say "  The metiche backend is not deployed yet. This configures your"
    say "  clients; the endpoint above will not answer until it ships."

    read_join_code
    validate_join_code
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
        info "dry run — nothing was changed"
    elif [ "$DID_SOMETHING" -eq 1 ]; then
        info "restart your assistant, then ask it to join the team."
    else
        info "everything was already configured."
    fi
}

main
