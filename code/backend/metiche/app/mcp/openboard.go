package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	account_entity "github.com/mklfarha/metiche/backend/entity/account"
	team_entity "github.com/mklfarha/metiche/backend/entity/team"
	"github.com/mklfarha/metiche/backend/enums"

	"github.com/mklfarha/metiche/backend/app/browser"
)

// ─────────────────────────────────────────────
// Tool: open_board
// ─────────────────────────────────────────────
//
// Mints a single-use, ten-minute sign-in link for the board
// (docs/BOARD_LOGIN.md §2.3). The person's own terminal already holds a
// credential — this client's agent token — so "open my board" needs no
// password and no sign-up: the token mints a link, the browser spends it, and
// the board holds a session for the ACCOUNT from then on.
//
// What this file guarantees, and where:
//
//   - AN AGENT TOKEN, NOT A PERSON'S. A legacy account token names nobody's
//     client, and revoking a lost laptop's browsers works by retiring that
//     laptop's agent (decision 3 and 4, §9). So a legacy token is refused with
//     the installer hint, and the agent is re-read so a retirement since the
//     token resolved is honoured. A retired agent never reaches this code: its
//     token is a 401 at authMiddleware.
//   - THE SECRET IS SHOWN ONCE. browser.MintLink stores only its hash. This
//     tool writes no team_event (a link is not board state), so there is no
//     response_snapshot to hold it, takes no idempotency_key (a replay would
//     have to return the same secret, which means storing it), and logs
//     nothing but the fact of a failure. The one place the secret exists after
//     this returns is the caller's result.
//   - NO OPEN REDIRECT. The destination is decided HERE from the caller's own
//     memberships and stored server-side; the link's fragment carries only the
//     secret, and MintLink refuses any path that is not "/", "/teams" or
//     "/t/<slug>".
//   - BOUNDED. 30 mints an hour per account (openBoard limiter, ratelimit.go)
//     and at most 5 outstanding links (MintLink, under the account row lock).
//     Keyed on the account, so the untrusted client address (F4) plays no part.

// errOpenBoardNeedsAgent is decision 3 (§9), word for word.
var errOpenBoardNeedsAgent = errors.New(
	"not_permitted: open_board needs this client's own token; re-run the metiche installer")

// OpenBoardParams: both optional. With no team_slug the destination follows
// the caller's memberships.
type OpenBoardParams struct {
	TeamSlug     string `json:"team_slug,omitempty" jsonschema:"The team whose board to open, if you know it (from a .metiche file or list_teams). Omit it and the link opens your only team's board, or your list of teams when you are on several."`
	RequestedVia string `json:"requested_via,omitempty" jsonschema:"Who is asking: agent (the default, when a person asked you), cli or installer. Recorded for the person's account page only."`
}

// OpenBoardTeam is the team the link lands on, when it lands on one.
type OpenBoardTeam struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// OpenBoardResult is §2.3's result. Not an Envelope: a link is not an event on
// a team, there is no sequence it advanced, and with no team_slug there may be
// no team at all — the reasoning ListTeamsResult gives.
type OpenBoardResult struct {
	OK bool `json:"ok"`
	// Team is absent when the link opens "/" or "/teams".
	Team *OpenBoardTeam `json:"team,omitempty"`
	// BoardURL is the plain address, no secret: safe to share.
	BoardURL string `json:"board_url"`
	// LoginURL carries the secret in its fragment. Shown once.
	LoginURL       string    `json:"login_url"`
	LoginExpiresAt time.Time `json:"login_expires_at"`
	SingleUse      bool      `json:"single_use"`
	Note           string    `json:"note"`
}

const openBoardNote = "Give login_url to the person once, or open it for them. It signs ONE browser in as this account, " +
	"works once, and expires at login_expires_at. Do not write it to a file, a commit, an issue or a chat channel. " +
	"board_url is the plain address to share."

