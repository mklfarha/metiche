package webapi

import (
	"net/http"

	"go.uber.org/zap"

	"github.com/mklfarha/metiche/backend/app/authz"
	"github.com/mklfarha/metiche/backend/enums"
)

// accessResponse is GET /v1/teams/{slug}/access.
//
// Role is a pointer so that "no membership" is JSON null rather than an empty
// string a client could mistake for a role.
type accessResponse struct {
	Visibility string  `json:"visibility"` // "public" | "private"
	Role       *string `json:"role"`       // "owner" | "member" | null
}

// handleAccess serves GET /v1/teams/{slug}/access.
//
// It is how the board decides, per request and uncached, what to do with a
// slug for a viewer (docs/BOARD_LOGIN.md §2.7, §4.2): a public team goes to
// the shared anonymous hub, a private one to a hub read with the viewer's own
// session. Everything that decides access has already happened in the guard
// wrapped around this handler, so:
//
//   - a refusal never gets here, and is the package's one byte-identical
//     notFound — the same bytes as an unknown slug;
//   - a private team only gets here for a live member, so its role is set;
//   - a public team gets here for anyone. Its role is looked up best-effort
//     from whatever credential came with the request, and a credential that
//     does not resolve (or a non-member's) is simply role null, never a
//     refusal: a garbage session must not make a public board disappear.
func (a *API) handleAccess(w http.ResponseWriter, r *http.Request) {
	team, ok := authz.TeamFromContext(r.Context())
	if !ok {
		// Unreachable through RegisterOn. Fail closed.
		a.notFound(w, r)
		return
	}

	role, err := a.roles.ViewerRole(r.Context(), team, authz.CredentialFromContext(r.Context()))
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		// Only a PUBLIC team reaches this (a private grant already carries
		// its role). Its grant never depended on the credential, so an outage
		// of the credential lookup must not take a public board down: the
		// answer degrades to role null. The board routes on visibility.
		a.logger.Warn("resolving the viewer's role on a public team failed; answering role null", zap.Error(err))
		role = enums.MEMBER_ROLE_INVALID
	}

	out := accessResponse{Visibility: "private"}
	if team.Public {
		out.Visibility = "public"
	}
	if role != enums.MEMBER_ROLE_INVALID {
		s := role.String()
		out.Role = &s
	}
	// The answer is per viewer and changes the moment a membership does.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
