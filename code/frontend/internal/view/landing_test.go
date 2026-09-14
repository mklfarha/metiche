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

// unbuiltKinds are the collision kinds whose tools (publish_contract,
// record_decision, report_judgement, resolve_conflict) do not exist yet. Path
// overlap is the only detection that runs today.
var unbuiltKinds = []string{
	"contract mismatch",
	"nobody is building this",
	"decision contradiction",
	"duplicate work",
}

// TestUnbuiltCollisionsAreMarkedComingNext: a collision kind that cannot fire
// today may only be named inside an element that says "Coming next". The page
// is split into its elements at every <article> and <li>, so a label that
// leaks into the board illustration, the rules list or the hero is caught.
func TestUnbuiltCollisionsAreMarkedComingNext(t *testing.T) {
	body := renderLanding(t, "/t/demo")

	blocks := regexp.MustCompile(`(?s)<article\b.*?</article>|<li\b.*?</li>`).FindAllString(body, -1)
	outside := regexp.MustCompile(`(?s)<article\b.*?</article>|<li\b.*?</li>`).ReplaceAllString(body, "")

	for _, kind := range unbuiltKinds {
		named := 0
		for _, b := range blocks {
			if !strings.Contains(strings.ToLower(stripTags(b)), kind) {
				continue
			}
			named++
			if !strings.Contains(stripTags(b), comingNext) {
				t.Errorf("%q is rendered without its %q marker: %s", kind, comingNext, strings.Join(strings.Fields(stripTags(b)), " "))
			}
		}
		if named == 0 {
			t.Errorf("%q is not on the page at all; it should be shown as coming next", kind)
		}
		if strings.Contains(strings.ToLower(stripTags(outside)), kind) {
			t.Errorf("%q is named outside a marked card", kind)
		}
	}

	// Every card in the collisions section is either the live one or marked.
	what := landingSection(t, body, "what")
	cards := regexp.MustCompile(`(?s)<article class="([^"]*)">(.*?)</article>`).FindAllStringSubmatch(what, -1)
	if len(cards) != 4 {
		t.Fatalf("collisions section has %d cards, want 4", len(cards))
	}
	live := 0
	for _, c := range cards {
		class, inner := c[1], stripTags(c[2])
		switch {
		case strings.Contains(class, "k-path"):
			live++
			if strings.Contains(inner, comingNext) || strings.Contains(class, "soon") {
				t.Errorf("path overlap works today but is marked coming next")
			}
		case !strings.Contains(class, "soon") || !strings.Contains(inner, comingNext):
			t.Errorf("unbuilt card %q lacks the soon class or its %q badge", class, comingNext)
		}
	}
	if live != 1 {
		t.Errorf("%d live collision cards, want exactly path overlap", live)
	}

	// The section must not claim four live detections.
	text := strings.ToLower(stripTags(body))
	for _, bad := range []string{"four ways", "all four", "four kinds"} {
		if strings.Contains(text, bad) {
			t.Errorf("landing still says %q", bad)
		}
	}

	// Dismissals and self-demotion need resolve_conflict.
	honest := landingSection(t, body, "honest")
	for _, li := range regexp.MustCompile(`(?s)<li\b.*?</li>`).FindAllString(honest, -1) {
		plain := strings.ToLower(stripTags(li))
		if (strings.Contains(plain, "dismiss") || strings.Contains(plain, "demote")) && !strings.Contains(stripTags(li), comingNext) {
			t.Errorf("rules bullet depends on resolve_conflict but is not marked: %s", strings.Join(strings.Fields(stripTags(li)), " "))
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
	for _, tool := range []string{"publish_contract", "record_decision", "report_judgement", "resolve_conflict"} {
		if strings.Contains(text, tool) {
			t.Errorf("landing lists %s, which does not exist yet", tool)
		}
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
