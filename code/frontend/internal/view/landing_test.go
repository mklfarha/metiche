package view

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
)

// TestInstallLineIsExactlyTheCommand: the install block is copied two ways —
// by the copy button, which takes the <pre>'s text, and by hand. Both must
// yield the command and nothing else: no prompt glyph, no stray markup.
func TestInstallLineIsExactlyTheCommand(t *testing.T) {
	plain := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(installHTML, "")
	if plain != installCommand {
		t.Fatalf("installHTML as text = %q, want %q", plain, installCommand)
	}
	if strings.HasPrefix(strings.TrimSpace(plain), "#") || strings.HasPrefix(strings.TrimSpace(plain), "$") {
		t.Fatalf("install line carries a prompt: %q", plain)
	}

	var buf bytes.Buffer
	if err := Landing("/t/demo").Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	if !strings.Contains(body, `<pre id="install-cmd">`+installHTML+`</pre>`) {
		t.Fatalf("landing does not render the install line in #install-cmd")
	}
	// The button is revealed by script; with JS off it must not be a dead control.
	if !regexp.MustCompile(`<button[^>]*data-copy="install-cmd"[^>]*\bhidden\b`).MatchString(body) {
		t.Fatalf("copy button is not shipped hidden")
	}
}

// TestLandingDescribesWhatIsDeployed pins the claims the page makes about
// setup and sign-in to what the installer and the board actually do.
func TestLandingDescribesWhatIsDeployed(t *testing.T) {
	render := func(boardURL string) string {
		var buf bytes.Buffer
		if err := Landing(boardURL).Render(context.Background(), &buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	body := render("/t/demo")
	text := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(body, "")

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
	if strings.Contains(render(""), "Just looking?") {
		t.Errorf("demo link rendered with no demo board")
	}
}
