package view

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
)

func renderLanding(t *testing.T, boardURL string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Landing(boardURL).Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

var tagRE = regexp.MustCompile(`<[^>]+>`)

func stripTags(s string) string { return tagRE.ReplaceAllString(s, "") }

// landingSection returns the rendered <section id="..."> and nothing else.
func landingSection(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `id="`+id+`"`)
	if start < 0 {
		t.Fatalf("landing has no section #%s", id)
	}
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatalf("section #%s is not closed", id)
	}
	return body[start : start+end]
}

// TestInstallLineIsExactlyTheCommand: the install block is copied two ways —
// by the copy button, which takes the <pre>'s text, and by hand. Both must
// yield the command and nothing else: no prompt glyph, no stray markup.
func TestInstallLineIsExactlyTheCommand(t *testing.T) {
	plain := stripTags(installHTML)
	if plain != installCommand {
		t.Fatalf("installHTML as text = %q, want %q", plain, installCommand)
	}
	if strings.HasPrefix(strings.TrimSpace(plain), "#") || strings.HasPrefix(strings.TrimSpace(plain), "$") {
		t.Fatalf("install line carries a prompt: %q", plain)
	}

	body := renderLanding(t, "/t/demo")
	if !strings.Contains(body, `<pre id="install-cmd">`+installHTML+`</pre>`) {
		t.Fatalf("landing does not render the install line in #install-cmd")
	}
	if n := strings.Count(body, installCommand); n != 0 {
		// installHTML carries markup, so the plain command should never appear
		// verbatim; a second copy would be a second line to keep in sync.
		t.Fatalf("install command rendered as plain text %d times", n)
	}
	if n := strings.Count(body, `id="install-cmd"`); n != 1 {
		t.Fatalf("install line rendered %d times, want exactly one copyable line", n)
	}
	// The button is revealed by script; with JS off it must not be a dead control.
	if !regexp.MustCompile(`<button[^>]*data-copy="install-cmd"[^>]*\bhidden\b`).MatchString(body) {
		t.Fatalf("copy button is not shipped hidden")
	}
}

// TestLandingDescribesWhatIsDeployed pins the claims the page makes about
// setup and sign-in to what the installer and the board actually do.
func TestLandingDescribesWhatIsDeployed(t *testing.T) {
	body := renderLanding(t, "/t/demo")
	text := stripTags(body)

	// The installer writes each assistant's own token into that assistant's
	// home-directory config. There is no committed .mcp.json expanding an
	// environment variable, so the page must not show one.
	for _, bad := range []string{"${METICHE_TOKEN}", "METICHE_TOKEN", ".mcp.json", "no secret"} {
		if strings.Contains(text, bad) {
			t.Errorf("landing still shows %q", bad)
		}
	}
	if !strings.Contains(text, `"Bearer mtk_…"`) {
		t.Errorf("setup block does not show the token as the mtk_… placeholder")
	}
	// A placeholder, never something shaped like a real token.
	if tok := regexp.MustCompile(`mtk_[A-Za-z0-9_-]{6,}`).FindString(text); tok != "" {
		t.Errorf("landing shows a token-shaped string %q", tok)
	}

	// start_session identifies the repository from git; base_commit is not
	// what the skill teaches.
	if !strings.Contains(text, "start_session(repo_url, project_key, branch") {
		t.Errorf("agent loop does not show start_session(repo_url, project_key, branch, …)")
	}
	if strings.Contains(text, "base_commit") {
		t.Errorf("agent loop still shows base_commit")
	}

	// /join opens demo teams only (web.Server.joinCode), so the landing page
	// must not offer a code box that promises to open a real team's board.
	if regexp.MustCompile(`<form[^>]*action="/join"`).MatchString(body) || strings.Contains(body, `name="code"`) {
		t.Errorf("landing still carries a join-code form")
	}
	if strings.Contains(text, "Paste it to open") || strings.Contains(text, "Already have a code") {
		t.Errorf("landing still promises that a pasted code opens your team's board")
	}
	if !strings.Contains(text, "Open your board signed in") || !strings.Contains(text, "a 404") {
		t.Errorf("join section does not describe signed-in board access")
	}

	// The board has no write controls; nobody dismisses a conflict from it.
	if strings.Contains(text, "dismiss one") {
		t.Errorf("board section still says a person can dismiss a conflict")
	}

	// The demo link in the sign-in box follows boardURL, and goes with it.
	if !strings.Contains(body, `href="/t/demo">Just looking?`) {
		t.Errorf("sign-in box has no demo link")
	}
	if strings.Contains(renderLanding(t, ""), "Just looking?") {
		t.Errorf("demo link rendered with no demo board")
	}
}

