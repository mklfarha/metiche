package coordination

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// docs/DUPLICATES.md §7.1. Every vector is the spec's, pinned as written: a row
// that disagrees with the code is a bug in one of the two, settled through the
// coordinator, never by editing the expectation.

type dupVector struct {
	a, b    string
	project string
	overlap bool   // the two sessions' claims overlap
	rule    string // expected ScoreDuplicate rule; "" = not a candidate
	why     string // expected reason for a non-candidate
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

func dupName(prefix string, i int, a, b string) string {
	slug := func(s string) string {
		s = strings.Trim(nonWord.ReplaceAllString(strings.ToLower(s), "_"), "_")
		if len(s) > 28 {
			s = s[:28]
		}
		return s
	}
	return fmt.Sprintf("%s_%02d_%s__%s", prefix, i, slug(a), slug(b))
}

func scorePair(v dupVector) (DuplicateTerms, DuplicateTerms, DuplicateScore) {
	a := DuplicateTermsOf(v.a, v.project)
	b := DuplicateTermsOf(v.b, v.project)
	return a, b, ScoreDuplicate(a, b, nil, v.overlap)
}

// §7.1 "Should be candidates (hackathon, no issue id)". The spec's "single
// concept" rule is Rule "words_single" (§9.1).
var dupShouldMatch = []dupVector{
	{a: "add login page", b: "build the login screen", rule: "words"},
	{a: "login form", b: "sign in page", rule: "words"},
	{a: "dark mode toggle", b: "add dark mode", rule: "words"},
	{a: "stripe checkout", b: "checkout flow with stripe", rule: "words"},
	{a: "docker setup", b: "set up docker compose", rule: "words_single"},
	{a: "leaderboard endpoint", b: "api route for the leaderboard", rule: "words"},
	{a: "realtime chat with websockets", b: "websocket chat", rule: "words"},
	{a: "openai summaries", b: "summarize notes with openai", rule: "words"},
	{a: "deploy to vercel", b: "vercel deployment", rule: "words"},
	{a: "navbar", b: "fix navbar on mobile", rule: "words_single"},
	{a: "upload profile pictures", b: "profile photo upload", rule: "words"},
	{a: "leaderboard", b: "leaderboard page", rule: "words_single"},
	{a: "Add POST /api/login: password check, mint session cookie, wire the handler", b: "implement login endpoint with refresh tokens and session cookie", rule: "words"},
	{a: "navbar icons", b: "navbar spacing tweaks", overlap: true, rule: "words_and_paths"},
}

// §7.1 "Should not be candidates". why: guard = layer guard decided it;
// c0 = no core key shared; s0 = nothing shared; s1 = one shared key.
var dupShouldNotMatch = []dupVector{
	{a: "login page", b: "login endpoint", why: "guard"},
	{a: "signup page", b: "login page", why: "c0"},
	{a: "user profile page", b: "user profile api", why: "guard"},
	{a: "fix typo in footer", b: "footer links", why: "s1"},
	{a: "add tests for auth", b: "auth middleware", why: "s1"},
	{a: "add loading spinner", b: "loading states on the dashboard", why: "s1"},
	{a: "seed the database", b: "database schema", why: "c0"},
	{a: "deploy to vercel", b: "vercel env vars", why: "s1"},
	{a: "chat ui", b: "chat backend", why: "guard"},
	{a: "logout button", b: "login page", why: "s0"},
	{a: "fix login bug", b: "login page styling", why: "s1"},
	{a: "map view of events", b: "events api", why: "guard"},
	{a: "refactor session store to redis", b: "session cookie expiry bug", why: "s1"},
	{a: "shop checkout", b: "shop search", project: "shop", why: "s0"},
	{a: "navbar icons", b: "navbar spacing tweaks", why: "s1"},
}

func TestDuplicateSpecShouldMatch(t *testing.T) {
	for i, v := range dupShouldMatch {
		t.Run(dupName("match", i+1, v.a, v.b), func(t *testing.T) {
			for _, flip := range []bool{false, true} {
				a, b, s := scorePair(v)
				if flip {
					s = ScoreDuplicate(b, a, nil, v.overlap)
				}
				t.Logf("keys A %v | keys B %v | shared %v core %d ratio %.2f -> candidate=%v rule=%q (flipped=%v)",
					a.Keys, b.Keys, s.Shared, s.Core, s.Ratio, s.Candidate, s.Rule, flip)
				if !s.Candidate || s.Rule != v.rule {
					t.Errorf("%q vs %q: candidate=%v rule=%q, want candidate rule %q", v.a, v.b, s.Candidate, s.Rule, v.rule)
				}
			}
		})
	}
}

func TestDuplicateSpecShouldNotMatch(t *testing.T) {
	for i, v := range dupShouldNotMatch {
		t.Run(dupName("nomatch", i+1, v.a, v.b), func(t *testing.T) {
			for _, flip := range []bool{false, true} {
				a, b, s := scorePair(v)
				if flip {
					a, b = b, a
					s = ScoreDuplicate(a, b, nil, v.overlap)
				}
				t.Logf("keys A %v | keys B %v | shared %v core %d ratio %.2f -> candidate=%v (%s, flipped=%v)",
					a.Keys, b.Keys, s.Shared, s.Core, s.Ratio, s.Candidate, v.why, flip)
				if s.Candidate {
					t.Errorf("%q vs %q: candidate by %q, want not a candidate (%s)", v.a, v.b, s.Rule, v.why)
				}
				switch v.why {
				case "guard":
					if !duplicateLayerGuard(a, b) {
						t.Errorf("want the layer guard to decide it; layers %v vs %v", a.Layers, b.Layers)
					}
				case "c0":
					if s.Core != 0 || len(s.Shared) == 0 {
						t.Errorf("want shared layer keys and core 0, got shared %v core %d", s.Shared, s.Core)
					}
				case "s0":
					if len(s.Shared) != 0 {
						t.Errorf("want nothing shared, got %v", s.Shared)
					}
				case "s1":
					if len(s.Shared) != 1 {
						t.Errorf("want exactly one shared key, got %v", s.Shared)
					}
				}
			}
		})
	}
}

// §4.2's worked examples, keys as the table prints them.
func TestDuplicateWorkedExampleKeys(t *testing.T) {
	cases := []struct {
		a, b         string
		keysA, keysB []string
		vagueB       []string
		candidate    bool
	}{
		{"add login page", "build the login screen", []string{"login", "page"}, []string{"login", "page"}, nil, true},
		{"login form", "sign in page", []string{"login", "page"}, []string{"login", "page"}, nil, true},
		{"dark mode toggle", "add dark mode", []string{"dark", "mode", "toggl"}, []string{"dark", "mode"}, nil, true},
		{"stripe checkout", "checkout flow with stripe", []string{"strip", "check"}, []string{"check", "flow", "strip"}, []string{"flow"}, true},
		{"docker setup", "set up docker compose", []string{"docke"}, []string{"docke", "compo"}, nil, true},
		{"login page", "login endpoint", []string{"login", "page"}, []string{"login", "endpo"}, nil, false},
		{"signup page", "login page", []string{"signu", "page"}, []string{"login", "page"}, nil, false},
		{"add tests for auth", "auth middleware", []string{"test", "auth"}, []string{"auth", "middl"}, nil, false},
	}
	for _, tc := range cases {
		a, b := DuplicateTermsOf(tc.a, ""), DuplicateTermsOf(tc.b, "")
		if !reflect.DeepEqual(a.Keys, tc.keysA) || !reflect.DeepEqual(b.Keys, tc.keysB) {
			t.Errorf("%q / %q: keys %v / %v, want %v / %v", tc.a, tc.b, a.Keys, b.Keys, tc.keysA, tc.keysB)
		}
		for _, k := range tc.vagueB {
			if !b.Vague[k] {
				t.Errorf("%q: %q should be vague", tc.b, k)
			}
		}
		if got := ScoreDuplicate(a, b, nil, false).Candidate; got != tc.candidate {
			t.Errorf("%q / %q: candidate %v, want %v", tc.a, tc.b, got, tc.candidate)
		}
	}
	if !DuplicateTermsOf("add tests for auth", "").Vague["test"] {
		t.Error("test must be vague")
	}
}

// ─────────────────────────────────────────────
// Our own hackathon pairs
// ─────────────────────────────────────────────

// dupOwnPair is a pair a weekend hackathon team would plausibly write. person
// is what a sensible person would say (true = the same work, worth asking);
// rule is what §4.2's rules actually do. The test pins the rules, so a change
// in either direction is visible; a row where the two disagree is logged as a
// FINDING and reported, never bent to agree.
type dupOwnPair struct {
	a, b    string
	project string
	overlap bool
	person  bool
	rule    string // "" = not a candidate
	note    string
}

var dupOwnPairs = []dupOwnPair{
	// A person says: the same work.
	{a: "google oauth login", b: "sign in with google", person: true, rule: "words"},
	{a: "user profile page", b: "profile screen", person: true, rule: "words"},
	{a: "stripe payments", b: "payment integration with stripe", person: true, rule: "words"},
	{a: "file upload", b: "upload files to s3", person: true, rule: "words"},
	{a: "readme", b: "write the README", person: true, rule: "words_single"},
	{a: "landing page", b: "build the landing page hero", person: true, rule: "words"},
	{a: "password reset flow", b: "forgot password page", person: true, rule: "",
		note: "one shared key (passw); 'reset' and 'forgot' name the same page but share no key"},
	{a: "set up the database", b: "database schema and migrations", person: true, rule: "",
		note: "both fold to the one layer key schem, so C = 0 and no rule can fire"},
	{a: "CI pipeline", b: "github actions ci", person: true, rule: "",
		note: "one shared key (ci); 'pipeline' and 'github actions' are the same thing, no synonym row"},
	{a: "add dark mode", b: "light and dark theme switcher", person: true, rule: "",
		note: "one shared key (dark); 'mode' vs 'theme switcher'"},
	{a: "realtime updates with websockets", b: "live updates via socket.io", person: true, rule: "",
		note: "'update' is stoplisted; websockets (webso) and socket (socke) have different 5-rune keys"},
	// A person says: related, but different work.
	{a: "stripe checkout page", b: "stripe checkout endpoint", person: false, rule: ""},
	{a: "chat message schema", b: "chat message list ui", person: false, rule: ""},
	{a: "leaderboard page", b: "leaderboard scoring logic", person: false, rule: ""},
	{a: "deploy frontend to vercel", b: "deploy backend to render", person: false, rule: ""},
	{a: "email notifications", b: "email login", person: false, rule: ""},
	{a: "add comments to posts", b: "add likes to posts", person: false, rule: ""},
	{a: "test the signup user flow", b: "test the checkout user flow", person: false, rule: ""},
	{a: "login endpoint", b: "logout endpoint", person: false, rule: ""},
	{a: "unit tests for the api", b: "api rate limiting", person: false, rule: ""},
	{a: "mobile responsive navbar", b: "navbar logo", person: false, rule: ""},
	{a: "image upload", b: "image resizing", person: false, rule: ""},
	{a: "login page tests", b: "login page", person: false, rule: "words",
		note: "'test' is vague, which blocks only the single-concept rule; login+page still meet words 2/2"},
	{a: "dark mode toggle", b: "dark mode colors for charts", person: false, rule: "words",
		note: "dark+mode is 2 of 3 keys (0.67): the toggle and the chart palette share their subject only"},
}

func TestDuplicateOwnHackathonPairs(t *testing.T) {
	for i, p := range dupOwnPairs {
		t.Run(dupName("own", i+1, p.a, p.b), func(t *testing.T) {
			a, b, s := scorePair(dupVector{a: p.a, b: p.b, project: p.project, overlap: p.overlap})
			verdict := "agrees with a person"
			if s.Candidate != p.person {
				verdict = "FINDING: disagrees with a person (" + p.note + ")"
			}
			t.Logf("keys A %v | keys B %v | shared %v core %d ratio %.2f -> candidate=%v rule=%q; person says duplicate=%v; %s",
				a.Keys, b.Keys, s.Shared, s.Core, s.Ratio, s.Candidate, s.Rule, p.person, verdict)
			if s.Rule != p.rule {
				t.Errorf("%q vs %q: rule %q, pinned %q", p.a, p.b, s.Rule, p.rule)
			}
			if (p.rule != "") == p.person && p.note != "" {
				t.Errorf("row has a finding note but rules and person agree")
			}
		})
	}
}

// ─────────────────────────────────────────────
// DuplicateTermsOf
// ─────────────────────────────────────────────

func TestDuplicateTermsPhraseJoining(t *testing.T) {
	cases := []struct {
		in   string
		keys []string
		word map[string]string
	}{
		{"sign in", []string{"login"}, map[string]string{"login": "signin"}},
		{"Sign-In page", []string{"login", "page"}, map[string]string{"login": "signin"}},
		{"SignIn", []string{"login"}, nil},
		{"log in", []string{"login"}, nil},
		{"sign up", []string{"signu"}, map[string]string{"signu": "signup"}},
		{"sign out", []string{"logou"}, map[string]string{"logou": "signout"}},
		{"log out", []string{"logou"}, map[string]string{"logou": "logout"}},
		{"set up docker", []string{"docke"}, nil}, // joined to setup, then stoplisted
		{"check out cart", []string{"check", "cart"}, map[string]string{"check": "checkout"}},
		{"signing in", []string{"signi"}, nil}, // a whole-word join only
		{"catalog in view", []string{"catal", "page"}, nil},
	}
	for _, tc := range cases {
		got := DuplicateTermsOf(tc.in, "")
		if !reflect.DeepEqual(got.Keys, tc.keys) {
			t.Errorf("DuplicateTermsOf(%q).Keys = %v, want %v", tc.in, got.Keys, tc.keys)
		}
		for k, w := range tc.word {
			if got.Words[k] != w {
				t.Errorf("DuplicateTermsOf(%q).Words[%q] = %q, want %q", tc.in, k, got.Words[k], w)
			}
		}
	}
}

func TestDuplicateTermsStoplist(t *testing.T) {
	stop := "build create implement write wire hook fix update handle support improve refactor clean cleanup setup set get start finish do try quick initial first basic simple"
	got := DuplicateTermsOf(stop+" widget", "")
	if !reflect.DeepEqual(got.Keys, []string{"widge"}) {
		t.Fatalf("stoplist leaked: %v", got.Keys)
	}
	for _, w := range strings.Fields(stop) {
		if keys := DuplicateTermsOf(w, "").Keys; len(keys) != 0 {
			t.Errorf("%q not dropped: %v", w, keys)
		}
	}
	// Tokenize's own stopwords (add, new, make, use) go too.
	if keys := DuplicateTermsOf("add new make use", "").Keys; len(keys) != 0 {
		t.Errorf("Tokenize stopwords leaked: %v", keys)
	}
	// Plurals of the verbs fold first.
	if keys := DuplicateTermsOf("builds updates", "").Keys; len(keys) != 0 {
		t.Errorf("plural verbs leaked: %v", keys)
	}
}

func TestDuplicateTermsProjectKeyDropped(t *testing.T) {
	got := DuplicateTermsOf("share recipes page", "recipe-share")
	if !reflect.DeepEqual(got.Keys, []string{"page"}) {
		t.Errorf("project key tokens not dropped: %v", got.Keys)
	}
	if got := DuplicateTermsOf("shop checkout", "shop"); !reflect.DeepEqual(got.Keys, []string{"check"}) {
		t.Errorf("shop checkout in shop = %v, want [check]", got.Keys)
	}
	if got := DuplicateTermsOf("shop checkout", ""); !reflect.DeepEqual(got.Keys, []string{"shop", "check"}) {
		t.Errorf("without a project key = %v", got.Keys)
	}
}

func TestDuplicateTermsEverySynonymRow(t *testing.T) {
	rows := []struct {
		key   string
		layer Layer
		words []string
	}{
		{"page", LayerUI, []string{"page", "screen", "view", "ui", "frontend", "form", "modal", "dialog", "screens"}},
		{"endpo", LayerAPI, []string{"endpoint", "route", "api", "handler", "backend", "server", "controller", "routes"}},
		{"schem", LayerData, []string{"schema", "table", "migration", "database", "db", "sql", "migrations"}},
		{"login", "", []string{"login", "signin", "logon"}},
		{"signu", "", []string{"signup", "register", "registration"}},
		{"logou", "", []string{"logout", "signout"}},
		{"auth", "", []string{"auth", "authentication", "authn"}},
		{"image", "", []string{"image", "img", "picture", "photo", "photos"}},
		{"butto", "", []string{"button", "btn"}},
	}
	for _, row := range rows {
		for _, w := range row.words {
			got := DuplicateTermsOf(w, "")
			if !reflect.DeepEqual(got.Keys, []string{row.key}) {
				t.Errorf("%q folds to %v, want [%s]", w, got.Keys, row.key)
				continue
			}
			if got.Layers[row.key] != row.layer {
				t.Errorf("%q layer %q, want %q", w, got.Layers[row.key], row.layer)
			}
			if row.layer == "" && len(got.Layers) != 0 {
				t.Errorf("%q must not be a layer word: %v", w, got.Layers)
			}
		}
	}
}

func TestDuplicateTermsFiveRuneKey(t *testing.T) {
	pairs := [][2]string{
		{"summary", "summarize"},
		{"deploy", "deployment"},
		{"notify", "notification"},
		{"websocket", "websockets"},
	}
	for _, p := range pairs {
		a, b := DuplicateTermsOf(p[0], "").Keys, DuplicateTermsOf(p[1], "").Keys
		if len(a) != 1 || !reflect.DeepEqual(a, b) {
			t.Errorf("%q %v and %q %v must share one key", p[0], a, p[1], b)
		}
		if len([]rune(a[0])) != DuplicateKeyRunes {
			t.Errorf("key %q is not %d runes", a[0], DuplicateKeyRunes)
		}
	}
	if got := DuplicateTermsOf("map", "").Keys; !reflect.DeepEqual(got, []string{"map"}) {
		t.Errorf("a short word is its own key: %v", got)
	}
	if got := DuplicateTermsOf("überschrift", "").Keys; !reflect.DeepEqual(got, []string{"übers"}) {
		t.Errorf("keys are cut on runes, not bytes: %v", got)
	}
}

func TestDuplicateTermsVagueSet(t *testing.T) {
	vague := "test bug issue error feature stuff thing code work task part flow demo mvp app user style lint typo doc"
	for _, w := range append(strings.Fields(vague), "tests", "bugs", "issues", "features", "styles", "docs", "users") {
		got := DuplicateTermsOf(w, "")
		if len(got.Keys) != 1 || !got.Vague[got.Keys[0]] {
			t.Errorf("%q: keys %v vague %v, want one vague key", w, got.Keys, got.Vague)
		}
	}
	for _, w := range []string{"styling", "testing", "documentation", "login"} {
		got := DuplicateTermsOf(w, "")
		if len(got.Keys) != 1 || got.Vague[got.Keys[0]] {
			t.Errorf("%q: keys %v vague %v, want one non-vague key", w, got.Keys, got.Vague)
		}
	}
	// Vague keys stay in the set.
	if got := DuplicateTermsOf("fix login bug", ""); !reflect.DeepEqual(got.Keys, []string{"login", "bug"}) {
		t.Errorf("vague key dropped from the set: %v", got.Keys)
	}
}

func TestDuplicateTermsWordsKeepOriginals(t *testing.T) {
	got := DuplicateTermsOf("build the login screen", "")
	if got.Words["login"] != "login" || got.Words["page"] != "screen" {
		t.Errorf("Words = %v, want login->login, page->screen", got.Words)
	}
	// First seen wins.
	got = DuplicateTermsOf("login view and page", "")
	if got.Words["page"] != "view" || !reflect.DeepEqual(got.Keys, []string{"login", "page"}) {
		t.Errorf("first-seen word: %v %v", got.Keys, got.Words)
	}
	if w := DuplicateTermsOf("summarize notes", "").Words["summa"]; w != "summarize" {
		t.Errorf("Words[summa] = %q", w)
	}
}

func TestDuplicateTermsDeterministic(t *testing.T) {
	in := "Add POST /api/login: password check, mint session cookie, wire the handler"
	first := DuplicateTermsOf(in, "shop")
	for i := 0; i < 20; i++ {
		if again := DuplicateTermsOf(in, "shop"); !reflect.DeepEqual(again, first) {
			t.Fatalf("run %d: %+v, want %+v", i, again, first)
		}
	}
	empty := DuplicateTermsOf("   ", "")
	if len(empty.Keys) != 0 || empty.Words == nil || empty.Vague == nil || empty.Layers == nil {
		t.Errorf("blank summary: %+v; want no keys and non-nil maps", empty)
	}
}

// ─────────────────────────────────────────────
// FrequentTerms
// ─────────────────────────────────────────────

func termsWith(n int, key string, withKey int) []DuplicateTerms {
	out := make([]DuplicateTerms, n)
	for i := range out {
		keys := []string{fmt.Sprintf("uniq%d", i)}
		if i < withKey {
			keys = append(keys, key)
		}
		out[i] = DuplicateTerms{Keys: keys}
	}
	return out
}

func TestFrequentTermsBelowMinScanned(t *testing.T) {
	if got := FrequentTerms(nil); got != nil {
		t.Errorf("nil scan: %v", got)
	}
	// Seven plans all saying login: a hackathon project never filters.
	if got := FrequentTerms(termsWith(7, "login", 7)); got != nil {
		t.Errorf("7 summaries must not filter: %v", got)
	}
	five := []DuplicateTerms{}
	for _, s := range []string{"add login page", "login endpoint", "login tests", "login bug", "build the login screen"} {
		five = append(five, DuplicateTermsOf(s, ""))
	}
	if got := FrequentTerms(five); got != nil {
		t.Errorf("five live plans never filter: %v", got)
	}
}

func TestFrequentTermsBar(t *testing.T) {
	// n = 8: the bar is max(4, 3) = 4.
	if got := FrequentTerms(termsWith(8, "login", 4)); !got["login"] {
		t.Errorf("n=8, key in 4: want dropped, got %v", got)
	}
	// n = 9: the bar is max(4, 3) = 4.
	if got := FrequentTerms(termsWith(9, "login", 4)); !got["login"] {
		t.Errorf("n=9, key in 4: want dropped, got %v", got)
	}
	if got := FrequentTerms(termsWith(9, "login", 3)); got["login"] {
		t.Errorf("n=9, key in 3: want kept, got %v", got)
	}
	// n = 30: the bar is ⌈30/3⌉ = 10.
	if got := FrequentTerms(termsWith(30, "login", 10)); !got["login"] {
		t.Errorf("n=30, key in 10: want dropped, got %v", got)
	}
	if got := FrequentTerms(termsWith(30, "login", 9)); got["login"] {
		t.Errorf("n=30, key in 9: want kept, got %v", got)
	}
	// n = 31: ⌈31/3⌉ = 11.
	if got := FrequentTerms(termsWith(31, "login", 10)); got["login"] {
		t.Errorf("n=31, key in 10: want kept (bar 11), got %v", got)
	}
	// A key repeated inside one summary counts once.
	all := termsWith(9, "login", 3)
	all[0].Keys = append(all[0].Keys, "login", "login")
	if got := FrequentTerms(all); got["login"] {
		t.Errorf("repeats within one summary counted twice: %v", got)
	}
}

// The filter in use: a sprint project's domain word stops matching.
func TestFrequentTermsInScoring(t *testing.T) {
	summaries := []string{
		"invoice pdf export", "invoice email reminders", "invoice list page", "invoice totals rounding",
		"invoice currency", "invoice search", "invoice pdf styling", "invoice tax rates", "invoice drafts",
	}
	var all []DuplicateTerms
	for _, s := range summaries {
		all = append(all, DuplicateTermsOf(s, ""))
	}
	drop := FrequentTerms(all)
	if !drop["invoi"] {
		t.Fatalf("the domain word must be dropped: %v", drop)
	}
	a, b := DuplicateTermsOf("invoices", ""), DuplicateTermsOf("invoice drafts", "")
	if s := ScoreDuplicate(a, b, nil, false); s.Rule != "words_single" {
		t.Fatalf("without the filter these pair on the domain word alone: %+v", s)
	}
	if s := ScoreDuplicate(a, b, drop, false); s.Candidate {
		t.Errorf("with the filter the domain word must not pair them: %+v", s)
	}
	// Real overlap beyond the domain word still pairs.
	if s := ScoreDuplicate(DuplicateTermsOf("invoice pdf export", ""), DuplicateTermsOf("export invoice as pdf", ""), drop, false); !s.Candidate {
		t.Errorf("pdf+export must still pair after the filter: %+v", s)
	}
}

// ─────────────────────────────────────────────
// ScoreDuplicate boundaries
// ─────────────────────────────────────────────

// synth builds two terms sharing `shared` keys, the first `layerShared` of
// them ui layer words, plus extraMine / extraTheirs keys of their own.
func synth(shared, layerShared, extraMine, extraTheirs int) (DuplicateTerms, DuplicateTerms) {
	mk := func(side string, extra int) DuplicateTerms {
		t := DuplicateTerms{Words: map[string]string{}, Vague: map[string]bool{}, Layers: map[string]Layer{}}
		for i := 0; i < shared; i++ {
			k := fmt.Sprintf("s%02d", i)
			t.Keys = append(t.Keys, k)
			if i < layerShared {
				t.Layers[k] = LayerUI
			}
		}
		for i := 0; i < extra; i++ {
			t.Keys = append(t.Keys, fmt.Sprintf("%s%02d", side, i))
		}
		return t
	}
	return mk("m", extraMine), mk("t", extraTheirs)
}

func TestScoreDuplicateBoundaries(t *testing.T) {
	cases := []struct {
		name                    string
		shared, layer, exM, exT int
		overlap                 bool
		wantRule                string
		wantRatio               float64
	}{
		// words: C ≥ 1, S ≥ 2, S/m ≥ 0.6.
		{"words 0.60 (3/5, C 2)", 3, 1, 2, 2, false, "words", 0.6},
		{"words 0.59 (13/22, C 2)", 13, 11, 9, 9, false, "", 13.0 / 22},
		{"words S 2 of 2", 2, 1, 0, 0, false, "words", 1},
		// long: C ≥ 3, S/m ≥ 0.4.
		{"long 0.40 (4/10, C 4)", 4, 0, 6, 6, false, "words_long", 0.4},
		{"long 0.39 (7/18, C 7)", 7, 0, 11, 11, false, "", 7.0 / 18},
		{"long C 3 (3/7)", 3, 0, 4, 4, false, "words_long", 3.0 / 7},
		{"long C 2 (3/7, one layer)", 3, 1, 4, 4, false, "", 3.0 / 7},
		// single concept: one side exactly the key, the other ≤ 2 non-vague.
		{"single, other side 2", 1, 0, 0, 1, false, "words_single", 1},
		{"single, other side 3", 1, 0, 0, 2, false, "", 1},
		{"single, flipped sides", 1, 0, 1, 0, false, "words_single", 1},
		// words_and_paths: overlap, C ≥ 1, S/m ≥ 0.5.
		{"paths 0.50 (1/2)", 1, 0, 1, 1, true, "words_and_paths", 0.5},
		{"paths 0.50 without overlap", 1, 0, 1, 1, false, "", 0.5},
		{"paths 0.49 (24/49, C 1)", 24, 23, 25, 25, true, "", 24.0 / 49},
		{"paths 0.33 (1/3)", 1, 0, 2, 2, true, "", 1.0 / 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := synth(tc.shared, tc.layer, tc.exM, tc.exT)
			s := ScoreDuplicate(a, b, nil, tc.overlap)
			t.Logf("shared %d core %d ratio %.4f -> %q", len(s.Shared), s.Core, s.Ratio, s.Rule)
			if s.Rule != tc.wantRule || s.Candidate != (tc.wantRule != "") {
				t.Errorf("rule %q candidate %v, want %q", s.Rule, s.Candidate, tc.wantRule)
			}
			if s.Ratio != tc.wantRatio {
				t.Errorf("ratio %v, want %v", s.Ratio, tc.wantRatio)
			}
			if s.Core != tc.shared-tc.layer || len(s.Shared) != tc.shared {
				t.Errorf("core %d shared %d, want %d %d", s.Core, len(s.Shared), tc.shared-tc.layer, tc.shared)
			}
		})
	}
}

