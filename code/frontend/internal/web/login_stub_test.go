package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mklfarha/metiche/frontend/internal/feed"
)

// stubLogin is the backend's browser-session API and private-team access, as
// docs/BOARD_LOGIN.md §2.4, §2.7, §2.8 and §4.2 describe them. It lives on
// stubBackend and shares its lock.
type stubLogin struct {
	now func() time.Time

	private  map[string]string          // slug -> name
	members  map[string]map[string]bool // slug -> account keys
	sessions map[string]*stubSession    // by secret
	links    map[string]*stubSession    // link secret -> the session it mints; deleted once used

	down        bool // /v1/browser/* and /access answer 503
	sessionDown bool // GET /v1/browser/session alone answers 503

	sessionChecks int
	accessChecks  int
	exchanges     []map[string]string
	sawBearer     bool
	sessionReads  map[string]int // /v1/teams/{slug}[/...] reads carrying a session, per slug
}

type stubSession struct {
	secret, key, account, name, redirect string
	revoked                              bool
	// endReason and revokedAt are set when a session is revoked; expired marks
	// one that ran out. Both kinds stay in GET /v1/browser/sessions, as the
	// real backend lists them until the sweeper deletes them.
	endReason string
	revokedAt time.Time
	expired   bool
}

// end revokes s with an end_reason. Called with b.mu held.
func (l *stubLogin) end(s *stubSession, reason string) {
	s.revoked, s.endReason, s.revokedAt = true, reason, l.now().UTC()
}

func newStubLogin() stubLogin {
	return stubLogin{now: time.Now, private: map[string]string{}, members: map[string]map[string]bool{},
		sessions: map[string]*stubSession{}, links: map[string]*stubSession{}, sessionReads: map[string]int{}}
}

// observe records what a team read carried. Called with b.mu held.
func (l *stubLogin) observe(r *http.Request) {
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-Metiche-Token") != "" {
		l.sawBearer = true
	}
	if r.Header.Get(feed.BrowserSessionHeader) != "" {
		rest, _ := strings.CutPrefix(r.URL.Path, "/v1/teams/")
		slug, _, _ := strings.Cut(rest, "/")
		l.sessionReads[slug]++
	}
}

func (b *stubBackend) makePrivateTeam(slug, name string, accounts ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.public, slug)
	b.login.private[slug] = name
	if b.login.members[slug] == nil {
		b.login.members[slug] = map[string]bool{}
	}
	for _, a := range accounts {
		b.login.members[slug][a] = true
	}
}

func (b *stubBackend) removeMember(slug, account string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.login.members[slug], account)
}

// addSession creates a live session as if a link had been exchanged.
func (b *stubBackend) addSession(secret, key, account, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.sessions[secret] = &stubSession{secret: secret, key: key, account: account, name: name}
}

// addEndedSession adds a session that is no longer signed in. reason is an
// end_reason ("revoked", "revoked_by_agent", …), or "expired".
func (b *stubBackend) addEndedSession(secret, key, account, name, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &stubSession{secret: secret, key: key, account: account, name: name}
	if reason == "expired" {
		s.expired = true
	} else {
		b.login.end(s, reason)
	}
	b.login.sessions[secret] = s
}

func (b *stubBackend) addLink(link string, s stubSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.links[link] = &s
}

func (b *stubBackend) revoked(secret string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.login.sessions[secret]
	return s == nil || s.revoked
}

func (b *stubBackend) setDown(down bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.down = down
}

func (b *stubBackend) setSessionDown(down bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.sessionDown = down
}

func (b *stubBackend) loginCounts() (sessionChecks, accessChecks, exchanges int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.login.sessionChecks, b.login.accessChecks, len(b.login.exchanges)
}

// readableLocked is the private branch of Guard.Authorize: a valid session
// whose account is a live member. Called with b.mu held.
func (b *stubBackend) readableLocked(slug string, r *http.Request) (string, bool) {
	name, ok := b.login.private[slug]
	if !ok {
		return "", false
	}
	s := b.login.sessions[r.Header.Get(feed.BrowserSessionHeader)]
	if s == nil || s.revoked || !b.login.members[slug][s.account] {
		return "", false
	}
	return name, true
}