// TestLandingLinksToTheDocs: the landing nav and footer name the docs, and the
// "manual route" link goes to the docs' Getting started page on this site
// rather than to a markdown file on GitHub.
func TestLandingLinksToTheDocs(t *testing.T) {
	for _, boardURL := range []string{"/t/demo", ""} {
		body := renderLanding(t, boardURL)

		nav := regexp.MustCompile(`(?s)<nav class="lp-nav">.*?</nav>`).FindString(body)
		if !strings.Contains(nav, `<a href="/docs">Docs</a>`) {
			t.Errorf("boardURL=%q: landing nav has no Docs link", boardURL)
		}
		foot := regexp.MustCompile(`(?s)<footer class="lp-foot">.*?</footer>`).FindString(body)
		if !strings.Contains(foot, `<a href="/docs">Docs</a>`) {
			t.Errorf("boardURL=%q: landing footer has no Docs link", boardURL)
		}

		join := landingSection(t, body, "join")
		if !regexp.MustCompile(`<a href="/docs/getting-started">The manual route is written down\b`).MatchString(join) {
			t.Errorf("boardURL=%q: \"The manual route is written down\" does not link to /docs/getting-started", boardURL)
		}
		if strings.Contains(body, "ONBOARDING.md") {
			t.Errorf("boardURL=%q: landing still links to docs/ONBOARDING.md", boardURL)
		}
	}
}

// comingNext is the marker every not-yet-built feature carries.
const comingNext = "Coming next"

// unbuilt are the things that cannot happen today: dismissals need
// resolve_conflict, which is not on the server. Every collision kind runs
// today — path overlap, contract mismatch ("nobody is building this"
// included), decision contradiction and duplicate work.
var unbuilt = []string{"dismiss", "demote"}

// liveKinds are the collision cards, by class, each of which must be marked as
// working today.
var liveKinds = []string{"k-path", "k-contract", "k-decision", "k-dup"}

// TestUnbuiltThingsAreMarkedComingNext: anything that cannot happen today may
// only be named inside an element that says "Coming next". The page is split
// into its elements at every <article>, <li> and <p>, so a claim that leaks
// into a card, the rules list or the tool list is caught.
func TestUnbuiltThingsAreMarkedComingNext(t *testing.T) {
	body := renderLanding(t, "/t/demo")

	blocks := regexp.MustCompile(`(?s)<article\b.*?</article>|<li\b.*?</li>|<p\b.*?</p>`).FindAllString(body, -1)
	for _, word := range unbuilt {
		for _, b := range blocks {
			plain := stripTags(b)
			if strings.Contains(strings.ToLower(plain), word) && !strings.Contains(plain, comingNext) {
				t.Errorf("%q is rendered without its %q marker: %s", word, comingNext, strings.Join(strings.Fields(plain), " "))
			}
		}
	}
	// Dismissals are still on the page, as coming next, in the tool list and
	// the rules.
	if !strings.Contains(stripTags(landingSection(t, body, "honest")), "Dismissals") {
		t.Errorf("the rules no longer say dismissals are coming next")
	}

	// Every card in the collisions section is live, and none is marked soon.
	what := landingSection(t, body, "what")
	if strings.Contains(what, comingNext) || strings.Contains(what, "soon") {
		t.Errorf("the collisions section still marks something as not built")
	}
	cards := regexp.MustCompile(`(?s)<article class="([^"]*)">(.*?)</article>`).FindAllStringSubmatch(what, -1)
	if len(cards) != len(liveKinds) {
		t.Fatalf("collisions section has %d cards, want %d", len(cards), len(liveKinds))
	}
	seen := map[string]bool{}
	for _, c := range cards {
		class, inner := c[1], stripTags(c[2])
		one := strings.Join(strings.Fields(inner), " ")
		kind := ""
		for _, k := range liveKinds {
			if strings.Contains(class, k) {
				kind = k
			}
		}
		if kind == "" {
			t.Errorf("unexpected collision card %q", class)
			continue
		}
		seen[kind] = true
		if !strings.Contains(inner, "works today") {
			t.Errorf("%q works today but is not marked live: %s", class, one)
		}
		low := strings.ToLower(inner)
		switch kind {
		case "k-contract":
			if !strings.Contains(low, "nobody is building this") || !strings.Contains(inner, "is told") {
				t.Errorf("the contract card does not say, in the present tense, that nobody is building this is caught: %s", one)
			}
		case "k-decision", "k-dup":
			// A judgement is made by the plan's own agent, in its own model,
			// never by the server. The card has to say both, because "metiche
			// judges your plan" would imply a model and a key here.
			if !strings.Contains(low, "its own model") {
				t.Errorf("%s does not say the plan's own agent judges it with its own model: %s", kind, one)
			}
			if !strings.Contains(low, "never judges") || !strings.Contains(low, "no model") {
				t.Errorf("%s does not say the server never judges and has no model: %s", kind, one)
			}
		}
		if kind != "k-dup" {
			continue
		}
		// Duplicate work is written for a hackathon: no ticket, two short
		// plans in different words, caught from the wording alone, and the
		// issue id only the strongest signal when there is one.
		for _, want := range []string{"add login page", "build the login screen", "wording alone", "strongest signal", "a person is asked only when"} {
			if !strings.Contains(low, want) {
				t.Errorf("the duplicate card does not say %q: %s", want, one)
			}
		}
		for _, bad := range []string{" will ", "same issue and tell both"} {
			if strings.Contains(low, bad) {
				t.Errorf("the duplicate card is not in the present tense (%q): %s", bad, one)
			}
		}
	}
	for _, k := range liveKinds {
		if !seen[k] {
			t.Errorf("no %s card in the collisions section", k)
		}
	}
}

