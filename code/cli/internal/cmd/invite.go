package cmd

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mklfarha/metiche/cli/internal/wire"
)

func (a *app) cmdInvite(args []string) error {
	sub, rest := subcommand(args, "list", "create", "revoke")
	switch sub {
	case "list":
		return a.inviteList(rest)
	case "create":
		return a.inviteCreate(rest)
	case "revoke":
		return a.inviteRevoke(rest)
	}
	return fail(exitUsage, "usage", "usage: metiche invite list | create | revoke")
}

func uses(u int64, max *int64) string {
	if max == nil {
		return fmt.Sprintf("%d/∞", u)
	}
	return fmt.Sprintf("%d/%d", u, *max)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (a *app) inviteList(args []string) error {
	fs := a.flags("invite list")
	team := fs.String("team", "", "team slug")
	all := fs.Bool("all", false, "include exhausted, expired and revoked invites")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	s, err := a.connect(false)
	if err != nil {
		return err
	}
	defer s.close()
	ref, err := a.resolveTeam(s, *team)
	if err != nil {
		return err
	}
	var li wire.ListInvites
	if err := s.c.Call(a.ctx(), "list_invites", map[string]any{"team_slug": ref.slug}, &li); err != nil {
		return a.refused(a.mapErr(err), ref)
	}
	shown := []wire.InviteInfo{}
	for _, inv := range li.Invites {
		if *all || inv.State == "active" {
			shown = append(shown, inv)
		}
	}
	if a.json {
		d := doc("invite.list")
		d["team"], d["scope"], d["invites"], d["note"] = li.TeamSlug, li.Scope, shown, li.Note
		a.writeJSON(d)
		return nil
	}
	if len(shown) == 0 {
		a.out("no invites to show on %s%s.", ref.slug, map[bool]string{true: "", false: " (active only; --all shows the rest)"}[*all])
		return nil
	}
	rows := [][]string{{"ID", "LABEL", "USES", "EXPIRES", "STATUS", "CREATED BY"}}
	for _, inv := range shown {
		exp := "never"
		if inv.ExpiresAt != nil {
			exp = inv.ExpiresAt.UTC().Format("2006-01-02 15:04")
		}
		by := inv.CreatedBy
		if inv.CreatedByYou {
			by += " (you)"
		}
		rows = append(rows, []string{short(inv.InviteID), orDash(inv.Label), uses(inv.Uses, inv.MaxUses), exp, inv.State, by})
	}
	a.table(rows)
	if li.Scope == "created_by_you" {
		a.out("(you see only the invites you created)")
	}
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var durDays = regexp.MustCompile(`^(\d+)d$`)

// expiresHours reads 48h, 7d, 90m as whole hours, rounded up.
func expiresHours(v string) (int, error) {
	v = strings.TrimSpace(v)
	if m := durDays.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n * 24, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--expires must be a duration like 48h or 7d, got %q", v)
	}
	return int(math.Ceil(d.Hours())), nil
}

func (a *app) inviteCreate(args []string) error {
	fs := a.flags("invite create")
	team := fs.String("team", "", "team slug")
	label := fs.String("label", "", "a note to recognise the invite by")
	maxUses := fs.Int("max-uses", 0, "how many people can join with it (server default 1)")
	expires := fs.String("expires", "", "how long it works, e.g. 48h or 7d (server default 7d)")
	quiet := fs.Bool("quiet", false, "print only the join code")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fail(exitUsage, "usage", "invite create takes no arguments; a code is never an argument either")
	}
	callArgs := map[string]any{"idempotency_key": newIdempotencyKey()}
	if *label != "" {
		callArgs["label"] = *label
	}
	if *maxUses < 0 {
		return fail(exitUsage, "usage", "--max-uses must be positive")
	}
	if *maxUses > 0 {
		callArgs["max_uses"] = *maxUses
	}
	if *expires != "" {
		h, err := expiresHours(*expires)
		if err != nil {
			return fail(exitUsage, "usage", "%v", err)
		}
		callArgs["expires_in_hours"] = h
	}
	s, err := a.connect(false)
	if err != nil {
		return err
	}
	defer s.close()
	ref, err := a.resolveTeam(s, *team)
	if err != nil {
		return err
	}
	callArgs["team_slug"] = ref.slug
	var ci wire.CreateInvite
	if err := s.c.Call(a.ctx(), "create_invite", callArgs, &ci); err != nil {
		return a.refused(a.mapErr(err), ref)
	}
	if a.json {
		d := doc("invite.create")
		d["team_slug"], d["invite_id"], d["label"], d["max_uses"], d["expires_at"], d["state"], d["created"] =
			ci.TeamSlug, ci.InviteID, ci.Label, ci.MaxUses, ci.ExpiresAt.UTC().Format(time.RFC3339), ci.State, ci.Created
		d["join_code"] = ci.Code
		d["join_code_note"] = "Shown once. Share it only with the people you want on this team; it cannot be listed again."
		a.writeJSON(d)
		return nil
	}
	if *quiet {
		a.out("%s", ci.Code)
		return nil
	}
	a.out("created invite %s on %s · max %s · expires %s", short(ci.InviteID), ci.TeamSlug,
		plural(int(ci.MaxUses), "use", "uses"), ci.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	a.out("")
	a.out("  join code   %s   (shown once: share it with your teammates; it cannot be listed again)", ci.Code)
	a.out("")
	a.out("  They run:   curl -fsSL https://metiche.xyz/install.sh | sh")
	a.out("              (the installer asks for the code; never put it on a command line)")
	return nil
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (a *app) inviteRevoke(args []string) error {
	fs := a.flags("invite revoke")
	team := fs.String("team", "", "team slug")
	pos, err := a.parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail(exitUsage, "usage", "usage: metiche invite revoke <invite-id> [--team <slug>]")
	}
	id := strings.ToLower(strings.TrimSpace(pos[0]))
	if !uuidShape.MatchString(id) && len(id) < 8 {
		return fail(exitUsage, "usage", "give the full invite id or at least its first 8 characters")
	}
	s, err := a.connect(false)
	if err != nil {
		return err
	}
	defer s.close()
	ref, err := a.resolveTeam(s, *team)
	if err != nil {
		return err
	}
	if !uuidShape.MatchString(id) {
		var li wire.ListInvites
		if err := s.c.Call(a.ctx(), "list_invites", map[string]any{"team_slug": ref.slug}, &li); err != nil {
			return a.refused(a.mapErr(err), ref)
		}
		var matches []string
		for _, inv := range li.Invites {
			if strings.HasPrefix(inv.InviteID, id) {
				matches = append(matches, inv.InviteID)
			}
		}
		switch len(matches) {
		case 0:
			return fail(exitRefused, "not_found", "no invite starting %s on %s that you can revoke; see `metiche invite list --all`", id, ref.slug)
		case 1:
			id = matches[0]
		default:
			e := fail(exitUsage, "ambiguous_invite", "%s matches %d invites; give more of the id", id, len(matches))
			e.extra = map[string]any{"candidates": matches}
			return e
		}
	}
	var rv wire.RevokeInvite
	if err := s.c.Call(a.ctx(), "revoke_invite", map[string]any{"team_slug": ref.slug, "invite_id": id}, &rv); err != nil {
		return a.refused(a.mapErr(err), ref)
	}
	if a.json {
		d := doc("invite.revoke")
		d["team_slug"], d["invite_id"], d["state"], d["already_revoked"], d["uses"], d["max_uses"] = rv.TeamSlug, rv.InviteID, rv.State, rv.AlreadyRevoked, rv.Uses, rv.MaxUses
		a.writeJSON(d)
		return nil
	}
	if rv.AlreadyRevoked {
		a.out("already revoked: invite %s (%q). Nothing changed.", short(rv.InviteID), rv.Label)
		return nil
	}
	spent := fmt.Sprintf("%d uses spent", rv.Uses)
	if rv.MaxUses != nil {
		spent = fmt.Sprintf("%d of %d uses spent", rv.Uses, *rv.MaxUses)
	}
	a.out("revoked invite %s (%q, %s).", short(rv.InviteID), rv.Label, spent)
	a.out("People who already joined keep their access; nobody new can join with this code.")
	return nil
}