// OpenBoard mints a sign-in link for the caller's account.
func (h *Handler) OpenBoard(ctx context.Context, _ *mcp.CallToolRequest, in OpenBoardParams) (*mcp.CallToolResult, any, error) {
	acct, err := h.requireAccount(ctx)
	if err != nil {
		return nil, nil, err
	}
	id, _ := IdentityFromContext(ctx)
	if id.Agent == nil {
		return nil, nil, errOpenBoardNeedsAgent
	}
	// Re-read, as RequireAgent does: the token resolved moments ago, but a
	// retirement in between must still win. MintLink checks it again inside
	// its transaction.
	ag, err := h.agentByID(ctx, id.Agent.ID)
	if err != nil {
		return nil, nil, err
	}
	if ag.AccountUUID != acct.ID || ag.Status != enums.AGENT_STATUS_ACTIVE {
		return nil, nil, errOpenBoardNeedsAgent
	}

	via, err := parseRequestedVia(in.RequestedVia)
	if err != nil {
		return nil, nil, err
	}
	base, err := browser.BoardBaseURLFromEnv()
	if err != nil {
		// A deploy problem, not the caller's. The configured value is not a
		// secret, but it is not the caller's business either.
		h.logger.Error("open_board: METICHE_BOARD_BASE_URL is not a valid board base URL", zap.Error(err))
		return nil, nil, errors.New(
			"unavailable: this metiche server is not configured with a valid board address, so it cannot make sign-in links. " +
				"Tell whoever runs it; board_url and sign-in are both affected")
	}

	if ok, wait := h.accountLimiters().openBoard.Allow(acct.ID.String()); !ok {
		return nil, nil, fmt.Errorf(
			"rate_limited: too many sign-in links for this account in the last hour; try again in %s", wait)
	}

	redirect, team, err := h.boardDestination(ctx, acct, in.TeamSlug)
	if err != nil {
		return nil, nil, err
	}

	link, err := browser.MintLink(ctx, h.core.DB(), browser.MintLinkRequest{
		AccountUUID:  acct.ID,
		AgentUUID:    ag.ID,
		RedirectPath: redirect,
		RequestedVia: via,
		BoardBaseURL: base,
	})
	switch {
	case errors.Is(err, browser.ErrTooManyLinks):
		return nil, nil, fmt.Errorf(
			"rate_limited: this account already has %d unused sign-in links. Use one of them, or wait for them to expire "+
				"(each lasts %s) before asking for another", browser.MaxOutstandingLinks, browser.LinkTTL)
	case errors.Is(err, browser.ErrMintRefused):
		return nil, nil, errOpenBoardNeedsAgent
	case err != nil:
		// browser's errors never carry the secret (MintLink returns it only
		// in MintedLink), so the error is safe to log.
		h.logger.Warn("open_board: could not mint a sign-in link", zap.Error(err))
		return nil, nil, errors.New(
			"unavailable: could not create a sign-in link right now; nothing was stored. Retry in a few seconds, " +
				"or call health to check whether metiche is up")
	}

	return jsonValue(OpenBoardResult{
		OK:             true,
		Team:           team,
		BoardURL:       link.BoardURL,
		LoginURL:       link.LoginURL,
		LoginExpiresAt: link.ExpiresAt.UTC(),
		SingleUse:      true,
		Note:           openBoardNote,
	})
}

// boardDestination decides where the link lands:
//
//   - team_slug given: that team, after RequireTeam's live-membership check;
//   - otherwise, from this account's live memberships of ACTIVE teams:
//     exactly one → /t/<slug>; several or none → /teams (§2.3).
func (h *Handler) boardDestination(ctx context.Context, acct account_entity.Account, slug string) (string, *OpenBoardTeam, error) {
	if strings.TrimSpace(slug) != "" {
		res, err := h.RequireTeam(ctx, slug)
		if err != nil {
			return "", nil, err
		}
		return teamDestination(res.Team)
	}

	members, err := h.liveMemberships(ctx, acct.ID)
	if err != nil {
		return "", nil, err
	}
	var teams []team_entity.Team
	for _, m := range members {
		t, err := h.teamByID(ctx, m.TeamUUID)
		if err != nil {
			return "", nil, err
		}
		if t.Status == enums.RECORD_STATUS_ACTIVE {
			teams = append(teams, t)
		}
	}
	switch len(teams) {
	case 1:
		return teamDestination(teams[0])
	default:
		// Several teams, or none: the signed-in "your teams" page (§2.3). With
		// none it is still the right place — it is where joining one starts.
		return browser.RedirectTeams, nil, nil
	}
}

func teamDestination(t team_entity.Team) (string, *OpenBoardTeam, error) {
	path, err := browser.TeamRedirectPath(t.Slug)
	if err != nil {
		return "", nil, fmt.Errorf("invalid_request: team %q has a slug the board cannot open", t.Slug)
	}
	return path, &OpenBoardTeam{Slug: t.Slug, Name: t.Name, Visibility: t.Visibility.String()}, nil
}

// parseRequestedVia maps the client's word to the enum. Empty is agent, the
// caller this tool is mostly for.
func parseRequestedVia(v string) (enums.BoardLinkSource, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "agent":
		return enums.BOARD_LINK_SOURCE_AGENT, nil
	case "cli":
		return enums.BOARD_LINK_SOURCE_CLI, nil
	case "installer":
		return enums.BOARD_LINK_SOURCE_INSTALLER, nil
	}
	return enums.BOARD_LINK_SOURCE_INVALID, errors.New(
		"invalid_request: requested_via must be agent, cli or installer (or omitted)")
}
