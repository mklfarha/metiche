package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mklfarha/metiche/cli/internal/credential"
	"github.com/mklfarha/metiche/cli/internal/mcpclient"
	"github.com/mklfarha/metiche/cli/internal/secret"
	"github.com/mklfarha/metiche/cli/internal/wire"
)

// session is an authenticated connection and what it knows about itself.
type session struct {
	c      *mcpclient.Client
	who    wire.Whoami
	source string // env | anchor_file | client
	client string
	path   string
}

func (s *session) close() {
	if s != nil {
		s.c.Close()
	}
}

// dial opens a connection with a token (or none).
func (a *app) dial(tok secret.Secret) (*mcpclient.Client, error) {
	return mcpclient.Dial(a.ctx(), mcpclient.Options{
		Endpoint: a.endpoint.URL, Token: tok, Timeout: a.timeout,
		Strict: a.env("METICHE_CLI_STRICT_DECODE") == "1",
	})
}

// health is an anonymous health call on a fresh connection.
func (a *app) health() (wire.Health, error) {
	c, err := a.dial(secret.Secret{})
	if err != nil {
		return wire.Health{}, err
	}
	defer c.Close()
	var h wire.Health
	err = c.Call(a.ctx(), "health", nil, &h)
	return h, err
}

// deadTokenOr4 applies install.sh's token_is_dead rule: a 401 means a dead
// token only when an anonymous health reports the database reachable.
func (a *app) unauthorized() error {
	h, err := a.health()
	if err != nil || !h.OK || h.Database != "reachable" {
		return fail(exitNoServer, "unreachable", "metiche answered 401, and its database is not reachable: this is an outage, not a dead token. Retry in a few minutes.")
	}
	return nil
}

// whoamiWith dials with a token and asks who it is. The connection is returned
// open on success.
func (a *app) whoamiWith(tok secret.Secret) (*mcpclient.Client, wire.Whoami, error) {
	c, err := a.dial(tok)
	if err != nil {
		return nil, wire.Whoami{}, err
	}
	var w wire.Whoami
	if err := c.Call(a.ctx(), "whoami", nil, &w); err != nil {
		c.Close()
		var me *mcpclient.Error
		if errors.As(err, &me) && me.Kind == mcpclient.KindTool && me.Code == "unknown_tool" {
			return nil, wire.Whoami{}, fail(exitError, "server_too_old", "this metiche server has no whoami tool; it is older than this CLI")
		}
		return nil, wire.Whoami{}, err
	}
	return c, w, nil
}

type candidate struct {
	source, client, path string
	tok                  secret.Secret
}

// connect resolves the credential (docs/CLI.md §3) and returns an open
// session. needAgent requires an agent token (teams create, open for a private
// team): a legacy account anchor then falls back to a client's agent token
// under the one-account rule.
func (a *app) connect(needAgent bool) (*session, error) {
	var cands []candidate
	af := credential.ReadAnchorFile(a.paths)
	envTok, envSet := credential.EnvToken(a.env)
	switch {
	case envSet && envTok.Empty():
		a.note("metiche: $METICHE_TOKEN is set but is not a metiche token; ignoring it")
	case envSet && !af.Token.Empty() && !envTok.Equal(af.Token) && credential.InBackups(a.paths, envTok):
		a.note("metiche: this shell's $METICHE_TOKEN is a stale token from before the last install; using ~/.metiche/env instead. Open a new terminal.")
	case envSet:
		cands = append(cands, candidate{source: "env", tok: envTok})
	}
	if !af.Token.Empty() && (len(cands) == 0 || !cands[0].tok.Equal(af.Token)) {
		cands = append(cands, candidate{source: "anchor_file", path: a.paths.Tilde(af.Path), tok: af.Token})
	}

	var legacy *session
	rejected := []string{}
	for _, cd := range cands {
		c, w, err := a.whoamiWith(cd.tok)
		if err != nil {
			if e := a.classifyConnect(err); e != nil {
				return nil, e
			}
			rejected = append(rejected, describe(cd))
			continue
		}
		s := &session{c: c, who: w, source: cd.source, path: cd.path}
		if needAgent && w.TokenScope != "agent" {
			legacy = s
			break
		}
		return s, nil
	}

	// Fallback: the client configs, one account only.
	var accepted []*session
	accounts := map[string]bool{}
	if legacy != nil {
		accounts[legacy.who.AccountKey] = true
	}
	for _, client := range credential.Clients {
		e := credential.ReadEntry(a.paths, client)
		if e.Token.Empty() {
			continue
		}
		dup := false
		for _, cd := range cands {
			if cd.tok.Equal(e.Token) {
				dup = true
			}
		}
		if dup && legacy == nil {
			continue
		}
		c, w, err := a.whoamiWith(e.Token)
		if err != nil {
			if ce := a.classifyConnect(err); ce != nil {
				return nil, ce
			}
			continue
		}
		accounts[w.AccountKey] = true
		accepted = append(accepted, &session{c: c, who: w, source: "client", client: client, path: a.paths.Tilde(e.Path)})
	}
	closeAll := func(except *session) {
		for _, s := range accepted {
			if s != except {
				s.close()
			}
		}
		if legacy != nil && legacy != except {
			legacy.close()
		}
	}
	if len(accounts) > 1 {
		closeAll(nil)
		return nil, fail(exitNoCred, "several_accounts", "the tokens on this machine belong to %d accounts; run `metiche doctor`", len(accounts))
	}
	for _, s := range accepted {
		if needAgent && s.who.TokenScope != "agent" {
			continue
		}
		closeAll(s)
		why := "the anchor was rejected"
		if legacy != nil {
			why = "the anchor is a pre-per-agent account token"
		} else if len(cands) == 0 {
			why = "no anchor token on this machine"
		}
		a.note("credential: %s's token (%s — run metiche doctor)", credential.Titles[s.client], why)
		return s, nil
	}
	if legacy != nil && !needAgent {
		closeAll(legacy)
		return legacy, nil
	}
	closeAll(nil)
	if legacy != nil {
		return nil, fail(exitNoCred, "no_agent_token", "this needs a client's own token, and ~/.metiche/env holds a pre-per-agent account token; re-run the installer: curl -fsSL https://metiche.xyz/install.sh | sh")
	}
	detail := "no usable metiche token on this machine"
	if len(rejected) > 0 {
		detail += " (rejected: " + strings.Join(rejected, ", ") + ")"
	}
	return nil, fail(exitNoCred, "no_credential", "%s. Run `metiche doctor` to see why, then re-run the installer.", detail)
}