func problem404(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"title":"not found","detail":"no such team"}`))
}

func (b *stubBackend) serveAccess(w http.ResponseWriter, r *http.Request, slug string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.login.accessChecks++
	if b.login.down {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, ok := b.public[slug]; ok {
		writeJSON(w, map[string]any{"visibility": "public", "role": nil})
		return
	}
	if _, ok := b.readableLocked(slug, r); ok {
		writeJSON(w, map[string]any{"visibility": "private", "role": "member"})
		return
	}
	problem404(w)
}

func (b *stubBackend) serveBrowser(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.total++
	if r.Header.Get("Authorization") != "" {
		b.login.sawBearer = true
	}
	if b.login.down {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	cur := b.login.sessions[r.Header.Get(feed.BrowserSessionHeader)]
	valid := cur != nil && !cur.revoked && !cur.expired
	unauthorized := func() { http.Error(w, `{"title":"unauthorized"}`, http.StatusUnauthorized) }
	path := r.URL.Path

	switch {
	case r.Method == http.MethodPost && path == "/v1/browser/sessions":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		b.login.exchanges = append(b.login.exchanges, in)
		s, ok := b.login.links[in["link_secret"]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"title":"not found","detail":"no such sign-in link"}`))
			return
		}
		delete(b.login.links, in["link_secret"])
		b.login.sessions[s.secret] = s
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_secret": s.secret, "session_key": s.key, "expires_at": b.login.now().Add(30 * 24 * time.Hour).UTC(),
			"redirect_path": s.redirect, "account_key": s.account, "display_name": s.name,
		})
	case r.Method == http.MethodGet && path == "/v1/browser/session":
		b.login.sessionChecks++
		if b.login.sessionDown {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		if !valid {
			unauthorized()
			return
		}
		writeJSON(w, map[string]any{"session_key": cur.key, "account_key": cur.account, "display_name": cur.name,
			"expires_at": b.login.now().Add(24 * time.Hour).UTC()})
	case r.Method == http.MethodDelete && path == "/v1/browser/session":
		if !valid {
			unauthorized()
			return
		}
		b.login.end(cur, "signed_out")
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/v1/browser/sessions":
		if !valid {
			unauthorized()
			return
		}
		// Like the backend: every row of the account, ended and expired ones
		// included, each with its state.
		var list []map[string]any
		for _, s := range b.login.sessions {
			if s.account != cur.account {
				continue
			}
			row := map[string]any{"key": s.key, "created_at": b.login.now().UTC(), "user_agent": "test agent",
				"current": s == cur, "state": "live", "revoked_at": nil, "end_reason": ""}
			switch {
			case s.revoked:
				row["state"], row["revoked_at"], row["end_reason"] = "ended", s.revokedAt, s.endReason
			case s.expired:
				row["state"] = "expired"
			}
			list = append(list, row)
		}
		sort.Slice(list, func(i, j int) bool { return list[i]["key"].(string) < list[j]["key"].(string) })
		writeJSON(w, map[string]any{"sessions": list})
	case r.Method == http.MethodDelete && path == "/v1/browser/sessions":
		if !valid {
			unauthorized()
			return
		}
		for _, s := range b.login.sessions {
			if s.account == cur.account && !s.revoked && !s.expired {
				b.login.end(s, "signed_out_everywhere")
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/browser/sessions/"):
		if !valid {
			unauthorized()
			return
		}
		key := strings.TrimPrefix(path, "/v1/browser/sessions/")
		for _, s := range b.login.sessions {
			if s.key == key && s.account == cur.account && !s.revoked {
				b.login.end(s, "revoked")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		problem404(w)
	case r.Method == http.MethodGet && path == "/v1/browser/teams":
		if !valid {
			unauthorized()
			return
		}
		var teams []map[string]any
		for slug, name := range b.login.private {
			if b.login.members[slug][cur.account] {
				teams = append(teams, map[string]any{"slug": slug, "name": name, "visibility": "private", "role": "member"})
			}
		}
		writeJSON(w, map[string]any{"teams": teams})
	default:
		problem404(w)
	}
}
