package webapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/browser"
	metichemcp "github.com/mklfarha/metiche/backend/app/mcp"
	"github.com/mklfarha/metiche/backend/core"
	account_entity "github.com/mklfarha/metiche/backend/entity/account"
)

// Invite management for the board (docs/BOARD_LOGIN.md §10.10): list, create
// and revoke a team's invites, for a SIGNED-IN MEMBER only.
//
//	GET    /v1/teams/{slug}/invites               200 ListInvitesResult (never a code)
//	POST   /v1/teams/{slug}/invites               body {"label","max_uses","expires_in_hours"}
//	                                              201 CreateInviteResult (the code, this once)
//	DELETE /v1/teams/{slug}/invites/{invite_id}   200 RevokeInviteResult
//
//	404 {"title":"not found","detail":"no such team"}   unknown team, not a live member, no
//	    session, a session that does not validate, or ANY bearer credential — one set of bytes,
//	    the same notFound every other route in this package answers for an unknown slug
//	400 invalid_argument · 403 not_permitted · 429 rate_limited (Retry-After) · 404 not_found
//	    (an invite the caller may not revoke), each problem+json whose detail is the refusal's
//	    text, word for word what the MCP tool says
//	503 the decision could not be made (database), never a 404
//
// Every response carries Cache-Control: no-store: a create response holds a
// live join code, and every other answer is per viewer.
//
// # Why these are not behind authz.Middleware like the reads
//
// authz grants a PUBLIC team to anyone without reading the credential, which is
// right for reading a board and wrong here: minting a door key needs a member
// of any team, public or not. And authz accepts a bearer token, which this
// surface refuses: the board never holds one, so a bearer here is an agent or a
// script that belongs on the MCP tools, where the same rules already apply.
//
// # One source of truth
//
// The rules are app/mcp's CreateInviteAs, ListInvitesAs and RevokeInviteAs, on
// the SAME *mcp.Handler the MCP endpoint serves (app/rest.go passes it in), so
// the per-account create budget is shared with create_invite. Nothing here
// restates a default, a cap or a scope.
//
// BOARD ONLY; NOT ROUTED BY ANY INGRESS (app/rest.go AllowedRoutes,
// TestBrowserRoutesAreNotRoutedByAnyIngress).

// Route patterns, for app/rest.go's AllowedRoutes.
const (
	PathInvites = "/v1/teams/{slug}/invites"
	PathInvite  = "/v1/teams/{slug}/invites/{invite_id}"
)

// maxInviteBody bounds a create body: a label is at most 80 characters.
const maxInviteBody = 4 << 10

// InviteService is the part of app/mcp's Handler these routes use.
type InviteService interface {
	InviteScope(ctx context.Context, account account_entity.Account, teamSlug string) (metichemcp.Resolved, error)
	CreateInviteAs(ctx context.Context, rs metichemcp.Resolved, in metichemcp.InviteRequest) (metichemcp.CreateInviteResult, error)
	ListInvitesAs(ctx context.Context, rs metichemcp.Resolved) (metichemcp.ListInvitesResult, error)
	RevokeInviteAs(ctx context.Context, rs metichemcp.Resolved, inviteID string) (metichemcp.RevokeInviteResult, error)
}

// InvitesAPI serves the three routes.
type InvitesAPI struct {
	db     *sql.DB
	svc    InviteService
	logger *zap.Logger
	// base supplies this package's notFound and unavailable, so a refusal
	// here is the same bytes as an unknown slug on every other route.
	base *API
}

// NewInvitesAPI builds the routes over a database handle (for the session
// check) and the invite rules.
func NewInvitesAPI(db *sql.DB, svc InviteService, logger *zap.Logger) *InvitesAPI {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &InvitesAPI{db: db, svc: svc, logger: logger, base: &API{db: db, logger: logger}}
}

// RegisterOn mounts the routes on the ROOT router with full /v1 paths.
func (a *InvitesAPI) RegisterOn(r chi.Router) {
	r.Get(PathInvites, a.member(a.handleList))
	r.Post(PathInvites, a.member(a.handleCreate))
	r.Delete(PathInvite, a.member(a.handleRevoke))
}