func describe(cd candidate) string {
	if cd.source == "env" {
		return "$METICHE_TOKEN"
	}
	return cd.path
}

// classifyConnect returns nil for a rejected token (try the next one), and an
// exit error for everything that is not about the token.
func (a *app) classifyConnect(err error) error {
	var ce *cliError
	if errors.As(err, &ce) {
		return ce
	}
	var me *mcpclient.Error
	if !errors.As(err, &me) {
		return fail(exitError, "error", "%v", err)
	}
	if me.Kind == mcpclient.KindUnauthorized {
		return a.unauthorized()
	}
	return a.mapErr(err)
}

// mapErr turns any error into the §1.2 exit code.
func (a *app) mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *cliError
	if errors.As(err, &ce) {
		return ce
	}
	var me *mcpclient.Error
	if !errors.As(err, &me) {
		return fail(exitError, "error", "%v", err)
	}
	switch me.Kind {
	case mcpclient.KindUnreachable:
		return fail(exitNoServer, "unreachable", "%s", me.Detail)
	case mcpclient.KindUnauthorized:
		if e := a.unauthorized(); e != nil {
			return e
		}
		return fail(exitNoCred, "rejected", "metiche rejected the token. Run `metiche doctor`, then re-run the installer.")
	case mcpclient.KindTool:
		switch me.Code {
		case "not_permitted", "not_found", "rate_limited":
			return fail(exitRefused, me.Code, "%s", me.Detail)
		case "already_exists":
			return fail(exitConflict, me.Code, "%s", me.Detail)
		case "ambiguous_team":
			return fail(exitUsage, me.Code, "%s", me.Detail)
		case "unavailable":
			return fail(exitNoServer, me.Code, "%s", me.Detail)
		case "unknown_tool":
			return fail(exitError, "server_too_old", "%s", me.Detail)
		}
		return fail(exitError, orDefault(me.Code, "tool_error"), "%s", me.Detail)
	}
	return fail(exitError, "error", "%s", me.Detail)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// teams lists the session's teams.
func (a *app) listTeams(s *session) (wire.ListTeams, error) {
	var lt wire.ListTeams
	if err := s.c.Call(a.ctx(), "list_teams", nil, &lt); err != nil {
		return lt, a.mapErr(err)
	}
	return lt, nil
}

// teamState reads one scope of a team, following pages up to max rows.
func (a *app) teamState(s *session, slug, scope string, max int) (wire.TeamState, error) {
	var all wire.TeamState
	cursor := ""
	for {
		args := map[string]any{"team_slug": slug, "scope": scope, "limit": 50}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var page wire.TeamState
		if err := s.c.Call(a.ctx(), "get_team_state", args, &page); err != nil {
			return all, a.mapErr(err)
		}
		if all.Team == nil {
			all = page
		} else {
			all.Sessions = append(all.Sessions, page.Sessions...)
			all.Projects = append(all.Projects, page.Projects...)
			all.Members = append(all.Members, page.Members...)
		}
		cursor = page.NextCursor
		n := len(all.Sessions) + len(all.Projects) + len(all.Members)
		if cursor == "" || n >= max {
			return all, nil
		}
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