func TestScoreDuplicateSingleConceptVague(t *testing.T) {
	// Vague keys on the other side do not count towards its 2.
	a := DuplicateTermsOf("navbar", "")
	b := DuplicateTermsOf("navbar mobile bug demo", "")
	if s := ScoreDuplicate(a, b, nil, false); s.Rule != "words_single" {
		t.Errorf("vague keys counted towards the other side's 2: %+v", s)
	}
	// A vague key on the lone side blocks it ("add tests for auth").
	a = DuplicateTermsOf("auth tests", "")
	b = DuplicateTermsOf("auth", "")
	if s := ScoreDuplicate(b, a, nil, false); s.Rule != "words_single" {
		t.Errorf("'auth' alone vs 'auth tests' is single concept from the lone side: %+v", s)
	}
	a = DuplicateTermsOf("add tests for auth", "")
	b = DuplicateTermsOf("auth middleware", "")
	if s := ScoreDuplicate(a, b, nil, false); s.Candidate {
		t.Errorf("the vague key must block single concept: %+v", s)
	}
}

func TestScoreDuplicateNoNonVagueKeys(t *testing.T) {
	cases := [][2]string{
		{"fix bugs", "fix bugs"},
		{"demo stuff", "login page bug"},
		{"", "login page"},
	}
	for _, c := range cases {
		s := ScoreDuplicate(DuplicateTermsOf(c[0], ""), DuplicateTermsOf(c[1], ""), nil, true)
		if s.Candidate || s.Ratio != 0 {
			t.Errorf("%q / %q: m = 0 must never be a candidate: %+v", c[0], c[1], s)
		}
	}
}

