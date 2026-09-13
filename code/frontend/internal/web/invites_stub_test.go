package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// stubInvites is the backend's /v1/teams/{slug}/invites routes as
// app/webapi/invites.go serves them: a browser session only (a bearer is the
// team 404), a live member only, the owner sees and revokes everything and a
// member only their own, the same caps and the same refusal texts. Codes are
// obvious fakes. It lives on stubLogin and shares the backend's lock.
type stubInvites struct {
	owners  map[string]map[string]bool // slug -> account keys that own the team
	bySlug  map[string][]*stubInvite
	seq     int
	codes   []string         // every code ever returned, in order
	creates []map[string]any // every POST body that reached a create
	calls   int              // every request to an invite route

	limited bool // POST answers 429
	down    bool // every invite route answers 503
}

type stubInvite struct {
	id, label, creator, creatorName, state string
	uses, max                              int64
	expires, created                       time.Time
}

func newStubInvites() stubInvites {
	return stubInvites{owners: map[string]map[string]bool{}, bySlug: map[string][]*stubInvite{}}
}

// makeOwner makes account the owner (and so a member) of slug.
func (b *stubBackend) makeOwner(slug, account string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.login.members[slug] == nil {
		b.login.members[slug] = map[string]bool{}
	}
	b.login.members[slug][account] = true
	if b.login.inv.owners[slug] == nil {
		b.login.inv.owners[slug] = map[string]bool{}
	}
	b.login.inv.owners[slug][account] = true
}

// addPublicMember makes account a member of a public team: /access then
// answers its role instead of null.
func (b *stubBackend) addPublicMember(slug, account string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.login.members[slug] == nil {
		b.login.members[slug] = map[string]bool{}
	}
	b.login.members[slug][account] = true
}

func (b *stubBackend) setInvitesLimited(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.inv.limited = v
}

func (b *stubBackend) setInvitesDown(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.inv.down = v
}

func (b *stubBackend) inviteCreates() []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]map[string]any(nil), b.login.inv.creates...)
}

func (b *stubBackend) inviteCodes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.login.inv.codes...)
}

// inviteByLabel returns the id and state of the invite with label on slug.
func (b *stubBackend) inviteByLabel(slug, label string) (id, state string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, inv := range b.login.inv.bySlug[slug] {
		if inv.label == label {
			return inv.id, inv.state
		}
	}
	return "", ""
}

// roleLocked is /access's role for this request's session on slug. Called
// with b.mu held.
func (b *stubBackend) roleLocked(slug string, r *http.Request) any {
	s := b.login.sessions[r.Header.Get(feed.BrowserSessionHeader)]
	if s == nil || s.revoked || s.expired || !b.login.members[slug][s.account] {
		return nil
	}
	if b.login.inv.owners[slug][s.account] {
		return "owner"
	}
	return "member"
}

func problemJSON(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": title, "status": status, "detail": detail})
}

