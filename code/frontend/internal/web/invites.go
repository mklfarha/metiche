package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/mklfarha/metiche/frontend/internal/feed"
	"github.com/mklfarha/metiche/frontend/internal/view"
)

// Invites (docs/BOARD_LOGIN.md §10.10): the one place the board changes
// anything, and only for a SIGNED-IN MEMBER of the team.
//
//	GET  /t/{slug}/invites                      the page: create form and list
//	POST /t/{slug}/invites                      create; the answer IS the page with the code
//	POST /t/{slug}/invites/{invite}/revoke      revoke; 303 back to the page
//
// Everybody who is not a signed-in live member — anonymous, a signed-in
// non-member, a demo board — gets the NotFound page an unknown slug gets, with
// its 404. The backend decides membership on every call; nothing here is
// cached.
//
// THE CODE. It exists in exactly one response: the POST that created it,
// rendered as a page (not a redirect, which would have to carry the code in a
// URL or a cookie to survive). That response is Cache-Control: private,
// no-store, has no Location, and the board keeps no copy, so a reload — a GET —
// cannot show it again.
//
// The POSTs pass the same checks as sign-out: the Origin / Sec-Fetch-Site check
// and the session's CSRF token (§6.3), both before anything reaches the
// backend.

// Bounds shown on the form. Display only: app/mcp's inviteBounds enforces them
// server-side, for the board and for the MCP tools alike.
const (
	inviteDefaultMaxUses = 1
	inviteDefaultHours   = 7 * 24
	inviteOwnerMaxUses   = 100
	inviteOwnerMaxHours  = 30 * 24
	inviteMemberMaxUses  = 25
	inviteMemberMaxHours = 7 * 24
	maxInviteForm        = 4096
)

// inviteIDPattern bounds an invite id taken from a URL before it is sent on.
var inviteIDPattern = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)

// refusalCode is the machine prefix of a backend refusal ("not_permitted: ").
var refusalCode = regexp.MustCompile(`^[a-z_]+: `)

const unavailableText = "metiche is unavailable right now; try again shortly"

// inviteReq is a request that has reached a team: the viewer is signed in and
// the board resolved. Membership is still the backend's call, made next.
type inviteReq struct {
	slug string
	v    *Viewer
	t    *Team
	r    *http.Request // carries the signed-in mark
	anon *http.Request // without it, for the page an ended session gets
}

// notFoundPage is the page (and status) an unknown team gets.
func (s *Server) notFoundPage(w http.ResponseWriter, r *http.Request, slug string) {
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, view.NotFound(slug))
}

// signedInViewer is step one of every invite route: login on and a valid
// session, or the response is already written — the unknown-team page for
// anybody anonymous, 503 (cookie kept) when the session could not be checked.
func (s *Server) signedInViewer(w http.ResponseWriter, r *http.Request, slug string) (*Viewer, bool) {
	if s.login == nil {
		s.notFoundPage(w, r, slug)
		return nil, false
	}
	v, vs := s.viewer(w, r)
	switch vs {
	case viewerSignedIn:
		return v, true
	case viewerUnavailable:
		http.Error(w, unavailableText, http.StatusServiceUnavailable)
	default:
		s.notFoundPage(w, r, slug)
	}
	return nil, false
}

// inviteTeam resolves the board for a signed-in viewer, exactly as a board
// page does. A demo board has no invites: it is the unknown-team page.
func (s *Server) inviteTeam(w http.ResponseWriter, r *http.Request, v *Viewer, slug string) (*inviteReq, bool) {
	q := &inviteReq{slug: slug, v: v, anon: r, r: s.withViewer(r, v)}
	t, res := s.resolveTeam(r.Context(), v, viewerSignedIn, slug)
	switch res {
	case resolveFound:
		if t.Demo {
			s.notFoundPage(w, q.r, slug)
			return nil, false
		}
		q.t = t
		return q, true
	case resolveUnavailable, resolveFull:
		http.Error(w, unavailableText, http.StatusServiceUnavailable)
	case resolveSessionOver:
		s.login.clearCookie(w)
		s.notFoundPage(w, q.anon, slug)
	default:
		s.notFoundPage(w, q.r, slug)
	}
	return nil, false
}

// inviteDenied renders the backend's 404 (or 401) for an invite call: the
// viewer is not a live member, or the session ended. recheckRefused tells the
// two apart, exactly as for a refused board.
func (s *Server) inviteDenied(w http.ResponseWriter, q *inviteReq, err error) {
	switch s.refused(q.r.Context(), q.v, errors.Is(err, feed.ErrSessionInvalid)) {
	case resolveSessionOver:
		s.login.clearCookie(w)
		s.notFoundPage(w, q.anon, q.slug)
	case resolveUnavailable:
		http.Error(w, unavailableText, http.StatusServiceUnavailable)
	default:
		s.notFoundPage(w, q.r, q.slug)
	}
}