func TestScoreDuplicateVagueNeverShared(t *testing.T) {
	a := DuplicateTermsOf("fix login bug", "")
	b := DuplicateTermsOf("login bug", "")
	s := ScoreDuplicate(a, b, nil, false)
	if !reflect.DeepEqual(s.Shared, []string{"login"}) {
		t.Errorf("shared %v, want [login]: bug is vague", s.Shared)
	}
}

func TestScoreDuplicateSharedInMinesOrder(t *testing.T) {
	a := DuplicateTermsOf("upload profile pictures", "")
	b := DuplicateTermsOf("profile photo upload", "")
	if s := ScoreDuplicate(a, b, nil, false); !reflect.DeepEqual(s.Shared, []string{"uploa", "profi", "image"}) {
		t.Errorf("shared %v, want mine's order", s.Shared)
	}
	if s := ScoreDuplicate(b, a, nil, false); !reflect.DeepEqual(s.Shared, []string{"profi", "image", "uploa"}) {
		t.Errorf("flipped shared %v, want mine's order", s.Shared)
	}
}

// The layer guard reads each plan's layers before the frequency filter, so a
// project where "page" is frequent still does not pair a page with an endpoint.
func TestScoreDuplicateLayerGuardSurvivesTheFilter(t *testing.T) {
	a, b := DuplicateTermsOf("login page", ""), DuplicateTermsOf("login endpoint", "")
	drop := map[string]bool{"page": true}
	if s := ScoreDuplicate(a, b, drop, false); s.Candidate {
		t.Errorf("page dropped as frequent: the guard must still hold: %+v", s)
	}
	if s := ScoreDuplicate(b, a, drop, true); s.Candidate {
		t.Errorf("flipped with overlap: %+v", s)
	}
	// Shared layers are no obstacle: two pages, one api and ui plan.
	a, b = DuplicateTermsOf("login page and api", ""), DuplicateTermsOf("login screen", "")
	if s := ScoreDuplicate(a, b, nil, false); !s.Candidate {
		t.Errorf("layer sets that intersect must not be guarded: %+v", s)
	}
}

