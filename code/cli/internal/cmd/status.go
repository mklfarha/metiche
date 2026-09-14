package cmd

import (
	"fmt"
	"strings"

	"github.com/mklfarha/metiche/cli/internal/config"
	"github.com/mklfarha/metiche/cli/internal/credential"
	"github.com/mklfarha/metiche/cli/internal/wire"
)

func (a *app) cmdStatus(args []string) error {
	fs := a.flags("status")
	onlyTeam := fs.String("team", "", "show one team")
	noClients := fs.Bool("no-clients", false, "skip the per-client whoami calls")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	h, err := a.health()
	if err != nil {
		return a.mapErr(err)
	}
	if !h.OK || h.Database != "reachable" {
		return fail(exitNoServer, "unreachable", "metiche at %s is up but its database is not; retry in a few minutes", a.endpoint.URL)
	}
	s, err := a.connect(false)
	if err != nil {
		return err
	}
	defer s.close()
	lt, err := a.listTeams(s)
	if err != nil {
		return err
	}
	teams := lt.Teams
	if *onlyTeam != "" {
		teams = nil
		for _, t := range lt.Teams {
			if t.Slug == strings.ToLower(*onlyTeam) {
				teams = append(teams, t)
			}
		}
		if len(teams) == 0 {
			return fail(exitRefused, "not_found", "%s is not one of your teams", *onlyTeam)
		}
	}

	machineID := config.MachineID(a.env, a.hostname)
	type clientRow struct {
		Client, ClientKey, AgentKey, LastRequestAt, State string
	}
	var clients []clientRow
	if !*noClients {
		for _, c := range credential.Clients {
			e := credential.ReadEntry(a.paths, c)
			row := clientRow{Client: c}
			switch {
			case !e.FileExists || !e.Present:
				row.State = "not configured"
			case e.Token.Empty():
				row.State = "no token in its config"
			default:
				cl, w, err := a.whoamiWith(e.Token)
				if err != nil {
					row.State = "token rejected or unreachable (run metiche doctor)"
				} else {
					cl.Close()
					if w.Agent != nil {
						row.ClientKey, row.AgentKey = w.Agent.ClientKey, w.Agent.Key
					}
					row.LastRequestAt = lastClientRequest(w)
					row.State = "ok"
				}
			}
			clients = append(clients, row)
		}
	}

	base := config.BoardBase(a.env)
	type teamOut struct {
		choice   wire.TeamChoice
		state    wire.TeamState
		sessions []wire.StateSession
	}
	var outs []teamOut
	for _, t := range teams {
		ps, err := a.teamState(s, t.Slug, "projects", 100)
		if err != nil {
			return err
		}
		ss, err := a.teamState(s, t.Slug, "sessions", 100)
		if err != nil {
			return err
		}
		outs = append(outs, teamOut{choice: t, state: ps, sessions: ss.Sessions})
	}

	agentKey := ""
	if s.who.Agent != nil {
		agentKey = s.who.Agent.Key
	}
	if a.json {
		d := doc("status")
		d["endpoint"] = a.endpoint.URL
		d["server"] = map[string]any{"protocol_version": h.ProtocolVersion, "database": h.Database}
		d["credential"] = map[string]any{"source": s.source, "path": nullable(s.path), "client": nullable(s.client), "token_scope": s.who.TokenScope, "agent_key": nullable(agentKey)}
		d["account_key"] = s.who.AccountKey
		cl := []map[string]any{}
		for _, c := range clients {
			cl = append(cl, map[string]any{"client": c.Client, "state": c.State, "client_key": nullable(c.ClientKey), "agent_key": nullable(c.AgentKey), "last_request_at": nullable(c.LastRequestAt)})
		}
		d["machine"] = map[string]any{"machine_id": machineID, "clients": cl}
		ts := []map[string]any{}
		for _, o := range outs {
			vis := ""
			if o.state.Team != nil {
				vis = o.state.Team.Visibility
			}
			var boardURL any
			note := ""
			if vis == "public" {
				boardURL = config.BoardURL(base, o.choice.Slug)
			} else {
				note = "private team: a 404 until metiche open signs this browser in"
			}
			projects := o.state.Projects
			if projects == nil {
				projects = []wire.StateProject{}
			}
			ts = append(ts, map[string]any{"slug": o.choice.Slug, "name": o.choice.Name, "role": o.choice.Role, "members": o.choice.Members,
				"active_session": o.choice.ActiveSession, "visibility": vis, "board_url": boardURL, "board_note": nullable(note),
				"projects": projects, "sessions": nonNilSessions(o.sessions)})
		}
		d["teams"] = ts
		a.writeJSON(d)
		return nil
	}

	a.out("metiche   %s · server %s · database %s", a.endpoint.URL, h.ProtocolVersion, h.Database)
	cred := "credential: " + s.source
	if s.path != "" {
		cred += " " + s.path
	}
	if s.client != "" {
		cred += " (" + credential.Titles[s.client] + "'s token)"
	}
	if agentKey != "" {
		cred += fmt.Sprintf(" (agent %s)", agentKey)
	} else {
		cred += " (" + s.who.TokenScope + " token)"
	}
	a.out("you       account %s · %s", s.who.AccountKey, cred)
	if !*noClients {
		a.out("")
		a.out("this machine (%s)", machineID)
		for _, c := range clients {
			if c.State != "ok" {
				a.out("  %-12s %s", credential.Titles[c.Client], c.State)
				continue
			}
			last := "no request from the client since the server started"
			if c.LastRequestAt != "" {
				last = "last request " + a.ago(c.LastRequestAt)
			}
			a.out("  %-12s %s  agent %s  %s", credential.Titles[c.Client], c.ClientKey, c.AgentKey, last)
		}
	}
	a.out("")
	if len(outs) == 0 {
		a.out("teams (0)  %s", lt.Note)
		return nil
	}
	a.out("teams (%d)", len(outs))
	for _, o := range outs {
		vis := ""
		if o.state.Team != nil {
			vis = o.state.Team.Visibility
		}
		live := ""
		if o.choice.ActiveSession {
			live = " · you are live here"
		}
		a.out("  %s  %s   %s · %s · %s%s", o.choice.Slug, o.choice.Name, vis, o.choice.Role, plural(o.choice.Members, "member", "members"), live)
		if vis == "public" {
			a.out("    board     %s", config.BoardURL(base, o.choice.Slug))
		} else {
			a.out("    board     %s   private: metiche open signs this browser in", config.BoardURL(base, o.choice.Slug))
		}
		if len(o.state.Projects) == 0 {
			a.out("    projects  (none yet)")
		}
		for i, p := range o.state.Projects {
			label := "projects"
			if i > 0 {
				label = "        "
			}
			a.out("    %s  %s   %d live   last activity %s", label, p.Key, p.LiveSessions, a.ago(p.LastActivityAt))
		}
		if len(o.sessions) == 0 {
			a.out("    live      (nobody)")
		}
		for i, x := range o.sessions {
			label := "live    "
			if i > 0 {
				label = "        "
			}
			mine := ""
			if x.Mine {
				mine = "  mine"
			}
			a.out("    %s  %s  %s · %s  %s  %s  %q  %s%s", label, x.Key, x.Member, x.Agent, x.Project, x.Branch, x.StatusLine, a.ago(x.LastSeen), mine)
		}
	}
	return nil
}

// lastClientRequest is the newest recent request that did not come from this
// CLI: the client's own.
func lastClientRequest(w wire.Whoami) string {
	latest := ""
	for _, r := range w.RecentRequests {
		if strings.HasPrefix(r.UserAgent, "metiche-cli/") {
			continue
		}
		if r.LastAt > latest {
			latest = r.LastAt
		}
	}
	return latest
}