// memberRequest marks a render as the page of a confirmed member, which is
// what shows the Invites tab.
func memberRequest(r *http.Request) *http.Request {
	vv := view.ViewerFrom(r.Context())
	if !vv.SignedIn {
		return r
	}
	vv.Member = true
	return r.WithContext(view.WithViewer(r.Context(), vv))
}

// viewerIsMember answers, for a board page, whether to show the Invites tab. A
// private board is only ever granted to a live member. A public board is
// granted to anyone, so the viewer's own memberships are asked for, uncached —
// GET /v1/browser/teams, not /access: a public team's requests never carry a
// session (TestBackendSeesOneCredentialPerRequest). Any failure is "no tab",
// never a failed page.
func (s *Server) viewerIsMember(ctx context.Context, v *Viewer, t *Team) bool {
	if v == nil || s.login == nil || t.Demo {
		return false
	}
	if t.viewer {
		return true
	}
	teams, err := s.login.cfg.Backend.Teams(ctx, v.secret)
	if err != nil {
		return false
	}
	for _, mine := range teams {
		if mine.Slug == t.Slug {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- GET

func (s *Server) invites(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	v, ok := s.signedInViewer(w, r, slug)
	if !ok {
		return
	}
	q, ok := s.inviteTeam(w, r, v, slug)
	if !ok {
		return
	}
	s.renderInvitesWithList(w, q, http.StatusOK, view.InvitesParams{})
}

// ---------------------------------------------------------------- POSTs

// invitePost runs the checks both POSTs share, in this order: login on, same
// origin (403), a session cookie (else the unknown-team page), its CSRF token
// (403), a valid session, and the board. Nothing reaches the backend's invite
// routes before all of them pass.
func (s *Server) invitePost(w http.ResponseWriter, r *http.Request) (*inviteReq, bool) {
	slug := chi.URLParam(r, "slug")
	l := s.login
	if l == nil {
		s.notFoundPage(w, r, slug)
		return nil, false
	}
	if !l.sameOrigin(r, false) {
		privateHeaders(w)
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	secret := l.cookieValue(r)
	if secret == "" {
		s.notFoundPage(w, r, slug)
		return nil, false
	}
	privateHeaders(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxInviteForm)
	if !csrfOK(r, secret) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return nil, false
	}
	v, ok := s.signedInViewer(w, r, slug)
	if !ok {
		return nil, false
	}
	return s.inviteTeam(w, r, v, slug)
}

// createInvite is POST /t/{slug}/invites. Its success answer is the page with
// the one-time panel — 200, no-store, no Location.
func (s *Server) createInvite(w http.ResponseWriter, r *http.Request) {
	q, ok := s.invitePost(w, r)
	if !ok {
		return
	}
	backend := s.login.cfg.Backend
	form := view.InviteForm{Label: r.PostFormValue("label")}
	maxUses, okUses := formNumber(r.PostFormValue("max_uses"))
	hours, okHours := formNumber(r.PostFormValue("expires_in_hours"))
	form.MaxUses, form.Hours = maxUses, hours
	if !okUses || !okHours {
		s.renderInvitesWithList(w, q, http.StatusUnprocessableEntity, view.InvitesParams{
			Form: form, Error: "Max uses and the expiry must be whole numbers.",
		})
		return
	}

	created, err := backend.CreateInvite(r.Context(), q.v.secret, q.slug, feed.InviteRequest{
		Label: form.Label, MaxUses: maxUses, ExpiresInHours: hours,
	})
	var refusedErr *feed.InviteRefused
	switch {
	case err == nil:
		// The invite exists and its code is in hand. Nothing after this may
		// lose it: a list that cannot be read is a note on the page, not a 503.
		p := view.InvitesParams{Created: &view.CreatedInvite{
			Code: created.Code, Label: created.Label, MaxUses: created.MaxUses,
			ExpiresAt: created.ExpiresAt, ShareNote: created.ShareNote,
		}}
		list, lerr := backend.Invites(r.Context(), q.v.secret, q.slug)
		if lerr != nil {
			s.log.Warn("listing invites after a create failed; the code is still shown", "err", lerr)
			p.ListUnavailable = true
		}
		s.renderInvites(w, q, http.StatusOK, list, p)
	case errors.As(err, &refusedErr):
		status := http.StatusUnprocessableEntity
		if refusedErr.Status == http.StatusTooManyRequests {
			status = http.StatusTooManyRequests
		}
		s.renderInvitesWithList(w, q, status, view.InvitesParams{Form: form, Error: refusalText(refusedErr.Message)})
	case errors.Is(err, feed.ErrNotFound), errors.Is(err, feed.ErrSessionInvalid):
		s.inviteDenied(w, q, err)
	default:
		s.log.Warn("creating an invite failed", "err", err)
		http.Error(w, unavailableText, http.StatusServiceUnavailable)
	}
}

// revokeInvite is POST /t/{slug}/invites/{invite}/revoke.
func (s *Server) revokeInvite(w http.ResponseWriter, r *http.Request) {
	q, ok := s.invitePost(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "invite")
	err := feed.ErrInviteNotFound
	if inviteIDPattern.MatchString(id) {
		err = s.login.cfg.Backend.RevokeInvite(r.Context(), q.v.secret, q.slug, id)
	}
	var refusedErr *feed.InviteRefused
	switch {
	case err == nil:
		// Nothing secret in this answer, so the usual redirect-after-POST.
		redirectAfterPost(w, r, "/t/"+url.PathEscape(q.slug)+"/invites")
	case errors.Is(err, feed.ErrInviteNotFound):
		s.renderInvitesWithList(w, q, http.StatusUnprocessableEntity, view.InvitesParams{
			Error: "That invite is not one you can revoke: it belongs to someone else, or it does not exist.",
		})
	case errors.As(err, &refusedErr):
		s.renderInvitesWithList(w, q, http.StatusUnprocessableEntity, view.InvitesParams{Error: refusalText(refusedErr.Message)})
	case errors.Is(err, feed.ErrNotFound), errors.Is(err, feed.ErrSessionInvalid):
		s.inviteDenied(w, q, err)
	default:
		s.log.Warn("revoking an invite failed", "err", err)
		http.Error(w, unavailableText, http.StatusServiceUnavailable)
	}
}

// ---------------------------------------------------------------- render

// renderInvitesWithList reads the list — which is also the membership check —
// and renders the page.
func (s *Server) renderInvitesWithList(w http.ResponseWriter, q *inviteReq, status int, p view.InvitesParams) {
	list, err := s.login.cfg.Backend.Invites(q.r.Context(), q.v.secret, q.slug)
	switch {
	case errors.Is(err, feed.ErrNotFound), errors.Is(err, feed.ErrSessionInvalid):
		s.inviteDenied(w, q, err)
		return
	case err != nil:
		s.log.Warn("listing invites failed", "err", err)
		http.Error(w, unavailableText, http.StatusServiceUnavailable)
		return
	}
	s.renderInvites(w, q, status, list, p)
}

func (s *Server) renderInvites(w http.ResponseWriter, q *inviteReq, status int, list feed.InviteList, p view.InvitesParams) {
	p.Slug = q.slug
	p.Owner = list.Owner()
	p.More = list.More
	p.Form.DefaultMaxUses, p.Form.DefaultHours = inviteDefaultMaxUses, inviteDefaultHours
	p.Form.MaxUsesCap, p.Form.HoursCap = inviteMemberMaxUses, inviteMemberMaxHours
	if p.Owner {
		p.Form.MaxUsesCap, p.Form.HoursCap = inviteOwnerMaxUses, inviteOwnerMaxHours
	}
	if p.Form.MaxUses == 0 {
		p.Form.MaxUses = inviteDefaultMaxUses
	}
	if p.Form.Hours == 0 {
		p.Form.Hours = inviteDefaultHours
	}
	for _, inv := range list.Invites {
		p.Invites = append(p.Invites, view.InviteRow{
			ID: inv.InviteID, Label: inv.Label, CreatedBy: inv.CreatedBy, CreatedByYou: inv.CreatedByYou,
			Uses: inv.Uses, MaxUses: inv.MaxUses, ExpiresAt: inv.ExpiresAt, State: inv.State,
			LastUsedAt: inv.LastUsedAt, CreatedAt: inv.CreatedAt,
			CanRevoke: p.Owner || inv.CreatedByYou,
		})
	}
	// Per viewer, and on a create a live door key: never stored by anybody.
	privateHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	s.page(w, memberRequest(q.r), q.t, q.t.Hub.Snapshot(), view.TabInvites, view.InvitesPage(p))
}

// formNumber reads an optional whole number: empty is 0 (the default).
func formNumber(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil
}

// refusalText is a backend refusal without its machine prefix, as a sentence.
func refusalText(msg string) string {
	msg = refusalCode.ReplaceAllString(strings.TrimSpace(msg), "")
	r, size := utf8.DecodeRuneInString(msg)
	if r == utf8.RuneError {
		return msg
	}
	return string(unicode.ToUpper(r)) + msg[size:]
}
