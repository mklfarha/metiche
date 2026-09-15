package view

import (
	"bytes"
	"context"
	"html"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"github.com/mklfarha/metiche/frontend/internal/model"
	"github.com/mklfarha/metiche/frontend/internal/state"
)

var decisionsNow = time.Date(2026, 9, 15, 15, 0, 0, 0, time.UTC)

func renderHTML(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// flat is the rendered text with tags and runs of whitespace collapsed, so
// a line can be asserted the way a person reads it.
func flat(s string) string {
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

// section is the HTML of the card with this id: from its id to the end of
// its element (cards hold no nested article or card div of their own).
func section(t *testing.T, page, id string) string {
	t.Helper()
	at := strings.Index(page, `id="`+id+`"`)
	if at < 0 {
		t.Fatalf("no element with id %q", id)
	}
	rest := page[at:]
	for _, end := range []string{"</article>", `<div id="`} {
		if i := strings.Index(rest[1:], end); i >= 0 {
			rest = rest[:i+1]
		}
	}
	return rest
}

func decisionsSnapshot() state.Snapshot {
	return state.Snapshot{
		Team: model.Team{Slug: "test", Name: "Test"},
		Now:  decisionsNow,
		Decisions: []*model.Decision{
			// Scoped to a project, nothing judged: no judged line at all.
			{Key: "#errors-rfc7807", Title: "Errors are problem documents", Statement: "Every API error is an RFC 7807 document.",
				Status: "active", Revision: 1, DecidedBy: "Bob", ProjectKey: "shop", Scope: "api/**",
				RecordedAt: decisionsNow.Add(-3 * time.Hour), UpdatedAt: decisionsNow.Add(-3 * time.Hour)},
			// Team-wide and always-show, revised, every judged part set.
			{Key: "#auth-jwt-cookie", Title: "Auth is a JWT in an httpOnly cookie",
				Statement: "Sessions are a signed JWT in an httpOnly, Secure cookie.", Status: "active", AlwaysShow: true,
				Revision: 2, DecidedBy: "Ana", Scope: "internal/auth/**, web/src/auth/**",
				Rationale:  "One session mechanism for the web app and the API.",
				Supersedes: "#auth-bearer-header",
				RecordedAt: decisionsNow.Add(-2 * time.Hour), UpdatedAt: decisionsNow.Add(-20 * time.Minute),
				Judged:        model.DecisionJudged{NoConflict: 5, Conflict: 1, Unsure: 0, Pending: 2},
				OpenConflicts: []string{"CF-31"}},
			// Only one part not zero.
			{Key: "#ids-are-ulid", Title: "Ids are ULIDs", Statement: "Every id crossing an API boundary is a ULID.",
				Status: "active", Revision: 1, DecidedBy: "Ana", ProjectKey: "shop",
				RecordedAt: decisionsNow.Add(-time.Hour), UpdatedAt: decisionsNow.Add(-time.Hour),
				Judged: model.DecisionJudged{NoConflict: 1}},
			// Ended: never among the cards in force.
			{Key: "#auth-bearer-header", Title: "Bearer header", Statement: "Clients send Authorization: Bearer.",
				Status: "superseded", SupersededBy: "#auth-jwt-cookie", Revision: 1, DecidedBy: "Bob", ProjectKey: "shop",
				RecordedAt: decisionsNow.Add(-5 * time.Hour), EndedAt: decisionsNow.Add(-2 * time.Hour)},
		},
	}
}

// TestDecisionsPageCardsInForce: accepted cards only, always-show first, with
// the key and badges, title, statement, scope chips, the revision line, the
// judged line with its conflict links, and why folded; team-wide and scoped
// badges; zero parts, and an all-zero judged line, omitted.
func TestDecisionsPageCardsInForce(t *testing.T) {
	snap := decisionsSnapshot()
	page := renderHTML(t, DecisionsPage(snap, DecisionHistoryParams{Slug: "test", Now: decisionsNow, Demo: true, Rows: snap.PastDecisions()}))

	grid := page[strings.Index(page, `class="grid2"`):strings.Index(page, `id="past"`)]
	order := regexp.MustCompile(`id="(dec-[a-z0-9-]+)"`).FindAllStringSubmatch(grid, -1)
	if len(order) != 3 || order[0][1] != "dec-auth-jwt-cookie" {
		t.Fatalf("cards in force = %v, want 3 with the always-show one first", order)
	}
	if strings.Contains(grid, "#auth-bearer-header</a><span") || strings.Contains(grid, `id="dec-auth-bearer-header"`) {
		t.Fatal("a superseded decision is drawn among the decisions in force")
	}

	jwt := section(t, page, "dec-auth-jwt-cookie")
	jwtText := flat(jwt)
	for _, want := range []string{
		"#auth-jwt-cookie active always-show team-wide",
		"Auth is a JWT in an httpOnly cookie",
		"Sessions are a signed JWT in an httpOnly, Secure cookie.",
		"r2 · decided by Ana · 2h ago · revised 20m ago · supersedes #auth-bearer-header",
		"checked against 6 plans · 1 open conflict CF-31 · 2 waiting",
		"why One session mechanism for the web app and the API.",
	} {
		if !strings.Contains(jwtText, want) {
			t.Errorf("the always-show card does not read %q:\n%s", want, jwtText)
		}
	}
	for _, want := range []string{
		`<span class="dec-chip">internal/auth/**</span>`, `<span class="dec-chip">web/src/auth/**</span>`,
		`href="/t/test/conflicts#CF-31"`, `<details class="dec-why">`, `href="/t/test/decisions/history?key=%23auth-jwt-cookie"`,
	} {
		if !strings.Contains(jwt, want) {
			t.Errorf("the always-show card is missing %s", want)
		}
	}

	scoped := flat(section(t, page, "dec-errors-rfc7807"))
	if !strings.Contains(scoped, "#errors-rfc7807 active shop") || strings.Contains(scoped, "team-wide") || strings.Contains(scoped, "always-show") {
		t.Errorf("the scoped card's badges: %s", scoped)
	}
	if strings.Contains(scoped, "checked against") || strings.Contains(scoped, "waiting") || strings.Contains(scoped, "revised") {
		t.Errorf("the all-zero judged line or an r1 revision was drawn: %s", scoped)
	}
	if strings.Contains(section(t, page, "dec-errors-rfc7807"), "dec-judged") {
		t.Error("an all-zero judged line left an empty element behind")
	}

	one := flat(section(t, page, "dec-ids-are-ulid"))
	if !strings.Contains(one, "checked against 1 plan") || strings.Contains(one, "open conflict") || strings.Contains(one, "waiting") || strings.Contains(one, "· checked") {
		t.Errorf("zero parts were not omitted: %s", one)
	}
	if strings.Contains(page, "No decisions recorded") {
		t.Error("the empty state is shown next to decisions")
	}
	t.Logf("always-show card: %s", jwtText)
	t.Logf("scoped card: %s", scoped)
}

// TestDecisionsPagePastSection: superseded and revoked decisions under Past,
// newest first, with what replaced them, and Load older when there is a page.
func TestDecisionsPagePastSection(t *testing.T) {
	snap := decisionsSnapshot()
	revoked := &model.Decision{Key: "#ids-are-int", Title: "Integer ids", Statement: "Ids are integers.", Status: "revoked",
		Revision: 3, DecidedBy: "Bob", EndedAt: decisionsNow.Add(-30 * time.Minute)}
	p := DecisionHistoryParams{Slug: "test", Now: decisionsNow,
		Rows:     []*model.Decision{revoked, snap.PastDecisions()[0]},
		OlderURL: DecisionHistoryURL("test", "", "ZDF8MjAyNi0wOS0xNVQxMzowMjoxMVp8I2F1dGgtYmVhcmVyLWhlYWRlcg")}
	page := renderHTML(t, DecisionsPage(snap, p))

	pastAt := strings.Index(page, `id="past"`)
	if pastAt < 0 || strings.Index(page, `id="dec-auth-jwt-cookie"`) > pastAt {
		t.Fatal("the Past section is not below the decisions in force")
	}
	past := page[pastAt:]
	first, second := strings.Index(past, `id="past-dec-ids-are-int"`), strings.Index(past, `id="past-dec-auth-bearer-header"`)
	if first < 0 || second < first {
		t.Fatalf("past rows out of order or missing (%d, %d)", first, second)
	}
	bearer := flat(section(t, page, "past-dec-auth-bearer-header"))
	for _, want := range []string{"#auth-bearer-header superseded shop", "ended 2026-09-15 13:00 UTC", "superseded by #auth-jwt-cookie"} {
		if !strings.Contains(bearer, want) {
			t.Errorf("the superseded row does not read %q: %s", want, bearer)
		}
	}
	if !strings.Contains(flat(section(t, page, "past-dec-ids-are-int")), "#ids-are-int revoked team-wide") {
		t.Error("the revoked row lost its status")
	}
	for _, want := range []string{
		`id="decisions-more"`, "Load older decisions",
		`hx-get="/t/test/decisions/history?cursor=ZDF8MjAyNi0wOS0xNVQxMzowMjoxMVp8I2F1dGgtYmVhcmVyLWhlYWRlcg"`,
		`href="/t/test/decisions/history"`, `class="card dec-card settled"`,
	} {
		if !strings.Contains(past, want) {
			t.Errorf("the Past section is missing %s", want)
		}
	}
	t.Logf("superseded row: %s", bearer)
}

// TestDecisionsPageEmptyState: no decisions at all keeps the empty state and
// its hint, and a demo board offers no history link.
func TestDecisionsPageEmptyState(t *testing.T) {
	snap := state.Snapshot{Team: model.Team{Slug: "test"}, Now: decisionsNow}
	page := renderHTML(t, DecisionsPage(snap, DecisionHistoryParams{Slug: "test", Now: decisionsNow, Demo: true}))
	text := flat(page)
	for _, want := range []string{
		"No decisions recorded",
		"An agent records one with record_decision when the team settles something the rest of the code has to obey.",
		"No decision has been superseded or revoked yet.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("empty page does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(page, `class="grid2"`) || strings.Contains(page, "/decisions/history") {
		t.Error("the empty demo page drew a grid or a history link")
	}

	// Past decisions but none in force: still the empty block, told apart.
	snap.Decisions = []*model.Decision{{Key: "#old", Status: "revoked", EndedAt: decisionsNow}}
	page = renderHTML(t, DecisionsPage(snap, DecisionHistoryParams{Slug: "test", Now: decisionsNow, Demo: true, Rows: snap.PastDecisions()}))
	if !strings.Contains(page, "No decision in force") || !strings.Contains(page, `id="past-dec-old"`) {
		t.Errorf("a board with only past decisions:\n%s", flat(page))
	}
}

// TestDecisionHistoryPageOneDecision: ?key= is one decision and its revisions.
func TestDecisionHistoryPageOneDecision(t *testing.T) {
	snap := decisionsSnapshot()
	p := DecisionHistoryParams{Slug: "test", Now: decisionsNow, Key: "#auth-jwt-cookie", Rows: []*model.Decision{snap.AcceptedDecisions()[0]},
		Revisions: []model.DecisionRevision{
			{Sequence: 402, Kind: "decision_recorded", Summary: "backend revised #auth-jwt-cookie (r2)",
				Statement: "Sessions are a signed JWT in an httpOnly, Secure cookie.", OccurredAt: decisionsNow.Add(-20 * time.Minute)},
			{Sequence: 311, Kind: "decision_recorded", Summary: "backend recorded #auth-jwt-cookie",
				Statement: "Sessions are a JWT in a cookie.", OccurredAt: decisionsNow.Add(-2 * time.Hour)},
		}}
	text := flat(renderHTML(t, DecisionHistoryPage(snap, p)))
	for _, want := range []string{
		"← Decisions in force", "#auth-jwt-cookie", "Revisions",
		"Sessions are a signed JWT in an httpOnly, Secure cookie. recorded 2026-09-15 14:40 UTC · backend revised #auth-jwt-cookie (r2)",
		"Sessions are a JWT in a cookie. recorded 2026-09-15 13:00 UTC · backend recorded #auth-jwt-cookie",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the one-decision page does not read %q:\n%s", want, text)
		}
	}
}