func (b *stubBackend) serveInvites(w http.ResponseWriter, r *http.Request, slug, sub string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total++
	iv := &b.login.inv
	iv.calls++
	w.Header().Set("Cache-Control", "no-store")
	if iv.down {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	cur := b.login.sessions[r.Header.Get(feed.BrowserSessionHeader)]
	_, public := b.public[slug]
	_, private := b.login.private[slug]
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-Metiche-Token") != "" ||
		cur == nil || cur.revoked || cur.expired || !(public || private) || !b.login.members[slug][cur.account] {
		problem404(w)
		return
	}
	owner := iv.owners[slug][cur.account]
	id, item := strings.CutPrefix(sub, "invites/")

	switch {
	case r.Method == http.MethodGet && sub == "invites":
		rows := []map[string]any{}
		list := append([]*stubInvite(nil), iv.bySlug[slug]...)
		sort.SliceStable(list, func(i, j int) bool { return list[i].created.After(list[j].created) })
		for _, inv := range list {
			if !owner && inv.creator != cur.account {
				continue
			}
			rows = append(rows, map[string]any{
				"invite_id": inv.id, "label": inv.label, "created_by": inv.creatorName,
				"created_by_you": inv.creator == cur.account, "uses": inv.uses, "max_uses": inv.max,
				"expires_at": inv.expires, "state": inv.state, "last_used_at": nil, "created_at": inv.created,
			})
		}
		scope := "created_by_you"
		if owner {
			scope = "team"
		}
		writeJSON(w, map[string]any{"ok": true, "team_slug": slug, "scope": scope, "invites": rows,
			"note": "Codes are shown only by create_invite, once."})

	case r.Method == http.MethodPost && sub == "invites":
		var in struct {
			Label          string `json:"label"`
			MaxUses        int    `json:"max_uses"`
			ExpiresInHours int    `json:"expires_in_hours"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			problemJSON(w, http.StatusBadRequest, "invalid_argument", "invalid_argument: the body must be a JSON object")
			return
		}
		iv.creates = append(iv.creates, map[string]any{"label": in.Label, "max_uses": in.MaxUses, "expires_in_hours": in.ExpiresInHours})
		if iv.limited {
			problemJSON(w, http.StatusTooManyRequests, "rate_limited",
				"rate_limited: too many invites created by this account in the last hour; try again in 59m0s")
			return
		}
		if in.MaxUses < 0 || in.ExpiresInHours < 0 {
			problemJSON(w, http.StatusBadRequest, "invalid_argument",
				"invalid_argument: max_uses and expires_in_hours must be positive; omit them for the defaults (1 use, 168 hours)")
			return
		}
		if in.MaxUses == 0 {
			in.MaxUses = 1
		}
		if in.ExpiresInHours == 0 {
			in.ExpiresInHours = 168
		}
		switch {
		case in.MaxUses > 100:
			problemJSON(w, http.StatusBadRequest, "invalid_argument", "invalid_argument: max_uses may be at most 100")
			return
		case in.ExpiresInHours > 720:
			problemJSON(w, http.StatusBadRequest, "invalid_argument", "invalid_argument: expires_in_hours may be at most 720 (30 days)")
			return
		case !owner && (in.MaxUses > 25 || in.ExpiresInHours > 168):
			problemJSON(w, http.StatusForbidden, "not_permitted",
				"not_permitted: members may create invites with at most 25 uses and 168 hours (7 days); the team's owner can make larger ones")
			return
		}
		iv.seq++
		inv := &stubInvite{
			id: fmt.Sprintf("00000000-0000-4000-8000-%012d", iv.seq), label: in.Label,
			creator: cur.account, creatorName: cur.name, state: "active", max: int64(in.MaxUses),
			created: b.login.now().UTC().Add(time.Duration(iv.seq) * time.Second),
			expires: b.login.now().UTC().Add(time.Duration(in.ExpiresInHours) * time.Hour),
		}
		iv.bySlug[slug] = append(iv.bySlug[slug], inv)
		code := fmt.Sprintf("FAKEINV%03d", iv.seq)
		iv.codes = append(iv.codes, code)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "team_slug": slug, "invite_id": inv.id, "label": inv.label, "code": code,
			"max_uses": inv.max, "expires_at": inv.expires, "state": "active", "created": true,
			"share_note": "Share this code like a door code: only with the people you want on " + slug + ". " +
				"Your teammate runs the metiche installer (curl -fsSL https://metiche.xyz/install.sh | sh), chooses join and pastes the code.",
		})

	case r.Method == http.MethodDelete && item:
		for _, inv := range iv.bySlug[slug] {
			if inv.id == id && (owner || inv.creator == cur.account) {
				inv.state = "revoked"
				writeJSON(w, map[string]any{"ok": true, "team_slug": slug, "invite_id": inv.id, "state": "revoked"})
				return
			}
		}
		problemJSON(w, http.StatusNotFound, "not_found",
			"not_found: no invite with that invite_id on team "+slug+" that you can revoke. Call list_invites to see the invites you can revoke")

	default:
		problem404(w)
	}
}