// ─────────────────────────────────────────────
// WordingKey / WordingChanged
// ─────────────────────────────────────────────

func TestWordingKeyFormat(t *testing.T) {
	if got, want := WordingKey("Add the Login Pages!", "  ISSUE-412 "), "login page\x00issue-412"; got != want {
		t.Errorf("WordingKey = %q, want %q", got, want)
	}
}

func TestWordingChanged(t *testing.T) {
	base := "add login page"
	cosmetic := []string{"Add Login Page", "add login page.", "add login pages", "add the login page", "  add   login-page  ", "ADD LOGIN PAGE!!"}
	for _, s := range cosmetic {
		if WordingChanged(base, "", s, "") {
			t.Errorf("%q -> %q is cosmetic, not a change", base, s)
		}
	}
	if !WordingChanged(base, "", "add login page with captcha", "") {
		t.Error("a new noun is a change")
	}
	if !WordingChanged(base, "", "add signup page", "") {
		t.Error("a replaced noun is a change")
	}
	if !WordingChanged(base, "ISSUE-1", base, "ISSUE-2") || !WordingChanged(base, "", base, "ISSUE-1") {
		t.Error("an external_ref change is a change")
	}
	if WordingChanged(base, "issue-1", base, " ISSUE-1 ") {
		t.Error("external_ref case and space are cosmetic")
	}
	// Reverting is a change relative to the intermediate wording.
	mid := "wire the login page to POST /api/login"
	if !WordingChanged(base, "", mid, "") || !WordingChanged(mid, "", base, "") {
		t.Error("rewording and reverting are both changes")
	}
}