// RegisterInvites wires the routes into the REST server, over the MCP
// endpoint's own Handler.
func RegisterInvites(r chi.Router, coreImpl *core.Implementation, svc InviteService, logger *zap.Logger) *InvitesAPI {
	a := NewInvitesAPI(coreImpl.DB(), svc, logger)
	a.RegisterOn(r)
	return a
}

type memberHandler func(w http.ResponseWriter, r *http.Request, rs metichemcp.Resolved)

// member is the gate: no bearer, a valid browser session, a live member of the
// team in the URL. Every refusal is notFound; a failure to decide is 503.
func (a *InvitesAPI) member(next memberHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if strings.TrimSpace(r.Header.Get("Authorization")) != "" || strings.TrimSpace(r.Header.Get("X-Metiche-Token")) != "" {
			a.base.notFound(w, r)
			return
		}
		secret := strings.TrimSpace(r.Header.Get(browser.HeaderSession))
		if secret == "" {
			a.base.notFound(w, r)
			return
		}
		acct, err := browser.AccountBySession(r.Context(), a.db, secret)
		switch {
		case errors.Is(err, browser.ErrUnauthenticated):
			a.base.notFound(w, r)
			return
		case err != nil:
			a.failed(w, r, err, "checking a browser session")
			return
		}
		rs, err := a.svc.InviteScope(r.Context(), acct, chi.URLParam(r, "slug"))
		switch {
		case errors.Is(err, metichemcp.ErrNoInviteScope):
			a.base.notFound(w, r)
			return
		case err != nil:
			a.failed(w, r, err, "checking a team membership")
			return
		}
		next(w, r, rs)
	}
}

func (a *InvitesAPI) handleList(w http.ResponseWriter, r *http.Request, rs metichemcp.Resolved) {
	out, err := a.svc.ListInvitesAs(r.Context(), rs)
	if err != nil {
		a.refuse(w, r, err, "listing invites")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// createInviteBody is the POST body. Zero (or absent) numbers mean the
// defaults, as they do for create_invite.
type createInviteBody struct {
	Label          string `json:"label"`
	MaxUses        int    `json:"max_uses"`
	ExpiresInHours int    `json:"expires_in_hours"`
}

func (a *InvitesAPI) handleCreate(w http.ResponseWriter, r *http.Request, rs metichemcp.Resolved) {
	var body createInviteBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxInviteBody))
	if err := dec.Decode(&body); err != nil {
		writeProblem(w, http.StatusBadRequest, metichemcp.InviteInvalidArgument,
			"invalid_argument: the body must be a JSON object {label, max_uses, expires_in_hours} with whole numbers")
		return
	}
	out, err := a.svc.CreateInviteAs(r.Context(), rs, metichemcp.InviteRequest{
		Label: body.Label, MaxUses: body.MaxUses, ExpiresInHours: body.ExpiresInHours,
	})
	if err != nil {
		a.refuse(w, r, err, "creating an invite")
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *InvitesAPI) handleRevoke(w http.ResponseWriter, r *http.Request, rs metichemcp.Resolved) {
	out, err := a.svc.RevokeInviteAs(r.Context(), rs, chi.URLParam(r, "invite_id"))
	if err != nil {
		a.refuse(w, r, err, "revoking an invite")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// refuse renders a refusal with its own text, and anything else as 503.
func (a *InvitesAPI) refuse(w http.ResponseWriter, r *http.Request, err error, what string) {
	var ref *metichemcp.InviteRefusal
	if !errors.As(err, &ref) {
		a.failed(w, r, err, what)
		return
	}
	status := http.StatusBadRequest
	switch ref.Code {
	case metichemcp.InviteNotPermitted:
		status = http.StatusForbidden
	case metichemcp.InviteRateLimited:
		status = http.StatusTooManyRequests
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(ref.RetryAfter.Seconds()))))
	case metichemcp.InviteNotFound:
		status = http.StatusNotFound
	}
	writeProblem(w, status, ref.Code, ref.Error())
}

// failed is every error that is not a verdict: logged, never echoed (driver
// text can carry a DSN; an invite code is already redacted out of insert
// errors by app/mcp), and answered 503.
func (a *InvitesAPI) failed(w http.ResponseWriter, r *http.Request, err error, what string) {
	if r.Context().Err() != nil {
		return
	}
	a.logger.Warn("board invite request failed; answering unavailable", zap.String("what", what), zap.Error(err))
	a.base.unavailable(w, r)
}