// TestLandingHasNoToolCount: the tool list grows, so the page describes it by
// group and never by number.
func TestLandingHasNoToolCount(t *testing.T) {
	text := strings.ToLower(stripTags(renderLanding(t, "/t/demo")))
	count := regexp.MustCompile(`\b(\d+|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty)\s+(mcp\s+)?tools\b`)
	if m := count.FindString(text); m != "" {
		t.Errorf("landing hard-codes a tool count: %q", m)
	}
	// Unbuilt tools must not be listed as if they were available.
	if !strings.Contains(text, "publish_contract") {
		t.Errorf("landing does not list publish_contract, which is built")
	}
	for _, tool := range []string{"record_decision", "get_review_context", "report_judgement"} {
		if !strings.Contains(text, tool) {
			t.Errorf("landing does not list %s, which is built", tool)
		}
	}
	if strings.Contains(text, "resolve_conflict") {
		t.Errorf("landing lists resolve_conflict, which does not exist yet")
	}
}

// TestJoinStepsAreCreateInviteJoinOpen: the join section is numbered steps in
// that order, the invite step names create_invite, and the only code shown is
// a placeholder.
func TestJoinStepsAreCreateInviteJoinOpen(t *testing.T) {
	join := landingSection(t, renderLanding(t, "/t/demo"), "join")

	steps := regexp.MustCompile(`(?s)<li class="jstep">(.*?)</li>`).FindAllStringSubmatch(join, -1)
	if len(steps) < 3 || len(steps) > 4 {
		t.Fatalf("join section has %d numbered steps, want 3 or 4", len(steps))
	}
	if !strings.Contains(join, `<ol class="jsteps">`) {
		t.Errorf("join steps are not an ordered list")
	}
	want := []string{"create", "invite", "join", "board"}
	for i, s := range steps {
		head := strings.ToLower(stripTags(regexp.MustCompile(`(?s)<h3>(.*?)</h3>`).FindString(s[1])))
		if !strings.Contains(head, want[i]) {
			t.Errorf("step %d heading %q does not mention %q", i+1, head, want[i])
		}
	}

	text := stripTags(join)
	if !strings.Contains(text, "create_invite") && !strings.Contains(strings.ToLower(text), "create an invite") {
		t.Errorf("join steps never mention create_invite or creating an invite")
	}
	if !strings.Contains(stripTags(steps[1][1]), "create_invite") {
		t.Errorf("the invite step does not name create_invite")
	}
	// Nothing shaped like a real code: placeholders are in angle brackets.
	if !strings.Contains(text, "&lt;paste the code you were sent&gt;") {
		t.Errorf("join step does not show the code as a placeholder")
	}
	if m := regexp.MustCompile(`\b[A-Z0-9]{4,}-[A-Z0-9]{4,}\b|\bmj_[A-Za-z0-9]{6,}`).FindString(text); m != "" {
		t.Errorf("join section shows a code-shaped string %q", m)
	}
}