// ─────────────────────────────────────────────
// RankDuplicateCandidates
// ─────────────────────────────────────────────

func TestRankDuplicateCandidates(t *testing.T) {
	in := []DuplicateCandidate{
		{OtherIntentUUID: "u1", OtherIntentKey: "INT-9", Signal: DupSignalWords, Core: 1, Shared: 2},
		{OtherIntentUUID: "u2", OtherIntentKey: "INT-3", Signal: DupSignalWords, Core: 2, Shared: 2},
		{OtherIntentUUID: "u3", OtherIntentKey: "INT-5", Signal: DupSignalSameIssue, Core: 0, Shared: 0},
		{OtherIntentUUID: "u4", OtherIntentKey: "INT-7", Signal: DupSignalWordsAndPaths, Core: 1, Shared: 1},
		{OtherIntentUUID: "u5", OtherIntentKey: "INT-2", Signal: DupSignalWords, Core: 1, Shared: 3},
		{OtherIntentUUID: "u6", OtherIntentKey: "INT-1", Signal: DupSignalWords, Core: 1, Shared: 2},
		{OtherIntentUUID: "u0", OtherIntentKey: "INT-1", Signal: DupSignalWords, Core: 1, Shared: 2},
	}
	orig := append([]DuplicateCandidate(nil), in...)
	got := RankDuplicateCandidates(in, -1)
	var order []string
	for _, c := range got {
		order = append(order, c.OtherIntentUUID)
	}
	want := []string{"u3", "u4", "u2", "u5", "u0", "u6", "u1"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
	if !reflect.DeepEqual(in, orig) {
		t.Error("the input was modified")
	}
	if capped := RankDuplicateCandidates(in, 2); len(capped) != 2 || capped[0].OtherIntentUUID != "u3" || capped[1].OtherIntentUUID != "u4" {
		t.Errorf("cap 2 = %+v", capped)
	}
	if none := RankDuplicateCandidates(in, 0); len(none) != 0 {
		t.Errorf("cap 0 = %+v", none)
	}
	if RankDuplicateCandidates(nil, 2) != nil {
		t.Error("nil in, nil out")
	}
}

func TestDuplicateSignalString(t *testing.T) {
	for s, want := range map[DuplicateSignal]string{
		DupSignalWords: "words", DupSignalWordsAndPaths: "words_and_paths", DupSignalSameIssue: "same_issue", 0: "unknown",
	} {
		if s.String() != want {
			t.Errorf("%d.String() = %q, want %q", int(s), s.String(), want)
		}
	}
}

// ─────────────────────────────────────────────
// DuplicateSeverity (§4.4)
// ─────────────────────────────────────────────

func TestDuplicateSeverity(t *testing.T) {
	cases := []struct {
		name string
		in   DuplicateSeverityInput
		want Severity
	}{
		{"no_conflict earns nothing", DuplicateSeverityInput{Verdict: "no_conflict", Confidence: 0.99}, SeverityNone},
		{"empty verdict earns nothing", DuplicateSeverityInput{Confidence: 0.99}, SeverityNone},
		{"unsure is low", DuplicateSeverityInput{Verdict: "unsure", Confidence: 0.99, Requested: SeverityHigh}, SeverityLow},
		{"unsure with a shared issue is still low", DuplicateSeverityInput{Verdict: "unsure", Confidence: 0.99, SameIssue: true}, SeverityLow},
		{"conflict 0.69 is low", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.69, Requested: SeverityHigh}, SeverityLow},
		{"conflict 0.69 with issue is low", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.69, SameIssue: true}, SeverityLow},
		{"conflict 0.70 defaults to medium", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.70}, SeverityMedium},
		{"conflict requested low", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityLow}, SeverityLow},
		{"conflict requested high", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityHigh}, SeverityHigh},
		{"critical is clamped to high", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityCritical}, SeverityHigh},
		{"out of range is clamped to high", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: Severity(99)}, SeverityHigh},
		{"below range is clamped to low", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: Severity(-3)}, SeverityLow},
		{"issue floors low at medium", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityLow, SameIssue: true}, SeverityMedium},
		{"issue keeps high", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityHigh, SameIssue: true}, SeverityHigh},
		{"demoted rule records low", DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.9, Requested: SeverityHigh, Demoted: true}, SeverityLow},
		{"demoted no_conflict is still nothing", DuplicateSeverityInput{Verdict: "no_conflict", Confidence: 0.9, Demoted: true}, SeverityNone},
	}
	for _, tc := range cases {
		if got := DuplicateSeverity(tc.in); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// §4.4 step 5: no same-member softening. The input has no same-member field
// at all, and the decision rule's step-down (JudgedSeverity) must not leak in.
func TestDuplicateSeverityNoSameMemberSoftening(t *testing.T) {
	if _, ok := reflect.TypeOf(DuplicateSeverityInput{}).FieldByName("SameMember"); ok {
		t.Fatal("DuplicateSeverityInput must not soften for one person's two agents")
	}
	in := DuplicateSeverityInput{Verdict: "conflict", Confidence: 0.85, Requested: SeverityHigh}
	if got := DuplicateSeverity(in); got != SeverityHigh {
		t.Errorf("high stays high: %v", got)
	}
}

func TestDuplicateNoticeGrace(t *testing.T) {
	for cadence, want := range map[string]time.Duration{
		"hackathon":   2 * time.Minute,
		"sprint":      6 * time.Minute,
		"steady":      24 * time.Minute,
		"":            6 * time.Minute,
		"whatever":    6 * time.Minute,
		" Hackathon ": 2 * time.Minute,
	} {
		if got := DuplicateNoticeGrace(cadence); got != want {
			t.Errorf("DuplicateNoticeGrace(%q) = %v, want %v", cadence, got, want)
		}
	}
}
